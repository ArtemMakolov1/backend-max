package api

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/openairesearch"
	"maxpilot/backend/internal/store"
)

type directConnectionResponse struct {
	ID           string     `json:"id"`
	AccountID    string     `json:"account_id"`
	Status       string     `json:"status"`
	AccountLogin string     `json:"account_login"`
	DisplayName  string     `json:"display_name"`
	CurrencyCode string     `json:"currency_code"`
	Timezone     string     `json:"timezone"`
	ReadOnly     bool       `json:"read_only"`
	ErrorCode    string     `json:"error_code,omitempty"`
	LastSyncedAt *time.Time `json:"last_synced_at"`
}

type directIntegrationResponse struct {
	Configured                    bool                      `json:"configured"`
	WritesEnabled                 bool                      `json:"writes_enabled"`
	AutoLaunchEnabled             bool                      `json:"auto_launch_enabled"`
	MaxCampaignWeeklyBudgetMinor  int64                     `json:"max_campaign_weekly_budget_minor"`
	MaxWorkspaceWeeklyBudgetMinor int64                     `json:"max_workspace_weekly_budget_minor"`
	Sandbox                       bool                      `json:"sandbox"`
	Connected                     bool                      `json:"connected"`
	Connection                    *directConnectionResponse `json:"connection,omitempty"`
	Capabilities                  []string                  `json:"capabilities"`
}

type directCampaignResponse struct {
	ID                  string                        `json:"id"`
	ConnectionID        string                        `json:"connection_id"`
	Name                string                        `json:"name"`
	Objective           string                        `json:"objective"`
	LandingURL          string                        `json:"landing_url"`
	Brief               string                        `json:"brief"`
	Titles              []string                      `json:"titles"`
	Texts               []string                      `json:"texts"`
	Keywords            []string                      `json:"keywords"`
	NegativeKeywords    []string                      `json:"negative_keywords"`
	Regions             []string                      `json:"regions"`
	WeeklyBudgetMinor   int64                         `json:"weekly_budget_minor"`
	CurrencyCode        string                        `json:"currency_code"`
	StartsAt            string                        `json:"starts_at"`
	EndsAt              string                        `json:"ends_at"`
	Status              string                        `json:"status"`
	LaunchState         string                        `json:"launch_state"`
	ProviderCampaignID  *string                       `json:"provider_campaign_id"`
	ProviderState       *string                       `json:"provider_state"`
	ModerationStatus    *string                       `json:"moderation_status"`
	StatusClarification *string                       `json:"status_clarification"`
	AutoLaunch          store.DirectAutoLaunchSummary `json:"auto_launch"`
	Version             int64                         `json:"version"`
	CreatedAt           time.Time                     `json:"created_at"`
	UpdatedAt           time.Time                     `json:"updated_at"`
	SetupWarningCode    *string                       `json:"setup_warning_code"`
	GraphVerified       bool                          `json:"graph_verified"`
	GraphHash           *string                       `json:"graph_hash"`
	RevisionID          *string                       `json:"revision_id"`
}

type directCampaignDraftRequest struct {
	Name              string   `json:"name"`
	Objective         string   `json:"objective"`
	LandingURL        string   `json:"landing_url"`
	Brief             string   `json:"brief"`
	Titles            []string `json:"titles"`
	Texts             []string `json:"texts"`
	Keywords          []string `json:"keywords"`
	NegativeKeywords  []string `json:"negative_keywords"`
	Regions           []string `json:"regions"`
	WeeklyBudgetMinor int64    `json:"weekly_budget_minor"`
	CurrencyCode      string   `json:"currency_code"`
	StartsAt          string   `json:"starts_at"`
	EndsAt            string   `json:"ends_at"`
}

type directCampaignPatchRequest struct {
	Name               *string   `json:"name"`
	Objective          *string   `json:"objective"`
	LandingURL         *string   `json:"landing_url"`
	Brief              *string   `json:"brief"`
	Titles             *[]string `json:"titles"`
	Texts              *[]string `json:"texts"`
	Keywords           *[]string `json:"keywords"`
	NegativeKeywords   *[]string `json:"negative_keywords"`
	Regions            *[]string `json:"regions"`
	WeeklyBudgetMinor  *int64    `json:"weekly_budget_minor"`
	StartsAt           *string   `json:"starts_at"`
	EndsAt             *string   `json:"ends_at"`
	ExpectedVersion    int64     `json:"expected_version"`
	ExpectedGraphHash  string    `json:"expected_graph_hash"`
	ExpectedRevisionID string    `json:"expected_revision_id"`
}

type directCampaignSuggestionRequest struct {
	Objective         string   `json:"objective"`
	LandingURL        string   `json:"landing_url"`
	Brief             string   `json:"brief"`
	Audience          string   `json:"audience,omitempty"`
	Regions           []string `json:"regions"`
	WeeklyBudgetMinor int64    `json:"weekly_budget_minor,omitempty"`
	CurrencyCode      string   `json:"currency_code"`
	StartsAt          string   `json:"starts_at"`
	EndsAt            string   `json:"ends_at"`
}

type directOAuthCompleteRequest struct {
	Code  string `json:"code"`
	State string `json:"state"`
}

type directConsentRequest struct {
	Confirmation               string `json:"confirmation"`
	ExpectedConnectionID       string `json:"expected_connection_id"`
	ExpectedAccountID          string `json:"expected_account_id"`
	ExpectedCampaignName       string `json:"expected_campaign_name"`
	ExpectedProviderCampaignID string `json:"expected_provider_campaign_id"`
	ExpectedVersion            int64  `json:"expected_version"`
	WeeklyBudgetMinor          int64  `json:"weekly_budget_minor"`
	StartsAt                   string `json:"starts_at"`
	EndsAt                     string `json:"ends_at"`
	ExpectedGraphHash          string `json:"expected_graph_hash"`
	ExpectedRevisionID         string `json:"expected_revision_id"`
}

func (s *Server) registerDirectAdvertisingRoutes(r chi.Router) {
	r.Route("/advertising/direct", func(r chi.Router) {
		r.Get("/", s.getDirectIntegration)
		r.Post("/connect/start", s.startDirectConnection)
		r.Post("/connect/complete", s.completeDirectConnection)
		r.Delete("/connection", s.revokeDirectConnection)
		r.Get("/campaigns", s.listDirectCampaigns)
		r.Post("/campaigns", s.createDirectCampaign)
		r.Post("/campaigns/suggest", s.suggestDirectCampaign)
		r.Patch("/campaigns/{campaign_id}", s.updateDirectCampaign)
		r.Post("/campaigns/{campaign_id}/auto-launch-consent", s.grantDirectConsent)
		r.Delete("/campaigns/{campaign_id}/auto-launch-consent", s.revokeDirectConsent)
		r.Post("/campaigns/{campaign_id}/submit", s.submitDirectCampaign)
		r.Post("/campaigns/{campaign_id}/launch", s.launchDirectCampaign)
	})
}

func (s *Server) getDirectIntegration(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityAdsRead)
	if !ok {
		return
	}
	status, err := s.app.GetDirectIntegrationStatus(
		r.Context(), access.UserID, access.WorkspaceID,
	)
	if err != nil {
		s.writeError(w, err)
		return
	}
	response := directIntegrationResponse{
		Configured: status.Configured, WritesEnabled: status.WritesEnabled,
		AutoLaunchEnabled:             status.AutoLaunchEnabled,
		MaxCampaignWeeklyBudgetMinor:  store.DirectMaxCampaignWeeklyBudgetMinor,
		MaxWorkspaceWeeklyBudgetMinor: store.DirectMaxWorkspaceWeeklyBudgetMinor,
		Sandbox:                       status.Sandbox, Connected: status.Connected,
		Capabilities: directCapabilities(access),
	}
	if status.Connection != nil {
		response.Connection = publicDirectConnection(*status.Connection)
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, map[string]any{"integration": response})
}

func (s *Server) startDirectConnection(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") != s.frontendOrigin {
		s.problem(w, http.StatusForbidden, "origin_required",
			"An exact frontend Origin is required to start Yandex Direct authorization", nil)
		return
	}
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityAdsCredentialsManage)
	if !ok {
		return
	}
	sessionBinding, err := authenticatedSessionBinding(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	result, err := s.app.StartDirectOAuth(
		r.Context(), access.UserID, access.WorkspaceID, sessionBinding,
		"", "/app/#/advertising",
	)
	if err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, map[string]any{"connection": result})
}

func (s *Server) completeDirectConnection(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") != s.frontendOrigin {
		s.problem(w, http.StatusForbidden, "origin_required",
			"An exact frontend Origin is required to complete Yandex Direct authorization", nil)
		return
	}
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityAdsCredentialsManage)
	if !ok {
		return
	}
	sessionBinding, err := authenticatedSessionBinding(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	var request directOAuthCompleteRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	completion, err := s.app.CompleteDirectOAuthVerification(
		r.Context(), access.UserID, access.WorkspaceID, sessionBinding,
		request.State, request.Code,
	)
	if err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, map[string]any{
		"connection": publicDirectConnection(completion.Connection),
	})
}

func (s *Server) finishDirectOAuth(w http.ResponseWriter, r *http.Request) {
	userID, err := authenticatedUserID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	sessionBinding, err := authenticatedSessionBinding(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if state == "" || code == "" {
		http.Redirect(w, r, directOAuthErrorRedirect(s.frontendOrigin, "invalid_oauth"), http.StatusSeeOther)
		return
	}
	completion, err := s.app.CompleteDirectOAuthCallback(
		r.Context(), userID, sessionBinding, state, code,
	)
	if err != nil {
		s.logger.Warn("Yandex Direct OAuth callback failed", "error", err)
		http.Redirect(w, r, directOAuthErrorRedirect(s.frontendOrigin, "connect_failed"), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, s.frontendOrigin+completion.ReturnTo, http.StatusSeeOther)
}

func (s *Server) revokeDirectConnection(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityAdsCredentialsManage)
	if !ok {
		return
	}
	if err := s.app.RevokeDirectConnection(
		r.Context(), access.UserID, access.WorkspaceID,
	); err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusNoContent, nil)
}

func (s *Server) listDirectCampaigns(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityAdsRead)
	if !ok {
		return
	}
	campaigns, err := s.app.Store().ListDirectCampaigns(
		r.Context(), access.UserID, access.WorkspaceID,
	)
	if err != nil {
		s.writeError(w, err)
		return
	}
	items := make([]directCampaignResponse, 0, len(campaigns))
	for _, campaign := range campaigns {
		items = append(items, publicDirectCampaign(campaign))
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) createDirectCampaign(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityAdsWrite)
	if !ok {
		return
	}
	var request directCampaignDraftRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	startsAt, endsAt, err := parseDirectDates(request.StartsAt, request.EndsAt)
	if err != nil {
		s.writeError(w, err)
		return
	}
	campaign, err := s.app.CreateDirectCampaign(r.Context(), access.UserID, access.WorkspaceID, store.DirectCampaign{
		Name: request.Name, Objective: request.Objective, LandingURL: request.LandingURL,
		Brief: request.Brief, Regions: request.Regions,
		Titles: request.Titles, Texts: request.Texts, Keywords: request.Keywords,
		NegativeKeywords:  request.NegativeKeywords,
		WeeklyBudgetMinor: request.WeeklyBudgetMinor, CurrencyCode: request.CurrencyCode,
		StartsAt: startsAt, EndsAt: endsAt, CreatedAt: s.now().UTC(),
	})
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, map[string]any{"campaign": publicDirectCampaign(campaign)})
}

func (s *Server) updateDirectCampaign(w http.ResponseWriter, r *http.Request) {
	_, access, campaignID, ok := s.requireDirectCampaignCapability(w, r, app.CapabilityAdsWrite)
	if !ok {
		return
	}
	var request directCampaignPatchRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	changes := store.DirectCampaignChanges{
		Name: request.Name, Objective: request.Objective, LandingURL: request.LandingURL,
		Brief: request.Brief, Regions: request.Regions, Titles: request.Titles,
		Texts: request.Texts, Keywords: request.Keywords,
		NegativeKeywords:  request.NegativeKeywords,
		WeeklyBudgetMinor: request.WeeklyBudgetMinor, ExpectedVersion: request.ExpectedVersion,
	}
	if request.StartsAt != nil {
		value, err := parseDirectDate(*request.StartsAt)
		if err != nil {
			s.writeError(w, err)
			return
		}
		changes.StartsAt = &value
	}
	if request.EndsAt != nil {
		value, err := parseDirectDate(*request.EndsAt)
		if err != nil {
			s.writeError(w, err)
			return
		}
		changes.EndsAt = &value
	}
	campaign, err := s.app.UpdateDirectCampaign(
		r.Context(), access.UserID, access.WorkspaceID, campaignID, changes,
		request.ExpectedGraphHash, request.ExpectedRevisionID,
	)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"campaign": publicDirectCampaign(campaign)})
}

func (s *Server) suggestDirectCampaign(w http.ResponseWriter, r *http.Request) {
	workspace, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityAdsWrite)
	if !ok {
		return
	}
	var request directCampaignSuggestionRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	if _, _, err := parseDirectDates(request.StartsAt, request.EndsAt); err != nil {
		s.writeError(w, err)
		return
	}
	if !s.app.DirectCampaignSuggestionConfigured() {
		s.writeError(w, app.ErrResearchNotConfigured)
		return
	}
	release, err := s.aiLimiter.acquireForWorkspaceMetric(
		r.Context(), access.UserID, workspace, store.AIOperationResearch,
		store.UsageMetricAIResearchRequests, 1, s.now().UTC())
	if err != nil {
		s.writeError(w, err)
		return
	}
	defer release()
	ctx, cancel := contextWithTimeout(r, AIHandlerTimeout)
	defer cancel()
	result, err := s.app.SuggestDirectCampaign(ctx, access.UserID, access.WorkspaceID,
		openairesearch.SuggestDirectCampaignRequest{
			Objective: request.Objective, Brief: request.Brief, LandingURL: request.LandingURL,
			Audience: request.Audience, Regions: request.Regions,
			WeeklyBudgetMinor: request.WeeklyBudgetMinor, CurrencyCode: request.CurrencyCode,
		})
	if err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, map[string]any{"suggestion": result})
}

func (s *Server) grantDirectConsent(w http.ResponseWriter, r *http.Request) {
	_, access, campaignID, ok := s.requireDirectSpendCapability(w, r)
	if !ok {
		return
	}
	var request directConsentRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	startsAt, endsAt, err := parseDirectDates(request.StartsAt, request.EndsAt)
	if err != nil {
		s.writeError(w, err)
		return
	}
	rawProviderCampaignID := request.ExpectedProviderCampaignID
	providerCampaignID, err := strconv.ParseInt(rawProviderCampaignID, 10, 64)
	if !canonicalPositiveDirectInteger(rawProviderCampaignID) ||
		err != nil || providerCampaignID <= 0 {
		s.writeError(w, validationError("expected_provider_campaign_id must be a positive integer string"))
		return
	}
	campaign, err := s.app.GrantDirectAutoLaunchConsent(
		r.Context(), access.UserID, access.WorkspaceID, campaignID, store.DirectConsentRequest{
			Confirmation: request.Confirmation, ExpectedVersion: request.ExpectedVersion,
			ExpectedConnectionID: request.ExpectedConnectionID,
			ExpectedAccountID:    request.ExpectedAccountID,
			ExpectedCampaignName: request.ExpectedCampaignName,
			ExpectedProviderID:   providerCampaignID,
			ExpectedGraphHash:    request.ExpectedGraphHash,
			ExpectedRevisionID:   request.ExpectedRevisionID,
			WeeklyBudgetMinor:    request.WeeklyBudgetMinor,
			StartsAt:             startsAt, EndsAt: endsAt,
		})
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"campaign": publicDirectCampaign(campaign)})
}

func (s *Server) revokeDirectConsent(w http.ResponseWriter, r *http.Request) {
	_, access, campaignID, ok := s.requireDirectSpendCapability(w, r)
	if !ok {
		return
	}
	campaign, err := s.app.RevokeDirectAutoLaunchConsent(
		r.Context(), access.UserID, access.WorkspaceID, campaignID,
	)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"campaign": publicDirectCampaign(campaign)})
}

func (s *Server) submitDirectCampaign(w http.ResponseWriter, r *http.Request) {
	_, access, campaignID, ok := s.requireDirectCampaignCapability(w, r, app.CapabilityAdsApprove)
	if !ok {
		return
	}
	var request struct {
		ExpectedVersion int64 `json:"expected_version"`
	}
	if !s.decodeJSON(w, r, &request) {
		return
	}
	campaign, err := s.app.SubmitDirectCampaign(
		r.Context(), access.UserID, access.WorkspaceID, campaignID, request.ExpectedVersion,
	)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"campaign": publicDirectCampaign(campaign)})
}

func (s *Server) launchDirectCampaign(w http.ResponseWriter, r *http.Request) {
	_, access, campaignID, ok := s.requireDirectSpendCapability(w, r)
	if !ok {
		return
	}
	var request struct {
		Confirmation       string `json:"confirmation"`
		ExpectedVersion    int64  `json:"expected_version"`
		ExpectedGraphHash  string `json:"expected_graph_hash"`
		ExpectedRevisionID string `json:"expected_revision_id"`
	}
	if !s.decodeJSON(w, r, &request) {
		return
	}
	if request.Confirmation != "ЗАПУСТИТЬ" || request.ExpectedVersion <= 0 {
		s.writeError(w, validationError("exact launch confirmation and expected_version are required"))
		return
	}
	campaign, err := s.app.LaunchDirectCampaign(
		r.Context(), access.UserID, access.WorkspaceID, campaignID, request.ExpectedVersion,
		request.ExpectedGraphHash, request.ExpectedRevisionID,
	)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"campaign": publicDirectCampaign(campaign)})
}

func (s *Server) requireDirectCampaignCapability(
	w http.ResponseWriter, r *http.Request, capability app.Capability,
) (store.Workspace, app.AccessContext, string, bool) {
	workspace, access, ok := s.requireWorkspaceCapability(w, r, capability)
	if !ok {
		return store.Workspace{}, app.AccessContext{}, "", false
	}
	campaignID := strings.TrimSpace(chi.URLParam(r, "campaign_id"))
	if campaignID == "" || len(campaignID) > 128 {
		s.writeError(w, store.ErrNotFound)
		return store.Workspace{}, app.AccessContext{}, "", false
	}
	return workspace, access, campaignID, true
}

func (s *Server) requireDirectSpendCapability(
	w http.ResponseWriter, r *http.Request,
) (store.Workspace, app.AccessContext, string, bool) {
	workspace, access, campaignID, ok := s.requireDirectCampaignCapability(w, r, app.CapabilityAdsLaunch)
	if !ok {
		return store.Workspace{}, app.AccessContext{}, "", false
	}
	if !access.Can(app.CapabilityAdsBudgetManage) {
		s.problem(w, http.StatusForbidden, "workspace_forbidden",
			"Workspace budget management access is required", nil)
		return store.Workspace{}, app.AccessContext{}, "", false
	}
	return workspace, access, campaignID, true
}

func publicDirectConnection(connection store.DirectConnection) *directConnectionResponse {
	status := safeDirectConnectionStatus(connection.Status)
	return &directConnectionResponse{
		ID: connection.ID, AccountID: connection.AccountID, Status: status,
		AccountLogin: connection.ClientLogin, DisplayName: connection.AccountName,
		CurrencyCode: connection.CurrencyCode, Timezone: connection.Timezone,
		ReadOnly: connection.ReadOnly, ErrorCode: safeDirectConnectionErrorCode(connection.ErrorCode),
		LastSyncedAt: connection.LastVerifiedAt,
	}
}

func safeDirectConnectionStatus(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "active":
		return "active"
	case "error":
		return "error"
	case "revoked":
		return "revoked"
	default:
		return "error"
	}
}

func safeDirectConnectionErrorCode(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return ""
	}
	if len(value) > 64 || value[0] < 'a' || value[0] > 'z' {
		return "connection_error"
	}
	for _, character := range value[1:] {
		if (character < 'a' || character > 'z') &&
			(character < '0' || character > '9') && character != '_' {
			return "connection_error"
		}
	}
	return value
}

func canonicalPositiveDirectInteger(value string) bool {
	if value == "" || value[0] < '1' || value[0] > '9' {
		return false
	}
	for _, character := range value[1:] {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func publicDirectCampaign(campaign store.DirectCampaign) directCampaignResponse {
	var providerID *string
	if campaign.ProviderCampaignID != nil {
		value := strconv.FormatInt(*campaign.ProviderCampaignID, 10)
		providerID = &value
	}
	var moderationStatus, providerState, clarification, graphHash, revisionID, setupWarning *string
	if value := strings.TrimSpace(campaign.ProviderState); value != "" {
		providerState = &value
	}
	if value := strings.TrimSpace(campaign.ModerationStatus); value != "" {
		moderationStatus = &value
	}
	if value := strings.TrimSpace(campaign.SubmissionFailureClarification); value != "" {
		clarification = &value
	} else if value := strings.TrimSpace(campaign.ModerationClarification); value != "" {
		clarification = &value
	} else if value := strings.TrimSpace(campaign.LaunchFailureCode); value != "" {
		clarification = &value
	} else if value := strings.TrimSpace(campaign.ProviderState); value != "" {
		clarification = &value
	}
	graphVerified := campaign.GraphVerifiedAt != nil &&
		len(strings.TrimSpace(campaign.ProviderGraphHash)) == 64 &&
		strings.TrimSpace(campaign.ProviderRevisionID) != "" &&
		strings.TrimSpace(campaign.LaunchFailureCode) != "provider_snapshot_mismatch" &&
		campaign.ProviderCampaignID != nil &&
		campaign.ProviderAdGroupID != nil &&
		campaign.ProviderAdID != nil &&
		len(campaign.ProviderKeywordMappings) == len(campaign.Keywords)
	if graphVerified {
		hash, revision := campaign.ProviderGraphHash, campaign.ProviderRevisionID
		graphHash, revisionID = &hash, &revision
	} else {
		warning := strings.TrimSpace(campaign.SubmissionFailureCode)
		if warning == "" &&
			strings.TrimSpace(campaign.LaunchFailureCode) == "provider_snapshot_mismatch" {
			warning = "provider_snapshot_mismatch"
		}
		if warning == "" {
			warning = "provider_graph_unverified"
		}
		setupWarning = &warning
	}
	return directCampaignResponse{
		ID: campaign.ID, ConnectionID: campaign.ConnectionID, Name: campaign.Name,
		Objective: campaign.Objective, LandingURL: campaign.LandingURL, Brief: campaign.Brief,
		Titles: campaign.Titles, Texts: campaign.Texts, Keywords: campaign.Keywords,
		NegativeKeywords: campaign.NegativeKeywords,
		Regions:          campaign.Regions, WeeklyBudgetMinor: campaign.WeeklyBudgetMinor,
		CurrencyCode: campaign.CurrencyCode, StartsAt: campaign.StartsAt.Format(time.DateOnly),
		EndsAt: campaign.EndsAt.Format(time.DateOnly), Status: campaign.Status,
		LaunchState:        campaign.LaunchState,
		ProviderCampaignID: providerID, ProviderState: providerState,
		ModerationStatus:    moderationStatus,
		StatusClarification: clarification, AutoLaunch: campaign.AutoLaunch,
		Version: campaign.Version, CreatedAt: campaign.CreatedAt, UpdatedAt: campaign.UpdatedAt,
		SetupWarningCode: setupWarning, GraphVerified: graphVerified,
		GraphHash: graphHash, RevisionID: revisionID,
	}
}

func directCapabilities(access app.AccessContext) []string {
	result := make([]string, 0, 6)
	for _, capability := range access.Capabilities {
		if strings.HasPrefix(string(capability), "ads.") {
			result = append(result, string(capability))
		}
	}
	return result
}

func parseDirectDates(startsAt, endsAt string) (time.Time, time.Time, error) {
	start, err := parseDirectDate(startsAt)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	end, err := parseDirectDate(endsAt)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if end.Before(start) {
		return time.Time{}, time.Time{}, validationError("ends_at must not be before starts_at")
	}
	return start, end, nil
}

func parseDirectDate(value string) (time.Time, error) {
	parsed, err := time.Parse(time.DateOnly, strings.TrimSpace(value))
	if err != nil || parsed.Format(time.DateOnly) != strings.TrimSpace(value) {
		return time.Time{}, validationError("date must use YYYY-MM-DD")
	}
	return parsed.UTC(), nil
}

func directOAuthErrorRedirect(frontendOrigin, code string) string {
	return strings.TrimRight(frontendOrigin, "/") + "/app/?direct_error=" +
		url.QueryEscape(code) + "#/advertising"
}
