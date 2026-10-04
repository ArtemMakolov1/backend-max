package openairesearch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Check the full two-step workflow: a migration that fixes only the first
// call can silently enable Luna's medium effort during draft generation.
func TestLunaProducesCitedResearchAndStructuredDraftAtNoneEffort(t *testing.T) {
	t.Parallel()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		reasoning, _ := payload["reasoning"].(map[string]any)
		if payload["model"] != "gpt-6-luna" || reasoning["effort"] != "none" || payload["store"] != false {
			t.Errorf("call %d lost explicit model/effort/storage policy", calls)
		}
		if payload["max_output_tokens"] != float64(6000) {
			t.Error("existing output bound changed")
		}
		if calls == 1 {
			if payload["tool_choice"] != "required" || payload["max_tool_calls"] != float64(3) {
				t.Error("research lost its bounded required search")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "output": []any{
				map[string]any{"type": "message", "content": []any{
					map[string]any{"type": "output_text", "text": "Исследование темы", "annotations": []any{
						map[string]any{"type": "url_citation", "title": "Источник", "url": "https://example.com/research"},
					}},
				}},
			}})
			return
		}
		if _, ok := payload["tools"]; ok {
			t.Error("draft unexpectedly invokes more searches")
		}
		text, _ := payload["text"].(map[string]any)
		format, _ := text["format"].(map[string]any)
		if format["strict"] != true || format["type"] != "json_schema" {
			t.Error("draft lost strict structured output")
		}
		draft, _ := json.Marshal(Draft{Title: "Готовый пост", Content: "Текст поста", Format: "markdown", ImagePrompt: "An office cat illustration"})
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "output_text": string(draft)})
	}))
	defer server.Close()
	client, err := New(server.URL, "synthetic-key", "gpt-6-luna", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Generate(context.Background(), Request{Topic: "Смешные коты в офисе", Tone: "Дружелюбный", Format: "markdown", IncludeSources: true})
	if err != nil || len(result.Sources) != 1 || result.Draft.Title != "Готовый пост" || calls != 2 {
		t.Fatalf("two-step Luna result = %+v, calls=%d, error=%v", result, calls, err)
	}
}
