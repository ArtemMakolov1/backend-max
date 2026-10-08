package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"maxpilot/backend/internal/contentsearch"
	"maxpilot/backend/internal/openairesearch"
)

func TestWriteErrorLogsOnlySafeContentSearchDiagnostics(t *testing.T) {
	const private = "https://private.example/search?key=sk-proj-synthetic-private&query=private-query"
	for _, test := range []struct {
		name  string
		input contentsearch.Diagnostics
		want  contentsearch.Diagnostics
	}{
		{name: "HTTP rejected", input: contentsearch.Diagnostics{Provider: "exa", Code: "provider_rejected", Status: 403, TransportKind: "tls"}, want: contentsearch.Diagnostics{Provider: "exa", Code: "provider_rejected", Status: 403}},
		{name: "DNS failure", input: contentsearch.Diagnostics{Provider: "tavily", Code: "request_failed", TransportKind: "dns"}, want: contentsearch.Diagnostics{Provider: "tavily", Code: "request_failed", TransportKind: "dns"}},
		{name: "TLS failure", input: contentsearch.Diagnostics{Provider: "exa", Code: "request_failed", TransportKind: "tls"}, want: contentsearch.Diagnostics{Provider: "exa", Code: "request_failed", TransportKind: "tls"}},
		{name: "timeout", input: contentsearch.Diagnostics{Provider: "exa", Code: "response_unreadable", TransportKind: "timeout"}, want: contentsearch.Diagnostics{Provider: "exa", Code: "response_unreadable", TransportKind: "timeout"}},
		{name: "opaque old client", want: contentsearch.Diagnostics{Provider: "unknown", Code: "unknown"}},
		{name: "unsafe client values", input: contentsearch.Diagnostics{Provider: private, Code: private, Status: 600, TransportKind: private}, want: contentsearch.Diagnostics{Provider: "unknown", Code: "unknown"}},
		{name: "unsafe transport", input: contentsearch.Diagnostics{Provider: "exa", Code: "request_failed", Status: 99, TransportKind: private}, want: contentsearch.Diagnostics{Provider: "exa", Code: "request_failed", TransportKind: "unknown"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			server := &Server{logger: slog.New(slog.NewJSONHandler(&logs, nil))}
			response := httptest.NewRecorder()
			server.writeError(response, fmt.Errorf("private wrapper %s: %w", private, &openairesearch.Error{
				Code: "content_search_failed", Message: private, RequestID: private, SearchDiagnostics: test.input,
			}))
			if response.Code != http.StatusBadGateway {
				t.Fatalf("HTTP status changed: %d", response.Code)
			}
			var problem struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
				t.Fatal(err)
			}
			if problem.Error.Code != "content_search_error" || problem.Error.Message != "Не удалось получить материалы. Попробуйте ещё раз немного позже." {
				t.Fatalf("public problem contract changed: %s", response.Body.String())
			}
			var fields map[string]any
			if err := json.Unmarshal(logs.Bytes(), &fields); err != nil {
				t.Fatal(err)
			}
			if len(fields) != 7 || fields["level"] != "WARN" || fields["msg"] != "content source retrieval failed" || fields["provider"] != test.want.Provider || fields["code"] != test.want.Code || fields["status"] != float64(test.want.Status) || fields["transport_kind"] != test.want.TransportKind {
				t.Fatalf("WARN fields escaped the fixed contract: %#v", fields)
			}
			for _, output := range []string{logs.String(), response.Body.String()} {
				for _, forbidden := range []string{private, "sk-proj-synthetic-private", "private-query", "request_id", "SearchDiagnostics"} {
					if strings.Contains(output, forbidden) {
						t.Fatalf("private error data escaped logger or API: %s", output)
					}
				}
			}
			for _, internal := range []string{"provider", "transport_kind", "request_failed", "provider_rejected"} {
				if strings.Contains(response.Body.String(), internal) {
					t.Fatalf("internal diagnostic appeared in API response: %s", response.Body.String())
				}
			}
		})
	}
}
