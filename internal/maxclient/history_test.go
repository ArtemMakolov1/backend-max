package maxclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGetMessagesPageBuildsQueryAndParsesMessageEnvelopes(t *testing.T) {
	t.Parallel()

	const (
		chatID = "-9007199254740993"
		before = int64(1785700000123)
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/messages" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.URL.Query().Get("chat_id"); got != chatID {
			t.Errorf("chat_id = %q", got)
		}
		if got := r.URL.Query().Get("from"); got != "1785700000123" {
			t.Errorf("from = %q", got)
		}
		if got := r.URL.Query().Get("count"); got != "2" {
			t.Errorf("count = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "history-token" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"messages":[
{"sender":{"user_id":9007199254740995,"is_bot":true},"recipient":{"chat_id":-9007199254740993,"chat_type":"channel"},"timestamp":1785699999000,"body":{"mid":"mid.newest","seq":42,"text":"Newest"},"stat":{"views":17},"url":"https://max.ru/news/mid.newest"},
{"message_id":"mid.older","chat_id":"-9007199254740993","timestamp":1785699998000,"body":{"text":"Older"},"url":"https://evil.example/tracker"}
]}`)
	}))
	defer server.Close()

	client := mustClient(t, server.URL, "history-token", server.Client())
	messages, err := client.GetMessagesPage(context.Background(), chatID, before, 2)
	if err != nil {
		t.Fatalf("GetMessagesPage() error = %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("GetMessagesPage() returned %d messages", len(messages))
	}
	newest := messages[0]
	if newest.MessageID != "mid.newest" || newest.ChatID != chatID || newest.Text != "Newest" ||
		newest.TimestampMillis != 1785699999000 || newest.URL != "https://max.ru/news/mid.newest" ||
		newest.Views == nil || *newest.Views != 17 || newest.SenderUserID != "9007199254740995" ||
		!newest.SenderIsBot || len(newest.Raw) == 0 {
		t.Fatalf("newest message = %#v", newest)
	}
	older := messages[1]
	if older.MessageID != "mid.older" || older.ChatID != chatID || older.Text != "Older" ||
		older.TimestampMillis != 1785699998000 || older.URL != "" || older.Views != nil || older.SenderUserID != "" ||
		older.SenderIsBot {
		t.Fatalf("older message = %#v", older)
	}
}

func TestGetMessagesPageOmitsZeroBefore(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, present := r.URL.Query()["from"]; present {
			t.Errorf("zero before unexpectedly sent from=%q", r.URL.Query().Get("from"))
		}
		_, _ = io.WriteString(w, `{"messages":[]}`)
	}))
	defer server.Close()

	client := mustClient(t, server.URL, "token", server.Client())
	messages, err := client.GetMessagesPage(context.Background(), "-1", 0, 100)
	if err != nil || len(messages) != 0 {
		t.Fatalf("GetMessagesPage() = %#v, %v", messages, err)
	}
}

func TestGetMessagesPageParsesAttachmentCompleteness(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"messages":[{"recipient":{"chat_id":-42},"timestamp":1785700000000,"body":{"mid":"mid.media","text":"Media","attachments":[
{"type":"image","payload":{"token":"image-token","url":"https://cdn.max.ru/image.jpg"}},
{"type":"video","payload":{"token":"video-token","url":"https://evil.example/video.mp4"},"width":1920,"height":1080,"duration":7},
{"type":"inline_keyboard","payload":{"buttons":[[{"type":"link","text":"Site","url":"https://example.com"},{"type":"link","text":"Docs","url":"https://example.com/docs"}]]}},
{"type":"inline_keyboard","payload":{"buttons":[[{"type":"link","text":"Valid","url":"https://example.com/valid"},{"type":"callback","text":"Callback","payload":"secret"}],[{"type":"link","text":"Unsafe","url":"http://example.com"}]]}},
{"type":"inline_keyboard","payload":{"buttons":[[{"type":"link","text":"One","url":"https://example.com/1"}],[{"type":"link","text":"Two","url":"https://example.com/2"}],[{"type":"link","text":"Three","url":"https://example.com/3"}],[{"type":"link","text":"Four","url":"https://example.com/4"}]]}},
{"type":"audio","payload":{"token":"audio-token","url":"https://cdn.max.ru/audio.mp3"}},
{"type":"image","payload":{"url":"https://cdn.max.ru/no-token.jpg"}}
]}}]}`)
	}))
	defer server.Close()

	client := mustClient(t, server.URL, "token", server.Client())
	messages, err := client.GetMessagesPage(context.Background(), "-42", 0, 1)
	if err != nil {
		t.Fatalf("GetMessagesPage() error = %v", err)
	}
	if len(messages) != 1 || len(messages[0].Attachments) != 7 {
		t.Fatalf("messages = %#v", messages)
	}
	attachments := messages[0].Attachments
	if image := attachments[0]; image.Type != "image" || image.Token != "image-token" ||
		image.URL != "https://cdn.max.ru/image.jpg" || !image.Complete || len(image.Raw) == 0 {
		t.Fatalf("image = %#v", image)
	}
	video := attachments[1]
	if video.Type != "video" || video.Token != "video-token" || video.URL != "" || !video.Complete ||
		video.Width == nil || *video.Width != 1920 || video.Height == nil || *video.Height != 1080 ||
		video.DurationMS == nil || *video.DurationMS != 7000 {
		t.Fatalf("video = %#v", video)
	}
	keyboard := attachments[2]
	if !keyboard.Complete || len(keyboard.LinkButtons) != 2 || keyboard.LinkButtons[0].Text != "Site" ||
		keyboard.LinkButtons[1].URL != "https://example.com/docs" {
		t.Fatalf("keyboard = %#v", keyboard)
	}
	mixedKeyboard := attachments[3]
	if mixedKeyboard.Complete || len(mixedKeyboard.LinkButtons) != 1 || mixedKeyboard.LinkButtons[0].Text != "Valid" {
		t.Fatalf("mixed keyboard = %#v", mixedKeyboard)
	}
	overflowKeyboard := attachments[4]
	if overflowKeyboard.Complete || len(overflowKeyboard.LinkButtons) != 4 {
		t.Fatalf("four-button keyboard = %#v", overflowKeyboard)
	}
	if unsupported := attachments[5]; unsupported.Type != "audio" || unsupported.Complete || len(unsupported.Raw) == 0 {
		t.Fatalf("unsupported attachment = %#v", unsupported)
	}
	if missingToken := attachments[6]; missingToken.Complete || missingToken.Token != "" ||
		missingToken.URL != "https://cdn.max.ru/no-token.jpg" {
		t.Fatalf("missing-token image = %#v", missingToken)
	}
}

func TestGetMessagesPageRejectsInvalidRequestBeforeNetwork(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{"messages":[]}`)
	}))
	defer server.Close()
	client := mustClient(t, server.URL, "token", server.Client())

	for _, test := range []struct {
		name   string
		chatID string
		before int64
		count  int
	}{
		{name: "missing chat", count: 1},
		{name: "invalid chat", chatID: "channel", count: 1},
		{name: "negative before", chatID: "-1", before: -1, count: 1},
		{name: "zero count", chatID: "-1", count: 0},
		{name: "too large count", chatID: "-1", count: 101},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := client.GetMessagesPage(context.Background(), test.chatID, test.before, test.count); err == nil {
				t.Fatal("GetMessagesPage() accepted invalid request")
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid requests reached MAX API %d times", calls.Load())
	}
}

func TestGetMessagesPageRejectsInvalidProviderPayloads(t *testing.T) {
	t.Parallel()

	const valid = `{"recipient":{"chat_id":-42},"timestamp":1785700000000,"body":{"mid":"mid.valid","text":"Text"}}`
	tests := []struct {
		name     string
		body     string
		count    int
		contains string
	}{
		{name: "missing envelope", body: `{}`, count: 1, contains: "messages array"},
		{name: "null envelope", body: `{"messages":null}`, count: 1, contains: "messages array"},
		{name: "wrong envelope type", body: `{"messages":{}}`, count: 1, contains: "decode MAX message history messages"},
		{name: "invalid mid", body: `{"messages":[{"recipient":{"chat_id":-42},"timestamp":1,"body":{"mid":"bad id"}}]}`, count: 1, contains: "valid message ID"},
		{name: "mismatched mid", body: `{"messages":[{"message_id":"mid.one","recipient":{"chat_id":-42},"timestamp":1,"body":{"mid":"mid.two"}}]}`, count: 1, contains: "mismatched message ID"},
		{name: "zero timestamp", body: `{"messages":[{"recipient":{"chat_id":-42},"timestamp":0,"body":{"mid":"mid.zero"}}]}`, count: 1, contains: "non-positive timestamp"},
		{name: "negative views", body: `{"messages":[{"recipient":{"chat_id":-42},"timestamp":1,"body":{"mid":"mid.views"},"stat":{"views":-1}}]}`, count: 1, contains: "negative view count"},
		{name: "zero sender user ID", body: `{"messages":[{"sender":{"user_id":0},"recipient":{"chat_id":-42},"timestamp":1,"body":{"mid":"mid.sender-zero"}}]}`, count: 1, contains: "invalid sender user ID"},
		{name: "negative sender user ID", body: `{"messages":[{"sender":{"user_id":-7},"recipient":{"chat_id":-42},"timestamp":1,"body":{"mid":"mid.sender-negative"}}]}`, count: 1, contains: "invalid sender user ID"},
		{name: "non-numeric sender user ID", body: `{"messages":[{"sender":{"user_id":"sender"},"recipient":{"chat_id":-42},"timestamp":1,"body":{"mid":"mid.sender-text"}}]}`, count: 1, contains: "invalid sender user ID"},
		{name: "foreign chat", body: `{"messages":[{"recipient":{"chat_id":-43},"timestamp":1,"body":{"mid":"mid.foreign"}}]}`, count: 1, contains: "requested chat"},
		{name: "mismatched chat fields", body: `{"messages":[{"chat_id":-42,"recipient":{"chat_id":-43},"timestamp":1,"body":{"mid":"mid.chat"}}]}`, count: 1, contains: "mismatched chat ID"},
		{name: "duplicate mid", body: `{"messages":[` + valid + `,` + valid + `]}`, count: 2, contains: "duplicate message ID"},
		{name: "more than requested", body: `{"messages":[` + valid + `,` + strings.Replace(valid, "mid.valid", "mid.second", 1) + `]}`, count: 1, contains: "requested at most 1"},
		{name: "duration overflow", body: `{"messages":[{"recipient":{"chat_id":-42},"timestamp":1,"body":{"mid":"mid.duration","attachments":[{"type":"video","duration":9223372036854776,"payload":{"token":"video"}}]}}]}`, count: 1, contains: "overflows milliseconds"},
		{name: "malformed media payload", body: `{"messages":[{"recipient":{"chat_id":-42},"timestamp":1,"body":{"mid":"mid.payload","attachments":[{"type":"image","payload":"not-an-object"}]}}]}`, count: 1, contains: "decode image attachment payload"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			client := mustClient(t, server.URL, "token", server.Client())
			_, err := client.GetMessagesPage(context.Background(), "-42", 0, test.count)
			if err == nil || !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("GetMessagesPage() error = %v, want substring %q", err, test.contains)
			}
		})
	}
}

func TestHistoryProviderTokensAndRawPayloadsStayOutOfJSON(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(HistoryMessage{
		MessageID: "mid", Raw: json.RawMessage(`{"secret":"message"}`),
		Attachments: []HistoryAttachment{{
			Type: "image", Token: "provider-secret", Raw: json.RawMessage(`{"secret":"attachment"}`),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	value := string(encoded)
	for _, secret := range []string{"provider-secret", "message", "attachment"} {
		if strings.Contains(value, secret) {
			t.Fatalf("history JSON leaked %q: %s", secret, value)
		}
	}
}
