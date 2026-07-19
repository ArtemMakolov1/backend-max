package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/openairesearch"
	"maxpilot/backend/internal/store"
)

// RegisterWorkspaceBrandRoutes registers routes relative to an existing
// /workspaces/{workspace_id} route. Keeping the feature registration local
// avoids coupling its implementation to the main server route table.
func (s *Server) RegisterWorkspaceBrandRoutes(r chi.Router) {
	r.Get("/brand-kit", s.getWorkspaceBrandKit)
	r.Put("/brand-kit", s.updateWorkspaceBrandKit)
	r.Patch("/brand-kit", s.updateWorkspaceBrandKit)
	r.Post("/brand-kit/suggest", s.suggestWorkspaceBrandKit)
	r.Get("/channel-templates", s.listChannelTemplates)
	r.Post("/channel-templates", s.createChannelTemplate)
	r.Get("/channel-templates/{template_id}", s.getChannelTemplate)
	r.Put("/channel-templates/{template_id}", s.updateChannelTemplate)
	r.Patch("/channel-templates/{template_id}", s.updateChannelTemplate)
	r.Delete("/channel-templates/{template_id}", s.deleteChannelTemplate)
}

type brandProfileRequest struct {
	Audience       string   `json:"audience"`
	Tone           string   `json:"tone"`
	CTA            string   `json:"cta"`
	ForbiddenWords []string `json:"forbidden_words"`
	ExamplePosts   []string `json:"example_posts"`
	VisualStyle    string   `json:"visual_style"`
}

func (request brandProfileRequest) storeProfile() store.BrandProfile {
	return store.BrandProfile{
		Audience: request.Audience, Tone: request.Tone, CTA: request.CTA,
		ForbiddenWords: request.ForbiddenWords, ExamplePosts: request.ExamplePosts,
		VisualStyle: request.VisualStyle,
	}
}

type updateBrandKitRequest struct {
	brandProfileRequest
	ExpectedVersion int64 `json:"expected_version"`
}

type channelTemplateRequest struct {
	brandProfileRequest
	ChannelID *int64 `json:"channel_id"`
	Name      string `json:"name"`
	IsDefault bool   `json:"is_default"`
}

type updateChannelTemplateRequest struct {
	channelTemplateRequest
	ExpectedVersion int64 `json:"expected_version"`
}

type brandedWorkspaceResearchRequest struct {
	openairesearch.Request
	ChannelTemplateID *int64 `json:"channel_template_id,omitempty"`
	ChannelID         *int64 `json:"channel_id,omitempty"`
}

// decodeBrandedWorkspaceResearchRequest merges trusted, workspace-scoped
// selection with untrusted editorial values. The resulting fields still remain
// untrusted data in the OpenAI prompt; only the tenant lookup is authoritative.
func (s *Server) decodeBrandedWorkspaceResearchRequest(
	w http.ResponseWriter, r *http.Request, access app.AccessContext,
) (openairesearch.Request, bool) {
	var request brandedWorkspaceResearchRequest
	if !s.decodeJSON(w, r, &request) {
		return openairesearch.Request{}, false
	}
	context, err := s.app.Store().ResolveWorkspaceBrandContext(
		r.Context(), access.UserID, access.WorkspaceID, request.ChannelTemplateID, request.ChannelID,
	)
	if err != nil {
		s.writeError(w, err)
		return openairesearch.Request{}, false
	}
	profile := context.BrandKit.BrandProfile
	if context.Template != nil {
		profile = overlayBrandProfile(profile, context.Template.BrandProfile)
	}
	request.Request = applyBrandProfileDefaults(request.Request, profile)
	if err := openairesearch.ValidateRequest(request.Request); err != nil {
		s.writeError(w, err)
		return openairesearch.Request{}, false
	}
	return request.Request, true
}

func overlayBrandProfile(base, override store.BrandProfile) store.BrandProfile {
	if strings.TrimSpace(override.Audience) != "" {
		base.Audience = override.Audience
	}
	if strings.TrimSpace(override.Tone) != "" {
		base.Tone = override.Tone
	}
	if strings.TrimSpace(override.CTA) != "" {
		base.CTA = override.CTA
	}
	if len(override.ForbiddenWords) != 0 {
		base.ForbiddenWords = append([]string(nil), override.ForbiddenWords...)
	}
	if len(override.ExamplePosts) != 0 {
		base.ExamplePosts = append([]string(nil), override.ExamplePosts...)
	}
	if strings.TrimSpace(override.VisualStyle) != "" {
		base.VisualStyle = override.VisualStyle
	}
	return base
}

func applyBrandProfileDefaults(request openairesearch.Request, profile store.BrandProfile) openairesearch.Request {
	if strings.TrimSpace(request.Audience) == "" {
		request.Audience = profile.Audience
	}
	if strings.TrimSpace(request.Tone) == "" {
		request.Tone = profile.Tone
	}
	if strings.TrimSpace(request.CTA) == "" {
		request.CTA = profile.CTA
	}
	if request.ForbiddenWords == nil {
		request.ForbiddenWords = append([]string(nil), profile.ForbiddenWords...)
	}
	if request.ExamplePosts == nil {
		request.ExamplePosts = append([]string(nil), profile.ExamplePosts...)
	}
	if strings.TrimSpace(request.VisualStyle) == "" {
		request.VisualStyle = profile.VisualStyle
	}
	return request
}

func (s *Server) getWorkspaceBrandKit(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityWorkspaceRead)
	if !ok {
		return
	}
	kit, err := s.app.Store().GetWorkspaceBrandKit(r.Context(), access.UserID, access.WorkspaceID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, kit)
}

func (s *Server) updateWorkspaceBrandKit(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityPostsWrite)
	if !ok {
		return
	}
	var request updateBrandKitRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	kit, err := s.app.Store().UpdateWorkspaceBrandKit(r.Context(), access.UserID, access.WorkspaceID,
		store.WorkspaceBrandKitUpdate{BrandProfile: request.storeProfile(), ExpectedVersion: request.ExpectedVersion})
	if err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, kit)
}

// suggestWorkspaceBrandKit returns a non-persisted Brand Kit draft derived
// from the workspace's own posts. The user reviews the suggestion and saves it
// through the regular brand kit update endpoint, so the capability requirement
// matches updateWorkspaceBrandKit.
func (s *Server) suggestWorkspaceBrandKit(w http.ResponseWriter, r *http.Request) {
	workspace, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityPostsWrite)
	if !ok {
		return
	}
	if !s.app.BrandKitSuggestionConfigured() {
		s.writeError(w, app.ErrResearchNotConfigured)
		return
	}
	release, err := s.aiLimiter.acquireForWorkspaceMetric(
		r.Context(), access.UserID, workspace, store.AIOperationResearch,
		store.UsageMetricAIFormatRequests, 1, s.now().UTC())
	if err != nil {
		s.writeError(w, err)
		return
	}
	defer release()
	ctx, cancel := contextWithTimeout(r, AIHandlerTimeout)
	defer cancel()
	result, err := s.app.SuggestBrandKit(ctx, access.UserID, access.WorkspaceID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, result)
}

func (s *Server) listChannelTemplates(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityWorkspaceRead)
	if !ok {
		return
	}
	templates, err := s.app.Store().ListChannelTemplates(r.Context(), access.UserID, access.WorkspaceID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, templates)
}

func (s *Server) getChannelTemplate(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityWorkspaceRead)
	if !ok {
		return
	}
	templateID, err := parsePositivePathID(r, "template_id")
	if err != nil {
		s.writeError(w, store.ErrNotFound)
		return
	}
	template, err := s.app.Store().GetChannelTemplate(r.Context(), access.UserID, access.WorkspaceID, templateID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, template)
}

func (s *Server) createChannelTemplate(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityPostsWrite)
	if !ok {
		return
	}
	var request channelTemplateRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	template, err := s.app.Store().CreateChannelTemplate(r.Context(), access.UserID, access.WorkspaceID,
		store.ChannelTemplateCreate{
			ChannelID: request.ChannelID, Name: request.Name, BrandProfile: request.storeProfile(), IsDefault: request.IsDefault,
		})
	if err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusCreated, template)
}

func (s *Server) updateChannelTemplate(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityPostsWrite)
	if !ok {
		return
	}
	templateID, err := parsePositivePathID(r, "template_id")
	if err != nil {
		s.writeError(w, store.ErrNotFound)
		return
	}
	var request updateChannelTemplateRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	template, err := s.app.Store().UpdateChannelTemplate(r.Context(), access.UserID, access.WorkspaceID, templateID,
		store.ChannelTemplateUpdate{
			ChannelID: request.ChannelID, Name: request.Name, BrandProfile: request.storeProfile(),
			IsDefault: request.IsDefault, ExpectedVersion: request.ExpectedVersion,
		})
	if err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, template)
}

func (s *Server) deleteChannelTemplate(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityPostsWrite)
	if !ok {
		return
	}
	templateID, err := parsePositivePathID(r, "template_id")
	if err != nil {
		s.writeError(w, store.ErrNotFound)
		return
	}
	expectedVersion, err := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("expected_version")), 10, 64)
	if err != nil || expectedVersion <= 0 {
		s.writeError(w, errors.New("expected version must be a positive integer"))
		return
	}
	if err := s.app.Store().DeleteChannelTemplate(
		r.Context(), access.UserID, access.WorkspaceID, templateID, expectedVersion,
	); err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusNoContent, nil)
}
