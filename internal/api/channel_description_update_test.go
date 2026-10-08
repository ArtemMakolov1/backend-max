package api

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/maxclient"
	"maxpilot/backend/internal/store"
)

func TestWorkspaceChannelDescriptionUpdateValidatesRightsAndCanClear(t *testing.T) {
	fixture := newWorkspaceAPIFixture(t)
	fake := &claimWebhookMAX{
		chat: maxclient.ChatInfo{ChatID: fixture.channel.MAXChatID, OwnerID: "max-owner", Type: "channel", Status: "active", Title: fixture.channel.Title, Description: "Old description"},
		membership: maxclient.Membership{IsAdmin: true, Permissions: []maxclient.Permission{
			maxclient.PermissionReadAllMessages, maxclient.PermissionWrite, maxclient.PermissionChangeChatInfo,
		}},
	}
	application := app.New(fixture.storage, fixture.app.Media(), fake, nil, nil, fixture.logger)
	server := New(application, fixture.logger, "http://localhost:4321", "webhook-secret")
	owner := withTestSession(t, fixture.storage, server.Handler(), "ws-owner")
	viewer := withTestSession(t, fixture.storage, server.Handler(), "ws-viewer")
	path := "/api/v1/workspaces/" + fixture.workspace.ID + "/channels/" + postID(fixture.channel.ID) + "/max-info"
	request := func(handler http.Handler, description string) *httptest.ResponseRecorder {
		t.Helper()
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		if err := writer.WriteField("description", description); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, path, &body)
		req.Header.Set("Content-Type", writer.FormDataContentType())
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	assertProblemCode(t, request(viewer, "Forbidden"), http.StatusForbidden, "workspace_forbidden")
	assertProblemCode(t, request(owner, strings.Repeat("я", 16001)), http.StatusBadRequest, "validation_error")
	if len(fake.editChatPatches) != 0 {
		t.Fatal("forbidden/invalid description reached MAX")
	}
	for _, description := range []string{"Описание канала 🦊\nНовая строка", ""} {
		response := request(owner, description)
		if response.Code != http.StatusOK {
			t.Fatalf("description-only update = %d %s", response.Code, response.Body.String())
		}
		var channel store.Channel
		if err := json.Unmarshal(response.Body.Bytes(), &channel); err != nil {
			t.Fatal(err)
		}
		patch := fake.editChatPatches[len(fake.editChatPatches)-1]
		if channel.Description != description || patch.Description == nil || *patch.Description != description || patch.Title != nil || patch.IconToken != "" {
			t.Fatalf("description patch or synced cache mismatch: channel=%#v patch=%#v", channel, patch)
		}
		stored, err := fixture.storage.GetChannelForWorkspace(t.Context(), "ws-owner", fixture.workspace.ID, fixture.channel.ID)
		if err != nil || stored.Description != description {
			t.Fatalf("description cache=%#v, %v", stored, err)
		}
	}
	fake.membership.Permissions = fake.membership.Permissions[:2]
	assertProblemCode(t, request(owner, "No MAX permission"), http.StatusUnprocessableEntity, "max_channel_access")
	if len(fake.editChatPatches) != 2 {
		t.Fatal("description request without change_chat_info reached MAX")
	}
}
