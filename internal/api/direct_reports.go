package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/store"
)

func (s *Server) getDirectCampaignStatistics(w http.ResponseWriter, r *http.Request) {
	_, access, campaignID, ok := s.requireDirectCampaignCapability(
		w, r, app.CapabilityAdsRead,
	)
	if !ok {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	query := r.URL.Query()
	fromValues, toValues := query["from"], query["to"]
	from, ok := directStatisticsQueryDate(singleDirectStatisticsQueryValue(fromValues))
	if !ok {
		s.problem(w, http.StatusUnprocessableEntity, "direct_statistics_range_invalid",
			"Параметр from должен быть датой в формате YYYY-MM-DD.", nil)
		return
	}
	to, ok := directStatisticsQueryDate(singleDirectStatisticsQueryValue(toValues))
	if !ok || from.After(to) || to.Sub(from) >= 366*24*time.Hour {
		s.problem(w, http.StatusUnprocessableEntity, "direct_statistics_range_invalid",
			"Период статистики должен содержать не более 366 дней.", nil)
		return
	}
	statistics, err := s.app.GetDirectCampaignStatistics(
		r.Context(), access.UserID, access.WorkspaceID, campaignID, from, to,
	)
	if err != nil {
		switch {
		case errors.Is(err, app.ErrDirectReportsUnsupported):
			s.problem(w, http.StatusServiceUnavailable, "direct_reports_unsupported",
				"Получение статистики Яндекс Директа временно недоступно.", nil)
		case errors.Is(err, app.ErrDirectStatisticsUnavailable):
			s.problem(w, http.StatusConflict, "direct_statistics_unavailable",
				"Статистика появится после создания кампании в Яндекс Директе.", nil)
		case errors.Is(err, store.ErrDirectValidation):
			s.problem(w, http.StatusUnprocessableEntity, "direct_statistics_range_invalid",
				"Проверьте период статистики.", nil)
		default:
			s.writeError(w, err)
		}
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"statistics": statistics})
}

func directStatisticsQueryDate(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	parsed, err := time.Parse(time.DateOnly, value)
	return parsed, err == nil && parsed.Format(time.DateOnly) == value
}

func singleDirectStatisticsQueryValue(values []string) string {
	if len(values) != 1 {
		return ""
	}
	return values[0]
}
