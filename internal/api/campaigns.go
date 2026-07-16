package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/store"
)

// registerCampaignRoutes is intentionally feature-local. Call it from the
// /workspaces/{workspace_id} router; keeping the route graph here lets the
// campaign API evolve without expanding the shared server implementation.
func (s *Server) registerCampaignRoutes(r chi.Router) {
	r.Get("/calendar", s.listWorkspaceCalendar)
	r.Put("/calendar/posts/{post_id}", s.rescheduleWorkspaceCalendarPost)
	r.Get("/campaigns", s.listCampaigns)
	r.Post("/campaigns", s.createCampaign)
	r.Route("/campaigns", func(r chi.Router) {
		// Keep the trailing-slash aliases for already deployed clients.
		r.Get("/", s.listCampaigns)
		r.Post("/", s.createCampaign)
		r.Route("/{campaign_id}", func(r chi.Router) {
			r.Get("/", s.getCampaign)
			r.Patch("/", s.updateCampaign)
			r.Delete("/", s.archiveCampaign)
			r.Post("/variants", s.addCampaignVariants)
			r.Patch("/variants/{variant_id}", s.updateCampaignVariant)
			r.Delete("/variants/{variant_id}", s.deleteCampaignVariant)
			r.Post("/materialize", s.materializeCampaign)
			r.Post("/schedule", s.scheduleCampaign)
		})
	})
}

type campaignVariantRequest struct {
	ChannelID int64  `json:"channel_id"`
	Title     string `json:"title"`
	Content   string `json:"content"`
	Format    string `json:"format"`
	PlannedAt string `json:"planned_at"`
}

type createCampaignRequest struct {
	Name        string                   `json:"name"`
	Description string                   `json:"description"`
	Variants    []campaignVariantRequest `json:"variants"`
}

type updateCampaignRequest struct {
	Name              *string `json:"name,omitempty"`
	Description       *string `json:"description,omitempty"`
	ExpectedUpdatedAt string  `json:"expected_updated_at"`
}

type updateCampaignVariantRequest struct {
	ChannelID         *int64  `json:"channel_id,omitempty"`
	Title             *string `json:"title,omitempty"`
	Content           *string `json:"content,omitempty"`
	Format            *string `json:"format,omitempty"`
	PlannedAt         *string `json:"planned_at,omitempty"`
	ExpectedUpdatedAt string  `json:"expected_updated_at"`
}

type materializeCampaignRequest struct {
	VariantIDs []string `json:"variant_ids,omitempty"`
}

type scheduleCampaignItemRequest struct {
	VariantID         string `json:"variant_id"`
	ScheduledAt       string `json:"scheduled_at,omitempty"`
	ExpectedUpdatedAt string `json:"expected_updated_at"`
}

type scheduleCampaignRequest struct {
	Items []scheduleCampaignItemRequest `json:"items"`
}

type rescheduleCalendarPostRequest struct {
	ScheduledAt       string `json:"scheduled_at"`
	ExpectedUpdatedAt string `json:"expected_updated_at"`
}

func (s *Server) listWorkspaceCalendar(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityPostsRead)
	if !ok {
		return
	}
	now := s.now().UTC()
	from, err := parseOptionalRFC3339(r.URL.Query().Get("from"), now.Add(-31*24*time.Hour))
	if err != nil {
		s.problem(w, http.StatusBadRequest, "validation_error", "from must be an RFC3339 timestamp", nil)
		return
	}
	to, err := parseOptionalRFC3339(r.URL.Query().Get("to"), from.Add(366*24*time.Hour))
	if err != nil || !to.After(from) || to.Sub(from) > 366*24*time.Hour {
		s.problem(w, http.StatusBadRequest, "validation_error", "calendar range must be positive and no longer than 366 days", nil)
		return
	}
	var channelID *int64
	if value := strings.TrimSpace(r.URL.Query().Get("channel_id")); value != "" {
		parsed, parseErr := strconv.ParseInt(value, 10, 64)
		if parseErr != nil || parsed <= 0 {
			s.problem(w, http.StatusBadRequest, "validation_error", "channel_id must be a positive integer", nil)
			return
		}
		channelID = &parsed
	}
	items, err := s.app.Store().ListWorkspaceCalendar(r.Context(), access.UserID, access.WorkspaceID, from, to, channelID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, items)
}

func (s *Server) rescheduleWorkspaceCalendarPost(w http.ResponseWriter, r *http.Request) {
	_, access, postID, ok := s.requireWorkspacePostCapability(w, r, app.CapabilityPostsPublish)
	if !ok {
		return
	}
	var request rescheduleCalendarPostRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	scheduledAt, err := parseCampaignFuture(request.ScheduledAt, s.now().UTC())
	if err != nil {
		s.problem(w, http.StatusBadRequest, "validation_error", err.Error(), nil)
		return
	}
	expected, err := parseExpectedTimestamp(request.ExpectedUpdatedAt)
	if err != nil {
		s.problem(w, http.StatusBadRequest, "validation_error", err.Error(), nil)
		return
	}
	current, err := s.app.Store().GetPostForWorkspace(r.Context(), access.UserID, access.WorkspaceID, postID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	if !current.UpdatedAt.Equal(expected) {
		s.problem(w, http.StatusConflict, "calendar_reschedule_conflict", "Post changed in another session. Reload the calendar and retry.", nil)
		return
	}
	if err := s.app.ValidatePostForScheduling(r.Context(), current); err != nil {
		s.writeError(w, err)
		return
	}
	post, err := s.app.Store().RescheduleWorkspacePost(r.Context(), access.UserID, access.WorkspaceID,
		postID, scheduledAt, expected, s.now().UTC())
	if errors.Is(err, store.ErrCampaignApprovalRequired) {
		s.problem(w, http.StatusConflict, "post_approval_required", "The current post revision must be approved before scheduling.", nil)
		return
	}
	if errors.Is(err, store.ErrConflict) {
		s.problem(w, http.StatusConflict, "calendar_reschedule_conflict", "Post changed before it could be rescheduled. Reload the calendar and retry.", nil)
		return
	}
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, scopeWorkspacePostMedia(post, access.WorkspaceID))
}

func (s *Server) listCampaigns(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityPostsRead)
	if !ok {
		return
	}
	includeArchived := r.URL.Query().Get("include_archived") == "true"
	campaigns, err := s.app.Store().ListCampaigns(r.Context(), access.UserID, access.WorkspaceID, includeArchived)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, campaigns)
}

func (s *Server) createCampaign(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityPostsWrite)
	if !ok {
		return
	}
	var request createCampaignRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	variants, ok := s.parseCampaignVariants(w, request.Variants)
	if !ok {
		return
	}
	campaign, err := s.app.Store().CreateCampaign(r.Context(), access.UserID, access.WorkspaceID,
		store.Campaign{Name: request.Name, Description: request.Description}, variants)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, campaign)
}

func (s *Server) getCampaign(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityPostsRead)
	if !ok {
		return
	}
	campaignID, ok := s.campaignPathID(w, r, "campaign_id")
	if !ok {
		return
	}
	campaign, err := s.app.Store().GetCampaign(r.Context(), access.UserID, access.WorkspaceID, campaignID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, campaign)
}

func (s *Server) updateCampaign(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityPostsWrite)
	if !ok {
		return
	}
	campaignID, ok := s.campaignPathID(w, r, "campaign_id")
	if !ok {
		return
	}
	var request updateCampaignRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	expected, err := parseExpectedTimestamp(request.ExpectedUpdatedAt)
	if err != nil {
		s.problem(w, http.StatusBadRequest, "validation_error", err.Error(), nil)
		return
	}
	campaign, err := s.app.Store().UpdateCampaign(r.Context(), access.UserID, access.WorkspaceID, campaignID,
		store.CampaignChanges{Name: request.Name, Description: request.Description, ExpectedUpdatedAt: expected})
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, campaign)
}

func (s *Server) archiveCampaign(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityPostsWrite)
	if !ok {
		return
	}
	campaignID, ok := s.campaignPathID(w, r, "campaign_id")
	if !ok {
		return
	}
	expected, err := parseExpectedTimestamp(r.URL.Query().Get("expected_updated_at"))
	if err != nil {
		s.problem(w, http.StatusBadRequest, "validation_error", err.Error(), nil)
		return
	}
	if err := s.app.Store().ArchiveCampaign(r.Context(), access.UserID, access.WorkspaceID, campaignID, expected); err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusNoContent, nil)
}

func (s *Server) addCampaignVariants(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityPostsWrite)
	if !ok {
		return
	}
	campaignID, ok := s.campaignPathID(w, r, "campaign_id")
	if !ok {
		return
	}
	var request struct {
		Variants []campaignVariantRequest `json:"variants"`
	}
	if !s.decodeJSON(w, r, &request) {
		return
	}
	variants, ok := s.parseCampaignVariants(w, request.Variants)
	if !ok {
		return
	}
	campaign, err := s.app.Store().AddCampaignVariants(r.Context(), access.UserID, access.WorkspaceID, campaignID, variants)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, campaign)
}

func (s *Server) updateCampaignVariant(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityPostsWrite)
	if !ok {
		return
	}
	campaignID, ok := s.campaignPathID(w, r, "campaign_id")
	if !ok {
		return
	}
	variantID, ok := s.campaignPathID(w, r, "variant_id")
	if !ok {
		return
	}
	var request updateCampaignVariantRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	expected, err := parseExpectedTimestamp(request.ExpectedUpdatedAt)
	if err != nil {
		s.problem(w, http.StatusBadRequest, "validation_error", err.Error(), nil)
		return
	}
	changes := store.CampaignVariantChanges{
		ChannelID: request.ChannelID, Title: request.Title, Content: request.Content,
		Format: request.Format, ExpectedUpdatedAt: expected,
	}
	if request.PlannedAt != nil {
		parsed, err := parseCampaignFuture(*request.PlannedAt, s.now().UTC())
		if err != nil {
			s.problem(w, http.StatusBadRequest, "validation_error", err.Error(), nil)
			return
		}
		changes.PlannedAt = &parsed
	}
	variant, err := s.app.Store().UpdateCampaignVariant(r.Context(), access.UserID, access.WorkspaceID,
		campaignID, variantID, changes)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, variant)
}

func (s *Server) deleteCampaignVariant(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityPostsWrite)
	if !ok {
		return
	}
	campaignID, ok := s.campaignPathID(w, r, "campaign_id")
	if !ok {
		return
	}
	variantID, ok := s.campaignPathID(w, r, "variant_id")
	if !ok {
		return
	}
	expected, err := parseExpectedTimestamp(r.URL.Query().Get("expected_updated_at"))
	if err != nil {
		s.problem(w, http.StatusBadRequest, "validation_error", err.Error(), nil)
		return
	}
	if err := s.app.Store().DeleteCampaignVariant(r.Context(), access.UserID, access.WorkspaceID,
		campaignID, variantID, expected); err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusNoContent, nil)
}

func (s *Server) materializeCampaign(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityPostsWrite)
	if !ok {
		return
	}
	campaignID, ok := s.campaignPathID(w, r, "campaign_id")
	if !ok {
		return
	}
	var request materializeCampaignRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	campaign, err := s.app.Store().MaterializeCampaign(r.Context(), access.UserID, access.WorkspaceID, campaignID, request.VariantIDs)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, campaign)
}

func (s *Server) scheduleCampaign(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityPostsPublish)
	if !ok {
		return
	}
	campaignID, ok := s.campaignPathID(w, r, "campaign_id")
	if !ok {
		return
	}
	var request scheduleCampaignRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	items := make([]store.CampaignScheduleItem, 0, len(request.Items))
	for index, item := range request.Items {
		expected, err := parseExpectedTimestamp(item.ExpectedUpdatedAt)
		if err != nil {
			s.problem(w, http.StatusBadRequest, "validation_error", fmt.Sprintf("item %d: %s", index+1, err), nil)
			return
		}
		var scheduledAt time.Time
		if strings.TrimSpace(item.ScheduledAt) != "" {
			scheduledAt, err = parseCampaignFuture(item.ScheduledAt, s.now().UTC())
			if err != nil {
				s.problem(w, http.StatusBadRequest, "validation_error", fmt.Sprintf("item %d: %s", index+1, err), nil)
				return
			}
		}
		items = append(items, store.CampaignScheduleItem{
			VariantID: strings.TrimSpace(item.VariantID), ScheduledAt: scheduledAt, ExpectedUpdatedAt: expected,
		})
	}
	// Full publish validation is performed before the atomic transition. The
	// store then locks each exact post version and rechecks approval, lifecycle,
	// channel and content so a concurrent edit cannot invalidate this preflight.
	campaign, err := s.app.Store().GetCampaign(r.Context(), access.UserID, access.WorkspaceID, campaignID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	byID := make(map[string]store.CampaignVariant, len(campaign.Variants))
	for _, variant := range campaign.Variants {
		byID[variant.ID] = variant
	}
	preflight := make([]store.CampaignItemConflict, 0)
	for _, item := range items {
		variant, found := byID[item.VariantID]
		if !found || variant.PostID == nil {
			continue // Store reports tenant-safe not-found/not-materialized details.
		}
		post, getErr := s.app.Store().GetPostForWorkspace(r.Context(), access.UserID, access.WorkspaceID, *variant.PostID)
		if getErr == nil {
			getErr = s.app.ValidatePostForScheduling(r.Context(), post)
		}
		if getErr != nil {
			postID := *variant.PostID
			preflight = append(preflight, store.CampaignItemConflict{
				VariantID: item.VariantID, PostID: &postID, Code: "post_not_ready", Message: getErr.Error(),
			})
		}
	}
	if len(preflight) != 0 {
		s.problem(w, http.StatusConflict, "campaign_schedule_conflict",
			"No posts were scheduled. Resolve every item conflict and retry.", map[string]any{"items": preflight})
		return
	}
	campaign, err = s.app.Store().BatchScheduleCampaign(r.Context(), access.UserID, access.WorkspaceID,
		campaignID, items, s.now().UTC())
	var conflict *store.CampaignScheduleError
	if errors.As(err, &conflict) {
		s.problem(w, http.StatusConflict, "campaign_schedule_conflict",
			"No posts were scheduled. Resolve every item conflict and retry.", map[string]any{"items": conflict.Items})
		return
	}
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, campaign)
}

func (s *Server) parseCampaignVariants(w http.ResponseWriter, requests []campaignVariantRequest) ([]store.CampaignVariant, bool) {
	if len(requests) == 0 || len(requests) > 200 {
		s.problem(w, http.StatusBadRequest, "validation_error", "campaign must contain 1 to 200 variants", nil)
		return nil, false
	}
	variants := make([]store.CampaignVariant, 0, len(requests))
	for index, request := range requests {
		plannedAt, err := parseCampaignFuture(request.PlannedAt, s.now().UTC())
		if err != nil {
			s.problem(w, http.StatusBadRequest, "validation_error", fmt.Sprintf("variant %d: %s", index+1, err), nil)
			return nil, false
		}
		variants = append(variants, store.CampaignVariant{
			ChannelID: request.ChannelID, Title: request.Title, Content: request.Content,
			Format: request.Format, PlannedAt: plannedAt,
		})
	}
	return variants, true
}

func parseOptionalRFC3339(value string, fallback time.Time) (time.Time, error) {
	if strings.TrimSpace(value) == "" {
		return fallback.UTC(), nil
	}
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}, err
	}
	return parsed.UTC(), nil
}

func parseCampaignFuture(value string, now time.Time) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}, errors.New("planned time must be an RFC3339 timestamp")
	}
	parsed = parsed.UTC()
	if !parsed.After(now.UTC()) {
		return time.Time{}, errors.New("planned time must be in the future")
	}
	return parsed, nil
}

func parseExpectedTimestamp(value string) (time.Time, error) {
	if strings.TrimSpace(value) == "" {
		return time.Time{}, errors.New("expected_updated_at is required")
	}
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}, errors.New("expected_updated_at must be an RFC3339 timestamp")
	}
	return parsed.UTC(), nil
}

func (s *Server) campaignPathID(w http.ResponseWriter, r *http.Request, name string) (string, bool) {
	value := strings.TrimSpace(chi.URLParam(r, name))
	if value == "" || len(value) > 128 {
		s.problem(w, http.StatusBadRequest, "validation_error", "invalid campaign identifier", nil)
		return "", false
	}
	return value, true
}
