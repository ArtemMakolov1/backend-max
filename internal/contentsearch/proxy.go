package contentsearch

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var proxyHostname = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$`)

// ValidateExaProxyURL shares the deployment configuration contract with the
// client. It never returns the URL or a parser error containing credentials.
func ValidateExaProxyURL(raw string) error {
	_, err := parseExaProxyURL(raw)
	return err
}

func parseExaProxyURL(raw string) (*url.URL, error) {
	invalid := func() (*url.URL, error) {
		return nil, errors.New("EXA_PROXY_URL must be an authority-only HTTP(S) proxy URL")
	}
	// Deployment env files must not interpolate or reinterpret proxy userinfo;
	// reserved credential characters must be URI-encoded (for example %24).
	if strings.ContainsAny(raw, "\r\n\t$'\"\\") || len(raw) > 4096 {
		return invalid()
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	for _, r := range raw {
		if r <= 32 || r > 126 {
			return invalid()
		}
	}
	parsed, err := url.Parse(raw)
	if err != nil || strings.Contains(raw, "#") || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.Host == "" || parsed.Opaque != "" || parsed.Path != "" || parsed.RawPath != "" ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" {
		return invalid()
	}
	host := parsed.Hostname()
	if len(host) > 253 || (net.ParseIP(host) == nil && !proxyHostname.MatchString(host)) || strings.HasSuffix(parsed.Host, ":") {
		return invalid()
	}
	if port := parsed.Port(); port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return invalid()
		}
	}
	if parsed.User != nil {
		if parsed.User.Username() == "" {
			return invalid()
		}
		password, _ := parsed.User.Password()
		for _, value := range []string{parsed.User.Username(), password} {
			for _, r := range value {
				if r < 32 || r > 126 {
					return invalid()
				}
			}
		}
	}
	return parsed, nil
}

func exaProxy(raw string) (func(*http.Request) (*url.URL, error), error) {
	proxy, err := parseExaProxyURL(raw)
	if err != nil || proxy == nil {
		return nil, err
	}
	return func(request *http.Request) (*url.URL, error) {
		if request == nil || request.URL == nil || request.URL.String() != exaEndpoint {
			return nil, nil
		}
		// net/http puts URL userinfo in Proxy-Authorization for CONNECT, outside
		// the end-to-end HTTPS stream. Exa's API key remains inside that stream.
		return proxy, nil
	}, nil
}
