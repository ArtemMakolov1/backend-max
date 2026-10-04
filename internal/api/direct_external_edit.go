package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/store"
	"maxpilot/backend/internal/yandexdirect"
)

type directExternalAdResponse struct {
	ProviderAdID      string   `json:"provider_ad_id"`
	ProviderAdGroupID string   `json:"provider_ad_group_id"`
	AdType            string   `json:"ad_type"`
	ProviderStatus    string   `json:"provider_status"`
	ProviderState     string   `json:"provider_state"`
	Title             *string  `json:"title"`
	Title2            *string  `json:"title2"`
	Text              *string  `json:"text"`
	Titles            []string `json:"titles"`
	Texts             []string `json:"texts"`
	Href              *string  `json:"href"`
	EditableFields    []string `json:"editable_fields"`
	ReadOnlyReason    *string  `json:"read_only_reason"`
}
type directExternalDetailResponse struct {
	directExternalCampaignResponse
	store.DirectExternalEditControl
	ConnectionID   string                     `json:"connection_id"`
	EditableFields []string                   `json:"editable_fields"`
	ReadOnlyReason *string                    `json:"read_only_reason"`
	Ads            []directExternalAdResponse `json:"ads"`
}
type externalNamePatchRequest struct {
	store.DirectExternalEditFence
	Name string `json:"name"`
}
type externalAdPatchFields struct {
	Title  *string         `json:"title,omitempty"`
	Title2 json.RawMessage `json:"title2,omitempty"`
	Text   *string         `json:"text,omitempty"`
	Titles *[]string       `json:"titles,omitempty"`
	Texts  *[]string       `json:"texts,omitempty"`
	Href   *string         `json:"href,omitempty"`
}

func (r externalAdPatchFields) changes() (yandexdirect.ExternalAdChanges, error) {
	changes := yandexdirect.ExternalAdChanges{Title: r.Title, Text: r.Text, Titles: r.Titles, Texts: r.Texts, Href: r.Href}
	if len(r.Title2) > 0 {
		changes.Title2Set = true
		if err := json.Unmarshal(r.Title2, &changes.Title2); err != nil {
			return changes, fmt.Errorf("%w: title2 must be a string or null", store.ErrDirectValidation)
		}
	}
	return changes, nil
}

type externalAdPatchRequest struct {
	store.DirectExternalEditFence
	externalAdPatchFields
}
type externalCopyRequest struct {
	store.DirectExternalEditFence
	Brief        string                 `json:"brief"`
	CurrentDraft *externalAdPatchFields `json:"current_draft,omitempty"`
}

// Called inside the authenticated /advertising/direct workspace group.
func (s *Server) registerDirectExternalEditingRoutes(r chi.Router) {
	r.Get("/campaigns/external/{provider_campaign_id}", s.getDirectExternalDetail)
	r.Patch("/campaigns/external/{provider_campaign_id}", s.patchDirectExternalName)
	r.Patch("/campaigns/external/{provider_campaign_id}/ads/{provider_ad_id}", s.patchDirectExternalAd)
	r.Post("/campaigns/external/{provider_campaign_id}/import", s.bookmarkDirectExternal)
	r.Delete("/campaigns/external/{provider_campaign_id}/import", s.unbookmarkDirectExternal)
	r.Post("/campaigns/external/{provider_campaign_id}/ads/{provider_ad_id}/suggest", s.suggestDirectExternalCopy)
}
func (s *Server) externalIDs(w http.ResponseWriter, r *http.Request) (int64, int64, bool) {
	campaign, err := strconv.ParseInt(chi.URLParam(r, "provider_campaign_id"), 10, 64)
	if err != nil || campaign <= 0 {
		s.writeError(w, store.ErrNotFound)
		return 0, 0, false
	}
	var ad int64
	if raw := chi.URLParam(r, "provider_ad_id"); raw != "" {
		ad, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || ad <= 0 {
			s.writeError(w, store.ErrNotFound)
			return 0, 0, false
		}
	}
	return campaign, ad, true
}
func (s *Server) getDirectExternalDetail(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityAdsRead)
	if !ok {
		return
	}
	campaign, _, ok := s.externalIDs(w, r)
	if !ok {
		return
	}
	result, err := s.app.GetDirectExternalDetail(r.Context(), access.UserID, access.WorkspaceID, r.URL.Query().Get("connection_id"), campaign)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeExternalDetail(w, result, access.Can(app.CapabilityAdsWrite))
}
func (s *Server) patchDirectExternalName(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityAdsWrite)
	if !ok {
		return
	}
	campaign, _, ok := s.externalIDs(w, r)
	if !ok {
		return
	}
	var request externalNamePatchRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	result, err := s.app.EditDirectExternalCampaignName(r.Context(), access.UserID, access.WorkspaceID, campaign, request.DirectExternalEditFence, request.Name)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeExternalDetail(w, result, access.Can(app.CapabilityAdsWrite))
}
func (s *Server) patchDirectExternalAd(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityAdsWrite)
	if !ok {
		return
	}
	campaign, ad, ok := s.externalIDs(w, r)
	if !ok {
		return
	}
	var request externalAdPatchRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	patch, err := request.changes()
	if err != nil {
		s.writeError(w, err)
		return
	}
	result, err := s.app.EditDirectExternalAd(r.Context(), access.UserID, access.WorkspaceID, campaign, ad, request.DirectExternalEditFence, patch)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeExternalDetail(w, result, access.Can(app.CapabilityAdsWrite))
}
func (s *Server) bookmarkDirectExternal(w http.ResponseWriter, r *http.Request) {
	s.setExternalBookmark(w, r, true)
}
func (s *Server) unbookmarkDirectExternal(w http.ResponseWriter, r *http.Request) {
	s.setExternalBookmark(w, r, false)
}
func (s *Server) setExternalBookmark(w http.ResponseWriter, r *http.Request, bookmark bool) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityAdsWrite)
	if !ok {
		return
	}
	campaign, _, ok := s.externalIDs(w, r)
	if !ok {
		return
	}
	var request struct {
		ConnectionID string `json:"expected_connection_id"`
	}
	if !s.decodeJSON(w, r, &request) {
		return
	}
	if request.ConnectionID == "" {
		s.writeError(w, store.ErrDirectValidation)
		return
	}
	result, err := s.app.BookmarkDirectExternal(r.Context(), access.UserID, access.WorkspaceID, request.ConnectionID, campaign, bookmark)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeExternalDetail(w, result, access.Can(app.CapabilityAdsWrite))
}
func (s *Server) suggestDirectExternalCopy(w http.ResponseWriter, r *http.Request) {
	workspace, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityAdsWrite)
	if !ok {
		return
	}
	campaign, ad, ok := s.externalIDs(w, r)
	if !ok {
		return
	}
	var request externalCopyRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	var draft *yandexdirect.ExternalAdChanges
	if request.CurrentDraft != nil {
		changes, err := request.CurrentDraft.changes()
		if err != nil {
			s.writeError(w, err)
			return
		}
		draft = &changes
	}
	var release func()
	defer func() {
		if release != nil {
			release()
		}
	}()
	ctx, cancel := contextWithTimeout(r, AIHandlerTimeout)
	defer cancel()
	result, err := s.app.SuggestDirectExternalCopyWithBeforeGenerate(ctx, access.UserID, access.WorkspaceID, campaign, ad, request.DirectExternalEditFence, request.Brief, draft, func() error {
		var err error
		release, err = s.aiLimiter.acquireForWorkspaceMetric(ctx, access.UserID, workspace, store.AIOperationResearch, store.UsageMetricAIResearchRequests, 1, s.now().UTC())
		return err
	})
	if err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, map[string]any{"suggestion": result})
}
func (s *Server) writeExternalDetail(w http.ResponseWriter, detail app.DirectExternalDetail, canWrite bool) {
	campaign := detail.Provider.Campaign
	result := directExternalDetailResponse{directExternalCampaignResponse: directExternalCampaignResponse{ProviderCampaignID: strconv.FormatInt(campaign.ID, 10), Name: campaign.Name, CampaignType: campaign.Type, ProviderStatus: campaign.Status, ProviderState: campaign.State, ProviderStatusPayment: campaign.StatusPayment, StartsAt: campaign.StartDate, EndsAt: campaign.EndDate, Timezone: campaign.TimeZone, SyncedAt: s.now().UTC()}, DirectExternalEditControl: detail.Control, ConnectionID: detail.Connection.ID, EditableFields: []string{}, Ads: []directExternalAdResponse{}}
	var reason string
	switch {
	case !canWrite:
		reason = "member_read_only"
	case !s.app.DirectExternalEditConfigured():
		reason = "writes_unavailable"
	case detail.Connection.ReadOnly:
		reason = "account_read_only"
	case detail.Control.EditState != "idle":
		reason = "edit_in_progress"
	case campaign.State == "ARCHIVED":
		reason = "campaign_archived"
	}
	if reason == "" {
		result.EditableFields = []string{"name"}
	} else {
		result.ReadOnlyReason = &reason
	}
	for _, ad := range detail.Provider.Ads {
		out := directExternalAdResponse{ProviderAdID: strconv.FormatInt(ad.ID, 10), ProviderAdGroupID: strconv.FormatInt(ad.AdGroupID, 10), AdType: ad.Type, ProviderStatus: ad.Status, ProviderState: ad.State, Title2: ad.Title2, Titles: ad.Titles, Texts: ad.Texts, Href: ad.Href, EditableFields: []string{}}
		adReason := reason
		if adReason == "" && ad.State == "ARCHIVED" {
			adReason = "ad_archived"
		}
		switch ad.Type {
		case "TEXT_AD":
			out.Title = &ad.Title
			out.Text = &ad.Text
			if adReason == "" {
				out.EditableFields = []string{"title", "title2", "text", "href"}
			}
		case "RESPONSIVE_AD":
			if len(ad.Titles) == 0 || len(ad.Texts) == 0 || (ad.Href == nil && ad.BusinessID == nil) {
				adReason = "ad_fields_unavailable"
			}
			if adReason == "" {
				out.EditableFields = []string{"titles", "texts", "href"}
			}
		default:
			adReason = "unsupported_ad_type"
		}
		if adReason != "" {
			out.ReadOnlyReason = &adReason
		}
		result.Ads = append(result.Ads, out)
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, map[string]any{"campaign": result})
}
