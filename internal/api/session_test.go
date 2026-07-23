package api

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"maxpilot/backend/internal/store"
)

var testSessionSequence atomic.Uint64

type credentialedTestHandler struct {
	next   http.Handler
	cookie *http.Cookie
}

func (h credentialedTestHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.AddCookie(h.cookie)
	if isUnsafeMethod(r.Method) && r.Header.Get("Origin") == "" {
		r.Header.Set("Origin", "http://localhost:4321")
	}
	h.next.ServeHTTP(w, r)
}

func withTestSession(t *testing.T, storage *store.Store, handler http.Handler, userID string) http.Handler {
	t.Helper()
	raw := "test-session-" + userID + "-" + t.Name() + "-" +
		strconv.FormatUint(testSessionSequence.Add(1), 10)
	digest := sha256.Sum256([]byte(raw))
	now := time.Now().UTC()
	if err := storage.CreateAuthSession(t.Context(), store.AuthSession{
		TokenHash: hex.EncodeToString(digest[:]), YandexUserID: userID, Login: userID,
		DisplayName: userID, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	return credentialedTestHandler{next: handler, cookie: &http.Cookie{
		Name: sessionCookieName, Value: raw, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	}}
}
