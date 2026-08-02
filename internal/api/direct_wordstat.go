package api

import (
	"net/http"
	"time"

	"maxpilot/backend/internal/app"
)

type directKeywordSuggestionsRequest struct {
	Phrase string `json:"phrase"`
	Limit  int    `json:"limit"`
}

type directKeywordSuggestionResponse struct {
	Phrase string `json:"phrase"`
	Count  int64  `json:"count"`
	Source string `json:"source"`
}

type directKeywordSuggestionsResponse struct {
	CampaignID string                            `json:"campaign_id"`
	Phrase     string                            `json:"phrase"`
	TotalCount int64                             `json:"total_count"`
	Regions    []string                          `json:"regions"`
	Items      []directKeywordSuggestionResponse `json:"items"`
	FetchedAt  time.Time                         `json:"fetched_at"`
}

func (s *Server) suggestDirectCampaignKeywords(w http.ResponseWriter, r *http.Request) {
	_, access, campaignID, ok := s.requireDirectCampaignCapability(
		w, r, app.CapabilityAdsWrite,
	)
	if !ok {
		return
	}
	var request directKeywordSuggestionsRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	result, err := s.app.SuggestDirectCampaignKeywords(
		r.Context(), access.UserID, access.WorkspaceID, campaignID,
		request.Phrase, request.Limit,
	)
	if err != nil {
		s.writeError(w, err)
		return
	}
	items := make([]directKeywordSuggestionResponse, 0, len(result.Items))
	for _, item := range result.Items {
		items = append(items, directKeywordSuggestionResponse{
			Phrase: item.Phrase, Count: item.Count, Source: item.Source,
		})
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, map[string]any{
		"suggestions": directKeywordSuggestionsResponse{
			CampaignID: result.CampaignID, Phrase: result.Phrase,
			TotalCount: result.TotalCount, Regions: result.Regions,
			Items: items, FetchedAt: result.FetchedAt,
		},
	})
}
