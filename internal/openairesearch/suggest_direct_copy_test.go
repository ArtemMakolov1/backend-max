package openairesearch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSuggestDirectCopyUsesFactsWithoutBudgetDatesOrToolsAndValidatesOutput(t *testing.T) {
	t.Parallel()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if _, exists := payload["tools"]; exists {
			t.Error("creative suggester received tools")
		}
		if payload["max_output_tokens"] != float64(2000) || payload["store"] != false {
			t.Error("creative token/storage limits missing")
		}
		encoded, _ := json.Marshal(payload)
		for _, field := range []string{"weekly_budget_minor", "starts_at", "ends_at"} {
			if strings.Contains(string(encoded), field) {
				t.Errorf("invented campaign context %s", field)
			}
		}
		if !strings.Contains(string(encoded), "Текущий текст") {
			t.Error("authoritative ad facts absent")
		}
		variants := []DirectCopyVariant{{"Ясный заголовок", "Короткий текст"}, {"Другой заголовок", "Ещё один текст"}, {"Третий вариант", "Другая формулировка"}}
		if calls == 2 {
			variants[0].Title = strings.Repeat("я", 23)
		}
		output, _ := json.Marshal(SuggestDirectCopyResult{Variants: variants, Rationale: []string{}, RiskWarnings: []string{}})
		if calls == 3 {
			output = append(output, []byte("{}")...)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "response-copy", "status": "completed", "output": []any{map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": string(output)}}}}})
	}))
	defer server.Close()
	client, err := New(server.URL, "key", "model", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	request := SuggestDirectCopyRequest{Brief: "Сделай текст объявления яснее", CampaignName: "Кампания", AdType: "TEXT_AD", Titles: []string{"Заголовок"}, Texts: []string{"Текущий текст"}}
	result, err := client.SuggestDirectCopy(context.Background(), request)
	if err != nil || len(result.Variants) != 3 {
		t.Fatalf("copy response %+v %v", result, err)
	}
	if _, err = client.SuggestDirectCopy(context.Background(), request); err == nil {
		t.Fatal("oversized provider word accepted")
	}
	if _, err = client.SuggestDirectCopy(context.Background(), request); err == nil {
		t.Fatal("trailing structured output accepted")
	}
	request.Brief = "x"
	if _, err = client.SuggestDirectCopy(context.Background(), request); err == nil || calls != 3 {
		t.Fatalf("invalid brief called AI: %d %v", calls, err)
	}
}
