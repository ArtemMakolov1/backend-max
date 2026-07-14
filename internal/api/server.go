package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/maxclient"
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
	YandexClient    YandexOAuthClient
	RedirectURI     string
	AllowedUsers    []string
	SessionTTL      time.Duration
	SecureCookies   bool
	TrustXRealIP    bool
	RateLimitAtEdge bool
}

type Server struct {
	app               *app.App
	logger            *slog.Logger
	frontendOrigin    string
	webhookSecret     string
	adminAPIKey       string
	yandexClient      YandexOAuthClient
	yandexRedirect    string
	yandexAllowed     map[string]struct{}
	sessionTTL        time.Duration
	secureCookies     bool
	oauthStartLimiter *keyedWindowLimiter
	trustXRealIP      bool
	now               func() time.Time
}

func New(application *app.App, logger *slog.Logger, frontendOrigin, webhookSecret, adminAPIKey string, authOptions ...AuthOptions) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	server := &Server{
		app: application, logger: logger,
		frontendOrigin: strings.TrimRight(frontendOrigin, "/"), webhookSecret: webhookSecret,
		adminAPIKey: adminAPIKey, sessionTTL: 12 * time.Hour,
		oauthStartLimiter: newKeyedWindowLimiter(12, 600, time.Minute, 4096), now: time.Now,
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
		if options.SessionTTL > 0 {
			server.sessionTTL = options.SessionTTL
		}
		server.secureCookies = options.SecureCookies
		server.trustXRealIP = options.TrustXRealIP
		if options.RateLimitAtEdge {
			server.oauthStartLimiter = newKeyedWindowLimiter(0, 600, time.Minute, 0)
		}
	}
	return server
}

func (s *Server) Handler() http.Handler {
	router := chi.NewRouter()
	router.Use(middleware.RequestID)
	router.Use(middleware.Recoverer)
	router.Use(s.cors)
	router.Use(s.requestLogger)

	router.Get("/media/{filename}", s.serveMedia)
	router.Route("/api/v1", func(r chi.Router) {
		r.Get("/health", s.health)
		r.Post("/webhooks/max", s.maxWebhook)
		r.Get("/auth/session", s.authSession)
		r.Get("/auth/yandex/start", s.startYandexAuth)
		r.Get("/auth/yandex/callback", s.finishYandexAuth)
		r.Post("/auth/logout", s.logout)

		r.Group(func(r chi.Router) {
			r.Use(s.requireAdmin)

			r.Get("/channels", s.listChannels)
			r.Post("/channels", s.createChannel)
			r.Get("/channels/{id}", s.getChannel)
			r.Patch("/channels/{id}", s.updateChannel)
			r.Put("/channels/{id}", s.updateChannel)
			r.Delete("/channels/{id}", s.deleteChannel)
			r.Post("/channels/{id}/test", s.testChannel)

			r.Get("/posts", s.listPosts)
			r.Post("/posts", s.createPost)
			r.Get("/posts/{id}", s.getPost)
			r.Patch("/posts/{id}", s.updatePost)
			r.Put("/posts/{id}", s.updatePost)
			r.Delete("/posts/{id}", s.deletePost)
			r.Post("/posts/{id}/duplicate", s.duplicatePost)
			r.Post("/posts/{id}/image", s.uploadPostImage)
			r.Post("/posts/{id}/generate-image", s.generatePostImage)
			r.Post("/posts/{id}/schedule", s.schedulePost)
			r.Put("/posts/{id}/schedule", s.schedulePost)
			r.Post("/posts/{id}/cancel-schedule", s.cancelSchedule)
			r.Delete("/posts/{id}/schedule", s.cancelSchedule)
			r.Post("/posts/{id}/publish", s.publishPost)
			r.Post("/posts/{id}/sync", s.updatePublishedPost)
			r.Post("/posts/{id}/update-published", s.updatePublishedPost)
			r.Post("/posts/{id}/delete-publication", s.deletePublication)
			r.Delete("/posts/{id}/publication", s.deletePublication)

			r.Post("/images/generate", s.generateImage)
			r.Post("/research/generate", s.generateResearch)
			r.Post("/media", s.uploadMedia)

			r.Get("/integration/max", s.maxIntegrationStatus)
			r.Get("/integrations/max", s.maxIntegrationStatus)
			r.Post("/integration/max/test", s.testMAXIntegration)
			r.Post("/integrations/max/test", s.testMAXIntegration)
		})
	})
	return router
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
		"research_configured": s.app.ResearchConfigured(),
		"auth_required":       status.Required, "authenticated": status.Authenticated,
		"auth_methods": status.Methods, "auth_method": status.Method, "user": status.User,
		"session_expires_at": status.SessionExpiresAt,
	})
}

func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.authenticate(r); !ok {
			w.Header().Set("Cache-Control", "no-store")
			s.problem(w, http.StatusUnauthorized, "admin_auth_required", "A valid Yandex session or admin access key is required", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
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
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Accept, X-Admin-Key")
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
		start := time.Now()
		wrapped := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(wrapped, r)
		s.logger.Info("http request",
			"request_id", middleware.GetReqID(r.Context()), "method", r.Method,
			"path", r.URL.Path, "status", wrapped.status, "duration_ms", time.Since(start).Milliseconds())
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
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
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.problem(w, http.StatusNotFound, "not_found", "Resource was not found", nil)
	case errors.Is(err, store.ErrConflict):
		s.problem(w, http.StatusConflict, "state_conflict", err.Error(), nil)
	case errors.Is(err, app.ErrMAXNotConfigured):
		s.problem(w, http.StatusServiceUnavailable, "max_not_configured", err.Error(), nil)
	case errors.Is(err, app.ErrOpenAINotConfigured):
		s.problem(w, http.StatusServiceUnavailable, "openai_not_configured", err.Error(), nil)
	case errors.Is(err, app.ErrResearchNotConfigured):
		s.problem(w, http.StatusServiceUnavailable, "openai_research_not_configured", err.Error(), nil)
	case errors.Is(err, app.ErrConflict):
		s.problem(w, http.StatusConflict, "state_conflict", err.Error(), nil)
	case errors.Is(err, context.Canceled):
		s.problem(w, 499, "request_canceled", "Request was canceled", nil)
	case errors.Is(err, context.DeadlineExceeded):
		s.problem(w, http.StatusGatewayTimeout, "upstream_timeout", "An upstream request timed out", nil)
	default:
		var maxErr *maxclient.Error
		var openAIErr *openaiimg.Error
		var researchErr *openairesearch.Error
		var channelErr *app.ChannelAccessError
		if errors.As(err, &channelErr) {
			s.problem(w, http.StatusUnprocessableEntity, "max_channel_access", channelErr.Error(), channelErr.Diagnostics)
			return
		}
		if errors.As(err, &maxErr) {
			details := map[string]any{"upstream_status": maxErr.StatusCode, "request_id": maxErr.RequestID}
			s.problem(w, http.StatusBadGateway, "max_api_error", maxErr.Message, details)
			return
		}
		if errors.As(err, &openAIErr) {
			details := map[string]any{"upstream_status": openAIErr.StatusCode, "request_id": openAIErr.RequestID}
			s.problem(w, http.StatusBadGateway, "openai_api_error", openAIErr.Message, details)
			return
		}
		if errors.As(err, &researchErr) {
			details := map[string]any{"upstream_status": researchErr.StatusCode, "request_id": researchErr.RequestID}
			s.problem(w, http.StatusBadGateway, "openai_research_error", researchErr.Message, details)
			return
		}
		if isValidationError(err) {
			s.problem(w, http.StatusBadRequest, "validation_error", err.Error(), nil)
			return
		}
		s.logger.Error("request failed", "error", err)
		s.problem(w, http.StatusInternalServerError, "internal_error", "Internal server error", nil)
	}
}

func parseID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("id must be a positive integer")
	}
	return id, nil
}

func requestID() string {
	var data [12]byte
	if _, err := rand.Read(data[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(data[:])
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
