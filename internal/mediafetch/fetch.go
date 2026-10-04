// Package mediafetch retrieves public HTTPS media without granting callers
// access to internal networks or forwarding application credentials.
package mediafetch

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"maxpilot/backend/internal/maxclient"
)

type Limits struct {
	MaxBytes     int64
	Timeout      time.Duration
	MaxRedirects int
}

type Error struct {
	Code       string
	HTTPStatus int
	cause      error
}

func (e *Error) Error() string { return "media retrieval failed: " + e.Code }
func (e *Error) Unwrap() error { return e.cause }

type Download struct {
	Body          io.ReadCloser
	MIMEType      string
	FinalURL      string
	ContentLength int64
}

func (d *Download) Close() error { return d.Body.Close() }

type Fetcher struct {
	lookup    func(context.Context, string, string) ([]netip.Addr, error)
	dial      func(context.Context, string, string) (net.Conn, error)
	tlsConfig func(string) (*tls.Config, error)
}

func New() *Fetcher {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &Fetcher{lookup: net.DefaultResolver.LookupNetIP, dial: dialer.DialContext, tlsConfig: mediaTLSConfig}
}

func mediaTLSConfig(host string) (*tls.Config, error) {
	config := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}
	// Keep MAX's official additional root confined to its own media domains.
	if host == "max.ru" || strings.HasSuffix(host, ".max.ru") || host == "oneme.ru" || strings.HasSuffix(host, ".oneme.ru") {
		client, err := maxclient.NewHTTPClient("", 0)
		if err != nil {
			return nil, err
		}
		transport, ok := client.Transport.(*http.Transport)
		if !ok {
			return nil, errors.New("invalid MAX TLS transport")
		}
		config.RootCAs = transport.TLSClientConfig.RootCAs
	}
	return config, nil
}

// Fetch preserves the original hostname for TLS while dialing only the IPs
// already validated for that request. Every redirect gets a fresh validation.
// The returned body remains under the overall deadline until Close.
func (f *Fetcher) Fetch(ctx context.Context, rawURL string, limits Limits) (*Download, error) {
	if limits.MaxBytes <= 0 || limits.MaxBytes > 250<<20 || limits.Timeout <= 0 || limits.Timeout > 90*time.Second || limits.MaxRedirects < 0 || limits.MaxRedirects > 3 {
		return nil, &Error{Code: "invalid_limits"}
	}
	ctx, cancel := context.WithTimeout(ctx, limits.Timeout)
	keep := false
	defer func() {
		if !keep {
			cancel()
		}
	}()
	current := rawURL
	for hop := 0; ; hop++ {
		parsed, err := validatedURL(current)
		if err != nil {
			return nil, err
		}
		ips, err := f.lookup(ctx, "ip", parsed.Hostname())
		if err != nil {
			return nil, safeNetworkError(ctx, "dns_failed")
		}
		if len(ips) == 0 || len(ips) > 32 {
			return nil, &Error{Code: "unsafe_address"}
		}
		for _, ip := range ips {
			if !publicAddress(ip) {
				return nil, &Error{Code: "unsafe_address"}
			}
		}
		config, err := f.tlsConfig(parsed.Hostname())
		if err != nil {
			return nil, &Error{Code: "tls_config"}
		}
		transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, DisableCompression: true,
			TLSClientConfig: config, TLSHandshakeTimeout: 8 * time.Second, ResponseHeaderTimeout: 10 * time.Second,
			MaxResponseHeaderBytes: 64 << 10}
		transport.DialContext = func(dialCtx context.Context, network, address string) (net.Conn, error) {
			if address != net.JoinHostPort(parsed.Hostname(), "443") {
				return nil, &Error{Code: "unsafe_address"}
			}
			var last error
			for _, ip := range ips[:min(len(ips), 4)] {
				conn, dialErr := f.dial(dialCtx, network, net.JoinHostPort(ip.String(), "443"))
				if dialErr == nil {
					return conn, nil
				}
				last = dialErr
			}
			return nil, last
		}
		client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
		if err != nil {
			return nil, &Error{Code: "unsafe_url"}
		}
		request.Header.Set("Accept", "image/*, video/*, application/octet-stream")
		request.Header.Set("Accept-Encoding", "identity")
		request.Header.Set("User-Agent", "MaxPosty-Media/1.0")
		response, err := client.Do(request)
		if err != nil {
			transport.CloseIdleConnections()
			return nil, safeNetworkError(ctx, "network_error")
		}
		if response.StatusCode >= 300 && response.StatusCode <= 399 {
			location, locationErr := response.Location()
			_ = response.Body.Close()
			transport.CloseIdleConnections()
			if locationErr != nil {
				return nil, &Error{Code: "unsafe_url"}
			}
			if hop >= limits.MaxRedirects {
				return nil, &Error{Code: "redirect_limit"}
			}
			current = location.String()
			continue
		}
		if response.StatusCode != http.StatusOK {
			_ = response.Body.Close()
			transport.CloseIdleConnections()
			return nil, &Error{Code: "http_error", HTTPStatus: response.StatusCode}
		}
		if response.ContentLength > limits.MaxBytes {
			_ = response.Body.Close()
			transport.CloseIdleConnections()
			return nil, &Error{Code: "too_large"}
		}
		if encoding := response.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
			_ = response.Body.Close()
			transport.CloseIdleConnections()
			return nil, &Error{Code: "invalid_mime"}
		}
		header := make([]byte, min(int64(512), limits.MaxBytes+1))
		n, readErr := io.ReadFull(response.Body, header)
		if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
			_ = response.Body.Close()
			transport.CloseIdleConnections()
			return nil, safeNetworkError(ctx, "network_error")
		}
		if int64(n) > limits.MaxBytes {
			_ = response.Body.Close()
			transport.CloseIdleConnections()
			return nil, &Error{Code: "too_large"}
		}
		mimeType := sniffMediaType(header[:n])
		if !strings.HasPrefix(mimeType, "image/") && !strings.HasPrefix(mimeType, "video/") {
			_ = response.Body.Close()
			transport.CloseIdleConnections()
			return nil, &Error{Code: "invalid_mime"}
		}
		body := &boundedBody{reader: io.MultiReader(bytes.NewReader(header[:n]), response.Body), body: response.Body, remaining: limits.MaxBytes, cancel: cancel, transport: transport}
		keep = true
		return &Download{Body: body, MIMEType: mimeType, FinalURL: parsed.String(), ContentLength: response.ContentLength}, nil
	}
}

func sniffMediaType(header []byte) string {
	// QuickTime's ISO media brand is not covered by net/http's sniff table.
	// Do not trust the remote Content-Type or a .mov filename to widen it.
	if len(header) >= 12 && bytes.Equal(header[4:8], []byte("ftyp")) && bytes.Equal(header[8:12], []byte("qt  ")) {
		return "video/quicktime"
	}
	return http.DetectContentType(header)
}

func safeNetworkError(ctx context.Context, code string) error {
	if err := ctx.Err(); err != nil {
		return &Error{Code: "timeout", cause: err}
	}
	return &Error{Code: code}
}

type boundedBody struct {
	reader    io.Reader
	body      io.ReadCloser
	remaining int64
	cancel    context.CancelFunc
	transport *http.Transport
	once      sync.Once
	closeErr  error
}

func (b *boundedBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if int64(len(p)) > b.remaining+1 {
		p = p[:b.remaining+1]
	}
	n, err := b.reader.Read(p)
	if int64(n) > b.remaining {
		allowed := int(b.remaining)
		b.remaining = 0
		return allowed, &Error{Code: "too_large"}
	}
	b.remaining -= int64(n)
	return n, err
}
func (b *boundedBody) Close() error {
	b.once.Do(func() { b.cancel(); b.closeErr = b.body.Close(); b.transport.CloseIdleConnections() })
	return b.closeErr
}

func validatedURL(raw string) (*url.URL, error) {
	if len(raw) == 0 || len(raw) > 4096 || strings.ContainsAny(raw, "\r\n\t\\") {
		return nil, &Error{Code: "unsafe_url"}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") || !publicHostname(u.Hostname()) {
		return nil, &Error{Code: "unsafe_url"}
	}
	return u, nil
}

func publicHostname(host string) bool {
	host = strings.ToLower(host)
	if len(host) > 253 || strings.HasSuffix(host, ".") || net.ParseIP(host) != nil {
		return false
	}
	for _, suffix := range []string{".local", ".localhost", ".internal", ".lan", ".home.arpa", ".test", ".invalid", ".example", ".onion"} {
		if strings.HasSuffix(host, suffix) {
			return false
		}
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	for _, r := range labels[len(labels)-1] {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return len(labels[len(labels)-1]) >= 2
}

var blockedNetworks = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"), netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("224.0.0.0/4"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001::/23"), netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"), netip.MustParsePrefix("3fff::/20"),
}

func publicAddress(ip netip.Addr) bool {
	if !ip.IsValid() || ip.Zone() != "" || ip.Is4In6() || !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	if ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip) {
		return false
	}
	for _, network := range blockedNetworks {
		if network.Contains(ip) {
			return false
		}
	}
	return true
}
