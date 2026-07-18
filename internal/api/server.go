package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/email"
	"maxpilot/backend/internal/maxclient"
	"maxpilot/backend/internal/observability"
	"maxpilot/backend/internal/openaiimg"
	"maxpilot/backend/internal/openairesearch"
	"maxpilot/backend/internal/store"
	"maxpilot/backend/internal/yandexauth"
)

type YandexOAuthClient interface {
	AuthorizationURL(redirectURI, state, codeChallenge string) string
	ExchangeCode(context.Context, string, string) (string, error)
	UserInfo(context.Context, string) (yandexauth.Profile, error)
}

type AuthOptions struct {
	YandexClient           YandexOAuthClient
	RedirectURI            string
	AllowedUsers           []string
	ObservabilityAdmins    []string
	SessionTTL             time.Duration
	SecureCookies          bool
	TrustXRealIP           bool
	RateLimitAtEdge        bool
	AILimits               *AILimitOptions
	Metrics                *observability.Metrics
	MaxOwnedTeamWorkspaces int
	// WelcomeSender delivers the first-sign-in welcome email. When nil the
	// server falls back to a NoopSender so sign-in never depends on SMTP.
	WelcomeSender email.Sender
}

type Server struct {
	app                    *app.App
	logger                 *slog.Logger
	frontendOrigin         string
	webhookSecret          string
	yandexClient           YandexOAuthClient
	yandexRedirect         string
	yandexAllowed          map[string]struct{}
	observabilityAdmins    map[string]struct{}
	sessionTTL             time.Duration
	secureCookies          bool
	oauthStartLimiter      *keyedWindowLimiter
	aiLimiter              *aiRequestLimiter
	mediaUploads           *mediaUploadGate
	trustXRealIP           bool
	now                    func() time.Time
	metrics                *observability.Metrics
	activityMu             sync.Mutex
	activityDay            string
	activityUsers          map[string]struct{}
	maxOwnedTeamWorkspaces int
	welcomeSender          email.Sender
	// welcomeEmailDispatch runs the welcome-email delivery closure. Production
	// spawns a detached goroutine so it never blocks the OAuth redirect; tests
	// override it to run synchronously for deterministic assertions.
	welcomeEmailDispatch func(func())
}

type principalContextKey struct{}

func New(application *app.App, logger *slog.Logger, frontendOrigin, webhookSecret string, authOptions ...AuthOptions) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	aiLimits := DefaultAILimitOptions()
	metrics := observability.New()
	server := &Server{
		app: application, logger: logger,
		frontendOrigin: strings.TrimRight(frontendOrigin, "/"), webhookSecret: webhookSecret,
		sessionTTL:        12 * time.Hour,
		oauthStartLimiter: newKeyedWindowLimiter(12, 600, time.Minute, 4096), now: time.Now,
		mediaUploads:        newMediaUploadGate(8, 2),
		observabilityAdmins: make(map[string]struct{}), activityUsers: make(map[string]struct{}),
		maxOwnedTeamWorkspaces: 5,
		welcomeSender:          email.NewNoopSender(logger),
		welcomeEmailDispatch:   func(deliver func()) { go deliver() },
	}
	if len(authOptions) != 0 {
		options := authOptions[0]
		server.yandexClient = options.YandexClient
		server.yandexRedirect = strings.TrimSpace(options.RedirectURI)
		server.yandexAllowed = make(map[string]struct{}, len(options.AllowedUsers))
		for _, user := range options.AllowedUsers {
			if normalized := strings.ToLower(strings.TrimSpace(user)); normalized != "" {
				server.yandexAllowed[normalized] = struct{}{}
			}
		}
		for _, user := range options.ObservabilityAdmins {
			if normalized := strings.ToLower(strings.TrimSpace(user)); normalized != "" {
				server.observabilityAdmins[normalized] = struct{}{}
			}
		}
		if options.SessionTTL > 0 {
			server.sessionTTL = options.SessionTTL
		}
		server.secureCookies = options.SecureCookies
		server.trustXRealIP = options.TrustXRealIP
		if options.RateLimitAtEdge {
			server.oauthStartLimiter = newKeyedWindowLimiter(0, 600, time.Minute, 0)
		}
		if options.AILimits != nil {
			aiLimits = *options.AILimits
		}
		if options.Metrics != nil {
			metrics = options.Metrics
		}
		if options.MaxOwnedTeamWorkspaces > 0 {
			server.maxOwnedTeamWorkspaces = options.MaxOwnedTeamWorkspaces
		}
		if options.WelcomeSender != nil {
			server.welcomeSender = options.WelcomeSender
		}
	}
	server.metrics = metrics
	server.aiLimiter = newAIRequestLimiter(application.Store(), logger, aiLimits)
	return server
}

func (s *Server) Handler() http.Handler {
	router := chi.NewRouter()
	router.Use(middleware.RequestID)
	router.Use(s.observeHTTP)
	router.Use(middleware.Recoverer)
	router.Use(s.cors)
	router.Use(s.requestLogger)
	router.Get("/metrics", s.serveMetrics)

	router.With(s.requireSession).Get("/media/{filename}", s.serveMedia)
	router.Route("/api/v1", func(r chi.Router) {
		r.Get("/health", s.health)
		r.Get("/plans", s.listPublicBillingPlans)
		r.Post("/webhooks/max", s.maxWebhook)
		r.Get("/auth/session", s.authSession)
		r.Post("/auth/yandex/start", s.startYandexAuth)
		r.Get("/auth/yandex/callback", s.finishYandexAuth)
		r.Post("/auth/max/start", s.startMAXAuth)
		r.Post("/auth/max/{request_id}/complete", s.completeMAXAuth)
		r.Delete("/auth/max/{request_id}", s.cancelMAXAuth)
		r.Post("/auth/logout", s.logout)
		r.Get("/observability/auth", s.observabilityAuth)

		r.Group(func(r chi.Router) {
			r.Use(s.requireSession)

			r.Get("/workspaces", s.listWorkspaces)
			r.Post("/workspaces", s.createWorkspace)
			r.Post("/workspace-invitations/{token}/accept", s.acceptWorkspaceInvitation)
			r.Route("/workspaces/{workspace_id}", func(r chi.Router) {
				r.Get("/", s.getWorkspace)
				r.Get("/billing", s.getWorkspaceBilling)
				r.Patch("/", s.updateWorkspace)
				r.Delete("/", s.deleteWorkspace)
				s.RegisterWorkspaceBrandRoutes(r)
				s.RegisterAnalyticsContentRoutes(r)
				s.registerCampaignRoutes(r)
				r.Post("/transfer-ownership", s.transferWorkspaceOwnership)
				r.Get("/members", s.listWorkspaceMembers)
				r.Post("/members", s.addWorkspaceMember)
				r.Patch("/members/{user_id}", s.updateWorkspaceMember)
				r.Delete("/members/{user_id}", s.removeWorkspaceMember)
				r.Get("/invitations", s.listWorkspaceInvitations)
				r.Post("/invitations", s.createWorkspaceInvitation)
				r.Delete("/invitations/{invitation_id}", s.revokeWorkspaceInvitation)
				r.Get("/audit", s.listWorkspaceAudit)
				r.Get("/channels", s.listWorkspaceChannels)
				r.Post("/channels", s.startChannelConnect)
				r.Post("/channels/connect/start", s.startChannelConnect)
				r.Get("/channels/connect/{claim_id}", s.getChannelConnect)
				r.Get("/channels/{channel_id}", s.getWorkspaceChannel)
				r.Patch("/channels/{channel_id}", s.updateWorkspaceChannel)
				r.Post("/channels/{channel_id}/max-info", s.updateWorkspaceChannelMAXInfo)
				r.Delete("/channels/{channel_id}", s.deleteWorkspaceChannel)
				r.Get("/posts", s.listWorkspacePosts)
				r.Post("/posts", s.createWorkspacePost)
				r.Post("/posts/format-content", s.formatWorkspacePostContent)
				r.Post("/posts/suggest-image-prompt", s.suggestWorkspaceImagePrompt)
				r.Post("/research/generate", s.generateWorkspaceResearch)
				r.Post("/images/generate", s.generateWorkspaceImage)
				r.Post("/media", s.uploadWorkspaceMedia)
				r.Get("/media/{filename}", s.serveWorkspaceMedia)
				r.Get("/posts/{post_id}", s.getWorkspacePost)
				r.Patch("/posts/{post_id}", s.updateWorkspacePost)
				r.Put("/posts/{post_id}", s.updateWorkspacePost)
				r.Delete("/posts/{post_id}", s.deleteWorkspacePost)
				r.Post("/posts/{post_id}/duplicate", s.duplicateWorkspacePost)
				r.Post("/posts/{post_id}/schedule", s.scheduleWorkspacePost)
				r.Delete("/posts/{post_id}/schedule", s.cancelWorkspaceSchedule)
				r.Post("/posts/{post_id}/publish", s.publishWorkspacePost)
				r.Post("/posts/{post_id}/update-published", s.updateWorkspacePublishedPost)
				r.Post("/posts/{post_id}/sync-max", s.syncWorkspaceMAXPublication)
				r.Post("/posts/{post_id}/pin", s.pinWorkspacePost)
				r.Delete("/posts/{post_id}/pin", s.unpinWorkspacePost)
				r.Delete("/posts/{post_id}/publication", s.deleteWorkspacePublication)
				r.Get("/posts/{post_id}/view-history", s.getWorkspacePostViewHistory)
				r.Post("/posts/{post_id}/image", s.uploadWorkspacePostImage)
				r.Post("/posts/{post_id}/generate-image", s.generateWorkspacePostImage)
				r.Post("/posts/{post_id}/attachments", s.uploadWorkspacePostAttachment)
				r.Put("/posts/{post_id}/attachments/{attachment_id}", s.replaceWorkspacePostAttachment)
				r.Patch("/posts/{post_id}/attachments/order", s.reorderWorkspacePostAttachments)
				r.Delete("/posts/{post_id}/attachments/{attachment_id}", s.deleteWorkspacePostAttachment)
				r.Get("/posts/{post_id}/revisions", s.listPostRevisions)
				r.Get("/posts/{post_id}/reviews", s.listPostReviews)
				r.Post("/posts/{post_id}/review", s.submitPostReview)
				r.Post("/posts/{post_id}/review/submit", s.submitPostReview)
				r.Post("/posts/{post_id}/review/approve", s.approvePostReview)
				r.Post("/posts/{post_id}/review/request-changes", s.requestPostChanges)
				r.Post("/posts/{post_id}/reviews/{revision_id}/decision", s.decidePostReview)
				r.Get("/posts/{post_id}/comments", s.listPostComments)
				r.Post("/posts/{post_id}/comments", s.createPostComment)
				r.Patch("/posts/{post_id}/comments/{comment_id}", s.resolvePostComment)
				r.Delete("/posts/{post_id}/comments/{comment_id}", s.deletePostComment)
			})
			r.Get("/notifications", s.listNotifications)
			r.Patch("/notifications", s.markAllNotificationsRead)
			r.Patch("/notifications/{notification_id}", s.markNotificationRead)

			r.Get("/channels", s.listChannels)
			r.Get("/channels/discoverable", s.listDiscoverableChannels)
			r.Post("/channels/discoverable/refresh", s.refreshDiscoverableChannels)
			r.Post("/channels/connect/observed", s.connectObservedChannel)
			r.Post("/channels/connect/start", s.startChannelConnect)
			r.Get("/channels/connect/{claim_id}", s.getChannelConnect)
			r.Get("/channels/{id}", s.getChannel)
			r.Patch("/channels/{id}", s.updateChannel)
			r.Put("/channels/{id}", s.updateChannel)
			r.Delete("/channels/{id}", s.deleteChannel)
			r.Post("/channels/{id}/max-info", s.updateChannelMAXInfo)
			r.Post("/channels/{id}/test", s.testChannel)
			r.Get("/channels/{id}/participant-history", s.getChannelParticipantHistory)
			r.Get("/analytics", s.getAnalytics)

			r.Get("/posts", s.listPosts)
			r.Post("/posts", s.createPost)
			r.Post("/posts/format-content", s.formatPostContent)
			r.Post("/posts/suggest-image-prompt", s.suggestImagePrompt)
			r.Get("/posts/{id}", s.getPost)
			r.Patch("/posts/{id}", s.updatePost)
			r.Put("/posts/{id}", s.updatePost)
			r.Delete("/posts/{id}", s.deletePost)
			r.Post("/posts/{id}/duplicate", s.duplicatePost)
			r.Post("/posts/{id}/image", s.uploadPostImage)
			r.Post("/posts/{id}/attachments", s.uploadPostAttachment)
			r.Put("/posts/{id}/attachments/{attachment_id}", s.replacePostAttachment)
			r.Patch("/posts/{id}/attachments/order", s.reorderPostAttachments)
			r.Delete("/posts/{id}/attachments/{attachment_id}", s.deletePostAttachment)
			r.Post("/posts/{id}/generate-image", s.generatePostImage)
			r.Post("/posts/{id}/schedule", s.schedulePost)
			r.Put("/posts/{id}/schedule", s.schedulePost)
			r.Post("/posts/{id}/cancel-schedule", s.cancelSchedule)
			r.Delete("/posts/{id}/schedule", s.cancelSchedule)
			r.Post("/posts/{id}/publish", s.publishPost)
			r.Post("/posts/{id}/sync", s.updatePublishedPost)
			r.Post("/posts/{id}/update-published", s.updatePublishedPost)
			r.Post("/posts/{id}/sync-max", s.syncMAXPublication)
			r.Post("/posts/{id}/pin", s.pinPost)
			r.Delete("/posts/{id}/pin", s.unpinPost)
			r.Get("/posts/{id}/view-history", s.getPostViewHistory)
			r.Post("/posts/{id}/delete-publication", s.deletePublication)
			r.Delete("/posts/{id}/publication", s.deletePublication)

			r.Post("/images/generate", s.generateImage)
			r.Post("/research/generate", s.generateResearch)
			r.Post("/media", s.uploadMedia)

			r.Get("/integration/max", s.maxIntegrationStatus)
			r.Get("/integration/max/identity", s.getMAXIdentity)
			r.Post("/integration/max/identity", s.startMAXIdentity)
			r.Get("/integrations/max", s.maxIntegrationStatus)
			r.Post("/integration/max/test", s.testMAXIntegration)
			r.Post("/integrations/max/test", s.testMAXIntegration)
		})
	})
	return router
}

func (s *Server) serveMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !isDirectPrivateRequest(r) {
		http.NotFound(w, r)
		return
	}
	s.metrics.Handler().ServeHTTP(w, r)
}

// isDirectPrivateRequest keeps metrics available to a Prometheus container on
// the private Docker network while preventing the public reverse proxy from
// turning /metrics into an unauthenticated internet endpoint. A direct scraper
// does not attach proxy identity headers.
func isDirectPrivateRequest(r *http.Request) bool {
	for _, header := range []string{"Forwarded", "X-Forwarded-For", "X-Real-IP", "CF-Connecting-IP"} {
		if strings.TrimSpace(r.Header.Get(header)) != "" {
			return false
		}
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate())
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	if err := s.app.Store().Ping(ctx); err != nil {
		s.writeError(w, err)
		return
	}
	status := s.authenticationStatus(r)
	s.writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "max_configured": s.app.MAXConfigured(), "openai_configured": s.app.OpenAIConfigured(),
		"research_configured": s.app.ResearchConfigured(), "content_formatting_configured": s.app.ContentFormattingConfigured(),
		"auth_required": status.Required, "authenticated": status.Authenticated,
		"auth_methods": status.Methods, "auth_method": status.Method, "user": status.User,
		"session_expires_at": status.SessionExpiresAt, "observability_access": status.ObservabilityAccess,
	})
}

func (s *Server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, ok := s.authenticate(r)
		if !ok || (principal.Method != "yandex" && principal.Method != "max") || principal.User == nil || principal.User.ID == "" {
			w.Header().Set("Cache-Control", "no-store")
			s.problem(w, http.StatusUnauthorized, "authentication_required", "A valid sign-in session is required", nil)
			return
		}
		ctx := context.WithValue(r.Context(), principalContextKey{}, principal)
		s.touchUserActivity(ctx, principal.User.ID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) touchUserActivity(ctx context.Context, userID string) {
	now := s.now().UTC()
	day := now.Format(time.DateOnly)
	s.activityMu.Lock()
	if s.activityDay != day {
		s.activityDay = day
		s.activityUsers = make(map[string]struct{})
	}
	_, alreadyReserved := s.activityUsers[userID]
	if !alreadyReserved {
		// Reserve before I/O so concurrent first requests cannot stampede the
		// same daily UPSERT. A failed write releases the reservation for retry.
		s.activityUsers[userID] = struct{}{}
	}
	s.activityMu.Unlock()
	if alreadyReserved {
		return
	}
	touchCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := s.app.Store().TouchUserActivity(touchCtx, userID, now); err != nil {
		s.logger.Warn("user activity aggregation failed", "error", err)
		s.activityMu.Lock()
		if s.activityDay == day {
			delete(s.activityUsers, userID)
		}
		s.activityMu.Unlock()
		return
	}
}

func authenticatedUserID(r *http.Request) (string, error) {
	principal, ok := r.Context().Value(principalContextKey{}).(authPrincipal)
	if !ok || principal.User == nil || principal.User.ID == "" {
		return "", errors.New("authenticated user is missing")
	}
	return principal.User.ID, nil
}

func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := strings.TrimRight(r.Header.Get("Origin"), "/")
		if origin != "" && origin != s.frontendOrigin {
			s.problem(w, http.StatusForbidden, "origin_not_allowed", "Origin is not allowed", nil)
			return
		}
		if origin == s.frontendOrigin && origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Accept")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Max-Age", "600")
			w.Header().Add("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if isUnsafeMethod(r.Method) && hasSessionCookie(r) && r.URL.Path != "/api/v1/webhooks/max" && origin != s.frontendOrigin {
			s.problem(w, http.StatusForbidden, "origin_required", "An exact frontend Origin is required for session requests", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		wrapped := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(wrapped, r)
		route := chi.RouteContext(r.Context()).RoutePattern()
		if route == "" {
			route = "unmatched"
		}
		s.logger.Info("http request",
			"request_id", middleware.GetReqID(r.Context()), "method", r.Method,
			"route", route, "status", wrapped.status, "duration_ms", time.Since(start).Milliseconds())
	})
}

func (s *Server) observeHTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		s.metrics.IncHTTPInFlight()
		defer s.metrics.DecHTTPInFlight()
		wrapped := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(wrapped, r)
		route := chi.RouteContext(r.Context()).RoutePattern()
		if route == "" {
			route = "unmatched"
		}
		s.metrics.ObserveHTTPRequest(r.Method, route, wrapped.status, time.Since(start))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(body []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func (s *Server) decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		s.problem(w, http.StatusBadRequest, "invalid_json", "Request body is not valid JSON", err.Error())
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		s.problem(w, http.StatusBadRequest, "invalid_json", "Request body must contain one JSON value", nil)
		return false
	}
	return true
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if status != http.StatusNoContent {
		_ = json.NewEncoder(w).Encode(value)
	}
}

func (s *Server) problem(w http.ResponseWriter, status int, code, message string, details any) {
	payload := map[string]any{"code": code, "message": message}
	if details != nil {
		payload["details"] = details
	}
	s.writeJSON(w, status, map[string]any{"error": payload})
}

func (s *Server) writeError(w http.ResponseWriter, err error) {
	if err == nil {
		return
	}
	var planInactiveErr *store.WorkspacePlanInactiveError
	if errors.As(err, &planInactiveErr) {
		w.Header().Set("Cache-Control", "no-store")
		s.problem(w, http.StatusForbidden, "plan_inactive",
			"Workspace plan is inactive.", map[string]any{"status": planInactiveErr.Status})
		return
	}
	var aiLimitErr *store.AILimitError
	if errors.As(err, &aiLimitErr) {
		retryAfter := retryAfterSeconds(aiLimitErr.RetryAfter)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Retry-After", strconv.FormatInt(retryAfter, 10))
		s.problem(w, http.StatusTooManyRequests, "ai_rate_limited", "Лимит запросов к ИИ временно исчерпан. Попробуйте позже.", map[string]any{
			"reason": aiLimitErr.Reason, "retry_after_seconds": retryAfter,
		})
		return
	}
	var statsCooldownErr *app.MAXStatsCooldownError
	if errors.As(err, &statsCooldownErr) {
		retryAfter := retryAfterSeconds(statsCooldownErr.RetryAfter)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Retry-After", strconv.FormatInt(retryAfter, 10))
		s.problem(w, http.StatusTooManyRequests, "stats_refresh_cooldown",
			"Статистика только что обновлялась. Повторите через несколько секунд.",
			map[string]any{"retry_after_seconds": retryAfter})
		return
	}
	var refreshCooldownErr *app.DiscoverableRefreshCooldownError
	if errors.As(err, &refreshCooldownErr) {
		retryAfter := retryAfterSeconds(refreshCooldownErr.RetryAfter)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Retry-After", strconv.FormatInt(retryAfter, 10))
		s.problem(w, http.StatusTooManyRequests, "channels_refresh_cooldown",
			"Список каналов недавно обновлялся. Повторите через несколько секунд.",
			map[string]any{"retry_after_seconds": retryAfter})
		return
	}
	switch {
	case errors.Is(err, app.ErrApprovalRequired):
		s.problem(w, http.StatusConflict, "post_approval_required",
			"The current post revision must be approved before scheduling or publishing.", nil)
	case errors.Is(err, app.ErrNotEnoughPostsForBrandKit):
		s.problem(w, http.StatusConflict, "not_enough_posts",
			"Недостаточно постов с текстом для автозаполнения Brand Kit. Создайте хотя бы 3 поста с текстом и попробуйте ещё раз.", nil)
	case errors.Is(err, errMediaUploadRateLimited):
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Retry-After", "1")
		s.problem(w, http.StatusTooManyRequests, "media_upload_busy",
			"Другая загрузка изображения ещё выполняется. Повторите через несколько секунд.", nil)
	case errors.Is(err, store.ErrMediaQuotaExceeded):
		s.problem(w, http.StatusRequestEntityTooLarge, "media_quota_exceeded",
			"Хранилище изображений заполнено. Удалите ненужные черновики с изображениями и попробуйте ещё раз.", nil)
	case errors.Is(err, store.ErrMediaUploadBusy):
		s.problem(w, http.StatusConflict, "media_upload_in_progress",
			"Это изображение уже загружается. Подождите несколько секунд и попробуйте ещё раз.", nil)
	case errors.Is(err, store.ErrOwnedTeamWorkspaceLimit):
		s.problem(w, http.StatusConflict, "workspace_owner_limit_reached",
			"Team workspace ownership limit reached. Transfer ownership before creating or accepting another workspace; archived workspaces still retain their storage.", nil)
	case errors.Is(err, store.ErrNotFound):
		s.problem(w, http.StatusNotFound, "not_found", "Запрошенные данные не найдены.", nil)
	case errors.Is(err, store.ErrConflict):
		s.problem(w, http.StatusConflict, "state_conflict", storeConflictMessage(err), nil)
	case errors.Is(err, app.ErrMAXNotConfigured):
		s.problem(w, http.StatusServiceUnavailable, "max_not_configured",
			"Помощник MAX пока не настроен. Обратитесь в поддержку MaxPosty.", nil)
	case errors.Is(err, app.ErrOpenAINotConfigured):
		s.problem(w, http.StatusServiceUnavailable, "openai_not_configured",
			"Функции с ИИ сейчас недоступны. Попробуйте позже.", nil)
	case errors.Is(err, app.ErrResearchNotConfigured):
		s.problem(w, http.StatusServiceUnavailable, "openai_research_not_configured",
			"Исследование с ИИ сейчас недоступно. Попробуйте позже.", nil)
	case errors.Is(err, app.ErrConflict):
		s.problem(w, http.StatusConflict, "state_conflict",
			"Данные изменились во время операции. Обновите страницу и попробуйте ещё раз.", nil)
	case errors.Is(err, context.Canceled):
		s.problem(w, 499, "request_canceled", "Операция была отменена.", nil)
	case errors.Is(err, context.DeadlineExceeded):
		s.problem(w, http.StatusGatewayTimeout, "upstream_timeout",
			"Сервис не ответил вовремя. Попробуйте ещё раз.", nil)
	default:
		var maxErr *maxclient.Error
		var openAIErr *openaiimg.Error
		var researchErr *openairesearch.Error
		var channelErr *app.ChannelAccessError
		if errors.As(err, &channelErr) {
			s.problem(w, http.StatusUnprocessableEntity, "max_channel_access",
				"Помощнику MaxPosty не хватает прав в канале. Проверьте его права администратора и повторите операцию.",
				channelErr.Diagnostics)
			return
		}
		if errors.As(err, &maxErr) {
			if maxErr.StatusCode == http.StatusBadRequest &&
				(maxErr.Code == "errors.send-message.channel-notify" || maxErr.Message == "errors.send-message.channel-notify") {
				s.logger.Warn("MAX channel notification request failed", "status", maxErr.StatusCode,
					"code", maxErr.Code, "request_id", maxErr.RequestID, "error", maxErr.Message)
				s.problem(w, http.StatusUnprocessableEntity, "max_channel_notify_unsupported",
					"MAX требует уведомлять подписчиков о новых публикациях. Уведомления включены автоматически — повторите отправку.", nil)
				return
			}
			s.logger.Warn("MAX request failed", "status", maxErr.StatusCode,
				"code", maxErr.Code, "request_id", maxErr.RequestID, "error", maxErr.Message)
			s.problem(w, http.StatusBadGateway, "max_api_error",
				"MAX не смог выполнить операцию. Попробуйте ещё раз.", nil)
			return
		}
		if errors.As(err, &openAIErr) {
			s.logger.Warn("OpenAI image request failed", "status", openAIErr.StatusCode,
				"request_id", openAIErr.RequestID, "error", openAIErr.Message)
			s.problem(w, http.StatusBadGateway, "openai_api_error",
				"Функция с ИИ сейчас недоступна. Попробуйте позже.", nil)
			return
		}
		if errors.As(err, &researchErr) {
			s.logger.Warn("OpenAI research request failed", "status", researchErr.StatusCode,
				"request_id", researchErr.RequestID, "error", researchErr.Message)
			s.problem(w, http.StatusBadGateway, "openai_research_error",
				"Исследование с ИИ сейчас недоступно. Попробуйте позже.", nil)
			return
		}
		if errors.Is(err, errValidation) || isValidationError(err) {
			s.logger.Info("request validation failed", "error", err)
			s.problem(w, http.StatusBadRequest, "validation_error",
				"Проверьте заполнение полей и попробуйте ещё раз.", nil)
			return
		}
		s.logger.Error("request failed", "error", err)
		s.problem(w, http.StatusInternalServerError, "internal_error",
			"Не удалось выполнить операцию. Попробуйте ещё раз.", nil)
	}
}

func storeConflictMessage(err error) string {
	detail := strings.ToLower(err.Error())
	switch {
	case strings.Contains(detail, "post changed in another session"):
		return "Публикация была обновлена в другой вкладке. Обновите данные и повторите сохранение."
	case strings.Contains(detail, "linked post"):
		return "Сначала удалите или перенесите публикации, связанные с этим каналом."
	default:
		return "Данные изменились во время операции. Обновите страницу и попробуйте ещё раз."
	}
}

func parseID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, validationError("id must be a positive integer")
	}
	return id, nil
}

// errValidation is a typed sentinel for client-facing validation failures.
// Handlers wrap human-readable messages with validationError so writeError
// classifies them as HTTP 400 validation_error via errors.Is instead of the
// legacy substring heuristic in isValidationError, which remains a fallback
// for errors produced by lower layers.
var errValidation = errors.New("request validation failed")

type validationFailure struct{ message string }

func (e validationFailure) Error() string { return e.message }

func (validationFailure) Is(target error) bool { return target == errValidation }

// validationError marks message as a request validation failure.
func validationError(message string) error {
	return validationFailure{message: message}
}

func isValidationError(err error) bool {
	text := strings.ToLower(err.Error())
	for _, fragment := range []string{
		"required", "must", "unsupported", "invalid", "too long", "exceed", "not scheduled",
		"only images", "does not exist", "image is", "image prompt", "channel is inactive",
	} {
		if strings.Contains(text, fragment) {
			return true
		}
	}
	return false
}
