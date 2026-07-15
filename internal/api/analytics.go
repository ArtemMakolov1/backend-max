package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"maxpilot/backend/internal/store"
)

const defaultAnalyticsDays = 30

// getAnalytics returns a bounded, tenant-scoped report for one connected
// channel. The selected interval is inclusive and uses UTC calendar dates so
// clients can request the same report deterministically across time zones.
func (s *Server) getAnalytics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	userID, err := authenticatedUserID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	channelID, err := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("channel_id")), 10, 64)
	if err != nil || channelID <= 0 {
		s.problem(w, http.StatusBadRequest, "validation_error", "channel_id must be a positive integer", nil)
		return
	}

	toDay := utcAPIDate(s.now())
	fromDay := toDay.AddDate(0, 0, -(defaultAnalyticsDays - 1))
	if raw := strings.TrimSpace(r.URL.Query().Get("from")); raw != "" {
		fromDay, err = time.Parse(time.DateOnly, raw)
		if err != nil {
			s.problem(w, http.StatusBadRequest, "validation_error", "from must use YYYY-MM-DD", nil)
			return
		}
		fromDay = utcAPIDate(fromDay)
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("to")); raw != "" {
		toDay, err = time.Parse(time.DateOnly, raw)
		if err != nil {
			s.problem(w, http.StatusBadRequest, "validation_error", "to must use YYYY-MM-DD", nil)
			return
		}
		toDay = utcAPIDate(toDay)
	}
	if toDay.Before(fromDay) {
		s.problem(w, http.StatusBadRequest, "validation_error", "to must not precede from", nil)
		return
	}
	if days := int(toDay.Sub(fromDay)/(24*time.Hour)) + 1; days > store.MaxChannelAnalyticsDays {
		s.problem(w, http.StatusBadRequest, "validation_error",
			"analytics range must not exceed 366 days", nil)
		return
	}

	report, err := s.app.Store().GetChannelAnalyticsForUser(r.Context(), userID, channelID, fromDay, toDay)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"analytics": report})
}
