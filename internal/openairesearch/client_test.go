package openairesearch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func TestGenerateUsesWebSearchCitationsThenStructuredOutput(t *testing.T) {
	t.Parallel()
	researchText := "Рынок вырос в 2026 году citeturn0search0."
	markerStart := utf8.RuneCountInString("Рынок вырос в 2026 году ")
	markerEnd := utf8.RuneCountInString(strings.TrimSuffix(researchText, "."))

	var mu sync.Mutex
	payloads := make([]map[string]any, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer research-key" {
			t.Errorf("Authorization = %q", got)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		payloads = append(payloads, payload)
		call := len(payloads)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-request-id", "req-research")
		if call == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "resp-research", "status": "completed",
				"output": []any{
					map[string]any{"type": "web_search_call", "status": "completed"},
					map[string]any{"type": "message", "status": "completed", "content": []any{
						map[string]any{
							"type": "output_text", "text": researchText,
							"annotations": []any{map[string]any{
								"type": "url_citation", "start_index": markerStart, "end_index": markerEnd,
								"title": "Отчёт [2026]", "url": "https://example.com/report?q=ai#section",
							}},
						},
					}},
				},
			})
			return
		}
		draftJSON, _ := json.Marshal(Draft{
			Title: "ИИ для бизнеса", Content: "**Проверенный факт**", Format: "markdown", ImagePrompt: "Editorial illustration",
		})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "resp-draft", "status": "completed",
			"output": []any{map[string]any{"type": "message", "status": "completed", "content": []any{
				map[string]any{"type": "output_text", "text": string(draftJSON)},
			}}},
		})
	}))
	defer server.Close()

	client, err := New(server.URL, "research-key", "gpt-5.4-mini", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Generate(context.Background(), Request{
		Topic: "  ИИ для малого бизнеса  ", Angle: "Практика", Audience: "Предприниматели",
		Tone: "Деловой", Format: "markdown", IncludeSources: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Topic != "ИИ для малого бизнеса" || result.Draft.Format != "markdown" {
		t.Fatalf("unexpected result: %#v", result)
	}
	if strings.Contains(result.Report, "cite") || !strings.Contains(result.Report, `[Отчёт \[2026\]](<https://example.com/report?q=ai>)`) {
		t.Fatalf("report does not contain a visible safe citation: %q", result.Report)
	}
	if !reflect.DeepEqual(result.Sources, []Source{{Title: "Отчёт [2026]", URL: "https://example.com/report?q=ai"}}) {
		t.Fatalf("sources = %#v", result.Sources)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(payloads) != 2 {
		t.Fatalf("OpenAI calls = %d, want 2", len(payloads))
	}
	first, second := payloads[0], payloads[1]
	if first["model"] != "gpt-5.4-mini" || first["tool_choice"] != "required" || first["store"] != false {
		t.Fatalf("unexpected research payload: %#v", first)
	}
	tools, ok := first["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("research tools = %#v", first["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if tool["type"] != "web_search" || tool["search_context_size"] != "high" {
		t.Fatalf("web search tool = %#v", tool)
	}
	if !reflect.DeepEqual(first["include"], []any{"web_search_call.action.sources"}) {
		t.Fatalf("include = %#v", first["include"])
	}
	if _, exists := first["text"]; exists {
		t.Fatalf("research call unexpectedly has structured text format: %#v", first["text"])
	}
	if _, exists := second["tools"]; exists {
		t.Fatalf("draft call unexpectedly has tools: %#v", second["tools"])
	}
	text, _ := second["text"].(map[string]any)
	format, _ := text["format"].(map[string]any)
	if format["type"] != "json_schema" || format["name"] != "max_post_draft" || format["strict"] != true {
		t.Fatalf("structured output format = %#v", format)
	}
	schema, _ := format["schema"].(map[string]any)
	if schema["type"] != "object" || schema["additionalProperties"] != false ||
		!reflect.DeepEqual(schema["required"], []any{"title", "content", "format", "image_prompt"}) {
		t.Fatalf("structured output schema = %#v", schema)
	}
	properties, _ := schema["properties"].(map[string]any)
	formatProperty, _ := properties["format"].(map[string]any)
	if !reflect.DeepEqual(formatProperty["enum"], []any{"markdown"}) {
		t.Fatalf("format enum = %#v", formatProperty["enum"])
	}
	inputJSON, _ := json.Marshal(second["input"])
	if !strings.Contains(string(inputJSON), "include_sources") || !strings.Contains(string(inputJSON), "example.com/report") {
		t.Fatalf("draft input is missing research context: %s", inputJSON)
	}
}

func TestDraftPayloadRequiresMAXSupportedMarkdownHeading(t *testing.T) {
	t.Parallel()
	payload := draftPayload("gpt-5.4-mini", Request{
		Topic: "Тема поста", Tone: "Деловой", Format: "markdown",
	}, "Проверенный отчёт", nil)
	if len(payload.Input) == 0 {
		t.Fatal("draft payload has no system instruction")
	}
	instruction := payload.Input[0].Content
	for _, required := range []string{
		"каждый заголовок начинай ровно с `# `",
		"никогда не используй уровни `##`–`######`",
		"MAX их не поддерживает",
		"запрещены списки, таблицы, autolinks, горизонтальные линии, fenced code blocks, HTML и встроенные изображения",
		"Для html разрешены только <i>, <em>, <b>, <strong>",
	} {
		if !strings.Contains(instruction, required) {
			t.Fatalf("draft instruction is missing %q: %s", required, instruction)
		}
	}
	if strings.Contains(instruction, "ссылки, списки") {
		t.Fatalf("draft instruction still promises unsupported MAX lists: %s", instruction)
	}
}

func TestDecodeDraftRejectsUnsupportedMAXMarkup(t *testing.T) {
	t.Parallel()
	for _, content := range []string{"## Заголовок", "- пункт", "<b>HTML в markdown</b>", "[сайт](http://example.com)"} {
		value, _ := json.Marshal(Draft{
			Title: "Заголовок", Content: content, Format: "markdown", ImagePrompt: "Иллюстрация",
		})
		if _, err := decodeDraft(string(value), "markdown"); err == nil {
			t.Fatalf("decodeDraft() accepted unsupported content %q", content)
		}
	}
}

func TestResearchAPIKeyNeverFollowsRedirect(t *testing.T) {
	t.Parallel()
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetCalls.Add(1)
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("redirect target received Authorization %q", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL+"/stolen")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	client, err := New(origin.URL, "shared-openai-key", "gpt-5.4-mini", origin.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Generate(context.Background(), Request{
		Topic: "Безопасность редакционных процессов", Tone: "деловой", Format: "markdown",
	})
	if err == nil {
		t.Fatal("Generate followed or accepted redirect")
	}
	if targetCalls.Load() != 0 {
		t.Fatalf("redirect target calls = %d, want 0", targetCalls.Load())
	}
}

func TestValidateRequest(t *testing.T) {
	t.Parallel()
	valid := Request{Topic: "Тема поста", Tone: "Деловой", Format: "markdown"}
	tests := []struct {
		name    string
		mutate  func(*Request)
		message string
	}{
		{name: "empty topic", mutate: func(r *Request) { r.Topic = " " }, message: "topic is required"},
		{name: "short topic", mutate: func(r *Request) { r.Topic = "ИИ" }, message: "at least 5"},
		{name: "long topic unicode", mutate: func(r *Request) { r.Topic = strings.Repeat("я", 501) }, message: "500"},
		{name: "long angle", mutate: func(r *Request) { r.Angle = strings.Repeat("я", 501) }, message: "angle"},
		{name: "long audience", mutate: func(r *Request) { r.Audience = strings.Repeat("я", 501) }, message: "audience"},
		{name: "empty tone", mutate: func(r *Request) { r.Tone = "" }, message: "tone is required"},
		{name: "long tone", mutate: func(r *Request) { r.Tone = strings.Repeat("я", 101) }, message: "tone"},
		{name: "invalid format", mutate: func(r *Request) { r.Format = "text" }, message: "markdown or html"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request := valid
			test.mutate(&request)
			err := ValidateRequest(request)
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("ValidateRequest() error = %v, want %q", err, test.message)
			}
		})
	}
}

func TestDecodeDraftEnforcesUnicodeMAXLimitWithoutTruncatingMarkup(t *testing.T) {
	t.Parallel()
	makeDraft := func(content string) string {
		value, _ := json.Marshal(Draft{Title: "Заголовок", Content: content, Format: "html", ImagePrompt: "Иллюстрация"})
		return string(value)
	}
	content := strings.Repeat("я", 3992) + "<b>x</b>"
	draft, err := decodeDraft(makeDraft(content), "html")
	if err != nil {
		t.Fatal(err)
	}
	if draft.Content != content || utf8.RuneCountInString(draft.Content) != 4000 {
		t.Fatalf("valid Unicode content changed: %d runes", utf8.RuneCountInString(draft.Content))
	}
	tooLong := content + "я"
	_, err = decodeDraft(makeDraft(tooLong), "html")
	if err == nil || !strings.Contains(err.Error(), "exceeds 4000") {
		t.Fatalf("oversized content error = %v", err)
	}
}

func TestSafeSourceRejectsUnsafeSchemesAndHosts(t *testing.T) {
	t.Parallel()
	for _, rawURL := range []string{
		"javascript:alert(1)", "http://example.com/source", "https://localhost/source",
		"https://127.0.0.1/source", "https://10.0.0.1/source", "https://169.254.1.1/source",
		"https://[::1]/source", "https://example.local/source", "//example.com/source",
	} {
		if source, ok := safeSource("Unsafe", rawURL); ok {
			t.Errorf("safeSource(%q) = %#v, true", rawURL, source)
		}
	}
	if source, ok := safeSource("", "https://example.com/source#fragment"); !ok || source.Title != "example.com" || source.URL != "https://example.com/source" {
		t.Fatalf("safe HTTPS source = %#v, %v", source, ok)
	}
}

func TestGenerateReturnsUpstreamIncompleteAndRefusalErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		statusCode int
		body       string
		wantCode   string
		wantStatus int
	}{
		{name: "upstream", statusCode: http.StatusTooManyRequests, body: `{"error":{"message":"slow down","code":"rate_limit"}}`, wantCode: "rate_limit", wantStatus: http.StatusTooManyRequests},
		{name: "incomplete", statusCode: http.StatusOK, body: `{"id":"resp","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[]}`, wantCode: "response_incomplete"},
		{name: "refusal", statusCode: http.StatusOK, body: `{"id":"resp","status":"completed","output":[{"type":"message","content":[{"type":"refusal","refusal":"cannot help"}]}]}`, wantCode: "response_refused"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("x-request-id", "req-error")
				w.WriteHeader(test.statusCode)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			client, err := New(server.URL, "key", "gpt-5.4-mini", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Generate(context.Background(), Request{Topic: "Тема поста", Tone: "Деловой", Format: "markdown"})
			var apiErr *Error
			if !errors.As(err, &apiErr) || apiErr.Code != test.wantCode || apiErr.StatusCode != test.wantStatus || apiErr.RequestID != "req-error" {
				t.Fatalf("Generate() error = %#v", err)
			}
		})
	}
}

func TestGenerateHonorsContextDeadline(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
	}))
	defer server.Close()
	client, err := New(server.URL, "key", "gpt-5.4-mini", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = client.Generate(ctx, Request{Topic: "Тема поста", Tone: "Деловой", Format: "markdown"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Generate() error = %v, want deadline exceeded", err)
	}
}
