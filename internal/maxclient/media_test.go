package maxclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestUploadVideoStreamsMultipartAndUsesReservationToken(t *testing.T) {
	t.Parallel()

	firstChunkSeen := make(chan struct{})
	releaseRest := make(chan struct{})
	uploadServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.TLS == nil {
			t.Errorf("unexpected upload request: %s TLS=%v", r.Method, r.TLS != nil)
		}
		if r.ContentLength != -1 {
			t.Errorf("Content-Length = %d, want streaming request", r.ContentLength)
		}
		if !strings.Contains(strings.Join(r.TransferEncoding, ","), "chunked") {
			t.Errorf("Transfer-Encoding = %v, want chunked", r.TransferEncoding)
		}
		if authorization := r.Header.Get("Authorization"); authorization != "" {
			t.Errorf("upload Authorization = %q, want empty", authorization)
		}

		reader, err := r.MultipartReader()
		if err != nil {
			t.Errorf("MultipartReader() error = %v", err)
			return
		}
		part, err := reader.NextPart()
		if err != nil {
			t.Errorf("NextPart() error = %v", err)
			return
		}
		defer func() { _ = part.Close() }()
		if part.FormName() != "data" || part.FileName() != "clip.mp4" {
			t.Errorf("multipart part = %q %q", part.FormName(), part.FileName())
		}

		first := make([]byte, len("first"))
		if _, err := io.ReadFull(part, first); err != nil {
			t.Errorf("read first streamed bytes: %v", err)
			return
		}
		if string(first) != "first" {
			t.Errorf("first streamed bytes = %q", first)
		}
		close(firstChunkSeen)
		<-releaseRest
		rest, err := io.ReadAll(part)
		if err != nil {
			t.Errorf("read remaining streamed bytes: %v", err)
			return
		}
		if string(rest) != "second" {
			t.Errorf("remaining streamed bytes = %q", rest)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"retval":1,"token":"upload-response-token"}`)
	}))
	defer uploadServer.Close()

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/uploads" || r.URL.Query().Get("type") != "video" {
			t.Errorf("unexpected reservation request: %s %s", r.Method, r.URL.String())
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"url":   uploadServer.URL + "/signed-video",
			"token": "video-reservation-token",
		})
	}))
	defer apiServer.Close()

	sourceReader, sourceWriter := io.Pipe()
	writerDone := make(chan error, 1)
	go func() {
		if _, err := sourceWriter.Write([]byte("first")); err != nil {
			writerDone <- err
			return
		}
		<-releaseRest
		if _, err := sourceWriter.Write([]byte("second")); err != nil {
			writerDone <- err
			return
		}
		writerDone <- sourceWriter.Close()
	}()

	type uploadOutcome struct {
		media MediaToken
		err   error
	}
	uploadDone := make(chan uploadOutcome, 1)
	client := mustClient(t, apiServer.URL, "bot-token", uploadServer.Client())
	go func() {
		media, err := client.UploadMedia(context.Background(), MediaTypeVideo, "clip.mp4", sourceReader)
		uploadDone <- uploadOutcome{media: media, err: err}
	}()

	select {
	case <-firstChunkSeen:
		close(releaseRest)
	case <-time.After(3 * time.Second):
		_ = sourceWriter.CloseWithError(context.DeadlineExceeded)
		close(releaseRest)
		t.Fatal("upload server did not receive the first bytes before the source completed")
	}

	select {
	case err := <-writerDone:
		if err != nil {
			t.Fatalf("source writer error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("source writer did not finish")
	}
	select {
	case outcome := <-uploadDone:
		if outcome.err != nil {
			t.Fatalf("UploadMedia() error = %v", outcome.err)
		}
		if outcome.media != (MediaToken{Type: MediaTypeVideo, Token: "video-reservation-token"}) {
			t.Fatalf("UploadMedia() = %#v", outcome.media)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("UploadMedia() did not finish")
	}
}

func TestVideoUploadTokenSupportsMAXResponseVariants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		body        string
		reservation string
		want        string
	}{
		{name: "reservation is authoritative", body: `{"token":"response-token","retval":1}`, reservation: "reservation-token", want: "reservation-token"},
		{name: "top level compatibility fallback", body: `{"token":"response-token"}`, want: "response-token"},
		{name: "retval is not an attachment token", body: `{"retval":1}`, want: ""},
		{name: "invalid response", body: `not-json`, want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := mediaUploadToken(MediaTypeVideo, []byte(test.body), test.reservation, "url-token"); got != test.want {
				t.Fatalf("mediaUploadToken(video) = %q, want %q", got, test.want)
			}
		})
	}
}

func TestPublishAndEditPreserveMixedMediaOrderAndKeyboard(t *testing.T) {
	t.Parallel()

	type wireAttachment struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		var body struct {
			Attachments []wireAttachment `json:"attachments"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}

		var wantMedia []MediaToken
		switch call {
		case 1:
			if r.Method != http.MethodPost {
				t.Errorf("publish method = %s", r.Method)
			}
			wantMedia = []MediaToken{
				{Type: MediaTypeVideo, Token: "video-1"},
				{Type: MediaTypeImage, Token: "image-1"},
				{Type: MediaTypeVideo, Token: "video-2"},
			}
		case 2:
			if r.Method != http.MethodPut {
				t.Errorf("edit method = %s", r.Method)
			}
			wantMedia = []MediaToken{
				{Type: MediaTypeImage, Token: "image-2"},
				{Type: MediaTypeVideo, Token: "video-3"},
			}
		default:
			t.Errorf("unexpected call %d", call)
			return
		}

		if len(body.Attachments) != len(wantMedia)+1 {
			t.Errorf("attachments = %#v", body.Attachments)
			return
		}
		for i, want := range wantMedia {
			if body.Attachments[i].Type != string(want.Type) {
				t.Errorf("attachment %d type = %q, want %q", i, body.Attachments[i].Type, want.Type)
			}
			var payload attachmentPayload
			if err := json.Unmarshal(body.Attachments[i].Payload, &payload); err != nil || payload.Token != want.Token {
				t.Errorf("attachment %d payload = %#v, error = %v", i, payload, err)
			}
		}
		keyboard := body.Attachments[len(body.Attachments)-1]
		if keyboard.Type != "inline_keyboard" {
			t.Errorf("last attachment type = %q, want inline_keyboard", keyboard.Type)
		}

		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = io.WriteString(w, `{"message":{"body":{"mid":"mixed-mid"}}}`)
		} else {
			_, _ = io.WriteString(w, `{"success":true}`)
		}
	}))
	defer server.Close()

	client := mustClient(t, server.URL, "token", server.Client())
	message, err := client.Publish(context.Background(), PublishRequest{
		ChatID: "-123",
		Text:   "mixed",
		MediaTokens: []MediaToken{
			{Type: MediaTypeVideo, Token: "video-1"},
			{Type: MediaTypeImage, Token: "image-1"},
			{Type: MediaTypeVideo, Token: "video-2"},
		},
		LinkButtons: []LinkButton{{Text: "Открыть", URL: "https://example.com/publish"}},
	})
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if message.MessageID != "mixed-mid" {
		t.Fatalf("Publish() = %#v", message)
	}
	if err := client.Edit(context.Background(), EditRequest{
		MessageID: "mixed-mid",
		Text:      "edited mixed",
		MediaTokens: []MediaToken{
			{Type: MediaTypeImage, Token: "image-2"},
			{Type: MediaTypeVideo, Token: "video-3"},
		},
		LinkButtons: []LinkButton{{Text: "Открыть", URL: "https://example.com/edit"}},
	}); err != nil {
		t.Fatalf("Edit() error = %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("message calls = %d, want 2", calls.Load())
	}
}

func TestDefaultAttachmentRetryDelaysBudget(t *testing.T) {
	t.Parallel()

	if len(defaultAttachmentRetryDelays) == 0 || defaultAttachmentRetryDelays[0] != 0 {
		t.Fatalf("first attempt must be immediate: %v", defaultAttachmentRetryDelays)
	}
	var total time.Duration
	previous := time.Duration(0)
	for _, delay := range defaultAttachmentRetryDelays[1:] {
		if delay < previous {
			t.Fatalf("retry delays must not shrink: %v", defaultAttachmentRetryDelays)
		}
		previous = delay
		total += delay
	}
	// MAX needs tens of seconds to transcode an uploaded video before a
	// message referencing it stops failing with attachment.not.ready.
	if total < 30*time.Second || total > 60*time.Second {
		t.Fatalf("total retry budget = %v, want between 30s and 60s", total)
	}
}

func TestPublishAndEditRetryAttachmentNotReady(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		method  string
		invoke  func(*Client) error
		success string
	}{
		{
			name:   "publish",
			method: http.MethodPost,
			invoke: func(client *Client) error {
				_, err := client.Publish(context.Background(), PublishRequest{
					ChatID:      "-123",
					Text:        "post",
					MediaTokens: []MediaToken{{Type: MediaTypeVideo, Token: "video-token"}},
				})
				return err
			},
			success: `{"message":{"body":{"mid":"retry-mid"}}}`,
		},
		{
			name:   "edit",
			method: http.MethodPut,
			invoke: func(client *Client) error {
				return client.Edit(context.Background(), EditRequest{
					MessageID:   "retry-mid",
					Text:        "edited",
					MediaTokens: []MediaToken{{Type: MediaTypeImage, Token: "image-token"}},
				})
			},
			success: `{"success":true}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := calls.Add(1)
				if r.Method != test.method {
					t.Errorf("method = %s, want %s", r.Method, test.method)
				}
				var body struct {
					Attachments []struct {
						Type string `json:"type"`
					} `json:"attachments"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Attachments) != 1 {
					t.Errorf("request body = %#v, error = %v", body, err)
				}
				w.Header().Set("Content-Type", "application/json")
				if call < 3 {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = io.WriteString(w, `{"code":"attachment.not.ready","message":"try later"}`)
					return
				}
				_, _ = io.WriteString(w, test.success)
			}))
			defer server.Close()

			client := mustClient(t, server.URL, "token", server.Client())
			client.attachmentRetryDelays = []time.Duration{0, 0, 0}
			if err := test.invoke(client); err != nil {
				t.Fatalf("operation error = %v", err)
			}
			if calls.Load() != 3 {
				t.Fatalf("calls = %d, want 3", calls.Load())
			}
		})
	}
}

func TestPublishAndEditRetryHTTP200AttachmentNotReady(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		method  string
		invoke  func(*Client) error
		success string
	}{
		{
			name:   "publish",
			method: http.MethodPost,
			invoke: func(client *Client) error {
				_, err := client.Publish(context.Background(), PublishRequest{
					ChatID: "-123", Text: "post",
					MediaTokens: []MediaToken{{Type: MediaTypeImage, Token: "image-token"}},
				})
				return err
			},
			success: `{"message":{"body":{"mid":"http-200-retry-mid"}}}`,
		},
		{
			name:   "edit",
			method: http.MethodPut,
			invoke: func(client *Client) error {
				return client.Edit(context.Background(), EditRequest{
					MessageID: "http-200-retry-mid", Text: "edited",
					MediaTokens: []MediaToken{{Type: MediaTypeVideo, Token: "video-token"}},
				})
			},
			success: `{"success":true}`,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := calls.Add(1)
				if r.Method != test.method {
					t.Errorf("method = %s, want %s", r.Method, test.method)
				}
				w.Header().Set("Content-Type", "application/json")
				if call < 3 {
					_, _ = io.WriteString(w, `{"success":false,"code":"attachment.not.ready","message":"attachment.not.ready"}`)
					return
				}
				_, _ = io.WriteString(w, test.success)
			}))
			defer server.Close()

			client := mustClient(t, server.URL, "token", server.Client())
			client.attachmentRetryDelays = []time.Duration{0, 0, 0}
			if err := test.invoke(client); err != nil {
				t.Fatalf("operation error = %v", err)
			}
			if calls.Load() != 3 {
				t.Fatalf("calls = %d, want 3", calls.Load())
			}
		})
	}
}

func TestPublishDoesNotRetryUnrelatedAttachmentError(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"code":"attachment.invalid","message":"bad token"}`)
	}))
	defer server.Close()

	client := mustClient(t, server.URL, "token", server.Client())
	client.attachmentRetryDelays = []time.Duration{0, 0, 0}
	_, err := client.Publish(context.Background(), PublishRequest{
		ChatID:      "-123",
		Text:        "post",
		MediaTokens: []MediaToken{{Type: MediaTypeImage, Token: "bad-token"}},
	})
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code != "attachment.invalid" {
		t.Fatalf("Publish() error = %#v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
}

func TestPublishRejectsCombinedLegacyAndTypedMedia(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("invalid request reached MAX")
	}))
	defer server.Close()
	client := mustClient(t, server.URL, "token", server.Client())
	_, err := client.Publish(context.Background(), PublishRequest{
		ChatID:      "-123",
		Text:        "post",
		MediaTokens: []MediaToken{{Type: MediaTypeImage, Token: "typed"}},
		ImageTokens: []string{"legacy"},
	})
	if err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("Publish() error = %v", err)
	}
}
