package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/media"
	"maxpilot/backend/internal/store"
)

type maxAuthWebhookFake struct {
	*claimWebhookMAX
	hmacKey        string
	contactRuns    int
	contactUser    string
	comparisonCode string
	confirmPayload string
}

func (f *maxAuthWebhookFake) SendAuthContactRequest(_ context.Context, userID, comparisonCode, confirmPayload string) error {
	f.contactRuns++
	f.contactUser, f.comparisonCode, f.confirmPayload = userID, comparisonCode, confirmPayload
	return nil
}

func (f *maxAuthWebhookFake) VerifyContactHMAC(vcfInfo, proof string) bool {
	mac := hmac.New(sha256.New, []byte(f.hmacKey))
	_, _ = mac.Write([]byte(strings.ReplaceAll(vcfInfo, `\r\n`, "\r\n")))
	want := hex.EncodeToString(mac.Sum(nil))
	return constantTimeEqual(strings.ToLower(proof), want)
}

func TestMAXAuthDeviceFlowCreatesProviderNeutralSession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := store.Open(ctx, filepath.Join(t.TempDir(), "max-auth-api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	mediaStore, err := media.New(t.TempDir(), "http://localhost:8080")
	if err != nil {
		t.Fatal(err)
	}
	fake := &maxAuthWebhookFake{claimWebhookMAX: &claimWebhookMAX{}, hmacKey: "shared-bot-token"}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	now := time.Date(2026, time.July, 15, 9, 0, 0, 0, time.UTC)
	server := New(app.New(storage, mediaStore, fake, nil, nil, logger), logger,
		"http://localhost:4321", "webhook-secret", AuthOptions{YandexClient: &fakeYandexOAuth{}, SecureCookies: true})
	server.now = func() time.Time { return now }
	handler := server.Handler()

	wrongOriginStart := httptest.NewRequest(http.MethodPost, "/api/v1/auth/max/start", strings.NewReader(
		`{"return_to":"/app/","terms_accepted":true,"personal_data_accepted":true}`))
	wrongOriginStart.Header.Set("Content-Type", "application/json")
	wrongOriginStart.Header.Set("Origin", "https://evil.example")
	wrongOriginStartResponse := httptest.NewRecorder()
	handler.ServeHTTP(wrongOriginStartResponse, wrongOriginStart)
	if wrongOriginStartResponse.Code != http.StatusForbidden {
		t.Fatalf("wrong origin start=%d %s", wrongOriginStartResponse.Code, wrongOriginStartResponse.Body.String())
	}

	start := httptest.NewRequest(http.MethodPost, "/api/v1/auth/max/start", strings.NewReader(
		`{"return_to":"//evil.example/steal","terms_accepted":true,"personal_data_accepted":true}`))
	start.Header.Set("Content-Type", "application/json")
	start.Header.Set("Origin", "http://localhost:4321")
	startResponse := httptest.NewRecorder()
	handler.ServeHTTP(startResponse, start)
	if startResponse.Code != http.StatusCreated {
		t.Fatalf("start=%d %s", startResponse.Code, startResponse.Body.String())
	}
	var started maxAuthPublicAttempt
	if err := json.Unmarshal(startResponse.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	if started.Status != store.MAXAuthAttemptPending || started.ReturnTo != "/app/" || started.ComparisonCode == "" {
		t.Fatalf("start payload=%#v", started)
	}
	attemptCookie := responseCookie(t, startResponse, maxAuthAttemptCookieName)
	if !attemptCookie.HttpOnly || !attemptCookie.Secure || attemptCookie.SameSite != http.SameSiteStrictMode ||
		attemptCookie.Path != "/api/v1/auth/max" || attemptCookie.MaxAge <= 0 {
		t.Fatalf("attempt cookie=%#v", attemptCookie)
	}
	botURL, err := url.Parse(started.BotURL)
	if err != nil {
		t.Fatal(err)
	}
	deepPayload := botURL.Query().Get("start")
	if !strings.HasPrefix(deepPayload, "auth_") {
		t.Fatalf("bot start payload=%q", deepPayload)
	}
	botStarted := fmt.Sprintf(`{"update_type":"bot_started","timestamp":%d,"payload":%q,"user":{"user_id":777}}`,
		now.Add(time.Second).UnixMilli(), deepPayload)
	if response := performMAXWebhook(handler, botStarted); response.Code != http.StatusOK {
		t.Fatalf("bot_started=%d %s", response.Code, response.Body.String())
	}
	if fake.contactRuns != 1 || fake.contactUser != "777" || fake.comparisonCode != started.ComparisonCode {
		t.Fatalf("contact request fake=%#v", fake)
	}
	if !strings.HasPrefix(fake.confirmPayload, maxAuthConfirmPayloadPrefix) {
		t.Fatalf("attempt-bound callback payload=%q", fake.confirmPayload)
	}
	if response := performMAXWebhook(handler, botStarted); response.Code != http.StatusOK || fake.contactRuns != 1 {
		t.Fatalf("bot_started replay=%d sends=%d", response.Code, fake.contactRuns)
	}

	vcf := "BEGIN:VCARD\r\nVERSION:3.0\r\nTEL;TYPE=cell:79990000000\r\nFN:Artem Makolov\r\nEND:VCARD\r\n"
	mac := hmac.New(sha256.New, []byte(fake.hmacKey))
	_, _ = mac.Write([]byte(vcf))
	proof := hex.EncodeToString(mac.Sum(nil))
	wrongContact := fmt.Sprintf(`{"update_type":"message_created","timestamp":%d,"message":{"sender":{"user_id":999},"recipient":{"chat_type":"dialog"},"body":{"mid":"mid-wrong","attachments":[{"type":"contact","payload":{"vcf_info":%q,"hash":%q,"max_info":{"user_id":777}}}]}}}`,
		now.Add(2*time.Second).UnixMilli(), vcf, proof)
	if response := performMAXWebhook(handler, wrongContact); response.Code != http.StatusOK {
		t.Fatalf("mismatched contact=%d", response.Code)
	}
	forwardedContact := fmt.Sprintf(`{"update_type":"message_created","timestamp":%d,"message":{"sender":{"user_id":777},"recipient":{"chat_type":"dialog"},"body":{"mid":"mid-forwarded","attachments":[{"type":"contact","payload":{"vcf_info":%q,"max_info":{"user_id":777}}}]}}}`,
		now.Add(2*time.Second).UnixMilli(), vcf)
	if response := performMAXWebhook(handler, forwardedContact); response.Code != http.StatusOK {
		t.Fatalf("forwarded unsigned contact=%d", response.Code)
	}
	pending := httptest.NewRequest(http.MethodPost, "/api/v1/auth/max/"+started.RequestID+"/complete", nil)
	pending.Header.Set("Origin", "http://localhost:4321")
	pending.AddCookie(attemptCookie)
	pendingResponse := httptest.NewRecorder()
	handler.ServeHTTP(pendingResponse, pending)
	if pendingResponse.Code != http.StatusOK || !strings.Contains(pendingResponse.Body.String(), `"status":"awaiting_contact"`) {
		t.Fatalf("pending=%d %s", pendingResponse.Code, pendingResponse.Body.String())
	}

	contact := fmt.Sprintf(`{"update_type":"message_created","timestamp":%d,"message":{"sender":{"user_id":777},"recipient":{"chat_type":"dialog"},"body":{"mid":"mid-verified","attachments":[{"type":"contact","payload":{"vcf_info":%q,"hash":%q,"max_info":{"user_id":777,"first_name":"Artem","last_name":"Makolov","username":"makolov99","avatar_url":"https://cdn.max.ru/avatar.png"}}}]}}}`,
		now.Add(3*time.Second).UnixMilli(), vcf, proof)
	if response := performMAXWebhook(handler, contact); response.Code != http.StatusOK {
		t.Fatalf("contact=%d body=%s", response.Code, response.Body.String())
	}
	if response := performMAXWebhook(handler, contact); response.Code != http.StatusOK {
		t.Fatalf("contact replay=%d", response.Code)
	}
	stillAwaiting := httptest.NewRequest(http.MethodPost, "/api/v1/auth/max/"+started.RequestID+"/complete", nil)
	stillAwaiting.Header.Set("Origin", "http://localhost:4321")
	stillAwaiting.AddCookie(attemptCookie)
	stillAwaitingResponse := httptest.NewRecorder()
	handler.ServeHTTP(stillAwaitingResponse, stillAwaiting)
	if stillAwaitingResponse.Code != http.StatusOK || !strings.Contains(stillAwaitingResponse.Body.String(), `"status":"awaiting_contact"`) {
		t.Fatalf("contact alone completed auth=%d %s", stillAwaitingResponse.Code, stillAwaitingResponse.Body.String())
	}
	confirmation := fmt.Sprintf(`{"update_type":"message_callback","timestamp":%d,"callback":{"callback_id":"callback-auth","payload":%q,"user":{"user_id":777}}}`,
		now.Add(4*time.Second).UnixMilli(), fake.confirmPayload)
	if response := performMAXWebhook(handler, confirmation); response.Code != http.StatusOK {
		t.Fatalf("contact confirmation=%d %s", response.Code, response.Body.String())
	}
	if len(fake.callbackAnswers) != 1 || !strings.Contains(fake.callbackAnswers[0], "Вход подтверждён") ||
		len(fake.callbackMessages) != 1 || !strings.Contains(fake.callbackMessages[0], "Вход подтверждён") {
		t.Fatalf("callback answers=%#v messages=%#v", fake.callbackAnswers, fake.callbackMessages)
	}

	wrongOrigin := httptest.NewRequest(http.MethodPost, "/api/v1/auth/max/"+started.RequestID+"/complete", nil)
	wrongOrigin.Header.Set("Origin", "https://evil.example")
	wrongOrigin.AddCookie(attemptCookie)
	wrongOriginResponse := httptest.NewRecorder()
	handler.ServeHTTP(wrongOriginResponse, wrongOrigin)
	if wrongOriginResponse.Code != http.StatusForbidden {
		t.Fatalf("wrong origin=%d", wrongOriginResponse.Code)
	}
	complete := httptest.NewRequest(http.MethodPost, "/api/v1/auth/max/"+started.RequestID+"/complete", nil)
	complete.Header.Set("Origin", "http://localhost:4321")
	complete.AddCookie(attemptCookie)
	completeResponse := httptest.NewRecorder()
	handler.ServeHTTP(completeResponse, complete)
	if completeResponse.Code != http.StatusOK || !strings.Contains(completeResponse.Body.String(), `"status":"authenticated"`) ||
		!strings.Contains(completeResponse.Body.String(), `"auth_method":"max"`) ||
		!strings.Contains(completeResponse.Body.String(), `"is_new_user":true`) {
		t.Fatalf("complete=%d %s", completeResponse.Code, completeResponse.Body.String())
	}
	sessionCookie := responseCookie(t, completeResponse, sessionCookieName)
	server.yandexAllowed = map[string]struct{}{"different-yandex-user": {}}
	protected := httptest.NewRequest(http.MethodGet, "/api/v1/posts", nil)
	protected.AddCookie(sessionCookie)
	protectedResponse := httptest.NewRecorder()
	handler.ServeHTTP(protectedResponse, protected)
	if protectedResponse.Code != http.StatusOK {
		t.Fatalf("MAX session was subjected to Yandex allowlist: %d %s", protectedResponse.Code, protectedResponse.Body.String())
	}
	observability := httptest.NewRequest(http.MethodGet, "/api/v1/observability/auth", nil)
	observability.AddCookie(sessionCookie)
	observabilityResponse := httptest.NewRecorder()
	handler.ServeHTTP(observabilityResponse, observability)
	if observabilityResponse.Code != http.StatusSeeOther {
		t.Fatalf("MAX session observability status=%d", observabilityResponse.Code)
	}
}

func TestMAXAuthCancelAndExpiryAreNormalBrowserStates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := store.Open(ctx, filepath.Join(t.TempDir(), "max-auth-cancel-api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	mediaStore, err := media.New(t.TempDir(), "http://localhost:8080")
	if err != nil {
		t.Fatal(err)
	}
	fake := &maxAuthWebhookFake{claimWebhookMAX: &claimWebhookMAX{}, hmacKey: "shared-bot-token"}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	now := time.Date(2026, time.July, 15, 10, 0, 0, 0, time.UTC)
	server := New(app.New(storage, mediaStore, fake, nil, nil, logger), logger,
		"http://localhost:4321", "webhook-secret", AuthOptions{YandexClient: &fakeYandexOAuth{}, SecureCookies: true})
	server.now = func() time.Time { return now }
	handler := server.Handler()

	startAttempt := func() (maxAuthPublicAttempt, *http.Cookie) {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/max/start", strings.NewReader(
			`{"return_to":"/app/#/posts","terms_accepted":true,"personal_data_accepted":true}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", "http://localhost:4321")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusCreated {
			t.Fatalf("start=%d %s", response.Code, response.Body.String())
		}
		var attempt maxAuthPublicAttempt
		if err := json.Unmarshal(response.Body.Bytes(), &attempt); err != nil {
			t.Fatal(err)
		}
		return attempt, responseCookie(t, response, maxAuthAttemptCookieName)
	}

	cancelAttempt, cancelCookie := startAttempt()
	wrongCancel := httptest.NewRequest(http.MethodDelete, "/api/v1/auth/max/"+cancelAttempt.RequestID, nil)
	wrongCancel.Header.Set("Origin", "https://evil.example")
	wrongCancel.AddCookie(cancelCookie)
	wrongCancelResponse := httptest.NewRecorder()
	handler.ServeHTTP(wrongCancelResponse, wrongCancel)
	if wrongCancelResponse.Code != http.StatusForbidden {
		t.Fatalf("wrong origin cancel=%d", wrongCancelResponse.Code)
	}
	if _, err := storage.GetMAXAuthAttemptForBrowser(ctx, cancelAttempt.RequestID, sha256Hex(cancelCookie.Value), now); err != nil {
		t.Fatalf("wrong-origin cancel mutated attempt: %v", err)
	}

	cancel := httptest.NewRequest(http.MethodDelete, "/api/v1/auth/max/"+cancelAttempt.RequestID, nil)
	cancel.Header.Set("Origin", "http://localhost:4321")
	cancel.AddCookie(cancelCookie)
	cancelResponse := httptest.NewRecorder()
	handler.ServeHTTP(cancelResponse, cancel)
	if cancelResponse.Code != http.StatusNoContent {
		t.Fatalf("cancel=%d %s", cancelResponse.Code, cancelResponse.Body.String())
	}
	if _, err := storage.GetMAXAuthAttemptForBrowser(ctx, cancelAttempt.RequestID, sha256Hex(cancelCookie.Value), now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("canceled attempt lookup=%v, want not found", err)
	}

	expiringAttempt, expiringCookie := startAttempt()
	now = expiringAttempt.ExpiresAt
	complete := httptest.NewRequest(http.MethodPost, "/api/v1/auth/max/"+expiringAttempt.RequestID+"/complete", nil)
	complete.Header.Set("Origin", "http://localhost:4321")
	complete.AddCookie(expiringCookie)
	completeResponse := httptest.NewRecorder()
	handler.ServeHTTP(completeResponse, complete)
	if completeResponse.Code != http.StatusOK || !strings.Contains(completeResponse.Body.String(), `"status":"expired"`) {
		t.Fatalf("expired complete=%d %s", completeResponse.Code, completeResponse.Body.String())
	}
	if _, err := storage.GetMAXAuthAttemptForBrowser(ctx, expiringAttempt.RequestID, sha256Hex(expiringCookie.Value), now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired attempt was retained: %v", err)
	}
}
