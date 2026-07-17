package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/maxclient"
	"maxpilot/backend/internal/media"
	"maxpilot/backend/internal/store"
)

type workspaceAPIFixture struct {
	storage   *store.Store
	app       *app.App
	workspace store.Workspace
	channel   store.Channel
	post      store.Post
	logger    *slog.Logger
}

func TestWorkspaceAPIAuthorizationCollaborationAndApprovalGate(t *testing.T) {
	fixture := newWorkspaceAPIFixture(t)
	owner := fixture.handler(t, "ws-owner")
	editor := fixture.handler(t, "ws-editor")
	approver := fixture.handler(t, "ws-approver")
	viewer := fixture.handler(t, "ws-viewer")
	outsider := fixture.handler(t, "ws-outsider")
	base := "/api/v1/workspaces/" + fixture.workspace.ID

	t.Run("outsider workspace is undiscoverable while member denial is forbidden", func(t *testing.T) {
		response := performJSONRequest(outsider, http.MethodGet, base, "")
		assertProblemCode(t, response, http.StatusNotFound, "not_found")

		response = performJSONRequest(viewer, http.MethodPatch, base+"/posts/"+postID(fixture.post.ID), `{"title":"forbidden"}`)
		assertProblemCode(t, response, http.StatusForbidden, "workspace_forbidden")
	})

	t.Run("editor can read and update content created by owner", func(t *testing.T) {
		response := performJSONRequest(editor, http.MethodGet, base+"/posts/"+postID(fixture.post.ID), "")
		if response.Code != http.StatusOK {
			t.Fatalf("GET team post = %d %s", response.Code, response.Body.String())
		}
		response = performJSONRequest(editor, http.MethodPatch, base+"/posts/"+postID(fixture.post.ID), `{"title":"Edited by teammate"}`)
		if response.Code != http.StatusOK {
			t.Fatalf("PATCH cross-owner post = %d %s", response.Code, response.Body.String())
		}
		stored, err := fixture.storage.GetPostForWorkspace(t.Context(), "ws-editor", fixture.workspace.ID, fixture.post.ID)
		if err != nil || stored.Title != "Edited by teammate" {
			t.Fatalf("updated post = %#v, %v", stored, err)
		}
	})

	t.Run("invitation response exposes bearer once but never its hash", func(t *testing.T) {
		response := performJSONRequest(owner, http.MethodPost, base+"/invitations", `{"role":"viewer"}`)
		if response.Code != http.StatusCreated {
			t.Fatalf("create bearer invitation = %d %s", response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), "token_hash") {
			t.Fatalf("create invitation exposed token_hash: %s", response.Body.String())
		}
		var payload invitationResponse
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Token == "" || payload.AcceptURL == "" || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("one-time invitation payload = %#v, headers=%v", payload, response.Header())
		}
		response = performJSONRequest(owner, http.MethodGet, base+"/invitations", "")
		if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "token_hash") || strings.Contains(response.Body.String(), payload.Token) {
			t.Fatalf("invitation list leaked secret = %d %s", response.Code, response.Body.String())
		}
	})

	t.Run("only editor or owner can resolve a comment", func(t *testing.T) {
		comment, err := fixture.storage.CreatePostComment(t.Context(), "ws-editor", store.PostComment{
			WorkspaceID: fixture.workspace.ID, PostID: fixture.post.ID, Body: "Please clarify",
		})
		if err != nil {
			t.Fatal(err)
		}
		path := base + "/posts/" + postID(fixture.post.ID) + "/comments/" + postID(comment.ID)
		response := performJSONRequest(approver, http.MethodPatch, path, `{"resolved":true}`)
		assertProblemCode(t, response, http.StatusForbidden, "workspace_forbidden")
		response = performJSONRequest(editor, http.MethodPatch, path, `{"resolved":true}`)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "resolved_at") {
			t.Fatalf("editor resolve comment = %d %s", response.Code, response.Body.String())
		}
	})

	t.Run("current revision approval gates schedule and publish", func(t *testing.T) {
		scheduleAt := time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339)
		scheduleBody := `{"scheduled_at":"` + scheduleAt + `"}`
		response := performJSONRequest(editor, http.MethodPost, base+"/posts/"+postID(fixture.post.ID)+"/schedule", scheduleBody)
		assertProblemCode(t, response, http.StatusConflict, "post_approval_required")
		response = performJSONRequest(editor, http.MethodPost, base+"/posts/"+postID(fixture.post.ID)+"/publish", `{}`)
		assertProblemCode(t, response, http.StatusConflict, "post_approval_required")

		response = performJSONRequest(editor, http.MethodPost, base+"/posts/"+postID(fixture.post.ID)+"/review", "")
		if response.Code != http.StatusOK {
			t.Fatalf("submit review = %d %s", response.Code, response.Body.String())
		}
		var submitted struct {
			Revision store.PostRevision `json:"revision"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &submitted); err != nil || submitted.Revision.ID <= 0 {
			t.Fatalf("submitted revision = %#v, %v", submitted, err)
		}
		response = performJSONRequest(approver, http.MethodPost,
			base+"/posts/"+postID(fixture.post.ID)+"/review/approve",
			`{"revision_id":`+postID(submitted.Revision.ID)+`,"comment":"Approved"}`)
		if response.Code != http.StatusCreated {
			t.Fatalf("approve = %d %s", response.Code, response.Body.String())
		}
		response = performJSONRequest(editor, http.MethodPost, base+"/posts/"+postID(fixture.post.ID)+"/schedule", scheduleBody)
		if response.Code != http.StatusOK {
			t.Fatalf("schedule approved = %d %s", response.Code, response.Body.String())
		}

		response = performJSONRequest(editor, http.MethodPatch, base+"/posts/"+postID(fixture.post.ID), `{"content":"changed after approval"}`)
		if response.Code != http.StatusOK {
			t.Fatalf("edit approved = %d %s", response.Code, response.Body.String())
		}
		response = performJSONRequest(editor, http.MethodPost, base+"/posts/"+postID(fixture.post.ID)+"/publish", `{}`)
		assertProblemCode(t, response, http.StatusConflict, "post_approval_required")
	})
}

func TestCreateWorkspacePostWithExplicitNullScheduleCreatesDraftUnderApprovalPolicy(t *testing.T) {
	fixture := newWorkspaceAPIFixture(t)
	editor := fixture.handler(t, "ws-editor")
	base := "/api/v1/workspaces/" + fixture.workspace.ID

	response := performJSONRequest(editor, http.MethodPost, base+"/posts",
		`{"title":"Draft","content":"body","format":"markdown","scheduled_at":null}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("draft with explicit null schedule = %d %s", response.Code, response.Body.String())
	}
	var created store.Post
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Status != store.PostStatusDraft || created.ScheduledAt != nil {
		t.Fatalf("created post = %#v", created)
	}

	scheduleAt := time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339)
	response = performJSONRequest(editor, http.MethodPost, base+"/posts",
		`{"title":"Scheduled","content":"body","format":"markdown","channel_id":`+postID(fixture.channel.ID)+`,"scheduled_at":"`+scheduleAt+`"}`)
	assertProblemCode(t, response, http.StatusConflict, "post_approval_required")
}

func TestWorkspaceCapabilitiesAreExhaustiveAndStable(t *testing.T) {
	owner := app.AccessContextForRole("workspace", "owner", store.WorkspaceRoleOwner)
	want := []app.Capability{
		app.CapabilityWorkspaceRead, app.CapabilityWorkspaceUpdate, app.CapabilityWorkspaceDelete,
		app.CapabilityMembersRead, app.CapabilityMembersManage,
		app.CapabilityInvitesRead, app.CapabilityInvitesManage,
		app.CapabilityChannelsRead, app.CapabilityChannelsManage,
		app.CapabilityPostsRead, app.CapabilityPostsWrite, app.CapabilityPostsDelete,
		app.CapabilityMediaRead, app.CapabilityMediaWrite, app.CapabilityAIUse,
		app.CapabilityCommentsRead, app.CapabilityCommentsWrite, app.CapabilityCommentsResolve,
		app.CapabilityReviewSubmit, app.CapabilityReviewDecide, app.CapabilityPostsPublish,
		app.CapabilityAuditRead, app.CapabilityNotificationsRead, app.CapabilityNotificationsManage,
	}
	for _, capability := range want {
		if !owner.Can(capability) {
			t.Errorf("owner capability contract missing %q", capability)
		}
	}
	if app.AccessContextForRole("workspace", "approver", store.WorkspaceRoleApprover).Can(app.CapabilityPostsPublish) {
		t.Fatal("approver unexpectedly received posts.publish")
	}
}

func TestWorkspaceOwnerLimitReturnsDedicatedConflict(t *testing.T) {
	fixture := newWorkspaceAPIFixture(t)
	if _, err := fixture.storage.CreateWorkspace(
		t.Context(), "ws-editor", store.Workspace{Name: "Editor's existing workspace"},
	); err != nil {
		t.Fatal(err)
	}
	handler := withTestSession(t, fixture.storage,
		New(fixture.app, fixture.logger, "http://localhost:4321", "webhook-secret", AuthOptions{
			YandexClient: &fakeYandexOAuth{}, MaxOwnedTeamWorkspaces: 1,
		}).Handler(), "ws-owner")

	response := performJSONRequest(handler, http.MethodPost, "/api/v1/workspaces", `{"name":"Over the limit"}`)
	assertProblemCode(t, response, http.StatusConflict, "workspace_owner_limit_reached")

	response = performJSONRequest(handler, http.MethodPost,
		"/api/v1/workspaces/"+fixture.workspace.ID+"/transfer-ownership",
		`{"new_owner_user_id":"ws-editor"}`)
	assertProblemCode(t, response, http.StatusConflict, "workspace_owner_limit_reached")
}

func TestWorkspacePublicationControlsAreScopedAndCapabilityGated(t *testing.T) {
	fixture := newWorkspaceAPIFixture(t)
	ctx := t.Context()
	publishedAt := time.Now().UTC().Add(-time.Hour)
	post, err := fixture.storage.CreatePostForWorkspace(ctx, "ws-owner", fixture.workspace.ID, store.Post{
		Title: "Published campaign", Content: "Approved copy", Format: store.FormatMarkdown,
		Status: store.PostStatusPublished, ChannelID: &fixture.channel.ID,
		MAXMessageID: "mid.workspace.publication", PublishedAt: &publishedAt, Notify: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	views := int64(73)
	fake := &claimWebhookMAX{
		chat: maxclient.ChatInfo{
			ChatID: fixture.channel.MAXChatID, OwnerID: "max-owner", Type: "channel", Status: "active", Title: "Agency channel",
		},
		membership: maxclient.Membership{IsAdmin: true, Permissions: []maxclient.Permission{
			maxclient.PermissionReadAllMessages, maxclient.PermissionWrite, maxclient.PermissionPinMessage,
		}},
		message: maxclient.Message{
			MessageID: post.MAXMessageID, ChatID: fixture.channel.MAXChatID,
			URL: "https://max.ru/channel/workspace-publication", Views: &views,
		},
	}
	application := app.New(fixture.storage, fixture.app.Media(), fake, nil, nil, fixture.logger)
	handler := func(userID string) http.Handler {
		return withTestSession(t, fixture.storage,
			New(application, fixture.logger, "http://localhost:4321", "webhook-secret", AuthOptions{
				YandexClient: &fakeYandexOAuth{},
			}).Handler(), userID)
	}
	editor, viewer, outsider := handler("ws-editor"), handler("ws-viewer"), handler("ws-outsider")
	base := "/api/v1/workspaces/" + fixture.workspace.ID + "/posts/" + postID(post.ID)

	for _, request := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/update-published"},
		{http.MethodPost, "/sync-max"},
		{http.MethodPost, "/pin"},
		{http.MethodDelete, "/pin"},
		{http.MethodDelete, "/publication"},
	} {
		response := performJSONRequest(viewer, request.method, base+request.path, "")
		assertProblemCode(t, response, http.StatusForbidden, "workspace_forbidden")
	}
	response := performJSONRequest(outsider, http.MethodGet, base+"/view-history", "")
	assertProblemCode(t, response, http.StatusNotFound, "not_found")

	response = performJSONRequest(editor, http.MethodPost, base+"/sync-max", "")
	if response.Code != http.StatusOK {
		t.Fatalf("workspace sync = %d %s", response.Code, response.Body.String())
	}
	var synced store.Post
	if err := json.Unmarshal(response.Body.Bytes(), &synced); err != nil {
		t.Fatal(err)
	}
	if synced.MAXViews == nil || *synced.MAXViews != views || synced.MAXMessageURL != fake.message.URL {
		t.Fatalf("workspace sync response = %#v", synced)
	}

	response = performJSONRequest(viewer, http.MethodGet, base+"/view-history?limit=1", "")
	if response.Code != http.StatusOK {
		t.Fatalf("viewer history = %d %s", response.Code, response.Body.String())
	}
	var history []store.PostViewSnapshot
	if err := json.Unmarshal(response.Body.Bytes(), &history); err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].PostID != post.ID || history[0].Views != views {
		t.Fatalf("workspace history = %#v", history)
	}
	response = performJSONRequest(viewer, http.MethodGet, base+"/view-history?limit=1001", "")
	assertProblemCode(t, response, http.StatusBadRequest, "validation_error")

	revision, err := fixture.storage.SubmitPostForReview(ctx, "ws-editor", fixture.workspace.ID, post.ID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.storage.DecidePostReview(ctx, "ws-approver", fixture.workspace.ID, post.ID,
		revision.ID, store.ReviewDecisionApproved, "Approved", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	response = performJSONRequest(editor, http.MethodPost, base+"/update-published", "")
	if response.Code != http.StatusOK {
		t.Fatalf("workspace update published = %d %s", response.Code, response.Body.String())
	}

	response = performJSONRequest(editor, http.MethodPost, base+"/pin", "")
	if response.Code != http.StatusOK || fake.pinRuns != 1 {
		t.Fatalf("workspace pin = %d %s, calls=%d", response.Code, response.Body.String(), fake.pinRuns)
	}
	fake.pinnedMessage = &maxclient.Message{MessageID: post.MAXMessageID, ChatID: fixture.channel.MAXChatID}
	response = performJSONRequest(editor, http.MethodDelete, base+"/pin", "")
	if response.Code != http.StatusOK || fake.unpinRuns != 1 {
		t.Fatalf("workspace unpin = %d %s, calls=%d", response.Code, response.Body.String(), fake.unpinRuns)
	}

	response = performJSONRequest(editor, http.MethodDelete, base+"/publication", "")
	if response.Code != http.StatusOK {
		t.Fatalf("workspace publication delete = %d %s", response.Code, response.Body.String())
	}
	var deleted store.Post
	if err := json.Unmarshal(response.Body.Bytes(), &deleted); err != nil {
		t.Fatal(err)
	}
	if deleted.MAXMessageID != "" || deleted.MAXMessageURL != "" {
		t.Fatalf("workspace publication metadata survived delete: %#v", deleted)
	}
}

func newWorkspaceAPIFixture(t *testing.T) workspaceAPIFixture {
	t.Helper()
	ctx := context.Background()
	storage, err := store.Open(ctx, filepath.Join(t.TempDir(), "workspace-api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	users := []store.User{
		{ID: "ws-owner", Email: "owner@example.test", DisplayName: "Owner"},
		{ID: "ws-editor", Email: "editor@example.test", DisplayName: "Editor"},
		{ID: "ws-approver", Email: "approver@example.test", DisplayName: "Approver"},
		{ID: "ws-viewer", Email: "viewer@example.test", DisplayName: "Viewer"},
		{ID: "ws-outsider", Email: "outsider@example.test", DisplayName: "Outsider"},
	}
	for _, user := range users {
		if err := storage.UpsertUser(ctx, user); err != nil {
			t.Fatal(err)
		}
	}
	workspace, err := storage.CreateWorkspace(ctx, "ws-owner", store.Workspace{Name: "Agency"})
	if err != nil {
		t.Fatal(err)
	}
	for userID, role := range map[string]string{
		"ws-editor": store.WorkspaceRoleEditor, "ws-approver": store.WorkspaceRoleApprover, "ws-viewer": store.WorkspaceRoleViewer,
	} {
		if _, err := storage.AddWorkspaceMember(ctx, "ws-owner", store.WorkspaceMember{
			WorkspaceID: workspace.ID, UserID: userID, Role: role,
		}); err != nil {
			t.Fatal(err)
		}
	}
	channel, err := storage.CreateChannelForWorkspace(ctx, "ws-owner", workspace.ID, store.Channel{
		VerifiedMAXOwnerID: "max-owner", MAXChatID: "-990001", Title: "Agency channel", IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	post, err := storage.CreatePostForWorkspace(ctx, "ws-owner", workspace.ID, store.Post{
		Title: "Campaign", Content: "Ready", Format: store.FormatMarkdown,
		Status: store.PostStatusDraft, ChannelID: &channel.ID, Notify: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	mediaStore, err := media.New(t.TempDir(), "http://localhost:8080")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	application := app.New(storage, mediaStore, nil, nil, nil, logger)
	return workspaceAPIFixture{storage: storage, app: application, workspace: workspace, channel: channel, post: post, logger: logger}
}

func (f workspaceAPIFixture) handler(t *testing.T, userID string) http.Handler {
	t.Helper()
	return withTestSession(t, f.storage,
		New(f.app, f.logger, "http://localhost:4321", "webhook-secret", AuthOptions{YandexClient: &fakeYandexOAuth{}}).Handler(), userID)
}
