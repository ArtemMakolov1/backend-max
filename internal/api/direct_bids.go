package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"maxpilot/backend/internal/app"
)

type directCampaignBidsResponse struct {
	CampaignID        string    `json:"campaign_id"`
	CurrencyCode      string    `json:"currency_code"`
	Strategy          string    `json:"strategy"`
	WeeklyBudgetMinor int64     `json:"weekly_budget_minor"`
	BidCeilingMinor   *int64    `json:"bid_ceiling_minor"`
	MinimumBidMinor   int64     `json:"min_bid_minor"`
	MaximumBidMinor   int64     `json:"max_bid_minor"`
	Version           int64     `json:"version"`
	GraphHash         string    `json:"graph_hash"`
	RevisionID        string    `json:"revision_id"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type directBidCeilingPatchRequest struct {
	BidCeilingMinor    json.RawMessage `json:"bid_ceiling_minor"`
	ExpectedVersion    int64           `json:"expected_version"`
	ExpectedGraphHash  string          `json:"expected_graph_hash"`
	ExpectedRevisionID string          `json:"expected_revision_id"`
	Confirmation       string          `json:"confirmation"`
}

func (s *Server) getDirectCampaignBids(w http.ResponseWriter, r *http.Request) {
	_, access, campaignID, ok := s.requireDirectCampaignCapability(
		w, r, app.CapabilityAdsRead,
	)
	if !ok {
		return
	}
	bids, err := s.app.GetDirectCampaignBids(
		r.Context(), access.UserID, access.WorkspaceID, campaignID,
	)
	if err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, map[string]any{"bids": publicDirectCampaignBids(bids)})
}

func (s *Server) updateDirectCampaignBidCeiling(w http.ResponseWriter, r *http.Request) {
	_, access, campaignID, ok := s.requireDirectSpendCapability(w, r)
	if !ok {
		return
	}
	var request directBidCeilingPatchRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	ceiling, err := parseNullableDirectBidCeiling(request.BidCeilingMinor)
	if err != nil {
		s.writeError(w, err)
		return
	}
	bids, err := s.app.UpdateDirectCampaignBidCeiling(
		r.Context(), access.UserID, access.WorkspaceID, campaignID,
		app.DirectBidCeilingChange{
			BidCeilingMinor: ceiling, ExpectedVersion: request.ExpectedVersion,
			ExpectedGraphHash:  request.ExpectedGraphHash,
			ExpectedRevisionID: request.ExpectedRevisionID,
			Confirmation:       request.Confirmation,
		},
	)
	if err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, map[string]any{"bids": publicDirectCampaignBids(bids)})
}

func parseNullableDirectBidCeiling(raw json.RawMessage) (*int64, error) {
	if len(raw) == 0 {
		return nil, validationError("bid_ceiling_minor is required")
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var value int64
	if json.Unmarshal(raw, &value) != nil || value <= 0 {
		return nil, validationError("bid_ceiling_minor must be a positive integer or null")
	}
	return &value, nil
}

func publicDirectCampaignBids(value app.DirectCampaignBids) directCampaignBidsResponse {
	return directCampaignBidsResponse{
		CampaignID: value.CampaignID, CurrencyCode: value.CurrencyCode,
		Strategy:          strings.TrimSpace(value.Strategy),
		WeeklyBudgetMinor: value.WeeklyBudgetMinor,
		BidCeilingMinor:   value.BidCeilingMinor,
		MinimumBidMinor:   value.MinimumBidMinor, MaximumBidMinor: value.MaximumBidMinor,
		Version: value.Version, GraphHash: value.GraphHash,
		RevisionID: value.RevisionID, UpdatedAt: value.UpdatedAt,
	}
}
