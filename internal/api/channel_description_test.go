package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/maxclient"
	"maxpilot/backend/internal/openairesearch"
	"maxpilot/backend/internal/store"
)

func channelDescriptionTestResult() openairesearch.SuggestChannelDescriptionResult {
	return openairesearch.SuggestChannelDescriptionResult{Suggestions: []openairesearch.ChannelDescriptionSuggestion{
		{Style: "concise", Label: "Кратко", Text: "Короткое описание."},
		{Style: "expert", Label: "Экспертно", Text: "Профессиональное описание."},
		{Style: "promotional", Label: "С акцентом на пользу", Text: "Привлекательное описание."},
	}}
}

func TestWorkspaceChannelTestRefreshesMetadataForViewer(t *testing.T) {
	fixture := newWorkspaceAPIFixture(t)
	fake := &claimWebhookMAX{
		chat: maxclient.ChatInfo{
			ChatID: fixture.channel.MAXChatID, OwnerID: "max-owner", Type: "channel", Status: "active",
			Title: "Обновлённое название", Description: "Описание из MAX", ParticipantsCount: 42,
		},
		membership: maxclient.Membership{IsAdmin: true, Permissions: []maxclient.Permission{
			maxclient.PermissionReadAllMessages, maxclient.PermissionWrite,
		}},
	}
	application := app.New(fixture.storage, fixture.app.Media(), fake, nil, nil, fixture.logger)
	server := New(application, fixture.logger, "http://localhost:4321", "webhook-secret",
		AuthOptions{YandexClient: &fakeYandexOAuth{}})
	viewer := withTestSession(t, fixture.storage, server.Handler(), "ws-viewer")
	path := "/api/v1/workspaces/" + fixture.workspace.ID + "/channels/" + postID(fixture.channel.ID) + "/test"
	response := performJSONRequest(viewer, http.MethodPost, path, "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"description":"Описание из MAX"`) {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("cache = %q", response.Header().Get("Cache-Control"))
	}
}

func TestWorkspaceChannelDescriptionSuggestionUsesAICapabilityAndExactContract(t *testing.T) {
	fixture := newWorkspaceAPIFixture(t)
	fake := &fakeResearchClient{descriptionResult: channelDescriptionTestResult()}
	application := app.New(fixture.storage, fixture.app.Media(), nil, nil, fake, fixture.logger)
	server := New(application, fixture.logger, "http://localhost:4321", "webhook-secret",
		AuthOptions{YandexClient: &fakeYandexOAuth{}})
	path := "/api/v1/workspaces/" + fixture.workspace.ID + "/channels/" + postID(fixture.channel.ID) + "/description/suggest"

	viewer := withTestSession(t, fixture.storage, server.Handler(), "ws-viewer")
	response := performJSONRequest(viewer, http.MethodPost, path, `{}`)
	assertProblemCode(t, response, http.StatusForbidden, "workspace_forbidden")

	editor := withTestSession(t, fixture.storage, server.Handler(), "ws-editor")
	response = performJSONRequest(editor, http.MethodPost, path,
		`{"context":"Для небольших команд","current_description":"Черновик"}`)
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("response = %d cache=%q body=%s", response.Code, response.Header().Get("Cache-Control"), response.Body.String())
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sortedJSONKeys(payload), []string{"suggestions"}) {
		t.Fatalf("response keys = %#v", sortedJSONKeys(payload))
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.descriptionRequests) != 1 {
		t.Fatalf("requests = %#v", fake.descriptionRequests)
	}
	request := fake.descriptionRequests[0]
	if request.ChannelTitle != fixture.channel.Title || request.Context != "Для небольших команд" ||
		request.CurrentDescription != "Черновик" {
		t.Fatalf("request = %#v", request)
	}
}

func TestLegacyChannelDescriptionSuggestionIsTenantScopedAndValidatesUnicode(t *testing.T) {
	fake := &fakeResearchClient{descriptionResult: channelDescriptionTestResult()}
	_, storage, raw := newAIQuotaTestServer(t, nil, fake, testAILimitOptions(), "description-user", "description-foreign")
	workspaces, err := storage.ListWorkspaces(t.Context(), "description-user")
	if err != nil {
		t.Fatal(err)
	}
	var personal store.Workspace
	for _, access := range workspaces {
		if access.Workspace.IsPersonal {
			personal = access.Workspace
			break
		}
	}
	channel, err := storage.CreateChannel(t.Context(), store.Channel{
		UserID: "description-user", WorkspaceID: personal.ID, VerifiedMAXOwnerID: "max-owner",
		MAXChatID: "-777", Title: "Личный канал", Description: "Описание MAX", IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/channels/" + postID(channel.ID) + "/description/suggest"
	foreign := withTestSession(t, storage, raw, "description-foreign")
	response := performJSONRequest(foreign, http.MethodPost, path, `{}`)
	assertProblemCode(t, response, http.StatusNotFound, "not_found")

	owner := withTestSession(t, storage, raw, "description-user")
	response = performJSONRequest(owner, http.MethodPost, path,
		`{"context":"`+strings.Repeat("я", openairesearch.MaxSuggestChannelDescriptionContext+1)+`"}`)
	assertProblemCode(t, response, http.StatusBadRequest, "validation_error")
	response = performJSONRequest(owner, http.MethodPost, path, `{"channel_title":"Подмена"}`)
	assertProblemCode(t, response, http.StatusBadRequest, "invalid_json")
	response = performJSONRequest(owner, http.MethodPost, path, `{"context":"Практический канал"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("owner response = %d %s", response.Code, response.Body.String())
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.descriptionRequests) != 1 || fake.descriptionRequests[0].ChannelDescription != "Описание MAX" {
		t.Fatalf("requests = %#v", fake.descriptionRequests)
	}
}
