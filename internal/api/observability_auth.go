package api

import (
	"net/http"
	"strings"
)

// observabilityAuth is used by Caddy's forward_auth before proxying Grafana.
// It trusts only the existing Yandex session and a separate monitoring-admin
// allowlist; client-supplied auth-proxy headers are stripped at the edge.
func (s *Server) observabilityAuth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	principal, ok := s.authenticate(r)
	if !ok || principal.Method != "yandex" || principal.User == nil || principal.User.ID == "" {
		http.Redirect(w, r, "/app/", http.StatusSeeOther)
		return
	}
	identity, allowed := s.observabilityIdentity(principal.User)
	if !allowed {
		s.problem(w, http.StatusForbidden, "observability_forbidden", "Monitoring access is restricted", nil)
		return
	}
	s.touchUserActivity(r.Context(), principal.User.ID)
	w.Header().Set("X-WEBAUTH-USER", identity)
	// Provisioned dashboards are read-only. Viewer is sufficient and prevents
	// a stolen application session from administering Grafana or its data sources.
	w.Header().Set("X-WEBAUTH-ROLE", "Viewer")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) observabilityIdentity(user *authUser) (string, bool) {
	if user == nil {
		return "", false
	}
	for _, candidate := range []string{user.Login, user.ID} {
		normalized := strings.ToLower(strings.TrimSpace(candidate))
		if _, ok := s.observabilityAdmins[normalized]; ok && safeAuthProxyIdentity(normalized) {
			return normalized, true
		}
	}
	return "", false
}

func safeAuthProxyIdentity(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') ||
			char == '.' || char == '_' || char == '+' || char == '-' {
			continue
		}
		return false
	}
	return true
}
