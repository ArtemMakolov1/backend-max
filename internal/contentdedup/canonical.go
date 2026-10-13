// Package contentdedup contains provider-independent publication identity rules.
package contentdedup

import (
	"net/url"
	"strings"
)

// CanonicalSourceURL removes tracking parameters, fragments and superficial
// spelling differences without discarding identity-bearing article queries.
func CanonicalSourceURL(raw string) string {
	if len(raw) > 4096 {
		return ""
	}
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return ""
	}
	host := strings.TrimPrefix(strings.ToLower(strings.TrimSuffix(parsed.Hostname(), ".")), "www.")
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	port := parsed.Port()
	if port != "" && port != "80" && port != "443" {
		host = host + ":" + port
	}
	parsed.Scheme, parsed.Host, parsed.Fragment = "https", host, ""
	path := strings.TrimRight(parsed.EscapedPath(), "/")
	parsed.Path, _ = url.PathUnescape(path)
	parsed.RawPath = path
	query := parsed.Query()
	for key := range query {
		lower := strings.ToLower(key)
		if strings.HasPrefix(lower, "utm_") || lower == "fbclid" || lower == "gclid" || lower == "yclid" || lower == "ysclid" || lower == "mc_cid" || lower == "mc_eid" || lower == "roistat_visit" {
			query.Del(key)
		}
	}
	parsed.RawQuery = query.Encode()
	parsed.ForceQuery = false
	return parsed.String()
}
