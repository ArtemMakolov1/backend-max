package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"maxpilot/backend/internal/maxclient"
	"maxpilot/backend/internal/media"
	"maxpilot/backend/internal/store"
)

// minimalMP4 returns bytes whose header satisfies the media store's MP4 probe
// (an "ftyp" box at offset 4) without a real video payload.
func minimalMP4() []byte {
	header := make([]byte, 64)
	copy(header[4:8], []byte("ftyp"))
	copy(header[8:12], []byte("isom"))
	return header
}

type videoPublishServer struct {
	mu                sync.Mutex
	uploadReservation int
	uploadedBytes     int64
	editRequests      int
	publishRequests   []messageBody
	uploadServer      *httptest.Server
	apiServer         *httptest.Server
}

type messageBody struct {
	Text        string           `json:"text"`
	Attachments []map[string]any `json:"attachments"`
}

func newVideoPublishServer(t *testing.T) *videoPublishServer {
	t.Helper()
	server := &videoPublishServer{}
	server.uploadServer = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reader, err := r.MultipartReader()
		if err != nil {
			t.Errorf("upload multipart reader: %v", err)
			http.Error(w, "bad multipart", http.StatusBadRequest)
			return
		}
		part, err := reader.NextPart()
		if err != nil || part.FormName() != "data" {
			t.Errorf("upload part = %v, %v", part, err)
			http.Error(w, "bad part", http.StatusBadRequest)
			return
		}
		written, err := io.Copy(io.Discard, part)
		if err != nil {
			t.Errorf("read uploaded video: %v", err)
			http.Error(w, "read failed", http.StatusInternalServerError)
			return
		}
		server.mu.Lock()
		server.uploadedBytes += written
		server.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"retval":1}`)
	}))
	t.Cleanup(server.uploadServer.Close)

	server.apiServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/uploads":
			if r.URL.Query().Get("type") != string(maxclient.MediaTypeVideo) {
				t.Errorf("upload reservation type = %q, want video", r.URL.Query().Get("type"))
			}
			server.mu.Lock()
			server.uploadReservation++
			server.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{
				"url":   server.uploadServer.URL + "/signed-video",
				"token": "video-token",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/chats/-100200":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"chat_id":-100200,"owner_id":777,"type":"channel","status":"active","title":"Video channel","is_public":false}`)
		case r.Method == http.MethodGet && r.URL.Path == "/chats/-100200/members/me":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"user_id":42,"is_bot":true,"is_admin":true,"permissions":["read_all_messages","write","edit","delete","pin_message","change_chat_info"]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/messages":
			var body messageBody
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode publish body: %v", err)
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			server.mu.Lock()
			server.publishRequests = append(server.publishRequests, body)
			server.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"message":{"message_id":"mid-video-1","url":"https://max.ru/post/video-1"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/messages/mid-video-1":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"message_id":"mid-video-1","recipient":{"chat_id":-100200},"sender":{"user_id":42,"is_bot":true},"body":{"mid":"mid-video-1","text":"Смотрите видео"}}`)
		case r.Method == http.MethodPut && r.URL.Path == "/messages":
			server.mu.Lock()
			server.editRequests++
			server.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"success":true}`)
		default:
			t.Errorf("unexpected MAX API request: %s %s", r.Method, r.URL.String())
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(server.apiServer.Close)
	return server
}

// TestPublishPostWithVideoAttachment exercises the complete publish path for a
// video attachment with the real MAX client: S3 object → /uploads reservation
// → multipart upload → token attachment in POST /messages → provider-token
// caching.
func TestPublishPostWithVideoAttachment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	server := newVideoPublishServer(t)
	transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} // #nosec G402 -- test-only trust of the scripted MAX server
	client, err := maxclient.New(server.apiServer.URL, "test-token",
		&http.Client{Transport: transport, Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	application, storage := newTestApp(t, client)

	channel, err := storage.CreateChannel(ctx, store.Channel{
		MAXChatID: "-100200", Title: "Video channel", IsChannel: true, Active: true,
		VerifiedMAXOwnerID: "777",
	})
	if err != nil {
		t.Fatal(err)
	}
	post, err := storage.CreatePost(ctx, store.Post{
		Title: "Video post", Content: "Смотрите видео", Format: store.FormatMarkdown, ChannelID: &channel.ID,
	})
	if err != nil {
		t.Fatal(err)
	}

	file, err := application.SaveAttachmentMediaForUser(ctx, post.UserID,
		media.AttachmentTypeVideo, "clip.mp4", bytes.NewReader(minimalMP4()))
	if err != nil {
		t.Fatal(err)
	}
	if file.MIMEType != "video/mp4" {
		t.Fatalf("stored video MIME = %q", file.MIMEType)
	}
	post, err = storage.AddPostAttachmentForUser(ctx, post.UserID, post.ID, store.PostAttachment{
		Type: store.PostAttachmentVideo, Position: 0, StorageKey: file.Path,
		ProcessingStatus: store.AttachmentStatusReady, SizeBytes: file.Size, MIMEType: file.MIMEType,
	})
	if err != nil {
		t.Fatal(err)
	}

	published, err := application.PublishPost(ctx, post.ID)
	if err != nil {
		t.Fatalf("publish post with video: %v", err)
	}
	if published.Status != store.PostStatusPublished || published.MAXMessageID != "mid-video-1" {
		t.Fatalf("published post = %#v", published)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.uploadReservation != 1 || server.uploadedBytes != int64(len(minimalMP4())) {
		t.Fatalf("video upload reservations=%d bytes=%d, want one upload of %d bytes",
			server.uploadReservation, server.uploadedBytes, len(minimalMP4()))
	}
	if len(server.publishRequests) != 1 || len(server.publishRequests[0].Attachments) != 1 {
		t.Fatalf("publish requests = %#v", server.publishRequests)
	}
	attachment := server.publishRequests[0].Attachments[0]
	if attachment["type"] != "video" {
		t.Fatalf("attachment = %#v, want video", attachment)
	}
	payload, _ := attachment["payload"].(map[string]any)
	if payload["token"] != "video-token" {
		t.Fatalf("attachment payload = %#v, want cached video token", payload)
	}

	stored, err := storage.GetPost(ctx, post.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Attachments) != 1 || stored.Attachments[0].ProviderToken != "video-token" {
		t.Fatalf("provider token not cached: %#v", stored.Attachments)
	}
}

// TestPublishPostWithVideoReusesCachedToken verifies the CAS token cache so a
// second publish (for example after a MAX edit) does not re-upload the video.
func TestPublishPostWithVideoReusesCachedToken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	server := newVideoPublishServer(t)
	transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} // #nosec G402 -- test-only trust of the scripted MAX server
	client, err := maxclient.New(server.apiServer.URL, "test-token",
		&http.Client{Transport: transport, Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	application, storage := newTestApp(t, client)

	channel, err := storage.CreateChannel(ctx, store.Channel{
		MAXChatID: "-100200", Title: "Video channel", IsChannel: true, Active: true,
		VerifiedMAXOwnerID: "777",
	})
	if err != nil {
		t.Fatal(err)
	}
	post, err := storage.CreatePost(ctx, store.Post{
		Title: "Video post", Content: "Смотрите видео", Format: store.FormatMarkdown, ChannelID: &channel.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	file, err := application.SaveAttachmentMediaForUser(ctx, post.UserID,
		media.AttachmentTypeVideo, "clip.mp4", bytes.NewReader(minimalMP4()))
	if err != nil {
		t.Fatal(err)
	}
	post, err = storage.AddPostAttachmentForUser(ctx, post.UserID, post.ID, store.PostAttachment{
		Type: store.PostAttachmentVideo, Position: 0, StorageKey: file.Path,
		ProcessingStatus: store.AttachmentStatusReady, SizeBytes: file.Size, MIMEType: file.MIMEType,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := application.PublishPost(ctx, post.ID); err != nil {
		t.Fatalf("first publish: %v", err)
	}

	updated, err := application.UpdatePublishedPost(ctx, post.ID)
	if err != nil {
		t.Fatalf("edit published post with video: %v", err)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.uploadReservation != 1 {
		t.Fatalf("video was uploaded %d times, want the cached token to be reused", server.uploadReservation)
	}
	if server.editRequests != 1 {
		t.Fatalf("MAX edit requests = %d, want one", server.editRequests)
	}
	if updated.Status != store.PostStatusPublished {
		t.Fatalf("edited post = %#v", updated)
	}
}
