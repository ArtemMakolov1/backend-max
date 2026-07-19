package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/openairesearch"
	"maxpilot/backend/internal/store"
)

func suggestBrandKitFixture(t *testing.T) (workspaceAPIFixture, []store.Post) {
	t.Helper()
	fixture := newWorkspaceAPIFixture(t)
	published, err := fixture.storage.CreatePostForWorkspace(t.Context(), "ws-owner", fixture.workspace.ID, store.Post{
		Title: "Запуск", Content: "Запустили новую интеграцию: подключайте канал за минуту.",
		Format: store.FormatMarkdown, Status: store.PostStatusPublished,
	})
	if err != nil {
		t.Fatal(err)
	}
	draft, err := fixture.storage.CreatePostForWorkspace(t.Context(), "ws-owner", fixture.workspace.ID, store.Post{
		Title: "Планы", Content: "Подписывайтесь, чтобы не пропустить следующий разбор.",
		Format: store.FormatMarkdown, Status: store.PostStatusDraft,
	})
	if err != nil {
		t.Fatal(err)
	}
	return fixture, []store.Post{published, draft}
}

func TestSuggestWorkspaceBrandKitReturnsSuggestionWithoutSaving(t *testing.T) {
	fixture, _ := suggestBrandKitFixture(t)
	fake := &fakeResearchClient{brandKitResult: openairesearch.SuggestBrandKitResult{
		Tone: "Дружелюбный и практичный", Audience: "Небольшие команды",
		CTA: "Подписывайтесь на канал", VisualStyle: "",
		ExamplePosts: []string{"Подписывайтесь, чтобы не пропустить следующий разбор."},
	}}
	application := app.New(fixture.storage, fixture.app.Media(), nil, nil, fake, fixture.logger)
	server := New(application, fixture.logger, "http://localhost:4321", "webhook-secret",
		AuthOptions{YandexClient: &fakeYandexOAuth{}})
	handler := withTestSession(t, fixture.storage, server.Handler(), "ws-editor")

	response := performJSONRequest(handler, http.MethodPost,
		"/api/v1/workspaces/"+fixture.workspace.ID+"/brand-kit/suggest", "")
	if response.Code != http.StatusOK {
		t.Fatalf("suggest brand kit = %d %s", response.Code, response.Body.String())
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if got := sortedJSONKeys(payload); !reflect.DeepEqual(got, []string{
		"audience", "cta", "example_posts", "tone", "visual_style",
	}) {
		t.Fatalf("response keys = %#v", got)
	}
	var tone string
	if err := json.Unmarshal(payload["tone"], &tone); err != nil {
		t.Fatal(err)
	}
	if tone != "Дружелюбный и практичный" {
		t.Fatalf("tone = %q", tone)
	}

	fake.mu.Lock()
	if len(fake.brandKitRequests) != 1 {
		fake.mu.Unlock()
		t.Fatalf("brand kit requests = %#v", fake.brandKitRequests)
	}
	sent := fake.brandKitRequests[0]
	fake.mu.Unlock()
	if len(sent.Posts) != 3 || len(sent.Images) != 0 {
		t.Fatalf("suggestion material = %#v", sent)
	}
	// The single published post must lead the sample; the drafts follow in the
	// store's newest-first order.
	if sent.Posts[0].Text != "Запустили новую интеграцию: подключайте канал за минуту." {
		t.Fatalf("published post is not first: %#v", sent.Posts)
	}

	// The suggestion must never be persisted: the tenant saves it explicitly.
	kit, err := fixture.storage.GetWorkspaceBrandKit(t.Context(), "ws-owner", fixture.workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if kit.Version != 1 || kit.Tone != "" || kit.Audience != "" || kit.CTA != "" || kit.VisualStyle != "" {
		t.Fatalf("brand kit was modified by the suggestion: %#v", kit)
	}
}

func TestSuggestWorkspaceBrandKitRequiresEnoughPostsWithText(t *testing.T) {
	// The base fixture only has one post with text.
	fixture := newWorkspaceAPIFixture(t)
	fake := &fakeResearchClient{}
	application := app.New(fixture.storage, fixture.app.Media(), nil, nil, fake, fixture.logger)
	server := New(application, fixture.logger, "http://localhost:4321", "webhook-secret",
		AuthOptions{YandexClient: &fakeYandexOAuth{}})
	handler := withTestSession(t, fixture.storage, server.Handler(), "ws-editor")

	response := performJSONRequest(handler, http.MethodPost,
		"/api/v1/workspaces/"+fixture.workspace.ID+"/brand-kit/suggest", "")
	assertProblemCode(t, response, http.StatusConflict, "not_enough_posts")
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.brandKitRequests) != 0 {
		t.Fatalf("request without enough posts reached suggester: %#v", fake.brandKitRequests)
	}
}

func TestSuggestWorkspaceBrandKitRequiresPostsWriteCapability(t *testing.T) {
	fixture, _ := suggestBrandKitFixture(t)
	fake := &fakeResearchClient{}
	application := app.New(fixture.storage, fixture.app.Media(), nil, nil, fake, fixture.logger)
	server := New(application, fixture.logger, "http://localhost:4321", "webhook-secret",
		AuthOptions{YandexClient: &fakeYandexOAuth{}})
	for _, userID := range []string{"ws-viewer", "ws-approver"} {
		handler := withTestSession(t, fixture.storage, server.Handler(), userID)
		response := performJSONRequest(handler, http.MethodPost,
			"/api/v1/workspaces/"+fixture.workspace.ID+"/brand-kit/suggest", "")
		assertProblemCode(t, response, http.StatusForbidden, "workspace_forbidden")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.brandKitRequests) != 0 {
		t.Fatalf("forbidden request reached suggester: %#v", fake.brandKitRequests)
	}
}

func TestSuggestWorkspaceBrandKitReportsUnavailableConfiguration(t *testing.T) {
	fixture, _ := suggestBrandKitFixture(t)
	handler := fixture.handler(t, "ws-editor")
	response := performJSONRequest(handler, http.MethodPost,
		"/api/v1/workspaces/"+fixture.workspace.ID+"/brand-kit/suggest", "")
	assertProblemCode(t, response, http.StatusServiceUnavailable, "openai_research_not_configured")
}
