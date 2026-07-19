package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/openairesearch"
	"maxpilot/backend/internal/store"
)

func TestSuggestImagePromptReturnsDirectExactContract(t *testing.T) {
	t.Parallel()
	fake := &fakeResearchClient{suggestResult: openairesearch.SuggestImagePromptResult{
		Prompt: "Маяк освещает путь кораблю в тумане.",
	}}
	handler := newResearchTestHandler(t, fake, "")
	response := performJSONRequest(handler, http.MethodPost, "/api/v1/posts/suggest-image-prompt",
		`{"content":"Пост о запуске продукта","format":"markdown"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if got := sortedJSONKeys(payload); !reflect.DeepEqual(got, []string{"prompt"}) {
		t.Fatalf("response keys = %#v", got)
	}
	var prompt string
	if err := json.Unmarshal(payload["prompt"], &prompt); err != nil {
		t.Fatal(err)
	}
	if prompt != fake.suggestResult.Prompt {
		t.Fatalf("prompt = %q", prompt)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !reflect.DeepEqual(fake.suggestRequests, []openairesearch.SuggestImagePromptRequest{
		{Content: "Пост о запуске продукта", Format: "markdown"},
	}) {
		t.Fatalf("suggest requests = %#v", fake.suggestRequests)
	}
}

func TestSuggestImagePromptValidatesBeforeOpenAI(t *testing.T) {
	t.Parallel()
	fake := &fakeResearchClient{}
	handler := newResearchTestHandler(t, fake, "")
	tests := []struct {
		name string
		body string
	}{
		{name: "empty", body: `{"content":" "}`},
		{name: "invalid format", body: `{"content":"Текст","format":"text"}`},
		{name: "too long unicode", body: `{"content":"` + strings.Repeat("я", 4001) + `"}`},
		{name: "unknown field", body: `{"content":"Текст","brand_tone":"injected"}`},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			response := performJSONRequest(handler, http.MethodPost, "/api/v1/posts/suggest-image-prompt", test.body)
			wantCode := "validation_error"
			if test.name == "unknown field" {
				wantCode = "invalid_json"
			}
			assertProblemCode(t, response, http.StatusBadRequest, wantCode)
		})
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.suggestRequests) != 0 {
		t.Fatalf("invalid requests reached suggester: %#v", fake.suggestRequests)
	}
}

func TestSuggestImagePromptReportsUnavailableAndUpstreamFailure(t *testing.T) {
	t.Parallel()
	const body = `{"content":"Текст поста"}`
	t.Run("not configured", func(t *testing.T) {
		handler := newResearchTestHandler(t, nil, "")
		response := performJSONRequest(handler, http.MethodPost, "/api/v1/posts/suggest-image-prompt", body)
		assertProblemCode(t, response, http.StatusServiceUnavailable, "openai_research_not_configured")
	})
	t.Run("upstream", func(t *testing.T) {
		fake := &fakeResearchClient{suggestErr: &openairesearch.Error{
			StatusCode: http.StatusTooManyRequests, Code: "rate_limit", Message: "slow down", RequestID: "req-suggest-123",
		}}
		handler := newResearchTestHandler(t, fake, "")
		response := performJSONRequest(handler, http.MethodPost, "/api/v1/posts/suggest-image-prompt", body)
		assertProblemCode(t, response, http.StatusBadGateway, "openai_research_error")
		if strings.Contains(response.Body.String(), "rate_limit") || strings.Contains(response.Body.String(), "req-suggest-123") {
			t.Fatalf("upstream problem body = %s", response.Body.String())
		}
	})
}

func TestSuggestImagePromptRequiresYandexSession(t *testing.T) {
	t.Parallel()
	fake := &fakeResearchClient{}
	_, _, handler := newAIQuotaTestServer(t, nil, fake, testAILimitOptions(), "suggest-user")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/posts/suggest-image-prompt",
		strings.NewReader(`{"content":"Текст"}`)))
	assertProblemCode(t, response, http.StatusUnauthorized, "authentication_required")
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.suggestRequests) != 0 {
		t.Fatalf("unauthenticated request reached suggester: %#v", fake.suggestRequests)
	}
}

func TestSuggestWorkspaceImagePromptAppliesBrandKitToneAudienceAndVisualStyle(t *testing.T) {
	fixture := newWorkspaceAPIFixture(t)
	fake := &fakeResearchClient{suggestResult: openairesearch.SuggestImagePromptResult{
		Prompt: "Команда собирает мост из светящихся деталей.",
	}}
	application := app.New(fixture.storage, fixture.app.Media(), nil, nil, fake, fixture.logger)
	server := New(application, fixture.logger, "http://localhost:4321", "webhook-secret",
		AuthOptions{YandexClient: &fakeYandexOAuth{}})
	handler := withTestSession(t, fixture.storage, server.Handler(), "ws-editor")
	path := "/api/v1/workspaces/" + fixture.workspace.ID + "/posts/suggest-image-prompt"

	// A fresh workspace keeps its default empty Brand Kit: no brand context.
	response := performJSONRequest(handler, http.MethodPost, path, `{"content":"Пост о запуске","format":"markdown"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("suggest without brand kit = %d %s", response.Code, response.Body.String())
	}
	fake.mu.Lock()
	if len(fake.suggestRequests) != 1 || fake.suggestRequests[0].BrandTone != "" ||
		fake.suggestRequests[0].BrandAudience != "" || fake.suggestRequests[0].BrandVisualStyle != "" {
		fake.mu.Unlock()
		t.Fatalf("suggest requests without brand kit = %#v", fake.suggestRequests)
	}
	fake.mu.Unlock()

	kit, err := fixture.storage.GetWorkspaceBrandKit(t.Context(), "ws-owner", fixture.workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.storage.UpdateWorkspaceBrandKit(t.Context(), "ws-owner", fixture.workspace.ID,
		store.WorkspaceBrandKitUpdate{BrandProfile: store.BrandProfile{
			Audience: "Основатели стартапов", Tone: "Деловой", VisualStyle: "Светлый минимализм с одним акцентным объектом",
		}, ExpectedVersion: kit.Version}); err != nil {
		t.Fatal(err)
	}
	response = performJSONRequest(handler, http.MethodPost, path, `{"content":"Пост о запуске","format":"markdown"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("branded suggest = %d %s", response.Code, response.Body.String())
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if got := sortedJSONKeys(payload); !reflect.DeepEqual(got, []string{"prompt"}) {
		t.Fatalf("workspace response keys = %#v", got)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.suggestRequests) != 2 {
		t.Fatalf("suggest requests = %#v", fake.suggestRequests)
	}
	branded := fake.suggestRequests[1]
	if branded.Content != "Пост о запуске" || branded.Format != "markdown" ||
		branded.BrandTone != "Деловой" || branded.BrandAudience != "Основатели стартапов" ||
		branded.BrandVisualStyle != "Светлый минимализм с одним акцентным объектом" {
		t.Fatalf("branded suggest request = %#v", branded)
	}
}

func TestSuggestWorkspaceImagePromptRequiresAICapability(t *testing.T) {
	fixture := newWorkspaceAPIFixture(t)
	fake := &fakeResearchClient{}
	application := app.New(fixture.storage, fixture.app.Media(), nil, nil, fake, fixture.logger)
	server := New(application, fixture.logger, "http://localhost:4321", "webhook-secret",
		AuthOptions{YandexClient: &fakeYandexOAuth{}})
	viewer := withTestSession(t, fixture.storage, server.Handler(), "ws-viewer")
	response := performJSONRequest(viewer, http.MethodPost,
		"/api/v1/workspaces/"+fixture.workspace.ID+"/posts/suggest-image-prompt",
		`{"content":"Пост"}`)
	assertProblemCode(t, response, http.StatusForbidden, "workspace_forbidden")
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.suggestRequests) != 0 {
		t.Fatalf("forbidden request reached suggester: %#v", fake.suggestRequests)
	}
}
