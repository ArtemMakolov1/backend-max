package openairesearch

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"maxpilot/backend/internal/contentsearch"
)

type diagnosticRoundTripFunc func(*http.Request) (*http.Response, error)

func (f diagnosticRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestExternalDiscoveryCopiesSafeProviderDiagnosticsWithoutPrivateCause(t *testing.T) {
	const private = "https://private.example/search?key=sk-proj-synthetic-private&query=private-query"
	for _, test := range []struct {
		name   string
		source *contentsearch.Error
		want   contentsearch.Diagnostics
	}{
		{name: "HTTP rejection", source: &contentsearch.Error{Provider: "exa", Code: "provider_rejected", Status: 403}, want: contentsearch.Diagnostics{Provider: "exa", Code: "provider_rejected", Status: 403}},
		{name: "transport failure", source: &contentsearch.Error{Provider: "tavily", Code: "request_failed", TransportKind: "tls"}, want: contentsearch.Diagnostics{Provider: "tavily", Code: "request_failed", TransportKind: "tls"}},
		{name: "unsafe manually constructed fields", source: &contentsearch.Error{Provider: private, Code: private, Status: 600, TransportKind: private}, want: contentsearch.Diagnostics{Provider: "unknown", Code: "unknown"}},
		{name: "opaque transport cause", want: contentsearch.Diagnostics{Provider: "unknown", Code: "unknown"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			synthesisCalls := 0
			base, err := New("https://api.openai.com", "offline-key", "gpt-6-luna", &http.Client{Transport: diagnosticRoundTripFunc(func(*http.Request) (*http.Response, error) {
				synthesisCalls++
				return nil, errors.New("synthesis must not run")
			})})
			if err != nil {
				t.Fatal(err)
			}
			cause := errors.New(private)
			if test.source != nil {
				cause = fmt.Errorf("%s: %w", private, test.source)
			}
			_, err = base.WithContentSearch(&fakeContentSearcher{err: cause}).DiscoverContent(t.Context(), DiscoverContentRequest{Topic: "Новости", ContentKind: "article"})
			var wrapper *Error
			if !errors.As(err, &wrapper) || wrapper.Code != "content_search_failed" || wrapper.Message != "External source retrieval is unavailable" || wrapper.SearchDiagnostics != test.want || synthesisCalls != 0 {
				t.Fatalf("safe wrapper/diagnostic contract changed: %#v, calls=%d", err, synthesisCalls)
			}
			if test.source != nil {
				// This source error remains owned by the search implementation.
				// The serving wrapper must never share a mutable diagnostic pointer.
				test.source.Provider, test.source.Code, test.source.TransportKind, test.source.Status = private, private, private, 999
				if wrapper.SearchDiagnostics != test.want {
					t.Fatal("wrapper retained mutable source diagnostics")
				}
			}
			encoded, marshalErr := json.Marshal(wrapper)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			for _, output := range []string{wrapper.Error(), fmt.Sprintf("%#v", wrapper), string(encoded)} {
				for _, forbidden := range []string{private, "sk-proj-synthetic-private", "private-query"} {
					if strings.Contains(output, forbidden) {
						t.Fatalf("wrapper retained private source details: %s", output)
					}
				}
			}
			if strings.Contains(string(encoded), "SearchDiagnostics") || strings.Contains(string(encoded), "TransportKind") {
				t.Fatal("internal provider diagnostics serialized by the wrapper")
			}
			if errors.Unwrap(wrapper) != nil || errors.Is(wrapper, cause) {
				t.Fatal("wrapper retains unsafe transport cause")
			}
		})
	}
}
