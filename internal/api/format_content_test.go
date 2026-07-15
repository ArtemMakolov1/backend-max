package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"maxpilot/backend/internal/openairesearch"
)

func TestFormatPostContentReturnsDirectExactContract(t *testing.T) {
	t.Parallel()
	fake := &fakeResearchClient{formatResult: openairesearch.FormatResult{Content: "# Заголовок\n\n++Текст++"}}
	handler := newResearchTestHandler(t, fake, "")
	response := performJSONRequest(handler, http.MethodPost, "/api/v1/posts/format-content",
		`{"content":"Заголовок Текст","format":"markdown"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if got := sortedJSONKeys(payload); !reflect.DeepEqual(got, []string{"content"}) {
		t.Fatalf("response keys = %#v", got)
	}
	var content string
	if err := json.Unmarshal(payload["content"], &content); err != nil {
		t.Fatal(err)
	}
	if content != fake.formatResult.Content {
		t.Fatalf("content = %q", content)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !reflect.DeepEqual(fake.formatRequests, []openairesearch.FormatRequest{{Content: "Заголовок Текст", Format: "markdown"}}) {
		t.Fatalf("format requests = %#v", fake.formatRequests)
	}
}

func TestFormatPostContentValidatesBeforeOpenAI(t *testing.T) {
	t.Parallel()
	fake := &fakeResearchClient{}
	handler := newResearchTestHandler(t, fake, "")
	tests := []struct {
		name string
		body string
	}{
		{name: "empty", body: `{"content":" ","format":"markdown"}`},
		{name: "invalid format", body: `{"content":"Текст","format":"text"}`},
		{name: "too long unicode", body: `{"content":"` + strings.Repeat("я", 4001) + `","format":"markdown"}`},
		{name: "unknown field", body: `{"content":"Текст","format":"markdown","api_key":"secret"}`},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			response := performJSONRequest(handler, http.MethodPost, "/api/v1/posts/format-content", test.body)
			wantCode := "validation_error"
			if test.name == "unknown field" {
				wantCode = "invalid_json"
			}
			assertProblemCode(t, response, http.StatusBadRequest, wantCode)
		})
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.formatRequests) != 0 {
		t.Fatalf("invalid requests reached formatter: %#v", fake.formatRequests)
	}
}

func TestFormatPostContentReportsUnavailableAndUpstreamFailure(t *testing.T) {
	t.Parallel()
	const body = `{"content":"Текст поста","format":"html"}`
	t.Run("not configured", func(t *testing.T) {
		handler := newResearchTestHandler(t, nil, "")
		response := performJSONRequest(handler, http.MethodPost, "/api/v1/posts/format-content", body)
		assertProblemCode(t, response, http.StatusServiceUnavailable, "openai_research_not_configured")
	})
	t.Run("upstream", func(t *testing.T) {
		fake := &fakeResearchClient{formatErr: &openairesearch.Error{
			StatusCode: http.StatusTooManyRequests, Code: "rate_limit", Message: "slow down", RequestID: "req-format-123",
		}}
		handler := newResearchTestHandler(t, fake, "")
		response := performJSONRequest(handler, http.MethodPost, "/api/v1/posts/format-content", body)
		assertProblemCode(t, response, http.StatusBadGateway, "openai_research_error")
		if strings.Contains(response.Body.String(), "rate_limit") || strings.Contains(response.Body.String(), "req-format-123") {
			t.Fatalf("upstream problem body = %s", response.Body.String())
		}
	})
}

func TestFormatPostContentRequiresYandexSession(t *testing.T) {
	t.Parallel()
	fake := &fakeResearchClient{}
	_, _, handler := newAIQuotaTestServer(t, nil, fake, testAILimitOptions(), "format-user")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/posts/format-content",
		strings.NewReader(`{"content":"Текст","format":"markdown"}`)))
	assertProblemCode(t, response, http.StatusUnauthorized, "authentication_required")
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.formatRequests) != 0 {
		t.Fatalf("unauthenticated request reached formatter: %#v", fake.formatRequests)
	}
}
