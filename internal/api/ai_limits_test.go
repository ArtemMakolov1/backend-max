package api

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/media"
	"maxpilot/backend/internal/openaiimg"
	"maxpilot/backend/internal/openairesearch"
	"maxpilot/backend/internal/store"
)

type quotaImageClient struct {
	mu       sync.Mutex
	calls    int
	generate func(context.Context, openaiimg.GenerateRequest) (openaiimg.Result, error)
}

func (f *quotaImageClient) Generate(ctx context.Context, request openaiimg.GenerateRequest) (openaiimg.Result, error) {
	f.mu.Lock()
	f.calls++
	generate := f.generate
	f.mu.Unlock()
	if generate != nil {
		return generate(ctx, request)
	}
	return openaiimg.Result{Bytes: quotaTestPNG(), MIMEType: "image/png", Model: "fake"}, nil
}

func (f *quotaImageClient) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestAIImageQuotaIsSharedByBothImageRoutesAndRejectsBeforeUpstream(t *testing.T) {
	t.Parallel()
	image := &quotaImageClient{}
	options := testAILimitOptions()
	options.ImagePerMinute = 1
	server, storage, rawHandler := newAIQuotaTestServer(t, image, nil, options, "quota-user")
	quotaNow := time.Now().UTC().Truncate(time.Minute)
	server.now = func() time.Time { return quotaNow }
	handler := withTestSession(t, storage, rawHandler, "quota-user")

	invalid := performJSONRequest(handler, http.MethodPost, "/api/v1/images/generate", `{"prompt":""}`)
	assertProblemCode(t, invalid, http.StatusBadRequest, "validation_error")
	if image.callCount() != 0 {
		t.Fatalf("invalid request called image upstream %d times", image.callCount())
	}

	first := performJSONRequest(handler, http.MethodPost, "/api/v1/images/generate", `{"prompt":"Первая картинка"}`)
	if first.Code != http.StatusCreated {
		t.Fatalf("first image status = %d, body=%s", first.Code, first.Body.String())
	}
	standaloneRejected := performJSONRequest(handler, http.MethodPost, "/api/v1/images/generate", `{"prompt":"Вторая картинка"}`)
	assertAI429(t, standaloneRejected, store.AILimitReasonMinute, "60")
	if image.callCount() != 1 {
		t.Fatalf("standalone quota rejection called image upstream; calls=%d", image.callCount())
	}
	post, err := storage.CreatePost(context.Background(), store.Post{
		UserID: "quota-user", Title: "Post", Content: "Body", Format: store.FormatMarkdown, Status: store.PostStatusDraft,
	})
	if err != nil {
		t.Fatal(err)
	}
	second := performJSONRequest(handler, http.MethodPost,
		"/api/v1/posts/"+formatInt64(post.ID)+"/generate-image", `{"prompt":"Обход через пост"}`)
	assertAI429(t, second, store.AILimitReasonMinute, "60")
	if image.callCount() != 1 {
		t.Fatalf("quota rejection called image upstream; calls=%d", image.callCount())
	}
}

func TestAIResearchQuotaRejectsBeforeUpstream(t *testing.T) {
	t.Parallel()
	research := &fakeResearchClient{result: openairesearch.Result{Topic: "Тема поста"}}
	options := testAILimitOptions()
	options.ResearchPerMinute = 1
	server, storage, rawHandler := newAIQuotaTestServer(t, nil, research, options, "research-quota-user")
	quotaNow := time.Now().UTC().Truncate(time.Minute).Add(30 * time.Second)
	server.now = func() time.Time { return quotaNow }
	handler := withTestSession(t, storage, rawHandler, "research-quota-user")
	body := `{"topic":"Тема поста","tone":"Деловой","format":"markdown","include_sources":false}`

	first := performJSONRequest(handler, http.MethodPost, "/api/v1/research/generate", body)
	if first.Code != http.StatusOK {
		t.Fatalf("first research status = %d, body=%s", first.Code, first.Body.String())
	}
	second := performJSONRequest(handler, http.MethodPost, "/api/v1/research/generate", body)
	assertAI429(t, second, store.AILimitReasonMinute, "30")
	research.mu.Lock()
	calls := len(research.requests)
	research.mu.Unlock()
	if calls != 1 {
		t.Fatalf("quota rejection called research upstream; calls=%d", calls)
	}
}

func TestAIFormattingAndResearchShareTheResearchQuota(t *testing.T) {
	t.Parallel()
	research := &fakeResearchClient{
		result:       openairesearch.Result{Topic: "Тема поста"},
		formatResult: openairesearch.FormatResult{Content: "# Текст поста"},
	}
	options := testAILimitOptions()
	options.ResearchPerMinute = 1
	server, storage, rawHandler := newAIQuotaTestServer(t, nil, research, options, "format-quota-user")
	quotaNow := time.Now().UTC().Truncate(time.Minute).Add(30 * time.Second)
	server.now = func() time.Time { return quotaNow }
	handler := withTestSession(t, storage, rawHandler, "format-quota-user")

	formatted := performJSONRequest(handler, http.MethodPost, "/api/v1/posts/format-content",
		`{"content":"Текст поста","format":"markdown"}`)
	if formatted.Code != http.StatusOK {
		t.Fatalf("format status = %d, body=%s", formatted.Code, formatted.Body.String())
	}
	researchRejected := performJSONRequest(handler, http.MethodPost, "/api/v1/research/generate",
		`{"topic":"Тема поста","tone":"Деловой","format":"markdown","include_sources":false}`)
	assertAI429(t, researchRejected, store.AILimitReasonMinute, "30")
	research.mu.Lock()
	defer research.mu.Unlock()
	if len(research.formatRequests) != 1 || len(research.requests) != 0 {
		t.Fatalf("quota upstream calls: format=%d research=%d", len(research.formatRequests), len(research.requests))
	}
}

func TestAIEndpointsReportUnavailableWithoutOpenAIClient(t *testing.T) {
	t.Parallel()
	options := testAILimitOptions()
	_, storage, rawHandler := newAIQuotaTestServer(t, nil, nil, options, "no-openai-user")
	handler := withTestSession(t, storage, rawHandler, "no-openai-user")

	image := performJSONRequest(handler, http.MethodPost, "/api/v1/images/generate", `{"prompt":"Картинка для поста"}`)
	assertProblemCode(t, image, http.StatusServiceUnavailable, "openai_not_configured")

	research := performJSONRequest(handler, http.MethodPost, "/api/v1/research/generate",
		`{"topic":"Тема поста","tone":"Деловой","format":"markdown","include_sources":false}`)
	assertProblemCode(t, research, http.StatusServiceUnavailable, "openai_research_not_configured")
}

func TestAIGlobalSemaphoreRejectsAcrossUsersAndOperationsWithoutWaiting(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	unblock := make(chan struct{})
	var startedOnce sync.Once
	image := &quotaImageClient{generate: func(ctx context.Context, _ openaiimg.GenerateRequest) (openaiimg.Result, error) {
		startedOnce.Do(func() { close(started) })
		select {
		case <-unblock:
			return openaiimg.Result{Bytes: quotaTestPNG(), MIMEType: "image/png"}, nil
		case <-ctx.Done():
			return openaiimg.Result{}, ctx.Err()
		}
	}}
	research := &fakeResearchClient{result: openairesearch.Result{Topic: "Тема поста"}}
	options := testAILimitOptions()
	options.GlobalMaxConcurrent = 1
	server, storage, rawHandler := newAIQuotaTestServer(t, image, research, options, "global-a", "global-b")
	quotaNow := time.Now().UTC().Truncate(time.Minute)
	server.now = func() time.Time { return quotaNow }
	firstHandler := withTestSession(t, storage, rawHandler, "global-a")
	secondHandler := withTestSession(t, storage, rawHandler, "global-b")

	firstResponse := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		firstResponse <- performJSONRequest(firstHandler, http.MethodPost, "/api/v1/images/generate", `{"prompt":"Долгая картинка"}`)
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("first image request did not reach fake upstream")
	}

	begin := time.Now()
	second := performJSONRequest(secondHandler, http.MethodPost, "/api/v1/research/generate",
		`{"topic":"Тема поста","tone":"Деловой","format":"markdown","include_sources":false}`)
	if elapsed := time.Since(begin); elapsed > time.Second {
		t.Fatalf("global semaphore rejection blocked for %s", elapsed)
	}
	assertAI429(t, second, store.AILimitReasonGlobal, "1")
	research.mu.Lock()
	researchCalls := len(research.requests)
	research.mu.Unlock()
	if researchCalls != 0 {
		t.Fatalf("global rejection called research upstream %d times", researchCalls)
	}

	close(unblock)
	select {
	case response := <-firstResponse:
		if response.Code != http.StatusCreated {
			t.Fatalf("first response status = %d, body=%s", response.Code, response.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first request did not finish after release")
	}
}

func TestAILeaseAndGlobalSlotReleaseAfterUpstreamFailure(t *testing.T) {
	t.Parallel()
	image := &quotaImageClient{}
	var attempt int
	var attemptMu sync.Mutex
	image.generate = func(_ context.Context, _ openaiimg.GenerateRequest) (openaiimg.Result, error) {
		attemptMu.Lock()
		defer attemptMu.Unlock()
		attempt++
		if attempt == 1 {
			return openaiimg.Result{}, errors.New("forced upstream failure")
		}
		return openaiimg.Result{Bytes: quotaTestPNG(), MIMEType: "image/png"}, nil
	}
	options := testAILimitOptions()
	server, storage, rawHandler := newAIQuotaTestServer(t, image, nil, options, "release-user")
	quotaNow := time.Now().UTC().Truncate(time.Minute)
	server.now = func() time.Time { return quotaNow }
	handler := withTestSession(t, storage, rawHandler, "release-user")

	first := performJSONRequest(handler, http.MethodPost, "/api/v1/images/generate", `{"prompt":"Ошибка"}`)
	if first.Code != http.StatusInternalServerError {
		t.Fatalf("first status = %d, body=%s", first.Code, first.Body.String())
	}
	second := performJSONRequest(handler, http.MethodPost, "/api/v1/images/generate", `{"prompt":"Повтор"}`)
	if second.Code != http.StatusCreated {
		t.Fatalf("request after upstream failure status = %d, body=%s", second.Code, second.Body.String())
	}
	if image.callCount() != 2 {
		t.Fatalf("image calls = %d, want 2", image.callCount())
	}
}

func TestAILimitOptionsRequireLeaseLongerThanHandler(t *testing.T) {
	t.Parallel()
	options := testAILimitOptions()
	options.LeaseTTL = AIHandlerTimeout
	if err := options.Validate(); err == nil {
		t.Fatal("lease equal to handler timeout passed validation")
	}
	options.LeaseTTL = AIHandlerTimeout + time.Second
	if err := options.Validate(); err != nil {
		t.Fatalf("lease longer than handler rejected: %v", err)
	}
	if retryAfterSeconds(1500*time.Millisecond) != 2 || retryAfterSeconds(0) != 1 {
		t.Fatal("Retry-After rounding is not safe")
	}
}

func newAIQuotaTestServer(
	t *testing.T,
	image app.ImageClient,
	research app.ResearchClient,
	options AILimitOptions,
	userIDs ...string,
) (*Server, *store.Store, http.Handler) {
	t.Helper()
	storage, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "ai-quota.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	for _, userID := range userIDs {
		if err := storage.UpsertUser(context.Background(), store.User{ID: userID, DisplayName: userID}); err != nil {
			t.Fatal(err)
		}
	}
	mediaStore, err := media.New(t.TempDir(), "http://localhost:8080")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	application := app.New(storage, mediaStore, nil, image, research, logger)
	server := New(application, logger, "http://localhost:4321", "webhook-secret", AuthOptions{
		YandexClient: &fakeYandexOAuth{}, AILimits: &options,
	})
	return server, storage, server.Handler()
}

func testAILimitOptions() AILimitOptions {
	return AILimitOptions{
		GlobalMaxConcurrent: 2,
		UserMaxConcurrent:   1,
		ImagePerMinute:      10,
		ImagePerDay:         100,
		ResearchPerMinute:   10,
		ResearchPerDay:      100,
		LeaseTTL:            4 * time.Minute,
	}
}

func assertAI429(t *testing.T, response *httptest.ResponseRecorder, reason, retryAfter string) {
	t.Helper()
	assertProblemCode(t, response, http.StatusTooManyRequests, "ai_rate_limited")
	if got := response.Header().Get("Retry-After"); got != retryAfter {
		t.Fatalf("Retry-After = %q, want %q; body=%s", got, retryAfter, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" || !strings.Contains(response.Body.String(), `"reason":"`+reason+`"`) {
		t.Fatalf("AI 429 headers/body = %#v %s", response.Header(), response.Body.String())
	}
}

func formatInt64(value int64) string {
	return strconv.FormatInt(value, 10)
}

func quotaTestPNG() []byte {
	data, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	if err != nil {
		panic(err)
	}
	return data
}
