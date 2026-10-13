package contentsearch

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const proxyTestUser = "synthetic-proxy-user"
const proxyTestPassword = "synthetic-proxy-password"

func TestExaProxyValidationAndExactRouting(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://unexpected-global-proxy.invalid:9999")
	t.Setenv("HTTPS_PROXY", "http://unexpected-global-proxy.invalid:9999")
	for _, value := range []string{"", "  ", "http://proxy.example", "https://proxy.example:8443", "http://127.0.0.1:3128", "http://[::1]:3128", "https://user:p%40ss%23word@proxy.example:8443", "https://user:p%24ss%27word%22%5C@proxy.example:8443"} {
		if err := ValidateExaProxyURL(value); err != nil {
			t.Fatal("valid authority-only proxy rejected")
		}
	}
	for _, value := range []string{
		"proxy.example:3128", "ftp://proxy.example", "socks5://proxy.example:1080",
		"http://proxy.example/", "http://proxy.example/path", "http://proxy.example?", "http://proxy.example?secret=value",
		"http://proxy.example#", "http://proxy.example#secret", "http://proxy.example:", "http://proxy.example:0", "http://proxy.example:65536",
		"http://-proxy.example", "http://proxy..example", "http://@proxy.example", "http://user:%0Apassword@proxy.example",
		"http://user:password@proxy.example\n", "http://user:password@proxy.example\t", "http://user:%ZZ@proxy.example",
		"http://user:pass$word@proxy.example", "http://user:pass'word@proxy.example", "http://user:pass\"word@proxy.example", "http://user:pass\\word@proxy.example",
		"http://user:пароль@proxy.example", strings.Repeat("s", 4097),
	} {
		if err := ValidateExaProxyURL(value); err == nil || strings.Contains(err.Error(), "proxy.example") || strings.Contains(err.Error(), "password") || strings.Contains(err.Error(), "secret") {
			t.Fatal("invalid proxy accepted or echoed private URL components")
		}
	}
	client, err := NewClient(Config{ExaAPIKey: fakeExaKey})
	if err != nil {
		t.Fatal(err)
	}
	if client.http.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("global proxy environment selected a route without opt-in")
	}
	client, err = NewClient(Config{ExaAPIKey: fakeExaKey, ExaProxyURL: "https://proxy.example:8443"})
	if err != nil {
		t.Fatal(err)
	}
	transport := client.http.Transport.(*http.Transport)
	if transport.TLSClientConfig.MinVersion != tls.VersionTLS12 || transport.TLSClientConfig.InsecureSkipVerify || client.http.Timeout != providerTimeout || transport.ResponseHeaderTimeout != providerTimeout {
		t.Fatal("proxy changed TLS or timeout bounds")
	}
	for _, target := range []string{exaEndpoint, tavilyEndpoint, "http://api.exa.ai/search", "https://api.exa.ai/other", "https://api.exa.ai/search?private=query", "https://user@api.exa.ai/search", "https://api.exa.ai/search#private", "https://other.example/search", "https://api.exa.ai/%73earch"} {
		request, err := http.NewRequest(http.MethodPost, target, nil)
		if err != nil {
			t.Fatal(err)
		}
		proxy, err := transport.Proxy(request)
		if err != nil || (proxy != nil) != (target == exaEndpoint) {
			t.Fatal("proxy routing escaped the exact fixed Exa endpoint")
		}
	}
}

func proxyTestCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"api.exa.ai", "api.tavily.com"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, roots
}

func proxyTestServer(t *testing.T, certificate tls.Certificate, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

func TestExaHTTPAndHTTPSConnectKeepAPIAndProxyCredentialsIsolated(t *testing.T) {
	for _, scheme := range []string{"http", "https"} {
		t.Run(scheme, func(t *testing.T) {
			certificate, roots := proxyTestCertificate(t)
			var originCalls, connectCalls atomic.Int32
			origin := proxyTestServer(t, certificate, func(w http.ResponseWriter, request *http.Request) {
				originCalls.Add(1)
				if request.TLS == nil || request.Header.Get("Proxy-Authorization") != "" {
					t.Error("upstream request lost TLS or received proxy credentials")
				}
				switch request.Host {
				case "api.exa.ai":
					if request.Header.Get("x-api-key") != fakeExaKey || request.Header.Get("Authorization") != "" {
						t.Error("Exa did not receive its own API credential")
					}
					jsonResponse(w, `{"results":[{"title":"Exa source","url":"https://example.com/article","highlights":["Public source evidence"]}]}`)
				case "api.tavily.com":
					if request.Header.Get("Authorization") != "Bearer "+fakeTavilyKey || request.Header.Get("x-api-key") != "" {
						t.Error("Tavily received a wrong provider credential")
					}
					jsonResponse(w, `{"results":[{"title":"Tavily source","url":"https://example.org/article","content":"Public source evidence"}]}`)
				default:
					t.Error("unexpected upstream host")
				}
			})
			proxyHandler := func(w http.ResponseWriter, request *http.Request) {
				connectCalls.Add(1)
				wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(proxyTestUser+":"+proxyTestPassword))
				if request.Method != http.MethodConnect || request.Host != "api.exa.ai:443" || request.Header.Get("Proxy-Authorization") != wantAuth || request.Header.Get("x-api-key") != "" || request.Header.Get("Authorization") != "" {
					t.Error("CONNECT target or credential isolation failed")
				}
				upstream, err := net.DialTimeout("tcp", origin.Listener.Addr().String(), time.Second)
				if err != nil {
					t.Error("local CONNECT fixture could not reach its TLS origin")
					return
				}
				connection, buffered, err := w.(http.Hijacker).Hijack()
				if err != nil {
					_ = upstream.Close()
					t.Error("local CONNECT fixture could not hijack")
					return
				}
				defer connection.Close()
				defer upstream.Close()
				_, _ = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
				_ = buffered.Flush()
				go func() { _, _ = io.Copy(upstream, buffered); _ = upstream.Close() }()
				_, _ = io.Copy(connection, upstream)
			}
			var proxy *httptest.Server
			if scheme == "https" {
				proxy = proxyTestServer(t, certificate, proxyHandler)
			} else {
				proxy = httptest.NewServer(http.HandlerFunc(proxyHandler))
				t.Cleanup(proxy.Close)
			}
			proxyURL, _ := url.Parse(proxy.URL)
			proxyURL.User = url.UserPassword(proxyTestUser, proxyTestPassword)
			client, err := NewClient(Config{ExaAPIKey: fakeExaKey, TavilyAPIKey: fakeTavilyKey, ExaProxyURL: proxyURL.String()})
			if err != nil {
				t.Fatal("local proxy configuration rejected")
			}
			transport := client.http.Transport.(*http.Transport)
			transport.TLSClientConfig.RootCAs = roots
			dialer := &net.Dialer{Timeout: time.Second}
			transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				if address == "api.tavily.com:443" {
					address = origin.Listener.Addr().String()
				} else if address != proxy.Listener.Addr().String() {
					return nil, errors.New("fixture prevented an unexpected external dial")
				}
				return dialer.DialContext(ctx, network, address)
			}
			t.Cleanup(transport.CloseIdleConnections)
			result, err := client.Search(t.Context(), Request{Query: "public source query", ContentKind: "article"})
			if err != nil || result.Provider != "exa" || len(result.Sources) != 1 || connectCalls.Load() != 1 {
				t.Fatal("Exa search did not traverse the authenticated CONNECT tunnel")
			}
			result, err = client.Search(t.Context(), Request{Query: "public source query", ContentKind: "auto"})
			if err != nil || result.Provider != "tavily" || connectCalls.Load() != 1 || originCalls.Load() != 2 {
				t.Fatal("Tavily search did not stay direct")
			}
			if err := client.http.CheckRedirect(&http.Request{}, nil); !errors.Is(err, http.ErrUseLastResponse) {
				t.Fatal("proxy weakened the redirect policy")
			}
		})
	}
}

func TestExaProxyFailuresAreSanitizedAndNeverFallBackToDirectExa(t *testing.T) {
	var calls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		http.Error(w, proxyTestUser+":"+proxyTestPassword+" "+fakeExaKey, http.StatusProxyAuthRequired)
	}))
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL)
	proxyURL.User = url.UserPassword(proxyTestUser, proxyTestPassword)
	client, err := NewClient(Config{ExaAPIKey: fakeExaKey, ExaProxyURL: proxyURL.String()})
	if err != nil {
		t.Fatal("local proxy configuration rejected")
	}
	transport := client.http.Transport.(*http.Transport)
	t.Cleanup(transport.CloseIdleConnections)
	dialer := &net.Dialer{Timeout: time.Second}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != proxy.Listener.Addr().String() {
			t.Error("failed proxy attempted a direct Exa connection")
			return nil, errors.New("unexpected external dial prevented")
		}
		return dialer.DialContext(ctx, network, address)
	}
	_, err = client.Search(t.Context(), Request{Query: "public source query", ContentKind: "article"})
	var sourceError *Error
	if !errors.As(err, &sourceError) || sourceError.SafeDiagnostics().Code != "request_failed" || calls.Load() != 1 {
		t.Fatal("CONNECT rejection lost its safe transport classification")
	}
	for _, private := range []string{proxyURL.String(), proxyTestUser, proxyTestPassword, fakeExaKey} {
		if strings.Contains(err.Error(), private) {
			t.Fatal("proxy failure leaked private connection details")
		}
	}
}

func TestExaProxyCannotDisableUpstreamCertificateVerification(t *testing.T) {
	var originCalls atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { originCalls.Add(1) }))
	origin.Config.ErrorLog = log.New(io.Discard, "", 0)
	defer origin.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstream, err := net.DialTimeout("tcp", origin.Listener.Addr().String(), time.Second)
		if err != nil {
			return
		}
		connection, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			_ = upstream.Close()
			return
		}
		defer connection.Close()
		defer upstream.Close()
		_, _ = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = buffered.Flush()
		go func() { _, _ = io.Copy(upstream, buffered); _ = upstream.Close() }()
		_, _ = io.Copy(connection, upstream)
	}))
	defer proxy.Close()
	client, err := NewClient(Config{ExaAPIKey: fakeExaKey, ExaProxyURL: proxy.URL})
	if err != nil {
		t.Fatal("local proxy configuration rejected")
	}
	transport := client.http.Transport.(*http.Transport)
	defer transport.CloseIdleConnections()
	dialer := &net.Dialer{Timeout: time.Second}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != proxy.Listener.Addr().String() {
			return nil, errors.New("fixture prevented an unexpected external dial")
		}
		return dialer.DialContext(ctx, network, address)
	}
	// Trust the fixture's CA, but not its hostname: the certificate is not for
	// api.exa.ai, which must remain the original end-to-end TLS server name.
	transport.TLSClientConfig.RootCAs = x509.NewCertPool()
	transport.TLSClientConfig.RootCAs.AddCert(origin.Certificate())
	_, err = client.Search(t.Context(), Request{Query: "public source query", ContentKind: "article"})
	var sourceError *Error
	if !errors.As(err, &sourceError) || sourceError.SafeDiagnostics().TransportKind != "tls" || originCalls.Load() != 0 {
		t.Fatal("proxy allowed an invalid upstream TLS hostname or lost safe TLS diagnostics")
	}
}
