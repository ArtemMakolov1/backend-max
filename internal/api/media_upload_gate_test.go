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
	gate := newMediaUploadGate(8)
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
	if _, ok := gate.tryAcquire("user-a"); ok {
		t.Fatal("second upload for the same user acquired a slot")
	}
	gate.mu.Lock()
	trackedUsers := len(gate.users)
	gate.mu.Unlock()
	if trackedUsers != cap(gate.global) {
		t.Fatalf("tracked users = %d, want bounded capacity %d", trackedUsers, cap(gate.global))
	}
	for _, release := range releases {
		release()
		release() // Release is deliberately idempotent.
	}
	if len(gate.global) != 0 {
		t.Fatalf("global slots after release = %d, want 0", len(gate.global))
	}
}

func TestParallelMediaUploadsAreRejectedBeforeBodyRead(t *testing.T) {
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

	secondBody := &countedUploadBody{}
	secondRequest := httptest.NewRequest(http.MethodPost, "/api/v1/media", secondBody)
	secondRequest.Header.Set("Content-Type", "multipart/form-data; boundary=maxposty-test")
	secondResponse := httptest.NewRecorder()
	handler.ServeHTTP(secondResponse, secondRequest)
	if secondResponse.Code != http.StatusTooManyRequests {
		close(blockedBody.release)
		t.Fatalf("parallel upload status = %d, want 429; body=%s", secondResponse.Code, secondResponse.Body.String())
	}
	if secondBody.reads.Load() != 0 {
		close(blockedBody.release)
		t.Fatalf("rejected upload body was read %d times", secondBody.reads.Load())
	}
	if secondResponse.Header().Get("Retry-After") != "1" ||
		!strings.Contains(secondResponse.Body.String(), `"code":"media_upload_busy"`) {
		close(blockedBody.release)
		t.Fatalf("parallel upload response is not retryable/friendly: headers=%v body=%s",
			secondResponse.Header(), secondResponse.Body.String())
	}

	close(blockedBody.release)
	select {
	case <-firstDone:
	case <-time.After(5 * time.Second):
		t.Fatal("first upload did not finish after its body was released")
	}

	thirdBody := &countedUploadBody{}
	thirdRequest := httptest.NewRequest(http.MethodPost, "/api/v1/media", thirdBody)
	thirdRequest.Header.Set("Content-Type", "multipart/form-data; boundary=maxposty-test")
	thirdResponse := httptest.NewRecorder()
	handler.ServeHTTP(thirdResponse, thirdRequest)
	if thirdResponse.Code == http.StatusTooManyRequests || thirdBody.reads.Load() == 0 {
		t.Fatalf("upload slot was not released: status=%d reads=%d body=%s",
			thirdResponse.Code, thirdBody.reads.Load(), thirdResponse.Body.String())
	}
}
