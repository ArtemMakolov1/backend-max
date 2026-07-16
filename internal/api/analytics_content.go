package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/store"
)

const maxAnalyticsRepeatPlanningWindow = 90 * 24 * time.Hour

// RegisterAnalyticsContentRoutes mounts the feature-local analytics-to-content
// endpoints. Call it from inside /workspaces/{workspace_id}; keeping the
// registration helper here avoids coupling this feature to the shared router.
func RegisterAnalyticsContentRoutes(r chi.Router, server *Server) {
	if server == nil {
		return
	}
	r.Get("/analytics", server.getWorkspaceAnalyticsContent)
	r.Get("/analytics/content", server.getWorkspaceAnalyticsContent)
	r.Get("/analytics/content/posts/{post_id}", server.getWorkspacePostAnalytics)
	r.Post("/analytics/content/posts/{post_id}/variation", server.createWorkspaceAnalyticsVariation)
	r.Post("/analytics/content/posts/{post_id}/repeat", server.createWorkspaceAnalyticsRepeat)
}

// RegisterAnalyticsContentRoutes is also available as a Server method for
// embedders that already hold the configured server instance.
func (s *Server) RegisterAnalyticsContentRoutes(r chi.Router) {
	RegisterAnalyticsContentRoutes(r, s)
}

func (s *Server) getWorkspaceAnalyticsContent(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityPostsRead)
	if !ok {
		return
	}

	channelID, err := parseAnalyticsContentChannel(r.URL.Query().Get("channel_id"))
	if err != nil {
		s.problem(w, http.StatusBadRequest, "validation_error", err.Error(), nil)
		return
	}
	fromDay, toDay, err := s.analyticsContentPeriod(r)
	if err != nil {
		s.problem(w, http.StatusBadRequest, "validation_error", err.Error(), nil)
		return
	}
	tzOffsetMinutes, err := parseAnalyticsTimezoneOffset(r.URL.Query().Get("tz_offset_minutes"))
	if err != nil {
		s.problem(w, http.StatusBadRequest, "validation_error", err.Error(), nil)
		return
	}

	report, err := s.app.Store().GetWorkspaceAnalyticsContent(
		r.Context(), access.UserID, access.WorkspaceID, channelID,
		fromDay, toDay, s.now().UTC(), tzOffsetMinutes,
	)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"analytics": report})
}

func (s *Server) getWorkspacePostAnalytics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	_, access, postID, ok := s.requireWorkspacePostCapability(w, r, app.CapabilityPostsRead)
	if !ok {
		return
	}
	fromDay, toDay, err := s.analyticsContentPeriod(r)
	if err != nil {
		s.problem(w, http.StatusBadRequest, "validation_error", err.Error(), nil)
		return
	}
	report, err := s.app.Store().GetWorkspacePostAnalytics(
		r.Context(), access.UserID, access.WorkspaceID, postID, fromDay, toDay,
	)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"analytics": report})
}

func (s *Server) createWorkspaceAnalyticsVariation(w http.ResponseWriter, r *http.Request) {
	_, access, postID, ok := s.requireWorkspacePostCapability(w, r, app.CapabilityPostsWrite)
	if !ok {
		return
	}
	post, err := s.app.Store().CreateAnalyticsContentDraft(
		r.Context(), access.UserID, access.WorkspaceID, postID, "variation",
	)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, map[string]any{
		"post": scopeWorkspacePostMedia(post, access.WorkspaceID),
	})
}

type analyticsRepeatRequest struct {
	PlannedAt string `json:"planned_at"`
}

func (s *Server) createWorkspaceAnalyticsRepeat(w http.ResponseWriter, r *http.Request) {
	workspace, access, postID, ok := s.requireWorkspacePostCapability(w, r, app.CapabilityPostsWrite)
	if !ok {
		return
	}
	var request analyticsRepeatRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	plannedAt, err := time.Parse(time.RFC3339, strings.TrimSpace(request.PlannedAt))
	if err != nil {
		s.problem(w, http.StatusBadRequest, "validation_error", "planned_at must use RFC3339", nil)
		return
	}
	now := s.now().UTC()
	plannedAt = plannedAt.UTC()
	if !plannedAt.After(now) {
		s.problem(w, http.StatusBadRequest, "validation_error", "planned_at must be in the future", nil)
		return
	}
	if plannedAt.After(now.Add(maxAnalyticsRepeatPlanningWindow)) {
		s.problem(w, http.StatusBadRequest, "validation_error", "planned_at must be within 90 days", nil)
		return
	}

	post, campaign, err := s.app.Store().CreateAnalyticsRepeatPlan(
		r.Context(), access.UserID, access.WorkspaceID, postID, plannedAt,
	)
	if err != nil {
		s.writeError(w, err)
		return
	}
	variant := campaign.Variants[0]
	// The durable campaign variant owns planned_at. The linked post remains a
	// draft and must go through the normal review/schedule endpoint.
	s.writeJSON(w, http.StatusCreated, map[string]any{
		"post":              scopeWorkspacePostMedia(post, access.WorkspaceID),
		"planned_at":        variant.PlannedAt,
		"requires_approval": workspace.ApprovalRequired,
		"campaign_id":       campaign.ID,
		"variant_id":        variant.ID,
		"campaign":          campaign,
	})
}

func parseAnalyticsContentChannel(raw string) (*int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.EqualFold(raw, "all") {
		return nil, nil
	}
	channelID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || channelID <= 0 {
		return nil, errors.New("channel_id must be 'all' or a positive integer")
	}
	return &channelID, nil
}

func parseAnalyticsTimezoneOffset(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	offset, err := strconv.Atoi(raw)
	if err != nil || offset < -store.MaxAnalyticsTimezoneOffsetMinutes || offset > store.MaxAnalyticsTimezoneOffsetMinutes {
		return 0, errors.New("tz_offset_minutes must be between -840 and 840")
	}
	return offset, nil
}

func (s *Server) analyticsContentPeriod(r *http.Request) (time.Time, time.Time, error) {
	toDay := utcAPIDate(s.now())
	fromDay := toDay.AddDate(0, 0, -(defaultAnalyticsDays - 1))
	var err error
	if raw := strings.TrimSpace(r.URL.Query().Get("from")); raw != "" {
		fromDay, err = time.Parse(time.DateOnly, raw)
		if err != nil {
			return time.Time{}, time.Time{}, errors.New("from must use YYYY-MM-DD")
		}
		fromDay = utcAPIDate(fromDay)
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("to")); raw != "" {
		toDay, err = time.Parse(time.DateOnly, raw)
		if err != nil {
			return time.Time{}, time.Time{}, errors.New("to must use YYYY-MM-DD")
		}
		toDay = utcAPIDate(toDay)
	}
	if toDay.Before(fromDay) {
		return time.Time{}, time.Time{}, errors.New("to must not precede from")
	}
	if days := int(toDay.Sub(fromDay)/(24*time.Hour)) + 1; days > store.MaxChannelAnalyticsDays {
		return time.Time{}, time.Time{}, errors.New("analytics range must not exceed 366 days")
	}
	return fromDay, toDay, nil
}
