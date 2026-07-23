package app

import (
	"context"
	"sort"

	"maxpilot/backend/internal/store"
)

// Capability is a stable, role-independent permission identifier returned to
// clients. UI code must use these values instead of inferring permissions from
// a role name; that keeps role policy centralized on the server.
type Capability string

const (
	CapabilityWorkspaceRead        Capability = "workspace.read"
	CapabilityWorkspaceUpdate      Capability = "workspace.update"
	CapabilityWorkspaceDelete      Capability = "workspace.delete"
	CapabilityMembersRead          Capability = "members.read"
	CapabilityMembersManage        Capability = "members.manage"
	CapabilityInvitesRead          Capability = "invitations.read"
	CapabilityInvitesManage        Capability = "invitations.manage"
	CapabilityChannelsRead         Capability = "channels.read"
	CapabilityChannelsManage       Capability = "channels.manage"
	CapabilityPostsRead            Capability = "posts.read"
	CapabilityPostsWrite           Capability = "posts.write"
	CapabilityPostsDelete          Capability = "posts.delete"
	CapabilityMediaRead            Capability = "media.read"
	CapabilityMediaWrite           Capability = "media.write"
	CapabilityAIUse                Capability = "ai.use"
	CapabilityCommentsRead         Capability = "comments.read"
	CapabilityCommentsWrite        Capability = "comments.write"
	CapabilityCommentsResolve      Capability = "comments.resolve"
	CapabilityReviewSubmit         Capability = "review.submit"
	CapabilityReviewDecide         Capability = "review.decide"
	CapabilityPostsPublish         Capability = "posts.publish"
	CapabilityAuditRead            Capability = "audit.read"
	CapabilityNotificationsRead    Capability = "notifications.read"
	CapabilityNotificationsManage  Capability = "notifications.manage"
	CapabilityAdsRead              Capability = "ads.read"
	CapabilityAdsWrite             Capability = "ads.write"
	CapabilityAdsApprove           Capability = "ads.approve"
	CapabilityAdsLaunch            Capability = "ads.launch"
	CapabilityAdsBudgetManage      Capability = "ads.budget.manage"
	CapabilityAdsCredentialsManage Capability = "ads.credentials.manage"
)

// AccessContext is the authorization result for one user and one workspace.
// Capabilities are intentionally materialized so they can be returned by the
// API and logged without duplicating role-policy switches in every handler.
type AccessContext struct {
	WorkspaceID  string       `json:"workspace_id"`
	UserID       string       `json:"-"`
	Role         string       `json:"role"`
	Capabilities []Capability `json:"capabilities"`
}

func (a AccessContext) Can(capability Capability) bool {
	for _, granted := range a.Capabilities {
		if granted == capability {
			return true
		}
	}
	return false
}

// AccessContextForRole is the single role-to-capability policy table. Unknown
// roles deliberately receive no capabilities.
func AccessContextForRole(workspaceID, userID, role string) AccessContext {
	granted := map[Capability]bool{}
	add := func(capabilities ...Capability) {
		for _, capability := range capabilities {
			granted[capability] = true
		}
	}

	switch role {
	case "owner":
		add(
			CapabilityWorkspaceRead, CapabilityWorkspaceUpdate, CapabilityWorkspaceDelete,
			CapabilityMembersRead, CapabilityMembersManage,
			CapabilityInvitesRead, CapabilityInvitesManage,
			CapabilityChannelsRead, CapabilityChannelsManage,
			CapabilityPostsRead, CapabilityPostsWrite, CapabilityPostsDelete,
			CapabilityMediaRead, CapabilityMediaWrite, CapabilityAIUse,
			CapabilityCommentsRead, CapabilityCommentsWrite, CapabilityCommentsResolve,
			CapabilityReviewSubmit, CapabilityReviewDecide, CapabilityPostsPublish,
			CapabilityAuditRead, CapabilityNotificationsRead, CapabilityNotificationsManage,
			CapabilityAdsRead, CapabilityAdsWrite, CapabilityAdsApprove, CapabilityAdsLaunch,
			CapabilityAdsBudgetManage, CapabilityAdsCredentialsManage,
		)
	case "editor":
		add(
			CapabilityWorkspaceRead, CapabilityMembersRead,
			CapabilityChannelsRead,
			CapabilityPostsRead, CapabilityPostsWrite, CapabilityPostsDelete,
			CapabilityMediaRead, CapabilityMediaWrite, CapabilityAIUse,
			CapabilityCommentsRead, CapabilityCommentsWrite, CapabilityCommentsResolve,
			CapabilityReviewSubmit, CapabilityPostsPublish,
			CapabilityNotificationsRead, CapabilityNotificationsManage,
			CapabilityAdsRead, CapabilityAdsWrite,
		)
	case "approver":
		add(
			CapabilityWorkspaceRead, CapabilityMembersRead,
			CapabilityChannelsRead, CapabilityPostsRead,
			CapabilityMediaRead, CapabilityCommentsRead, CapabilityCommentsWrite, CapabilityReviewDecide,
			CapabilityNotificationsRead, CapabilityNotificationsManage,
			CapabilityAdsRead, CapabilityAdsApprove,
		)
	case "viewer":
		add(
			CapabilityWorkspaceRead, CapabilityMembersRead,
			CapabilityChannelsRead, CapabilityPostsRead, CapabilityMediaRead, CapabilityCommentsRead,
			CapabilityNotificationsRead, CapabilityNotificationsManage,
			CapabilityAdsRead,
		)
	}

	capabilities := make([]Capability, 0, len(granted))
	for capability := range granted {
		capabilities = append(capabilities, capability)
	}
	sort.Slice(capabilities, func(i, j int) bool { return capabilities[i] < capabilities[j] })
	return AccessContext{WorkspaceID: workspaceID, UserID: userID, Role: role, Capabilities: capabilities}
}

func AccessContextForWorkspace(workspace store.Workspace, userID, role string) AccessContext {
	access := AccessContextForRole(workspace.ID, userID, role)
	if !workspace.IsPersonal {
		return access
	}
	blocked := map[Capability]bool{
		CapabilityWorkspaceDelete: true,
		CapabilityMembersRead:     true,
		CapabilityMembersManage:   true,
		CapabilityInvitesRead:     true,
		CapabilityInvitesManage:   true,
	}
	filtered := access.Capabilities[:0]
	for _, capability := range access.Capabilities {
		if !blocked[capability] {
			filtered = append(filtered, capability)
		}
	}
	access.Capabilities = filtered
	return access
}

// ResolveWorkspaceAccess keeps the persistence representation of roles out of
// HTTP handlers and returns the authoritative workspace settings alongside the
// API capability set.
func (a *App) ResolveWorkspaceAccess(ctx context.Context, userID, workspaceID string) (store.Workspace, AccessContext, error) {
	access, err := a.store.ResolveWorkspaceAccess(ctx, userID, workspaceID)
	if err != nil {
		return store.Workspace{}, AccessContext{}, err
	}
	return access.Workspace, AccessContextForWorkspace(access.Workspace, userID, access.Member.Role), nil
}
