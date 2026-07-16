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

func TestGetMeUsesRawAuthorizationToken(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/api/me" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "raw-token" {
			t.Errorf("Authorization = %q, want raw token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"user_id":42,"first_name":"Editor","username":"channel_bot","is_bot":true}`)
	}))
	defer server.Close()

	client := mustClient(t, server.URL+"/api/", "raw-token", server.Client())
	bot, err := client.GetMe(context.Background())
	if err != nil {
		t.Fatalf("GetMe() error = %v", err)
	}
	if bot.UserID != 42 || bot.FirstName != "Editor" || !bot.IsBot {
		t.Fatalf("GetMe() = %#v", bot)
	}
	if err := client.Test(context.Background()); err != nil {
		t.Fatalf("Test() error = %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("request count = %d, want 2", got)
	}
}

func TestAPICredentialNeverFollowsRedirect(t *testing.T) {
	t.Parallel()

	var redirectedCalls atomic.Int32
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectedCalls.Add(1)
		if authorization := r.Header.Get("Authorization"); authorization != "" {
			t.Errorf("redirect target received Authorization %q", authorization)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer redirectTarget.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", redirectTarget.URL+"/stolen")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	client := mustClient(t, origin.URL, "shared-bot-secret", origin.Client())
	err := client.Test(context.Background())
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("Test() error = %v, want un-followed 307", err)
	}
	if redirectedCalls.Load() != 0 {
		t.Fatalf("redirect target calls = %d, want 0", redirectedCalls.Load())
	}
}

func TestGetChatIsReadOnlyAndPreservesLargeID(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/chats/-9007199254740993" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "token" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"chat_id":-9007199254740993,"type":"channel","status":"active","title":"Новости","link":"https://max.ru/news","icon":{"url":"https://cdn.max.ru/news.png"},"participants_count":12450}`)
	}))
	defer server.Close()

	client := mustClient(t, server.URL, "token", server.Client())
	chat, err := client.GetChat(context.Background(), "-9007199254740993")
	if err != nil {
		t.Fatalf("GetChat() error = %v", err)
	}
	if chat.ChatID != "-9007199254740993" || chat.Type != "channel" || chat.Status != "active" || chat.Title != "Новости" ||
		chat.Icon.URL != "https://cdn.max.ru/news.png" || chat.ParticipantsCount != 12450 {
		t.Fatalf("GetChat() = %#v", chat)
	}
}

func TestGetChatAndMembershipAreReadOnly(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "token" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/chats/-9223372036854775807":
			_, _ = io.WriteString(w, `{"chat_id":-9223372036854775807,"owner_id":777,"type":"channel","status":"active","title":"News","link":"https://max.ru/news-room"}`)
		case "/chats/-9223372036854775807/members/me":
			_, _ = io.WriteString(w, `{"user_id":42,"first_name":"Bot","is_bot":true,"is_admin":true,"permissions":["read_all_messages","post_edit_delete_message","edit_message","delete_message"]}`)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := mustClient(t, server.URL, "token", server.Client())
	chat, err := client.GetChat(context.Background(), "-9223372036854775807")
	if err != nil {
		t.Fatalf("GetChat() error = %v", err)
	}
	if chat.ChatID != "-9223372036854775807" || chat.OwnerID != "777" || chat.Type != "channel" || chat.Status != "active" {
		t.Fatalf("GetChat() = %#v", chat)
	}
	membership, err := client.GetMembership(context.Background(), chat.ChatID)
	if err != nil {
		t.Fatalf("GetMembership() error = %v", err)
	}
	if !membership.IsAdmin || !membership.HasPermission(PermissionReadAllMessages) ||
		!membership.HasPermission(PermissionWrite) || !membership.HasPermission(PermissionEdit) ||
		!membership.HasPermission(PermissionDelete) {
		t.Fatalf("unexpected membership: %#v", membership)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
}

func TestGetChatAdminsReturnsOwnerAndOrdinaryAdmins(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/chats/-9223372036854775807/members/admins" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "token" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"members":[{"user_id":777,"first_name":"Owner","is_owner":true,"is_admin":true,"permissions":["view_stats"]},{"user_id":999,"first_name":"Admin","is_owner":false,"is_admin":true,"permissions":["write"]}]}`)
	}))
	defer server.Close()

	client := mustClient(t, server.URL, "token", server.Client())
	admins, err := client.GetChatAdmins(context.Background(), "-9223372036854775807")
	if err != nil {
		t.Fatal(err)
	}
	if len(admins) != 2 || admins[0].UserID != 777 || !admins[0].IsOwner || !admins[0].IsAdmin ||
		admins[1].UserID != 999 || admins[1].IsOwner || !admins[1].IsAdmin {
		t.Fatalf("GetChatAdmins() = %#v", admins)
	}
	if _, err := client.GetChatAdmins(context.Background(), "not-a-chat"); err == nil {
		t.Fatal("GetChatAdmins accepted a non-numeric chat id")
	}
}

func TestSendIdentityLinkConfirmationUsesPrivateCallbackButtons(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/messages" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.URL.Query().Get("user_id"); got != "777" {
			t.Errorf("user_id = %q, want 777", got)
		}
		if got := r.Header.Get("Authorization"); got != "shared-token" {
			t.Errorf("Authorization = %q, want shared-token", got)
		}
		var body struct {
			Text        string `json:"text"`
			Attachments []struct {
				Type    string `json:"type"`
				Payload struct {
					Buttons [][]struct {
						Type    string `json:"type"`
						Text    string `json:"text"`
						Payload string `json:"payload"`
					} `json:"buttons"`
				} `json:"payload"`
			} `json:"attachments"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(body.Text, "Аккаунт Анны") || !strings.Contains(body.Text, "390214") {
			t.Errorf("confirmation text = %q", body.Text)
		}
		if len(body.Attachments) != 1 || body.Attachments[0].Type != "inline_keyboard" ||
			len(body.Attachments[0].Payload.Buttons) != 1 || len(body.Attachments[0].Payload.Buttons[0]) != 2 {
			t.Fatalf("unexpected attachments: %#v", body.Attachments)
		}
		buttons := body.Attachments[0].Payload.Buttons[0]
		if buttons[0].Type != "callback" || buttons[0].Payload != "link_confirm_token" ||
			buttons[1].Type != "callback" || buttons[1].Payload != "link_cancel_token" {
			t.Errorf("unexpected callback buttons: %#v", buttons)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()

	client := mustClient(t, server.URL, "shared-token", server.Client())
	if err := client.SendIdentityLinkConfirmation(context.Background(), "777", "Аккаунт Анны", "390214",
		"link_confirm_token", "link_cancel_token"); err != nil {
		t.Fatal(err)
	}
	if err := client.SendIdentityLinkConfirmation(context.Background(), "not-a-user", "Аккаунт Анны", "390214",
		"link_confirm_token", "link_cancel_token"); err == nil {
		t.Fatal("invalid MAX user id was accepted")
	}
}

func TestAnswerCallbackReplacesSourceMessageAndClearsKeyboard(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/answers" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.URL.Query().Get("callback_id"); got != "callback-42" {
			t.Errorf("callback_id = %q", got)
		}
		var body struct {
			Notification string `json:"notification"`
			Message      struct {
				Text        string `json:"text"`
				Attachments []any  `json:"attachments"`
			} `json:"message"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Notification != "Профиль связан" || body.Message.Text != "✅ Готово! Профиль связан." {
			t.Errorf("callback answer = %#v", body)
		}
		if body.Message.Attachments == nil || len(body.Message.Attachments) != 0 {
			t.Errorf("inline keyboard was not removed: %#v", body.Message.Attachments)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"success":true}`)
	}))
	defer server.Close()

	client := mustClient(t, server.URL, "shared-token", server.Client())
	if err := client.AnswerCallback(context.Background(), "callback-42", "Профиль связан", "✅ Готово! Профиль связан."); err != nil {
		t.Fatal(err)
	}
	if err := client.AnswerCallback(context.Background(), "", "Профиль связан", ""); err == nil {
		t.Fatal("empty callback ID was accepted")
	}
	if err := client.AnswerCallback(context.Background(), "callback-42", "", ""); err == nil {
		t.Fatal("empty callback answer was accepted")
	}
}

func TestAnswerCallbackReportsUnsuccessfulTwoHundredResponse(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"success":false,"message":"cannot replace callback message"}`)
	}))
	defer server.Close()

	client := mustClient(t, server.URL, "shared-token", server.Client())
	err := client.AnswerCallback(context.Background(), "callback-42", "Профиль связан", "✅ Готово!")
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code != "operation_failed" || apiErr.Message != "cannot replace callback message" {
		t.Fatalf("AnswerCallback() error = %#v", err)
	}
}

func TestGetChatRejectsInvalidAndMalformedChatID(t *testing.T) {
	t.Parallel()
	client := mustClient(t, "https://platform-api2.max.ru", "token", http.DefaultClient)
	for _, input := range []string{"", "not-a-number", "1.5"} {
		if _, err := client.GetChat(context.Background(), input); err == nil {
			t.Errorf("GetChat(%q) accepted invalid id", input)
		}
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"chat_id":"not-a-number","type":"channel","status":"active"}`)
	}))
	defer server.Close()
	client = mustClient(t, server.URL, "token", server.Client())
	if _, err := client.GetChat(context.Background(), "123"); err == nil || !strings.Contains(err.Error(), "numeric chat_id") {
		t.Fatalf("GetChat malformed response error = %v", err)
	}
}

func TestGetChatByLinkNormalizesWithoutURLInjection(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.RequestURI != "/chats/se13549123_biz" {
			t.Errorf("unexpected request: %s %s", r.Method, r.RequestURI)
		}
		_, _ = io.WriteString(w, `{"chat_id":-13549123,"owner_id":777,"type":"channel","status":"active","title":"Test"}`)
	}))
	defer server.Close()
	client := mustClient(t, server.URL, "token", server.Client())
	chat, err := client.GetChatByLink(context.Background(),
		"https://max.ru/se13549123_biz?chat_id=-1&access_token=stolen#ignored")
	if err != nil || chat.ChatID != "-13549123" || chat.OwnerID != "777" {
		t.Fatalf("GetChatByLink() = %#v, %v", chat, err)
	}
	for _, input := range []string{
		"https://user@max.ru/se13549123_biz",
		"https://evil.example/se13549123_biz",
		"https://max.ru/se13549123_biz/other",
		"https://max.ru/se13549123_biz%2Fother",
		"https://max.ru/%2e%2e%2Fsubscriptions",
	} {
		if _, err := client.GetChatByLink(context.Background(), input); err == nil {
			t.Errorf("GetChatByLink(%q) accepted unsafe link", input)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("unsafe links reached MAX API; calls=%d", calls.Load())
	}
}

func TestMembershipWritePermissionCoversPublicationManagement(t *testing.T) {
	t.Parallel()
	membership := Membership{Permissions: []Permission{PermissionReadAllMessages, PermissionWrite}}
	for _, permission := range []Permission{PermissionReadAllMessages, PermissionWrite, PermissionEdit, PermissionDelete} {
		if !membership.HasPermission(permission) {
			t.Errorf("write permission does not satisfy %q", permission)
		}
	}
	legacy := Membership{Permissions: []Permission{PermissionReadAllMessages, "post_edit_delete_message"}}
	for _, permission := range []Permission{PermissionWrite, PermissionEdit, PermissionDelete} {
		if !legacy.HasPermission(permission) {
			t.Errorf("legacy combined permission does not satisfy %q", permission)
		}
	}
}

func TestNormalizeChatLink(t *testing.T) {
	t.Parallel()
	for input, want := range map[string]string{
		"news":                          "news",
		"@news_room":                    "@news_room",
		"max.ru/news-room":              "news-room",
		"https://max.ru/news?from=test": "news",
		"https://MAX.RU/News":           "News",
	} {
		got, err := NormalizeChatLink(input)
		if err != nil || got != want {
			t.Errorf("NormalizeChatLink(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
}

func TestNumericIDUsesSignedInt64Contract(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"0", "9223372036854775807", "-9223372036854775808"} {
		if !numericID(value) {
			t.Errorf("numericID(%q) = false, want true", value)
		}
	}
	for _, value := range []string{"", "+1", "9223372036854775808", "-9223372036854775809", "1.0", "1e3"} {
		if numericID(value) {
			t.Errorf("numericID(%q) = true, want false", value)
		}
	}
}

func TestPublishBuildsMAXMessageContract(t *testing.T) {
	t.Parallel()

	notify := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/messages" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.URL.Query().Get("chat_id"); got != "-987654321" {
			t.Errorf("chat_id = %q", got)
		}
		if got := r.URL.Query().Get("disable_link_preview"); got != "true" {
			t.Errorf("disable_link_preview = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "token" {
			t.Errorf("Authorization = %q", got)
		}

		var body struct {
			Text        string `json:"text"`
			Format      Format `json:"format"`
			Notify      *bool  `json:"notify"`
			Attachments []struct {
				Type    string `json:"type"`
				Payload struct {
					Token string `json:"token"`
				} `json:"payload"`
			} `json:"attachments"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if body.Text != "**Новый пост**" || body.Format != FormatMarkdown {
			t.Errorf("message body = %#v", body)
		}
		if body.Notify == nil || *body.Notify {
			t.Errorf("notify = %#v, want false", body.Notify)
		}
		if len(body.Attachments) != 2 || body.Attachments[0].Type != "image" || body.Attachments[0].Payload.Token != "image-1" || body.Attachments[1].Payload.Token != "image-2" {
			t.Errorf("attachments = %#v", body.Attachments)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"message":{"body":{"mid":"mid-123","text":"**Новый пост**"},"url":"https://max.ru/channel/post"}}`)
	}))
	defer server.Close()

	client := mustClient(t, server.URL, "token", server.Client())
	message, err := client.Publish(context.Background(), PublishRequest{
		ChatID:             "-987654321",
		Text:               "**Новый пост**",
		Format:             FormatMarkdown,
		ImageTokens:        []string{"image-1", "image-2"},
		DisableLinkPreview: true,
		Notify:             &notify,
	})
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if message.MessageID != "mid-123" || message.URL != "https://max.ru/channel/post" || message.Text != "**Новый пост**" {
		t.Fatalf("Publish() = %#v", message)
	}
}

func TestPublishAndEditNormalizeLegacyMarkdownHeadingPayloads(t *testing.T) {
	t.Parallel()
	const publishInput = "## Привет\n++Меня зовут Фома++\n```markdown\n### Код не меняется\n```\n###### Итог"
	const publishPayload = "# Привет\n++Меня зовут Фома++\n```markdown\n### Код не меняется\n```\n# Итог"
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body struct {
			Text   string `json:"text"`
			Format Format `json:"format"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode message request: %v", err)
		}
		if body.Format != FormatMarkdown {
			t.Errorf("format = %q, want markdown", body.Format)
		}
		switch r.Method {
		case http.MethodPost:
			if body.Text != publishPayload {
				t.Errorf("publish text = %q, want %q", body.Text, publishPayload)
			}
			_, _ = io.WriteString(w, `{"message":{"body":{"mid":"mid-heading"}}}`)
		case http.MethodPut:
			if body.Text != "# Исправлено" {
				t.Errorf("edit text = %q, want %q", body.Text, "# Исправлено")
			}
			_, _ = io.WriteString(w, `{"success":true}`)
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))
	defer server.Close()

	client := mustClient(t, server.URL, "token", server.Client())
	if _, err := client.Publish(context.Background(), PublishRequest{
		ChatID: "-987654321", Text: publishInput, Format: FormatMarkdown,
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if err := client.Edit(context.Background(), EditRequest{
		MessageID: "mid-heading", Text: "##### Исправлено", Format: FormatMarkdown,
	}); err != nil {
		t.Fatalf("Edit() error = %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("message calls = %d, want 2", calls.Load())
	}
}

func TestPublishAndEditPreserveEmojiInJSONPayloads(t *testing.T) {
	t.Parallel()
	const emojiText = "🚀 ❤️ 👋🏽 🇷🇺 👨‍👩‍👧‍👦"

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body struct {
			Text   string `json:"text"`
			Format Format `json:"format"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode emoji message request: %v", err)
		}
		if body.Text != emojiText {
			t.Errorf("emoji text = %q, want %q", body.Text, emojiText)
		}
		if body.Format != FormatMarkdown {
			t.Errorf("format = %q, want markdown", body.Format)
		}

		switch r.Method {
		case http.MethodPost:
			_, _ = io.WriteString(w, `{"message":{"body":{"mid":"mid-emoji"}}}`)
		case http.MethodPut:
			_, _ = io.WriteString(w, `{"success":true}`)
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))
	defer server.Close()

	client := mustClient(t, server.URL, "token", server.Client())
	if _, err := client.Publish(context.Background(), PublishRequest{
		ChatID: "-987654321", Text: emojiText, Format: FormatMarkdown,
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if err := client.Edit(context.Background(), EditRequest{
		MessageID: "mid-emoji", Text: emojiText, Format: FormatMarkdown,
	}); err != nil {
		t.Fatalf("Edit() error = %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("message calls = %d, want 2", calls.Load())
	}
}

func TestEditAndDeleteBuildMAXContract(t *testing.T) {
	t.Parallel()

	var edited atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/messages" || r.URL.Query().Get("message_id") != "mid-7" {
			t.Errorf("unexpected URL: %s", r.URL.String())
		}
		if got := r.Header.Get("Authorization"); got != "token" {
			t.Errorf("Authorization = %q", got)
		}

		switch r.Method {
		case http.MethodPut:
			var body map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode edit request: %v", err)
			}
			if string(body["attachments"]) != "[]" {
				t.Errorf("attachments = %s, want [] to clear images", body["attachments"])
			}
			if string(body["format"]) != `"html"` {
				t.Errorf("format = %s", body["format"])
			}
			var text string
			if err := json.Unmarshal(body["text"], &text); err != nil {
				t.Fatalf("decode edit text: %v", err)
			}
			if text != "## Не Markdown\n<b>Исправлено</b>" {
				t.Errorf("HTML text was normalized: %q", text)
			}
			edited.Store(true)
		case http.MethodDelete:
			if !edited.Load() {
				t.Error("delete arrived before edit")
			}
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"success":true}`)
	}))
	defer server.Close()

	client := mustClient(t, server.URL, "token", server.Client())
	if err := client.Edit(context.Background(), EditRequest{
		MessageID:   "mid-7",
		Text:        "## Не Markdown\n<b>Исправлено</b>",
		Format:      FormatHTML,
		ImageTokens: []string{},
	}); err != nil {
		t.Fatalf("Edit() error = %v", err)
	}
	if err := client.Delete(context.Background(), "mid-7"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
}

func TestStructuredErrorPreservesRateLimitAndServerStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		status     int
		body       string
		code       string
		retryAfter string
	}{
		{name: "rate limit", status: http.StatusTooManyRequests, body: `{"code":"rate_limit","message":"slow down"}`, code: "rate_limit", retryAfter: "7"},
		{name: "server", status: http.StatusServiceUnavailable, body: `{"code":50301,"message":"unavailable"}`, code: "50301"},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", test.retryAfter)
				w.Header().Set("X-Request-Id", "request-123")
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()

			client := mustClient(t, server.URL, "token", server.Client())
			_, err := client.GetMe(context.Background())
			var apiErr *Error
			if !errors.As(err, &apiErr) {
				t.Fatalf("GetMe() error = %T %v, want *Error", err, err)
			}
			if apiErr.StatusCode != test.status || apiErr.Code != test.code || apiErr.RequestID != "request-123" || apiErr.Body != test.body {
				t.Fatalf("API error = %#v", apiErr)
			}
			if !apiErr.Temporary() {
				t.Error("Temporary() = false")
			}
			if test.retryAfter != "" && apiErr.RetryAfter != 7*time.Second {
				t.Errorf("RetryAfter = %s", apiErr.RetryAfter)
			}
		})
	}
}

func TestJSONResponseLimit(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, strings.Repeat("x", maxJSONResponseBytes+1))
	}))
	defer server.Close()

	client := mustClient(t, server.URL, "token", server.Client())
	_, err := client.GetMe(context.Background())
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("GetMe() error = %v, want response size error", err)
	}
}

func TestOversizedServerErrorStillPreservesStatus(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, strings.Repeat("x", maxJSONResponseBytes+1))
	}))
	defer server.Close()

	client := mustClient(t, server.URL, "token", server.Client())
	_, err := client.GetMe(context.Background())
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("GetMe() error = %T %v, want *Error", err, err)
	}
	if apiErr.StatusCode != http.StatusBadGateway || !strings.Contains(apiErr.Message, "exceeds") || len(apiErr.Body) != maxJSONResponseBytes {
		t.Fatalf("API error = %#v", apiErr)
	}
}

func TestUploadImageUsesPhotoTokenMapAndCanEditMessage(t *testing.T) {
	t.Parallel()

	uploadServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.TLS == nil {
			t.Errorf("unexpected upload request: %s TLS=%v", r.Method, r.TLS != nil)
		}
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("upload Authorization = %q, want empty", auth)
		}
		file, header, err := r.FormFile("data")
		if err != nil {
			t.Fatalf("read multipart data: %v", err)
		}
		defer func() {
			_ = file.Close()
		}()
		contents, err := io.ReadAll(file)
		if err != nil {
			t.Fatalf("read uploaded file: %v", err)
		}
		if header.Filename != "generated.png" || string(contents) != "PNG image bytes" {
			t.Errorf("uploaded %q = %q", header.Filename, contents)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"photos":{"8t/PabcNTw==":{"token":"image-token"}}}`)
	}))
	defer uploadServer.Close()

	var editCalled atomic.Bool
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "bot-token" {
			t.Errorf("MAX API Authorization = %q", auth)
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/uploads" && r.URL.Query().Get("type") == "image":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"url": uploadServer.URL + "/signed-upload"})
		case r.Method == http.MethodPut && r.URL.Path == "/messages" && r.URL.Query().Get("message_id") == "mid-with-new-image":
			var body struct {
				Attachments []struct {
					Type    string `json:"type"`
					Payload struct {
						Token string `json:"token"`
					} `json:"payload"`
				} `json:"attachments"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if len(body.Attachments) != 1 || body.Attachments[0].Type != "image" ||
				body.Attachments[0].Payload.Token != "image-token" {
				t.Fatalf("edit attachments = %#v", body.Attachments)
			}
			editCalled.Store(true)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"success":true}`)
		default:
			t.Errorf("unexpected MAX API request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer apiServer.Close()

	httpClient := uploadServer.Client()
	defer httpClient.CloseIdleConnections()
	client := mustClient(t, apiServer.URL, "bot-token", httpClient)
	result, err := client.UploadImage(context.Background(), "generated.png", strings.NewReader("PNG image bytes"))
	if err != nil {
		t.Fatalf("UploadImage() error = %v", err)
	}
	if result.Token != "image-token" {
		t.Fatalf("UploadImage() = %#v", result)
	}
	if err := client.Edit(context.Background(), EditRequest{
		MessageID: "mid-with-new-image", Text: "Пост с новой картинкой", Format: FormatMarkdown,
		ImageTokens: []string{result.Token},
	}); err != nil {
		t.Fatalf("Edit() error = %v", err)
	}
	if !editCalled.Load() {
		t.Fatal("uploaded image token was not attached by edit")
	}
}

func TestImageUploadTokenSupportsMAXResponseVariants(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		body      string
		fallbacks []string
		want      string
	}{
		{name: "top level", body: `{"token":"direct-token"}`, want: "direct-token"},
		{name: "photo map", body: `{"photos":{"second":{"token":"second-token"},"first":{"token":"first-token"}}}`, want: "first-token"},
		{name: "reservation fallback", body: `{}`, fallbacks: []string{"reservation-token"}, want: "reservation-token"},
		{name: "signed URL fallback", body: `not-json`, fallbacks: []string{"", "url-token"}, want: "url-token"},
		{name: "missing", body: `{"photos":{"empty":{"token":""}}}`, want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := imageUploadToken([]byte(test.body), test.fallbacks...); got != test.want {
				t.Fatalf("imageUploadToken() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestUploadImageReportsMissingTokenAsMAXAPIError(t *testing.T) {
	t.Parallel()
	uploadServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "upload-request-42")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer uploadServer.Close()
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"url": uploadServer.URL})
	}))
	defer apiServer.Close()

	client := mustClient(t, apiServer.URL, "bot-token", uploadServer.Client())
	_, err := client.UploadImage(context.Background(), "image.png", strings.NewReader("bytes"))
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusOK || apiErr.Code != "invalid_upload_response" ||
		apiErr.RequestID != "upload-request-42" {
		t.Fatalf("UploadImage() error = %#v, want typed MAX protocol error", err)
	}
}

func TestUploadImageRejectsHTTPRedirect(t *testing.T) {
	t.Parallel()

	var targetCalled atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetCalled.Store(true)
	}))
	defer target.Close()

	uploadServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("upload Authorization = %q, want empty", auth)
		}
		http.Redirect(w, r, target.URL+"/downgrade", http.StatusTemporaryRedirect)
	}))
	defer uploadServer.Close()

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"url": uploadServer.URL})
	}))
	defer apiServer.Close()

	httpClient := uploadServer.Client()
	defer httpClient.CloseIdleConnections()
	client := mustClient(t, apiServer.URL, "bot-token", httpClient)
	_, err := client.UploadImage(context.Background(), "image.png", strings.NewReader("bytes"))
	if err == nil || !strings.Contains(err.Error(), "unsafe image upload redirect") {
		t.Fatalf("UploadImage() error = %v, want unsafe redirect error", err)
	}
	if targetCalled.Load() {
		t.Error("insecure redirect target was contacted")
	}
}

func TestUploadImageRejectsInsecureReservationURL(t *testing.T) {
	t.Parallel()

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"url": "http://uploads.example.test/file"})
	}))
	defer apiServer.Close()

	client := mustClient(t, apiServer.URL, "bot-token", apiServer.Client())
	_, err := client.UploadImage(context.Background(), "image.png", strings.NewReader("bytes"))
	if err == nil || !strings.Contains(err.Error(), "absolute HTTPS") {
		t.Fatalf("UploadImage() error = %v, want HTTPS validation error", err)
	}
}

func TestEditReportsUnsuccessfulTwoHundredResponse(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"success":false,"message":"cannot edit"}`)
	}))
	defer server.Close()

	client := mustClient(t, server.URL, "token", server.Client())
	err := client.Edit(context.Background(), EditRequest{MessageID: "mid", Text: "text"})
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code != "operation_failed" || apiErr.Message != "cannot edit" {
		t.Fatalf("Edit() error = %#v", err)
	}
}

func mustClient(t *testing.T, baseURL, token string, httpClient *http.Client) *Client {
	t.Helper()
	client, err := New(baseURL, token, httpClient)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return client
}
