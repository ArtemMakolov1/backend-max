package openairesearch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"maxpilot/backend/internal/contentsearch"
)

func TestQwenDiscoveryRepairsInvalidJSONUsingOnlyOriginalEvidenceAndChannelStyle(t *testing.T) {
	const sourceURL = "https://example.com/source"
	for _, malformed := range []string{"prose fence", "wrapper object", "unknown field", "wrong type", "invalid syntax"} {
		t.Run(malformed, func(t *testing.T) {
			valid := deepSeekDiscoveryJSON(t, "Новый проверенный факт. Расскажите мнение.", "markdown", sourceURL)
			invalid := ""
			switch malformed {
			case "prose fence":
				invalid = "```json\n" + valid + "\n```\nprivate-invalid-marker"
			case "wrapper object":
				invalid = `{"result":` + valid + `,"private-invalid-marker":true}`
			case "unknown field", "wrong type":
				card := discoveryFixtureCard(sourceURL)
				if malformed == "unknown field" {
					card["private-invalid-marker"] = "private-invalid-marker"
				} else {
					card["draft"] = "private-invalid-marker"
				}
				encoded, _ := json.Marshal(map[string]any{"cards": []any{card}})
				invalid = string(encoded)
			case "invalid syntax":
				invalid = strings.TrimSuffix(valid, "}") + "\nprivate-invalid-marker"
			}
			var requests []qwenChatPayload
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests = append(requests, assertQwenRequest(t, r, true))
				if len(requests) == 1 {
					writeQwenText(t, w, invalid)
					return
				}
				if len(requests) > 2 {
					t.Error("invalid JSON exceeded the one-repair budget")
				}
				writeQwenText(t, w, valid)
			}))
			defer server.Close()
			searcher := &fakeContentSearcher{result: contentsearch.Result{Sources: []contentsearch.Source{{Title: "Настоящий источник", URL: sourceURL, Content: "Новый проверенный факт."}}}}
			// The app derives this topic from the selected owned channel when the
			// HTTP caller leaves its topic blank.
			request := DiscoverContentRequest{Topic: "Канал изобретений", ChannelTitle: "Канал изобретений", ChannelDescription: "Новые разработки и научные события", Tone: "Живой и простой", CTA: "Расскажите мнение", Format: "markdown",
				ChannelPosts: []ChannelPostStyle{{Text: "Образец выбранного канала.", CharacterCount: 500}}, ExamplePosts: []string{"Короткая история об изобретении."}, MediaContext: "Спокойные иллюстрации и короткие подписи."}
			result, err := mustQwenClient(t, server).WithContentSearch(searcher).DiscoverContent(t.Context(), request)
			if err != nil || len(result.Cards) != 1 || searcher.calls != 1 || len(requests) != 2 || !strings.HasSuffix(result.Cards[0].Draft.Content, "[Источник]("+sourceURL+")") {
				t.Fatalf("JSON repair lost card/authority or repeated search: calls=%d search=%d err=%v", len(requests), searcher.calls, err)
			}
			secondSystem := requests[1].Messages[0].Content.(string)
			if !strings.Contains(secondSystem, discoveryJSONRetryInstruction) {
				t.Fatal("fresh generation lacks the fixed JSON correction")
			}
			requests[1].Messages[0].Content = strings.Replace(secondSystem, discoveryJSONRetryInstruction, "", 1)
			if !reflect.DeepEqual(requests[0], requests[1]) {
				t.Fatal("repair changed original evidence, channel style, format or token policy")
			}
			for _, message := range requests[1].Messages {
				if strings.Contains(message.Content.(string), "private-invalid-marker") {
					t.Fatal("rejected model output became repair context")
				}
			}
		})
	}
}

func TestQwenDiscoveryJSONAndLinksShareOneRepairAndKeepFinalValidators(t *testing.T) {
	const sourceURL = "https://example.com/source"
	invalidJSON := "private-invalid-marker " + deepSeekDiscoveryJSON(t, "Проверенный факт.", "markdown", sourceURL)
	invalidLinks := deepSeekDiscoveryJSON(t, "private-invalid-marker https://example.com/other", "markdown", sourceURL)
	for _, test := range []struct {
		name, first, second, reason string
	}{
		{"JSON then JSON", invalidJSON, invalidJSON, "discovery_json_invalid"},
		{"JSON then links", invalidJSON, invalidLinks, "discovery_external_links"},
		{"links then JSON", invalidLinks, invalidJSON, "discovery_json_invalid"},
		{"JSON then unverified source", invalidJSON, deepSeekDiscoveryJSON(t, "Проверенный факт.", "markdown", "https://example.com/unverified"), "discovery_source_unverified"},
		{"JSON then invalid MAX markup", invalidJSON, deepSeekDiscoveryJSON(t, "- Пункт", "markdown", sourceURL), "discovery_format_unsupported"},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				if calls == 1 {
					writeQwenText(t, w, test.first)
					return
				}
				writeQwenText(t, w, test.second)
			}))
			defer server.Close()
			searcher := &fakeContentSearcher{result: contentsearch.Result{Sources: []contentsearch.Source{{Title: "Источник", URL: sourceURL, Content: "Проверенный факт."}}}}
			_, err := mustQwenClient(t, server).WithContentSearch(searcher).DiscoverContent(t.Context(), DiscoverContentRequest{Topic: "Тема"})
			assertDiscoveryFailureDiagnostic(t, err, "discovery_synthesis_retry", test.reason)
			if calls != 2 || searcher.calls != 1 {
				t.Fatalf("failure started a third synthesis or another search: model=%d search=%d", calls, searcher.calls)
			}
		})
	}
}

func TestQwenDiscoveryDoesNotRepairProviderFailuresOrUnprovenShapeErrors(t *testing.T) {
	for _, test := range []struct {
		name, body, code string
		status           int
	}{
		{"authorization", "private-provider-marker", "provider_http_error", http.StatusUnauthorized},
		{"invalid envelope", "private-provider-marker", "invalid_provider_response", http.StatusOK},
		{"refusal", `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","refusal":"private-provider-marker","content":"{}"}}]}`, "response_refused", http.StatusOK},
		{"token limit", `{"choices":[{"finish_reason":"length","message":{"role":"assistant","content":"private-provider-marker"}}]}`, "response_incomplete", http.StatusOK},
		{"null cards", `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"{\"cards\":null}"}}]}`, "invalid_structured_output", http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			searcher := &fakeContentSearcher{result: contentsearch.Result{Sources: []contentsearch.Source{{Title: "Источник", URL: "https://example.com/source", Content: "Проверенный факт."}}}}
			_, err := mustQwenClient(t, server).WithContentSearch(searcher).DiscoverContent(t.Context(), DiscoverContentRequest{Topic: "Тема"})
			var failure *Error
			if !errors.As(err, &failure) || failure.Code != test.code || calls != 1 || searcher.calls != 1 || strings.Contains(err.Error(), "private-provider-marker") {
				t.Fatalf("unproven/non-local error retried or leaked: calls=%d search=%d err=%v", calls, searcher.calls, err)
			}
		})
	}
}

func TestDiscoveryJSONRepairLeavesOtherProvidersAndTransportPolicyUnchanged(t *testing.T) {
	invalid := errors.New("invalid content discovery JSON")
	for _, provider := range []string{"openai", "deepseek"} {
		if discoveryRetryInstruction(provider, invalid) != "" {
			t.Fatal("Qwen JSON repair changed another provider's retry policy")
		}
	}
	calls := 0
	client, err := NewQwen("https://provider.example.com", "synthetic-key", "qwen3.6-plus", &http.Client{Transport: diagnosticRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("private-transport-marker https://private.example.com/token")
	})})
	if err != nil {
		t.Fatal(err)
	}
	searcher := &fakeContentSearcher{result: contentsearch.Result{Sources: []contentsearch.Source{{Title: "Источник", URL: "https://example.com/source", Content: "Проверенный факт."}}}}
	_, err = client.WithContentSearch(searcher).DiscoverContent(t.Context(), DiscoverContentRequest{Topic: "Тема"})
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "provider_transport_error" || calls != 1 || searcher.calls != 1 || strings.Contains(err.Error(), "private-transport-marker") {
		t.Fatalf("transport retried or leaked: calls=%d search=%d err=%v", calls, searcher.calls, err)
	}
}

type cancelDiscoveryResponseBody struct {
	io.Reader
	cancel context.CancelFunc
}

func (body cancelDiscoveryResponseBody) Close() error { body.cancel(); return nil }

func TestQwenDiscoveryJSONRepairPreservesDeadlineAndStopsOnCancellation(t *testing.T) {
	for _, cancelBeforeRetry := range []bool{false, true} {
		t.Run(map[bool]string{false: "shared deadline", true: "canceled before retry"}[cancelBeforeRetry], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			deadline, _ := ctx.Deadline()
			calls := 0
			client, err := NewQwen("https://provider.example.com", "synthetic-key", "qwen3.6-plus", &http.Client{Transport: diagnosticRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				got, ok := r.Context().Deadline()
				if !ok || !got.Equal(deadline) {
					t.Fatal("repair reset the caller deadline")
				}
				text := `{"cards":[]}`
				if calls == 1 {
					text = "private-invalid-marker"
				}
				raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": text}}}})
				body := io.NopCloser(strings.NewReader(string(raw)))
				if cancelBeforeRetry {
					body = cancelDiscoveryResponseBody{Reader: strings.NewReader(string(raw)), cancel: cancel}
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}, nil
			})})
			if err != nil {
				t.Fatal(err)
			}
			searcher := &fakeContentSearcher{result: contentsearch.Result{Sources: []contentsearch.Source{{Title: "Источник", URL: "https://example.com/source", Content: "Проверенный факт."}}}}
			_, err = client.WithContentSearch(searcher).DiscoverContent(ctx, DiscoverContentRequest{Topic: "Тема"})
			if cancelBeforeRetry {
				if !errors.Is(err, context.Canceled) || calls != 1 {
					t.Fatalf("canceled repair made another call: calls=%d err=%v", calls, err)
				}
			} else if err != nil || calls != 2 {
				t.Fatalf("bounded fresh repair failed: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestQwenDiscoveryRepairedCardsStillUseFreshIndexedPublicationCheck(t *testing.T) {
	const sourceURL = "https://example.com/source"
	calls, lookups := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			writeQwenText(t, w, `{"cards":"private-invalid-marker"}`)
			return
		}
		writeQwenText(t, w, deepSeekDiscoveryJSON(t, "Проверенный факт.", "markdown", sourceURL))
	}))
	defer server.Close()
	searcher := &fakeContentSearcher{result: contentsearch.Result{Sources: []contentsearch.Source{{Title: "Источник", URL: sourceURL, Content: "Проверенный факт."}}}}
	lookup := func(_ context.Context, candidates []DiscoveryPublishedCandidate) ([][]DiscoveryPublishedReference, error) {
		lookups++
		if lookups == 1 {
			return make([][]DiscoveryPublishedReference, len(candidates)), nil
		}
		return [][]DiscoveryPublishedReference{{{SourceURLs: []string{sourceURL}}}}, nil
	}
	result, err := mustQwenClient(t, server).WithContentSearch(searcher).DiscoverContent(t.Context(), DiscoverContentRequest{Topic: "Тема", PublishedReferenceLookup: lookup})
	if err != nil || calls != 2 || searcher.calls != 1 || lookups != 2 || len(result.Cards) != 0 || result.EmptyReason != "already_published" {
		t.Fatalf("repaired concurrently published card survived: calls=%d lookups=%d err=%v", calls, lookups, err)
	}
}
