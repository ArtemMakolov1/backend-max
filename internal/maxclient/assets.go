package maxclient

import (
	"net/url"
	"strings"
)

const maxAssetURLLimit = 4096

// SafeAssetURL returns a browser-safe MAX asset URL or an empty string.
// Channel icons are displayed directly by the browser; keeping the allowlist
// here also prevents an alternative MAXClient implementation from persisting
// an arbitrary tracking or credential-bearing URL.
func SafeAssetURL(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" || len(value) > maxAssetURLLimit || strings.IndexFunc(value, func(r rune) bool {
		return r < 0x20 || r == 0x7f
	}) >= 0 {
		return ""
	}

	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Opaque != "" || parsed.User != nil ||
		parsed.Port() != "" || parsed.Fragment != "" || parsed.Path == "" {
		return ""
	}

	host := strings.ToLower(parsed.Hostname())
	if !officialMAXAssetHost(host) {
		return ""
	}
	return parsed.String()
}

func officialMAXAssetHost(host string) bool {
	return host == "max.ru" || strings.HasSuffix(host, ".max.ru") ||
		host == "oneme.ru" || strings.HasSuffix(host, ".oneme.ru")
}
