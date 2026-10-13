package contentsearch

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestExaTLSRelayDialOnlyOverridesFixedOrigin(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, address := range []string{"api.exa.ai:443", "api.exa.ai:80", "api.tavily.com:443", "API.exa.ai:443", "other.example:443"} {
		want := address
		if address == "api.exa.ai:443" {
			want = "relay.example:8443"
		}
		dial := exaRelayDialContext("relay.example:8443", func(gotCtx context.Context, network, target string) (net.Conn, error) {
			if gotCtx != ctx || network != "tcp" || target != want {
				t.Error("relay changed a non-Exa destination or lost dial context")
			}
			return nil, gotCtx.Err()
		})
		if _, err := dial(ctx, "tcp", address); !errors.Is(err, context.Canceled) {
			t.Fatal("relay lost cancellation")
		}
	}
}

// This fixture forwards opaque TCP bytes without terminating or creating TLS.
// The original provider certificate and ClientHello SNI reach the origin.
func tlsRelayFixture(t *testing.T, origin string) (string, *atomic.Int32) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	calls := new(atomic.Int32)
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			calls.Add(1)
			go func() {
				defer func() { _ = connection.Close() }()
				_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
				upstream, err := net.DialTimeout("tcp", origin, time.Second)
				if err != nil {
					t.Error("local TLS relay could not reach its fixture origin")
					return
				}
				defer func() { _ = upstream.Close() }()
				_ = upstream.SetDeadline(time.Now().Add(5 * time.Second))
				var first [1]byte
				if _, err := io.ReadFull(connection, first[:]); err != nil {
					return
				}
				if first[0] != 0x16 {
					t.Error("relay received plaintext instead of a TLS ClientHello")
					return
				}
				if _, err := upstream.Write(first[:]); err != nil {
					return
				}
				go func() { _, _ = io.Copy(upstream, connection); _ = upstream.Close() }()
				_, _ = io.Copy(connection, upstream)
			}()
		}
	}()
	return listener.Addr().String(), calls
}

func offlineRelayTransport(t *testing.T, client *Client, relay, directOrigin string) *http.Transport {
	t.Helper()
	transport := client.http.Transport.(*http.Transport)
	if transport.Proxy != nil || transport.TLSClientConfig.ServerName != "" || transport.TLSClientConfig.MinVersion != tls.VersionTLS12 || transport.TLSClientConfig.InsecureSkipVerify || transport.TLSHandshakeTimeout != 10*time.Second || transport.ResponseHeaderTimeout != providerTimeout || client.http.Timeout != providerTimeout {
		t.Fatal("TLS relay changed proxy, origin TLS or timeout policy")
	}
	dialer := &net.Dialer{Timeout: time.Second}
	transport.DialContext = exaRelayDialContext(relay, func(ctx context.Context, network, address string) (net.Conn, error) {
		if address == "api.tavily.com:443" && directOrigin != "" {
			address = directOrigin
		} else if address != relay {
			return nil, errors.New("fixture prevented an unexpected external dial")
		}
		return dialer.DialContext(ctx, network, address)
	})
	t.Cleanup(transport.CloseIdleConnections)
	return transport
}

func TestExaTLSRelayKeepsOriginTLSAndCredentialsAndTavilyDirect(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://unexpected-global-proxy.invalid:9999")
	certificate, roots := proxyTestCertificate(t)
	var originCalls atomic.Int32
	origin := proxyTestServer(t, certificate, func(w http.ResponseWriter, request *http.Request) {
		originCalls.Add(1)
		if request.TLS == nil || request.TLS.ServerName != request.Host || request.Header.Get("Proxy-Authorization") != "" {
			t.Error("relay changed origin SNI or exposed proxy authentication")
		}
		switch request.Host {
		case "api.exa.ai":
			if request.Header.Get("x-api-key") != fakeExaKey || request.Header.Get("Authorization") != "" {
				t.Error("relay changed the Exa credential")
			}
			jsonResponse(w, `{"results":[{"title":"Exa source","url":"https://example.com/article","highlights":["Public source evidence"]}]}`)
		case "api.tavily.com":
			if request.Header.Get("Authorization") != "Bearer "+fakeTavilyKey || request.Header.Get("x-api-key") != "" {
				t.Error("direct Tavily received the wrong provider credential")
			}
			jsonResponse(w, `{"results":[{"title":"Tavily source","url":"https://example.org/article","content":"Public source evidence"}]}`)
		default:
			t.Error("unexpected TLS origin")
		}
	})
	relay, relayCalls := tlsRelayFixture(t, origin.Listener.Addr().String())
	client, err := NewClient(Config{ExaAPIKey: fakeExaKey, TavilyAPIKey: fakeTavilyKey, ExaProxyURL: "tls://" + relay})
	if err != nil {
		t.Fatal(err)
	}
	transport := offlineRelayTransport(t, client, relay, origin.Listener.Addr().String())
	transport.TLSClientConfig.RootCAs = roots
	result, err := client.Search(t.Context(), Request{Query: "public source query", ContentKind: "article"})
	if err != nil || result.Provider != "exa" || len(result.Sources) != 1 || relayCalls.Load() != 1 {
		t.Fatal("Exa search did not traverse the TLS relay")
	}
	result, err = client.Search(t.Context(), Request{Query: "public source query", ContentKind: "auto"})
	if err != nil || result.Provider != "tavily" || len(result.Sources) != 1 || relayCalls.Load() != 1 || originCalls.Load() != 2 {
		t.Fatal("Tavily did not stay direct with the Exa TLS relay enabled")
	}
}

func TestExaTLSRelayPreservesResponseAndRedirectBounds(t *testing.T) {
	for _, test := range []struct {
		name string
		code string
	}{
		{name: "redirect", code: "provider_rejected"},
		{name: "large_response", code: "response_too_large"},
	} {
		t.Run(test.name, func(t *testing.T) {
			certificate, roots := proxyTestCertificate(t)
			var originCalls atomic.Int32
			origin := proxyTestServer(t, certificate, func(w http.ResponseWriter, request *http.Request) {
				originCalls.Add(1)
				if test.name == "redirect" {
					http.Redirect(w, request, "https://unexpected.invalid/private", http.StatusFound)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Content-Length", strconv.Itoa(maxResponseBytes+1))
				w.WriteHeader(http.StatusOK)
			})
			relay, relayCalls := tlsRelayFixture(t, origin.Listener.Addr().String())
			client, err := NewClient(Config{ExaAPIKey: fakeExaKey, ExaProxyURL: "tls://" + relay})
			if err != nil {
				t.Fatal(err)
			}
			transport := offlineRelayTransport(t, client, relay, "")
			transport.TLSClientConfig.RootCAs = roots
			_, err = client.Search(t.Context(), Request{Query: "public source query", ContentKind: "article"})
			var sourceError *Error
			if !errors.As(err, &sourceError) || sourceError.SafeDiagnostics().Code != test.code || originCalls.Load() != 1 || relayCalls.Load() != 1 {
				t.Fatal("TLS relay weakened the response or redirect bounds")
			}
		})
	}
}

func TestExaTLSRelayRejectsWrongOriginCertificateWithoutDirectRetry(t *testing.T) {
	var originCalls atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { originCalls.Add(1) }))
	origin.Config.ErrorLog = log.New(io.Discard, "", 0)
	defer origin.Close()
	relay, relayCalls := tlsRelayFixture(t, origin.Listener.Addr().String())
	relayURL := "tls://" + relay
	client, err := NewClient(Config{ExaAPIKey: fakeExaKey, ExaProxyURL: relayURL})
	if err != nil {
		t.Fatal(err)
	}
	transport := offlineRelayTransport(t, client, relay, "")
	transport.TLSClientConfig.RootCAs = x509.NewCertPool()
	transport.TLSClientConfig.RootCAs.AddCert(origin.Certificate())
	_, err = client.Search(t.Context(), Request{Query: "public source query", ContentKind: "article"})
	var sourceError *Error
	if !errors.As(err, &sourceError) || sourceError.SafeDiagnostics().TransportKind != "tls" || originCalls.Load() != 0 || relayCalls.Load() != 1 {
		t.Fatal("relay accepted a wrong origin certificate or retried directly")
	}
	for _, private := range []string{relayURL, relay, fakeExaKey} {
		if strings.Contains(err.Error(), private) {
			t.Fatal("TLS relay failure exposed private connection details")
		}
	}
}
