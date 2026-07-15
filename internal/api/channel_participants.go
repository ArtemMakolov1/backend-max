package api

import (
	"net/http"
	"strings"
	"time"
)

const maxParticipantHistoryDays = 5 * 366

func (s *Server) getChannelParticipantHistory(w http.ResponseWriter, r *http.Request) {
	userID, err := authenticatedUserID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	channelID, err := parseID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	today := utcAPIDate(s.now())
	fromDay := today.AddDate(0, 0, -89)
	toDay := today
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
	if days := int(toDay.Sub(fromDay)/(24*time.Hour)) + 1; days > maxParticipantHistoryDays {
		s.problem(w, http.StatusBadRequest, "validation_error", "participant history range must not exceed five years", nil)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	snapshots, err := s.app.Store().ListChannelParticipantSnapshotsForUser(r.Context(), userID, channelID, fromDay, toDay)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, snapshots)
}

func utcAPIDate(value time.Time) time.Time {
	year, month, day := value.UTC().Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}
