package maxclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestPublishAndEditBuildInlineKeyboardAttachment(t *testing.T) {
	t.Parallel()

	type wireAttachment struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	type wireBody struct {
		Attachments []wireAttachment `json:"attachments"`
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		var body wireBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		switch call {
		case 1:
			if r.Method != http.MethodPost || len(body.Attachments) != 2 ||
				body.Attachments[0].Type != "image" || body.Attachments[1].Type != "inline_keyboard" {
				t.Fatalf("publish attachments = %#v", body.Attachments)
			}
			var image attachmentPayload
			if err := json.Unmarshal(body.Attachments[0].Payload, &image); err != nil || image.Token != "image-1" {
				t.Fatalf("publish image payload = %#v, error = %v", image, err)
			}
			var keyboard inlineKeyboardPayload
			if err := json.Unmarshal(body.Attachments[1].Payload, &keyboard); err != nil {
				t.Fatalf("decode keyboard: %v", err)
			}
			if len(keyboard.Buttons) != 2 || len(keyboard.Buttons[0]) != 1 || len(keyboard.Buttons[1]) != 1 {
				t.Fatalf("keyboard rows = %#v", keyboard.Buttons)
			}
			if first := keyboard.Buttons[0][0]; first.Type != "link" || first.Text != "Сайт" || first.URL != "https://example.com" {
				t.Fatalf("first button = %#v", first)
			}
			if second := keyboard.Buttons[1][0]; second.Type != "link" || second.Text != "Каталог" || second.URL != "https://example.com/catalog" {
				t.Fatalf("second button = %#v", second)
			}
			_, _ = io.WriteString(w, `{"message":{"body":{"mid":"mid-buttons"}}}`)
		case 2:
			if r.Method != http.MethodPut || len(body.Attachments) != 2 ||
				body.Attachments[0].Type != "image" || body.Attachments[1].Type != "inline_keyboard" {
				t.Fatalf("edit attachments = %#v, want image and keyboard", body.Attachments)
			}
			var image attachmentPayload
			if err := json.Unmarshal(body.Attachments[0].Payload, &image); err != nil || image.Token != "image-2" {
				t.Fatalf("edit image payload = %#v, error = %v", image, err)
			}
			var keyboard inlineKeyboardPayload
			if err := json.Unmarshal(body.Attachments[1].Payload, &keyboard); err != nil ||
				len(keyboard.Buttons) != 1 || keyboard.Buttons[0][0].URL != "https://example.com/edit" {
				t.Fatalf("edit keyboard = %#v, error = %v", keyboard, err)
			}
			_, _ = io.WriteString(w, `{"success":true}`)
		case 3:
			if r.Method != http.MethodPut || len(body.Attachments) != 1 || body.Attachments[0].Type != "image" {
				t.Fatalf("clear attachments = %#v, want reuploaded image without keyboard", body.Attachments)
			}
			var image attachmentPayload
			if err := json.Unmarshal(body.Attachments[0].Payload, &image); err != nil || image.Token != "image-3" {
				t.Fatalf("clear image payload = %#v, error = %v", image, err)
			}
			_, _ = io.WriteString(w, `{"success":true}`)
		default:
			t.Fatalf("unexpected call %d", call)
		}
	}))
	defer server.Close()

	client := mustClient(t, server.URL, "token", server.Client())
	if _, err := client.Publish(context.Background(), PublishRequest{
		ChatID: "-123", Text: "Пост", Format: FormatMarkdown, ImageTokens: []string{"image-1"},
		LinkButtons: []LinkButton{
			{Text: " Сайт ", URL: " https://example.com "},
			{Text: "Каталог", URL: "https://example.com/catalog"},
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if err := client.Edit(context.Background(), EditRequest{
		MessageID: "mid-buttons", Text: "Пост", Format: FormatMarkdown,
		ImageTokens: []string{"image-2"},
		LinkButtons: []LinkButton{{Text: "Открыть", URL: "https://example.com/edit"}},
	}); err != nil {
		t.Fatalf("Edit() error = %v", err)
	}
	if err := client.Edit(context.Background(), EditRequest{
		MessageID: "mid-buttons", Text: "Пост", Format: FormatMarkdown,
		ImageTokens: []string{"image-3"}, LinkButtons: []LinkButton{},
	}); err != nil {
		t.Fatalf("Edit(clear buttons) error = %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("message calls = %d, want 3", calls.Load())
	}
}

func TestPublishRejectsUnsafeLinkButtonBeforeRequest(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("unsafe link button reached MAX")
	}))
	defer server.Close()
	client := mustClient(t, server.URL, "token", server.Client())
	if _, err := client.Publish(context.Background(), PublishRequest{
		ChatID: "-123", Text: "Пост", LinkButtons: []LinkButton{{Text: "Сайт", URL: "https://user@example.com"}},
	}); err == nil {
		t.Fatal("Publish() accepted a URL with userinfo")
	}
}
