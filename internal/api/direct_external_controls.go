package api

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/store"
	"maxpilot/backend/internal/yandexdirect"
)

type externalControlRequest struct {
	store.DirectExternalEditFence
	ClientRequestID  string    `json:"client_request_id"`
	BudgetKey        string    `json:"budget_key,omitempty"`
	AmountMicros     string    `json:"amount_micros,omitempty"`
	CurrencyCode     string    `json:"currency_code,omitempty"`
	Name             *string   `json:"name,omitempty"`
	RegionIDs        *[]int64  `json:"region_ids,omitempty"`
	NegativeKeywords *[]string `json:"negative_keywords,omitempty"`
	Keyword          string    `json:"keyword,omitempty"`
	SearchBidMicros  *string   `json:"search_bid_micros,omitempty"`
	NetworkBidMicros *string   `json:"network_bid_micros,omitempty"`
	Action           string    `json:"action,omitempty"`
	ConfirmSpend     bool      `json:"confirm_spend,omitempty"`
}

func (s *Server) registerDirectExternalControlsRoutes(r chi.Router) {
	r.Patch("/campaigns/external/{provider_campaign_id}/budget", s.externalControlHandler("budget", app.CapabilityAdsBudgetManage))
	r.Patch("/campaigns/external/{provider_campaign_id}/groups/{provider_group_id}", s.externalControlHandler("group", app.CapabilityAdsWrite))
	r.Post("/campaigns/external/{provider_campaign_id}/groups", s.externalControlHandler("create_group", app.CapabilityAdsWrite))
	r.Patch("/campaigns/external/{provider_campaign_id}/keywords/{provider_keyword_id}", s.externalControlHandler("keyword", app.CapabilityAdsWrite))
	r.Patch("/campaigns/external/{provider_campaign_id}/keywords/{provider_keyword_id}/bid", s.externalControlHandler("bid", app.CapabilityAdsBudgetManage))
	r.Post("/campaigns/external/{provider_campaign_id}/groups/{provider_group_id}/keywords", s.externalControlHandler("create_keyword", app.CapabilityAdsWrite))
	r.Post("/campaigns/external/{provider_campaign_id}/state", s.externalControlHandler("state", app.CapabilityAdsLaunch))
	r.Post("/campaigns/external/{provider_campaign_id}/operations/{client_request_id}/reconcile", s.reconcileDirectExternalCreation)
}
func (s *Server) externalControlHandler(kind string, capability app.Capability) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		_, access, ok := s.requireWorkspaceCapability(w, r, capability)
		if !ok {
			return
		}
		campaign, _, ok := s.externalIDs(w, r)
		if !ok {
			return
		}
		var request externalControlRequest
		if !s.decodeJSON(w, r, &request) {
			return
		}
		// The route chooses the operation and target. An unrelated input field
		// must never appear successful while the server silently ignores it.
		if !externalControlFieldsValid(request, kind) {
			s.writeError(w, store.ErrDirectValidation)
			return
		}
		var target int64
		for _, key := range []string{"provider_group_id", "provider_keyword_id"} {
			if value := chi.URLParam(r, key); value != "" {
				var err error
				target, err = strconv.ParseInt(value, 10, 64)
				if err != nil || target <= 0 {
					s.writeError(w, store.ErrNotFound)
					return
				}
			}
		}
		mutation := app.DirectExternalControlMutation{DirectExternalEditFence: request.DirectExternalEditFence, ClientRequestID: request.ClientRequestID, Kind: kind, TargetID: target, BudgetKey: request.BudgetKey, AmountMicros: request.AmountMicros, CurrencyCode: request.CurrencyCode, Group: yandexdirect.ExternalGroupChanges{Name: request.Name, RegionIDs: request.RegionIDs, NegativeKeywords: request.NegativeKeywords}, Keyword: request.Keyword, SearchBidMicros: request.SearchBidMicros, NetworkBidMicros: request.NetworkBidMicros, Action: request.Action, ConfirmSpend: request.ConfirmSpend}
		result, err := s.app.ApplyDirectExternalControl(r.Context(), access.UserID, access.WorkspaceID, campaign, mutation)
		if err != nil {
			s.writeError(w, err)
			return
		}
		s.writeExternalDetail(w, result, access.Can(app.CapabilityAdsWrite), access.Can(app.CapabilityAdsBudgetManage))
	}
}
func externalControlFieldsValid(r externalControlRequest, kind string) bool {
	budget := r.BudgetKey != "" || r.AmountMicros != ""
	group := r.Name != nil || r.RegionIDs != nil || r.NegativeKeywords != nil
	bid := r.SearchBidMicros != nil || r.NetworkBidMicros != nil
	state := r.Action != "" || r.ConfirmSpend
	if r.ConnectionID == "" || r.Version <= 0 || r.RevisionID == "" || r.ObservedHash == "" || !store.ValidDirectExternalRequestID(r.ClientRequestID) {
		return false
	}
	switch kind {
	case "budget":
		return r.BudgetKey != "" && r.AmountMicros != "" && r.CurrencyCode != "" && !group && !bid && !state && r.Keyword == ""
	case "group", "create_group":
		return group && !budget && !bid && !state && r.Keyword == "" && r.CurrencyCode == ""
	case "keyword", "create_keyword":
		return r.Keyword != "" && !budget && !group && !bid && !state && r.CurrencyCode == ""
	case "bid":
		return bid && r.CurrencyCode != "" && !budget && !group && !state && r.Keyword == ""
	case "state":
		return r.Action != "" && !budget && !group && !bid && r.Keyword == "" && r.CurrencyCode == ""
	}
	return false
}
func (s *Server) reconcileDirectExternalCreation(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityAdsWrite)
	if !ok {
		return
	}
	campaign, _, ok := s.externalIDs(w, r)
	if !ok {
		return
	}
	var request struct {
		store.DirectExternalEditFence
		ProviderResultID string `json:"provider_result_id"`
	}
	if !s.decodeJSON(w, r, &request) {
		return
	}
	resultID, err := strconv.ParseInt(request.ProviderResultID, 10, 64)
	if err != nil || resultID <= 0 {
		s.writeError(w, store.ErrDirectValidation)
		return
	}
	result, err := s.app.ReconcileDirectExternalCreation(r.Context(), access.UserID, access.WorkspaceID, campaign, request.DirectExternalEditFence, chi.URLParam(r, "client_request_id"), resultID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeExternalDetail(w, result, access.Can(app.CapabilityAdsWrite), access.Can(app.CapabilityAdsBudgetManage))
}
