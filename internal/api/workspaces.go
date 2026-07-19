package api

import (
	"errors"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/store"
)

const (
	maxWorkspaceNameRunes = 120
	maxWorkspaceIDLength  = 128
)

type workspaceResponse struct {
	Workspace store.Workspace   `json:"workspace"`
	Access    app.AccessContext `json:"access"`
}

type createWorkspaceRequest struct {
	Name string `json:"name"`
}

type updateWorkspaceRequest struct {
	Name                    *string `json:"name,omitempty"`
	ApprovalRequired        *bool   `json:"approval_required,omitempty"`
	RequireDistinctApprover *bool   `json:"require_distinct_approver,omitempty"`
}

type memberRequest struct {
	UserID string `json:"user_id"`
	Role   string `json:"role"`
}

type roleRequest struct {
	Role string `json:"role"`
}

type transferWorkspaceRequest struct {
	NewOwnerUserID string `json:"new_owner_user_id"`
}

type workspaceMemberResponse struct {
	store.WorkspaceMember
	DisplayName string `json:"display_name"`
	Email       string `json:"email,omitempty"`
	AvatarURL   string `json:"avatar_url,omitempty"`
}

type invitationRequest struct {
	Email         string `json:"email"`
	TargetUserID  string `json:"target_user_id,omitempty"`
	Role          string `json:"role"`
	ExpiresInDays int    `json:"expires_in_days,omitempty"`
}

type invitationResponse struct {
	Invitation store.WorkspaceInvitation `json:"invitation"`
	Token      string                    `json:"token"`
	AcceptURL  string                    `json:"accept_url"`
}

func (s *Server) listWorkspaces(w http.ResponseWriter, r *http.Request) {
	userID, err := authenticatedUserID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	stored, err := s.app.Store().ListWorkspaces(r.Context(), userID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	result := make([]workspaceResponse, 0, len(stored))
	for _, access := range stored {
		result = append(result, workspaceResponse{
			Workspace: access.Workspace,
			Access:    app.AccessContextForWorkspace(access.Workspace, userID, access.Member.Role),
		})
	}
	s.writeJSON(w, http.StatusOK, result)
}

func (s *Server) createWorkspace(w http.ResponseWriter, r *http.Request) {
	userID, err := authenticatedUserID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	var request createWorkspaceRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	name, err := validateWorkspaceName(request.Name)
	if err != nil {
		s.writeError(w, err)
		return
	}
	workspace, err := s.app.Store().CreateWorkspaceLimited(
		r.Context(), userID, store.Workspace{Name: name}, s.maxOwnedTeamWorkspaces,
	)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, workspaceResponse{
		Workspace: workspace,
		Access:    app.AccessContextForWorkspace(workspace, userID, store.WorkspaceRoleOwner),
	})
}

func (s *Server) getWorkspace(w http.ResponseWriter, r *http.Request) {
	workspace, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityWorkspaceRead)
	if !ok {
		return
	}
	s.writeJSON(w, http.StatusOK, workspaceResponse{Workspace: workspace, Access: access})
}

func (s *Server) updateWorkspace(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityWorkspaceUpdate)
	if !ok {
		return
	}
	var request updateWorkspaceRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	if request.Name == nil && request.ApprovalRequired == nil && request.RequireDistinctApprover == nil {
		s.problem(w, http.StatusBadRequest, "validation_error", "At least one workspace setting is required", nil)
		return
	}
	if request.Name != nil {
		name, err := validateWorkspaceName(*request.Name)
		if err != nil {
			s.writeError(w, err)
			return
		}
		request.Name = &name
	}
	workspace, err := s.app.Store().UpdateWorkspace(r.Context(), access.UserID, access.WorkspaceID, store.WorkspaceChanges{
		Name: request.Name, ApprovalRequired: request.ApprovalRequired,
		RequireDistinctApprover: request.RequireDistinctApprover,
	})
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, workspaceResponse{Workspace: workspace, Access: access})
}

func (s *Server) deleteWorkspace(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityWorkspaceDelete)
	if !ok {
		return
	}
	if err := s.app.Store().DeleteWorkspace(r.Context(), access.UserID, access.WorkspaceID); err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusNoContent, nil)
}

func (s *Server) transferWorkspaceOwnership(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityWorkspaceUpdate)
	if !ok {
		return
	}
	if !access.Can(app.CapabilityMembersManage) {
		s.problem(w, http.StatusForbidden, "workspace_forbidden", "Workspace ownership cannot be transferred", nil)
		return
	}
	var request transferWorkspaceRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	request.NewOwnerUserID = strings.TrimSpace(request.NewOwnerUserID)
	if request.NewOwnerUserID == "" || request.NewOwnerUserID == access.UserID {
		s.problem(w, http.StatusBadRequest, "validation_error", "A different existing member is required", nil)
		return
	}
	workspace, err := s.app.Store().TransferWorkspaceOwnershipLimited(
		r.Context(), access.UserID, access.WorkspaceID, request.NewOwnerUserID, s.maxOwnedTeamWorkspaces)
	if err != nil {
		s.writeError(w, err)
		return
	}
	_, updatedAccess, err := s.app.ResolveWorkspaceAccess(r.Context(), access.UserID, access.WorkspaceID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, workspaceResponse{Workspace: workspace, Access: updatedAccess})
}

func (s *Server) listWorkspaceMembers(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityMembersRead)
	if !ok {
		return
	}
	members, err := s.app.Store().ListWorkspaceMembers(r.Context(), access.UserID, access.WorkspaceID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	if !access.Can(app.CapabilityMembersManage) {
		for index := range members {
			members[index].Email = ""
		}
	}
	s.writeJSON(w, http.StatusOK, members)
}

func (s *Server) addWorkspaceMember(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityMembersManage)
	if !ok {
		return
	}
	var request memberRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	request.UserID = strings.TrimSpace(request.UserID)
	request.Role = strings.TrimSpace(request.Role)
	if request.UserID == "" || !validAssignableWorkspaceRole(request.Role) {
		s.problem(w, http.StatusBadRequest, "validation_error", "A user_id and assignable role are required", nil)
		return
	}
	member, err := s.app.Store().AddWorkspaceMember(r.Context(), access.UserID, store.WorkspaceMember{
		WorkspaceID: access.WorkspaceID, UserID: request.UserID, Role: request.Role,
	})
	if err != nil {
		s.writeError(w, err)
		return
	}
	enriched, err := s.enrichWorkspaceMember(r, member)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, enriched)
}

func (s *Server) updateWorkspaceMember(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityMembersManage)
	if !ok {
		return
	}
	memberUserID := strings.TrimSpace(chi.URLParam(r, "user_id"))
	var request roleRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	request.Role = strings.TrimSpace(request.Role)
	if memberUserID == "" || !validAssignableWorkspaceRole(request.Role) {
		s.problem(w, http.StatusBadRequest, "validation_error", "A valid member and role are required", nil)
		return
	}
	member, err := s.app.Store().UpdateWorkspaceMemberRole(
		r.Context(), access.UserID, access.WorkspaceID, memberUserID, request.Role)
	if err != nil {
		s.writeError(w, err)
		return
	}
	enriched, err := s.enrichWorkspaceMember(r, member)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, enriched)
}

func (s *Server) removeWorkspaceMember(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityMembersManage)
	if !ok {
		return
	}
	memberUserID := strings.TrimSpace(chi.URLParam(r, "user_id"))
	if memberUserID == "" {
		s.problem(w, http.StatusBadRequest, "validation_error", "A member user_id is required", nil)
		return
	}
	if err := s.app.Store().RemoveWorkspaceMember(r.Context(), access.UserID, access.WorkspaceID, memberUserID); err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusNoContent, nil)
}

func (s *Server) listWorkspaceInvitations(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityInvitesRead)
	if !ok {
		return
	}
	includeClosed := r.URL.Query().Get("include_closed") == "true"
	invitations, err := s.app.Store().ListWorkspaceInvitations(r.Context(), access.UserID, access.WorkspaceID, includeClosed)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, invitations)
}

func (s *Server) createWorkspaceInvitation(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityInvitesManage)
	if !ok {
		return
	}
	var request invitationRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	email := strings.TrimSpace(request.Email)
	targetUserID := strings.TrimSpace(request.TargetUserID)
	if email != "" && targetUserID != "" {
		s.problem(w, http.StatusBadRequest, "validation_error", "Use email or target_user_id, not both", nil)
		return
	}
	if email != "" {
		var normalizeErr error
		email, normalizeErr = normalizeInvitationEmail(email)
		if normalizeErr != nil {
			s.problem(w, http.StatusBadRequest, "validation_error", "Invitation email is invalid", nil)
			return
		}
	}
	if targetUserID != "" {
		if len(targetUserID) > 256 {
			s.problem(w, http.StatusBadRequest, "validation_error", "target_user_id is invalid", nil)
			return
		}
		if _, lookupErr := s.app.Store().GetUser(r.Context(), targetUserID); lookupErr != nil {
			s.writeError(w, lookupErr)
			return
		}
	}
	if !validAssignableWorkspaceRole(strings.TrimSpace(request.Role)) {
		s.problem(w, http.StatusBadRequest, "validation_error", "An assignable role is required", nil)
		return
	}
	days := request.ExpiresInDays
	if days == 0 {
		days = 7
	}
	if days < 1 || days > 30 {
		s.problem(w, http.StatusBadRequest, "validation_error", "expires_in_days must be between 1 and 30", nil)
		return
	}
	token, err := randomURLToken(32)
	if err != nil {
		s.writeError(w, err)
		return
	}
	now := s.now().UTC()
	invitation, err := s.app.Store().CreateWorkspaceInvitation(r.Context(), access.UserID, store.WorkspaceInvitation{
		WorkspaceID: access.WorkspaceID, Email: email, TargetUserID: targetUserID, Role: strings.TrimSpace(request.Role),
		TokenHash: sha256Hex(token), CreatedAt: now, ExpiresAt: now.Add(time.Duration(days) * 24 * time.Hour),
	})
	if err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusCreated, invitationResponse{
		Invitation: invitation,
		Token:      token,
		AcceptURL:  s.frontendOrigin + "/app/#/invite/" + token,
	})
}

func (s *Server) revokeWorkspaceInvitation(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityInvitesManage)
	if !ok {
		return
	}
	invitationID := strings.TrimSpace(chi.URLParam(r, "invitation_id"))
	if invitationID == "" {
		s.problem(w, http.StatusBadRequest, "validation_error", "An invitation id is required", nil)
		return
	}
	if err := s.app.Store().RevokeWorkspaceInvitation(
		r.Context(), access.UserID, access.WorkspaceID, invitationID, s.now().UTC()); err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusNoContent, nil)
}

func (s *Server) acceptWorkspaceInvitation(w http.ResponseWriter, r *http.Request) {
	userID, err := authenticatedUserID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	token := strings.TrimSpace(chi.URLParam(r, "token"))
	if token == "" {
		s.writeError(w, store.ErrNotFound)
		return
	}
	member, err := s.app.Store().AcceptWorkspaceInvitation(r.Context(), userID, sha256Hex(token), s.now().UTC())
	if err != nil {
		s.writeError(w, err)
		return
	}
	workspace, access, err := s.app.ResolveWorkspaceAccess(r.Context(), userID, member.WorkspaceID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, workspaceResponse{Workspace: workspace, Access: access})
}

func (s *Server) requireWorkspaceCapability(
	w http.ResponseWriter, r *http.Request, capability app.Capability,
) (store.Workspace, app.AccessContext, bool) {
	userID, err := authenticatedUserID(r)
	if err != nil {
		s.writeError(w, err)
		return store.Workspace{}, app.AccessContext{}, false
	}
	workspaceID := strings.TrimSpace(chi.URLParam(r, "workspace_id"))
	if workspaceID == "" || len(workspaceID) > maxWorkspaceIDLength {
		s.writeError(w, store.ErrNotFound)
		return store.Workspace{}, app.AccessContext{}, false
	}
	workspace, access, err := s.app.ResolveWorkspaceAccess(r.Context(), userID, workspaceID)
	if err != nil {
		// Membership is deliberately undiscoverable: an authenticated outsider
		// receives the same 404 as for a workspace that does not exist.
		s.writeError(w, err)
		return store.Workspace{}, app.AccessContext{}, false
	}
	if !access.Can(capability) {
		s.problem(w, http.StatusForbidden, "workspace_forbidden", "Your workspace access does not allow this action", map[string]string{
			"required_capability": string(capability),
		})
		return store.Workspace{}, app.AccessContext{}, false
	}
	return workspace, access, true
}

func validateWorkspaceName(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || utf8.RuneCountInString(value) > maxWorkspaceNameRunes {
		return "", errors.New("workspace name must contain 1 to 120 characters")
	}
	return value, nil
}

func validAssignableWorkspaceRole(role string) bool {
	return role == store.WorkspaceRoleEditor || role == store.WorkspaceRoleApprover || role == store.WorkspaceRoleViewer
}

func normalizeInvitationEmail(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	parsed, err := mail.ParseAddress(value)
	if err != nil || !strings.EqualFold(parsed.Address, value) || len(value) > 320 {
		return "", errors.New("invalid invitation email")
	}
	return value, nil
}

func parsePositivePathID(r *http.Request, name string) (int64, error) {
	value, err := strconv.ParseInt(strings.TrimSpace(chi.URLParam(r, name)), 10, 64)
	if err != nil || value <= 0 {
		return 0, errors.New(name + " must be a positive integer")
	}
	return value, nil
}

func (s *Server) enrichWorkspaceMember(r *http.Request, member store.WorkspaceMember) (workspaceMemberResponse, error) {
	user, err := s.app.Store().GetUser(r.Context(), member.UserID)
	if err != nil {
		return workspaceMemberResponse{}, err
	}
	return workspaceMemberResponse{
		WorkspaceMember: member,
		DisplayName:     user.DisplayName,
		Email:           user.Email,
		AvatarURL:       user.AvatarURL,
	}, nil
}
