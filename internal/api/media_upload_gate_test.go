package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/media"
	"maxpilot/backend/internal/store"
)

type blockingUploadBody struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingUploadBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.release
	return 0, io.EOF
}

type countedUploadBody struct {
	reads atomic.Int64
}

func (b *countedUploadBody) Read([]byte) (int, error) {
	b.reads.Add(1)
	return 0, io.EOF
}

func TestMediaUploadGateHasBoundedState(t *testing.T) {
	t.Parallel()
	gate := newMediaUploadGate(8, 2)
	releases := make([]func(), 0, 8)
	for index := 0; index < 8; index++ {
		release, ok := gate.tryAcquire("user-" + string(rune('a'+index)))
		if !ok {
			t.Fatalf("acquire slot %d failed", index)
		}
		releases = append(releases, release)
	}
	if _, ok := gate.tryAcquire("overflow"); ok {
		t.Fatal("ninth global upload acquired a slot")
	}
	// Free a global slot, then prove the per-user limit allows a two-file
	// concurrent batch but not an unbounded third upload.
	releases[len(releases)-1]()
	releases = releases[:len(releases)-1]
	secondUserA, ok := gate.tryAcquire("user-a")
	if !ok {
		t.Fatal("second concurrent upload for the same user was rejected")
	}
	if _, ok := gate.tryAcquire("user-a"); ok {
		t.Fatal("third upload for the same user acquired a slot")
	}
	gate.mu.Lock()
	trackedUsers := len(gate.users)
	userAUploads := gate.users["user-a"]
	gate.mu.Unlock()
	if trackedUsers > cap(gate.global) || userAUploads != 2 {
		t.Fatalf("tracked users = %d, user-a uploads = %d; want bounded state and two uploads", trackedUsers, userAUploads)
	}
	for _, release := range releases {
		release()
		release() // Release is deliberately idempotent.
	}
	secondUserA()
	secondUserA()
	if len(gate.global) != 0 {
		t.Fatalf("global slots after release = %d, want 0", len(gate.global))
	}
}

func TestThirdParallelMediaUploadIsRejectedBeforeBodyRead(t *testing.T) {
	ctx := context.Background()
	storage, err := store.Open(ctx, filepath.Join(t.TempDir(), "media-upload-gate.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	if err := storage.UpsertUser(ctx, store.User{ID: "upload-owner", Login: "upload-owner"}); err != nil {
		t.Fatal(err)
	}
	mediaStore, err := media.New(t.TempDir(), "http://localhost:8080")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := New(app.New(storage, mediaStore, nil, nil, nil, logger), logger,
		"http://localhost:4321", "webhook-secret", AuthOptions{YandexClient: &fakeYandexOAuth{}})
	handler := withTestSession(t, storage, server.Handler(), "upload-owner")

	blockedBody := &blockingUploadBody{started: make(chan struct{}), release: make(chan struct{})}
	firstRequest := httptest.NewRequest(http.MethodPost, "/api/v1/media", blockedBody)
	firstRequest.Header.Set("Content-Type", "multipart/form-data; boundary=maxposty-test")
	firstResponse := httptest.NewRecorder()
	firstDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(firstResponse, firstRequest)
		close(firstDone)
	}()
	select {
	case <-blockedBody.started:
	case <-time.After(5 * time.Second):
		close(blockedBody.release)
		t.Fatal("first upload did not start reading its body")
	}

	secondBlockedBody := &blockingUploadBody{started: make(chan struct{}), release: make(chan struct{})}
	secondRequest := httptest.NewRequest(http.MethodPost, "/api/v1/media", secondBlockedBody)
	secondRequest.Header.Set("Content-Type", "multipart/form-data; boundary=maxposty-test")
	secondResponse := httptest.NewRecorder()
	secondDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(secondResponse, secondRequest)
		close(secondDone)
	}()
	select {
	case <-secondBlockedBody.started:
	case <-time.After(5 * time.Second):
		close(blockedBody.release)
		close(secondBlockedBody.release)
		t.Fatal("second concurrent upload did not start reading its body")
	}

	thirdBody := &countedUploadBody{}
	thirdRequest := httptest.NewRequest(http.MethodPost, "/api/v1/media", thirdBody)
	thirdRequest.Header.Set("Content-Type", "multipart/form-data; boundary=maxposty-test")
	thirdResponse := httptest.NewRecorder()
	handler.ServeHTTP(thirdResponse, thirdRequest)
	if thirdResponse.Code != http.StatusTooManyRequests {
		close(blockedBody.release)
		close(secondBlockedBody.release)
		t.Fatalf("third upload status = %d, want 429; body=%s", thirdResponse.Code, thirdResponse.Body.String())
	}
	if thirdBody.reads.Load() != 0 {
		close(blockedBody.release)
		close(secondBlockedBody.release)
		t.Fatalf("rejected third upload body was read %d times", thirdBody.reads.Load())
	}
	if thirdResponse.Header().Get("Retry-After") != "1" ||
		!strings.Contains(thirdResponse.Body.String(), `"code":"media_upload_busy"`) {
		close(blockedBody.release)
		close(secondBlockedBody.release)
		t.Fatalf("third upload response is not retryable/friendly: headers=%v body=%s",
			thirdResponse.Header(), thirdResponse.Body.String())
	}

	close(blockedBody.release)
	close(secondBlockedBody.release)
	select {
	case <-firstDone:
	case <-time.After(5 * time.Second):
		t.Fatal("first upload did not finish after its body was released")
	}
	select {
	case <-secondDone:
	case <-time.After(5 * time.Second):
		t.Fatal("second upload did not finish after its body was released")
	}

	fourthBody := &countedUploadBody{}
	fourthRequest := httptest.NewRequest(http.MethodPost, "/api/v1/media", fourthBody)
	fourthRequest.Header.Set("Content-Type", "multipart/form-data; boundary=maxposty-test")
	fourthResponse := httptest.NewRecorder()
	handler.ServeHTTP(fourthResponse, fourthRequest)
	if fourthResponse.Code == http.StatusTooManyRequests || fourthBody.reads.Load() == 0 {
		t.Fatalf("upload slot was not released: status=%d reads=%d body=%s",
			fourthResponse.Code, fourthBody.reads.Load(), fourthResponse.Body.String())
	}
}
