package mediafetch

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func pngHeader() []byte { return []byte{137, 80, 78, 71, 13, 10, 26, 10, 0, 0, 0, 13, 73, 72, 68, 82} }

func fixtureFetcher(t *testing.T, handler http.HandlerFunc) (*Fetcher, *[]string) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	dials := []string{}
	f := New()
	f.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	f.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		dials = append(dials, address)
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	f.tlsConfig = func(host string) (*tls.Config, error) {
		return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: host}, nil
	}
	return f, &dials
}

func TestFetchPinsPublicDNSRetainsTLSHostnameAndSniffsMedia(t *testing.T) {
	f, dials := fixtureFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "example.com" || r.TLS.ServerName != "example.com" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Accept-Encoding") != "identity" {
			t.Error("hostname or credential isolation failed")
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write(pngHeader())
	})
	download, err := f.Fetch(t.Context(), "https://example.com/photo?signature=private", Limits{MaxBytes: 1024, Timeout: time.Second, MaxRedirects: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = download.Close() }()
	data, err := io.ReadAll(download.Body)
	if err != nil || !bytes.Equal(data, pngHeader()) || download.MIMEType != "image/png" || len(*dials) != 1 || (*dials)[0] != "8.8.8.8:443" {
		t.Fatalf("pin/sniff result=%#v error=%v dials=%v", download, err, *dials)
	}
}

func TestFetchRejectsUnsafeTargetsAndAnyPrivateDNSAnswerBeforeDial(t *testing.T) {
	for _, raw := range []string{"http://example.com/photo", "https://localhost/photo", "https://127.0.0.1/photo", "https://2130706433/photo", "https://0177.0.0.1/photo", "https://0x7f.0.0.1/photo", "https://example.com:8443/photo", "https://user:secret@example.com/photo", "https://a.internal/photo", "https://example.com/photo#fragment"} {
		f := New()
		f.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
			t.Fatal("unsafe URL reached DNS")
			return nil, nil
		}
		if _, err := f.Fetch(t.Context(), raw, Limits{MaxBytes: 1024, Timeout: time.Second}); err == nil {
			t.Fatalf("unsafe URL accepted: %s", raw)
		}
	}
	for _, address := range []string{"127.0.0.1", "169.254.169.254", "10.0.0.1", "100.64.0.1", "192.0.2.1", "::1", "fc00::1", "::ffff:8.8.8.8", "2002:808:808::1", "2001:db8::1"} {
		f := New()
		f.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr(address)}, nil
		}
		f.dial = func(context.Context, string, string) (net.Conn, error) {
			t.Fatal("mixed private DNS answer reached dial")
			return nil, nil
		}
		if _, err := f.Fetch(t.Context(), "https://example.com/photo", Limits{MaxBytes: 1024, Timeout: time.Second}); err == nil {
			t.Fatalf("unsafe answer accepted: %s", address)
		}
	}
}

func TestFetchRechecksRedirectDNSAndNeverForwardsCookies(t *testing.T) {
	requests := 0
	f, dials := fixtureFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path == "/photo" {
			w.Header().Set("Set-Cookie", "secret=private")
			w.Header().Set("Location", "/asset")
			w.WriteHeader(302)
			return
		}
		if r.Header.Get("Cookie") != "" {
			t.Error("redirect forwarded cookie")
		}
		_, _ = w.Write(pngHeader())
	})
	lookups := 0
	f.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		lookups++
		if lookups == 2 {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	if _, err := f.Fetch(t.Context(), "https://example.com/photo", Limits{MaxBytes: 1024, Timeout: time.Second, MaxRedirects: 3}); err == nil || requests != 1 || len(*dials) != 1 || lookups != 2 {
		t.Fatal("DNS rebinding after redirect was not rejected before second connection")
	}
	f.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	download, err := f.Fetch(t.Context(), "https://example.com/photo", Limits{MaxBytes: 1024, Timeout: time.Second, MaxRedirects: 3})
	if err != nil {
		t.Fatal(err)
	}
	_ = download.Close()
	if requests != 3 {
		t.Fatal("validated redirect did not reach safe media")
	}
}

func TestFetchBoundsUnknownLengthStreamAndHidesSignedErrorURLs(t *testing.T) {
	f, _ := fixtureFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/error" {
			w.WriteHeader(403)
			_, _ = w.Write([]byte("private-provider-error"))
			return
		}
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		_, _ = w.Write(append(pngHeader(), bytes.Repeat([]byte{1}, 1024)...))
	})
	download, err := f.Fetch(t.Context(), "https://example.com/photo", Limits{MaxBytes: 512, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = download.Close() }()
	data, err := io.ReadAll(download.Body)
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "too_large" || len(data) > 512 {
		t.Fatalf("unknown stream bypassed cap: bytes=%d error=%v", len(data), err)
	}
	_, err = f.Fetch(t.Context(), "https://example.com/error?signature=private-key", Limits{MaxBytes: 1024, Timeout: time.Second})
	if err == nil || strings.Contains(err.Error(), "private") || !errors.As(err, &failure) || failure.HTTPStatus != 403 {
		t.Fatal("provider body or signed URL escaped sanitized error")
	}
}

func TestFetchRejectsNonMediaAndWrongTLSHostname(t *testing.T) {
	f, _ := fixtureFetcher(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>not an image</html>")) })
	if _, err := f.Fetch(t.Context(), "https://example.com/page", Limits{MaxBytes: 1024, Timeout: time.Second}); err == nil {
		t.Fatal("HTML became media")
	}
	if _, err := f.Fetch(t.Context(), "https://wrong.example.com/photo", Limits{MaxBytes: 1024, Timeout: time.Second}); err == nil {
		t.Fatal("certificate hostname validation bypassed")
	}
}

func TestFetchBodyRemainsUnderDeadline(t *testing.T) {
	f, _ := fixtureFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(append(pngHeader(), bytes.Repeat([]byte{1}, 512)...))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	download, err := f.Fetch(t.Context(), "https://example.com/photo", Limits{MaxBytes: 4096, Timeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = download.Close() }()
	_, err = io.ReadAll(download.Body)
	if err == nil {
		t.Fatal("stream survived retrieval deadline")
	}
}

func TestFetchRecognizesActualVideoContainerHeadersWithoutTrustingFilenameOrContentType(t *testing.T) {
	for _, sample := range []struct {
		name, mime string
		data       []byte
	}{
		{name: "mp4", mime: "video/mp4", data: []byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'm', 'p', '4', '2', 0, 0, 0, 0, 'm', 'p', '4', '2', 'i', 's', 'o', 'm'}},
		{name: "mov", mime: "video/quicktime", data: []byte{0, 0, 0, 20, 'f', 't', 'y', 'p', 'q', 't', ' ', ' ', 0, 0, 0, 0, 'q', 't', ' ', ' '}},
		{name: "webm", mime: "video/webm", data: []byte{0x1a, 0x45, 0xdf, 0xa3, 0x9f, 0x42, 0x82, 0x84, 'w', 'e', 'b', 'm'}},
	} {
		t.Run(sample.name, func(t *testing.T) {
			f, _ := fixtureFetcher(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				_, _ = w.Write(sample.data)
			})
			download, err := f.Fetch(t.Context(), "https://example.com/asset", Limits{MaxBytes: 1024, Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = download.Close() }()
			if download.MIMEType != sample.mime {
				t.Fatalf("actual video container not recognized: %q want %q", download.MIMEType, sample.mime)
			}
		})
	}
}
