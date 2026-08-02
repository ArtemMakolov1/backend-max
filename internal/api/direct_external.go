package api

import (
	"net/http"
	"strconv"
	"time"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/store"
)

type directExternalCampaignResponse struct {
	ProviderCampaignID    string    `json:"provider_campaign_id"`
	Name                  string    `json:"name"`
	CampaignType          string    `json:"campaign_type"`
	ProviderStatus        string    `json:"provider_status"`
	ProviderState         string    `json:"provider_state"`
	ProviderStatusPayment string    `json:"provider_status_payment"`
	StartsAt              string    `json:"starts_at"`
	EndsAt                *string   `json:"ends_at"`
	Timezone              string    `json:"timezone"`
	SyncedAt              time.Time `json:"synced_at"`
}

func (s *Server) listDirectExternalCampaigns(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityAdsRead)
	if !ok {
		return
	}
	campaigns, err := s.app.ListDirectExternalCampaigns(
		r.Context(), access.UserID, access.WorkspaceID,
	)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeDirectExternalCampaigns(w, campaigns)
}

func (s *Server) syncDirectExternalCampaigns(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(
		w, r, app.CapabilityAdsCredentialsManage,
	)
	if !ok {
		return
	}
	campaigns, err := s.app.SyncDirectExternalCampaigns(
		r.Context(), access.UserID, access.WorkspaceID,
	)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeDirectExternalCampaigns(w, campaigns)
}

func (s *Server) writeDirectExternalCampaigns(
	w http.ResponseWriter, snapshot app.DirectExternalCampaignSnapshot,
) {
	items := make([]directExternalCampaignResponse, 0, len(snapshot.Items))
	for _, campaign := range snapshot.Items {
		items = append(items, publicDirectExternalCampaign(campaign))
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, map[string]any{
		"items": items, "synced_at": snapshot.SyncedAt,
	})
}

func publicDirectExternalCampaign(
	campaign store.DirectExternalCampaign,
) directExternalCampaignResponse {
	var endsAt *string
	if campaign.EndsAt != nil {
		value := campaign.EndsAt.UTC().Format(time.DateOnly)
		endsAt = &value
	}
	return directExternalCampaignResponse{
		ProviderCampaignID: strconv.FormatInt(campaign.ProviderCampaignID, 10),
		Name:               campaign.Name, CampaignType: campaign.CampaignType,
		ProviderStatus:        campaign.ProviderStatus,
		ProviderState:         campaign.ProviderState,
		ProviderStatusPayment: campaign.ProviderStatusPayment,
		StartsAt:              campaign.StartsAt.UTC().Format(time.DateOnly), EndsAt: endsAt,
		Timezone: campaign.Timezone, SyncedAt: campaign.SyncedAt.UTC(),
	}
}
