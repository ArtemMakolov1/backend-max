package contentsearch

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"testing"
)

const privateDiagnosticText = "https://private.example/search?key=sk-proj-synthetic-private&query=private-query"

func TestContentSearchDiagnosticsValidateEveryField(t *testing.T) {
	for _, test := range []struct {
		name  string
		input Diagnostics
		want  Diagnostics
	}{
		{name: "HTTP rejection", input: Diagnostics{Provider: "exa", Code: "provider_rejected", Status: 403, TransportKind: "tls"}, want: Diagnostics{Provider: "exa", Code: "provider_rejected", Status: 403}},
		{name: "typed transport", input: Diagnostics{Provider: "tavily", Code: "request_failed", TransportKind: "dns"}, want: Diagnostics{Provider: "tavily", Code: "request_failed", TransportKind: "dns"}},
		{name: "typed body read", input: Diagnostics{Provider: "exa", Code: "response_unreadable", TransportKind: "timeout"}, want: Diagnostics{Provider: "exa", Code: "response_unreadable", TransportKind: "timeout"}},
		{name: "all private values", input: Diagnostics{Provider: privateDiagnosticText, Code: privateDiagnosticText, Status: 600, TransportKind: privateDiagnosticText}, want: Diagnostics{Provider: "unknown", Code: "unknown"}},
		{name: "unknown transport", input: Diagnostics{Provider: "exa", Code: "request_failed", Status: 99, TransportKind: privateDiagnosticText}, want: Diagnostics{Provider: "exa", Code: "request_failed", TransportKind: "unknown"}},
		{name: "lower status bound", input: Diagnostics{Provider: "exa", Code: "provider_rejected", Status: 100}, want: Diagnostics{Provider: "exa", Code: "provider_rejected", Status: 100}},
		{name: "upper status bound", input: Diagnostics{Provider: "tavily", Code: "provider_rejected", Status: 599}, want: Diagnostics{Provider: "tavily", Code: "provider_rejected", Status: 599}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := test.input.Safe(); got != test.want {
				t.Fatalf("safe diagnostics = %#v, want %#v", got, test.want)
			}
			providerErr := &Error{Provider: test.input.Provider, Code: test.input.Code, Status: test.input.Status, TransportKind: test.input.TransportKind}
			if got := providerErr.SafeDiagnostics(); got != test.want || strings.Contains(providerErr.Error(), privateDiagnosticText) {
				t.Fatalf("error did not enforce the safe contract: %#v, %s", got, providerErr.Error())
			}
		})
	}
	var absent *Error
	if absent.SafeDiagnostics() != (Diagnostics{Provider: "unknown", Code: "unknown"}) {
		t.Fatal("nil diagnostic was not an explicit safe unknown")
	}
}

func TestContentSearchTransportClassificationNeverUsesPrivateStrings(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{name: "DNS", err: &net.DNSError{Name: privateDiagnosticText, Err: privateDiagnosticText, IsTimeout: true}, want: "dns"},
		{name: "certificate verification", err: &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{Cert: &x509.Certificate{DNSNames: []string{privateDiagnosticText}}}}, want: "tls"},
		{name: "hostname verification", err: x509.HostnameError{Certificate: &x509.Certificate{DNSNames: []string{privateDiagnosticText}}, Host: privateDiagnosticText}, want: "tls"},
		{name: "invalid certificate", err: x509.CertificateInvalidError{Cert: &x509.Certificate{}, Detail: privateDiagnosticText}, want: "tls"},
		{name: "invalid TLS record", err: tls.RecordHeaderError{Msg: privateDiagnosticText}, want: "tls"},
		{name: "TLS alert", err: tls.AlertError(42), want: "tls"},
		{name: "deadline", err: context.DeadlineExceeded, want: "timeout"},
		{name: "network timeout", err: &net.OpError{Op: privateDiagnosticText, Err: diagnosticTimeoutError{}}, want: "timeout"},
		{name: "no route", err: syscall.ENETUNREACH, want: "no_route"},
		{name: "host unreachable", err: syscall.EHOSTUNREACH, want: "no_route"},
		{name: "refused", err: syscall.ECONNREFUSED, want: "connection_refused"},
		{name: "reset", err: syscall.ECONNRESET, want: "connection_reset"},
		{name: "cancel", err: context.Canceled, want: "request_canceled"},
		{name: "private text naming TLS and DNS", err: errors.New("tls dns timeout " + privateDiagnosticText), want: "unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			// net/http wraps transport failures with a URL. Neither it nor the
			// error/certificate/hostname details may survive classification.
			wrapped := &url.Error{Op: privateDiagnosticText, URL: privateDiagnosticText, Err: fmt.Errorf("private cause %s: %w", privateDiagnosticText, test.err)}
			if got := safeTransportKind(wrapped); got != test.want {
				t.Fatalf("transport kind = %q, want %q", got, test.want)
			}
			client, err := newClient(Config{ExaAPIKey: fakeExaKey}, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, wrapped })})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Search(t.Context(), Request{Query: "public query", ContentKind: "article"})
			var providerErr *Error
			if !errors.As(err, &providerErr) || providerErr.SafeDiagnostics() != (Diagnostics{Provider: "exa", Code: "request_failed", TransportKind: test.want}) {
				t.Fatalf("transport metadata lost: %#v", err)
			}
			assertDiagnosticDoesNotRetainPrivateCause(t, err)
		})
	}
}

type diagnosticTimeoutError struct{}

func (diagnosticTimeoutError) Error() string   { return privateDiagnosticText }
func (diagnosticTimeoutError) Timeout() bool   { return true }
func (diagnosticTimeoutError) Temporary() bool { return false }

type diagnosticErrorBody struct {
	err   error
	reads int
}

func (b *diagnosticErrorBody) Read([]byte) (int, error) { b.reads++; return 0, b.err }
func (*diagnosticErrorBody) Close() error               { return nil }

func TestContentSearchHTTPRejectionAndBodyFailureRemainDistinct(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		cause  error
		want   Diagnostics
		reads  int
	}{
		{name: "HTTP forbidden", status: 403, cause: errors.New(privateDiagnosticText), want: Diagnostics{Provider: "tavily", Code: "provider_rejected", Status: 403}, reads: 0},
		{name: "body timeout", status: 200, cause: fmt.Errorf("%s: %w", privateDiagnosticText, context.DeadlineExceeded), want: Diagnostics{Provider: "tavily", Code: "response_unreadable", TransportKind: "timeout"}, reads: 1},
		{name: "unknown body failure", status: 200, cause: errors.New(privateDiagnosticText), want: Diagnostics{Provider: "tavily", Code: "response_unreadable", TransportKind: "unknown"}, reads: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := &diagnosticErrorBody{err: test.cause}
			client, err := newClient(Config{TavilyAPIKey: fakeTavilyKey}, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: test.status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: body}, nil
			})})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Search(t.Context(), Request{Query: "public query", ContentKind: "article"})
			var providerErr *Error
			if !errors.As(err, &providerErr) || providerErr.SafeDiagnostics() != test.want || body.reads != test.reads {
				t.Fatalf("HTTP/body diagnostics = %#v, reads = %d", err, body.reads)
			}
			assertDiagnosticDoesNotRetainPrivateCause(t, err)
		})
	}
}

func TestContentSearchDiagnosticsPreserveFallbackSuccessAndTerminalError(t *testing.T) {
	for _, fallbackSucceeds := range []bool{true, false} {
		t.Run(fmt.Sprint(fallbackSucceeds), func(t *testing.T) {
			var calls []string
			client, err := newClient(Config{ExaAPIKey: fakeExaKey, TavilyAPIKey: fakeTavilyKey}, &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls = append(calls, r.URL.Host)
				if r.URL.Host == "api.exa.ai" {
					return nil, &net.DNSError{Name: privateDiagnosticText, Err: privateDiagnosticText}
				}
				status, body := 401, privateDiagnosticText
				if fallbackSucceeds {
					status, body = 200, `{"results":[{"title":"Source","url":"https://example.com/page","content":"Public content"}]}`
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})})
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.Search(t.Context(), Request{Query: "public query", ContentKind: "article"})
			if strings.Join(calls, ",") != "api.exa.ai,api.tavily.com" {
				t.Fatalf("fallback order/bound changed: %v", calls)
			}
			if fallbackSucceeds {
				if err != nil || result.Provider != "tavily" || len(result.Sources) != 1 {
					t.Fatalf("fallback success changed: %#v %v", result, err)
				}
				return
			}
			var providerErr *Error
			if !errors.As(err, &providerErr) || providerErr.SafeDiagnostics() != (Diagnostics{Provider: "tavily", Code: "provider_rejected", Status: 401}) {
				t.Fatalf("last provider failure changed: %#v", err)
			}
			assertDiagnosticDoesNotRetainPrivateCause(t, err)
		})
	}
}

func assertDiagnosticDoesNotRetainPrivateCause(t *testing.T, err error) {
	t.Helper()
	encoded, marshalErr := json.Marshal(err)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	for _, text := range []string{err.Error(), fmt.Sprintf("%#v", err), string(encoded)} {
		for _, private := range []string{privateDiagnosticText, fakeExaKey, fakeTavilyKey, "sk-proj-synthetic-private", "private-query"} {
			if strings.Contains(text, private) {
				t.Fatalf("private transport data retained: %s", text)
			}
		}
	}
	if errors.Unwrap(err) != nil {
		t.Fatal("diagnostic error retains an unsafe cause")
	}
}
