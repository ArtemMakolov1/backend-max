package openairesearch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"maxpilot/backend/internal/contentsearch"
)

func TestDiscoveryDiagnosticsIdentifyBothSemanticPhasesWithoutResponseText(t *testing.T) {
	reference, candidates := semanticPublicationFixture(t)
	for _, postcheck := range []bool{false, true} {
		name, expectedStage, expectedCalls := "precheck", "discovery_precheck", 1
		if postcheck {
			name, expectedStage, expectedCalls = "postcheck", "discovery_postcheck", 2
		}
		t.Run(name, func(t *testing.T) {
			calls, lookups := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				if postcheck && calls == 1 {
					writeQwenText(t, w, deepSeekDiscoveryJSON(t, candidates[0].Text, "markdown", "https://example.com/source"))
					return
				}
				writeQwenText(t, w, `{"pairs":[{"pair_id":"p1","duplicate":false,"private-provider-marker":"https://private.example/token"}]}`)
			}))
			defer server.Close()
			lookup := func(_ context.Context, input []DiscoveryPublishedCandidate) ([][]DiscoveryPublishedReference, error) {
				lookups++
				if postcheck && lookups == 1 {
					return make([][]DiscoveryPublishedReference, len(input)), nil
				}
				return fixedPublishedReferenceLookup(reference)(t.Context(), input)
			}
			searcher := &fakeContentSearcher{result: contentsearch.Result{Sources: []contentsearch.Source{{Title: candidates[0].Title, Content: candidates[0].Text, URL: "https://example.com/source"}}}}
			_, err := mustQwenClient(t, server).WithContentSearch(searcher).DiscoverContent(t.Context(), DiscoverContentRequest{Topic: "Изобретения", PublishedReferenceLookup: lookup})
			assertDiscoveryFailureDiagnostic(t, err, expectedStage, "invalid_pair_decisions")
			if calls != expectedCalls || searcher.calls != 1 || lookups != expectedCalls {
				t.Fatalf("diagnostic changed phase calls: model=%d search=%d lookup=%d", calls, searcher.calls, lookups)
			}
		})
	}
}

func TestDiscoveryDiagnosticsIdentifySynthesisAndExistingRepairFailure(t *testing.T) {
	for _, test := range []struct {
		name, stage, reason string
	}{
		{name: "card title bounds", stage: "discovery_synthesis", reason: "card_title_bounds"},
		{name: "card summary bounds", stage: "discovery_synthesis", reason: "card_summary_bounds"},
		{name: "draft title bounds", stage: "discovery_synthesis", reason: "draft_title_bounds"},
		{name: "draft content bounds", stage: "discovery_synthesis", reason: "draft_content_bounds"},
		{name: "draft format", stage: "discovery_synthesis", reason: "draft_format_mismatch"},
		{name: "draft image prompt bounds", stage: "discovery_synthesis", reason: "draft_image_prompt_bounds"},
		{name: "unverified source", stage: "discovery_synthesis", reason: "discovery_source_unverified"},
		{name: "MAX markup", stage: "discovery_synthesis", reason: "discovery_format_unsupported"},
		{name: "repair exhausted", stage: "discovery_synthesis_retry", reason: "discovery_external_links"},
		{name: "repair invalid shape", stage: "discovery_synthesis_retry", reason: "discovery_shape_invalid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			card := discoveryFixtureCard("https://example.com/source")
			draft := card["draft"].(map[string]any)
			switch test.name {
			case "card title bounds":
				card["title"] = strings.Repeat("я", 121)
			case "card summary bounds":
				card["summary"] = strings.Repeat("я", 351)
			case "draft title bounds":
				draft["title"] = strings.Repeat("я", 201)
			case "draft content bounds":
				draft["content"] = strings.Repeat("я", maxDiscoveryDraftRunes+1)
			case "draft format":
				draft["format"] = "html"
			case "draft image prompt bounds":
				draft["image_prompt"] = strings.Repeat("я", 801)
			case "unverified source":
				card["source_url"] = "https://private.example.com/unverified-private-provider-marker"
			case "MAX markup":
				draft["content"] = "- private-provider-marker"
			default:
				draft["content"] = "private-provider-marker https://private.example/token"
			}
			encoded, _ := json.Marshal(map[string]any{"cards": []any{card}})
			replies := []string{string(encoded)}
			if test.stage == "discovery_synthesis_retry" {
				second := string(encoded)
				if test.name == "repair invalid shape" {
					second = `{"cards":null}`
				}
				replies = append(replies, second)
			}
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				if calls > len(replies) {
					t.Error("diagnostics introduced another model retry")
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				writeQwenText(t, w, replies[calls-1])
			}))
			defer server.Close()
			searcher := &fakeContentSearcher{result: contentsearch.Result{Sources: []contentsearch.Source{{Title: "Источник", URL: "https://example.com/source", Content: "Проверенный материал."}}}}
			_, err := mustQwenClient(t, server).WithContentSearch(searcher).DiscoverContent(t.Context(), DiscoverContentRequest{Topic: "Тема"})
			assertDiscoveryFailureDiagnostic(t, err, test.stage, test.reason)
			if calls != len(replies) || searcher.calls != 1 {
				t.Fatalf("diagnostics changed bounded calls: model=%d search=%d", calls, searcher.calls)
			}
		})
	}
}

func assertDiscoveryFailureDiagnostic(t *testing.T, err error, stage, reason string) {
	t.Helper()
	var providerErr *Error
	if !errors.As(err, &providerErr) || providerErr.Code != "invalid_structured_output" || providerErr.StatusCode != 0 || providerErr.Provider != "qwen" {
		t.Fatalf("provider result error changed: %v", err)
	}
	if got := providerErr.DiscoveryFailureDiagnostics(); got != (DiscoveryDiagnostics{Stage: stage, ValidationReason: reason}) {
		t.Fatalf("failure diagnostic=%#v, expected stage=%s reason=%s", got, stage, reason)
	}
	if providerErr.Message != "Qwen output does not meet the requested format and validation rules" {
		t.Fatal("public-safe provider error message changed")
	}
	encoded, marshalErr := json.Marshal(providerErr)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	for _, value := range []string{providerErr.Error(), string(encoded)} {
		if strings.Contains(value, "private-provider-marker") || strings.Contains(value, "private.example") || strings.Contains(value, stage) || strings.Contains(value, reason) {
			t.Fatal("private diagnostics or provider text leaked through Error/JSON")
		}
	}
}

func TestDiscoveryDiagnosticsCloneAndRejectInjectedPrivateFields(t *testing.T) {
	original := &Error{Provider: "qwen", StatusCode: 401, Code: "provider_http_error", Message: "Unauthorized", RequestID: "safe-id",
		discoveryDiagnostics: DiscoveryDiagnostics{Stage: "private-stage-marker", ValidationReason: "private-reason-marker"}}
	if original.DiscoveryFailureDiagnostics() != (DiscoveryDiagnostics{}) {
		t.Fatal("unexpected stage reached logging boundary")
	}
	annotated := annotateDiscoveryFailure(original, discoverySynthesis, errors.New("content discovery draft exceeds its text bounds private-reason-marker"))
	var clone *Error
	if !errors.As(annotated, &clone) || clone == original || clone.Code != original.Code || clone.StatusCode != original.StatusCode || clone.Message != original.Message || clone.RequestID != original.RequestID {
		t.Fatal("stage annotation mutated or replaced provider error fields")
	}
	if got := clone.DiscoveryFailureDiagnostics(); got != (DiscoveryDiagnostics{Stage: "discovery_synthesis", ValidationReason: "unknown"}) {
		t.Fatalf("dynamic validation text escaped exact allowlist: %#v", got)
	}
	if original.discoveryDiagnostics.Stage != "private-stage-marker" || original.discoveryDiagnostics.ValidationReason != "private-reason-marker" {
		t.Fatal("shared original error was mutated")
	}
	encoded, _ := json.Marshal(original)
	if strings.Contains(original.Error()+string(encoded), "private-") {
		t.Fatal("injected private diagnostics became public error fields")
	}
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, errors.New("ordinary validation error")} {
		if !errors.Is(annotateDiscoveryFailure(cause, discoveryPostcheck, nil), cause) {
			t.Fatal("diagnostics changed cancellation or non-provider error identity")
		}
	}
	var empty *Error
	if empty.DiscoveryFailureDiagnostics() != (DiscoveryDiagnostics{}) {
		t.Fatal("nil diagnostics did not remain empty")
	}
}
