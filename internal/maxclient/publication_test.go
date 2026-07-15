package maxclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPublicationMetadataAndPinContracts(t *testing.T) {
	t.Parallel()
	const chatID = "-13549123"
	const messageID = "mid.ffffbdb48e6c3775019d496b34394b84"
	var pinCalls, unpinCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/messages/"+messageID:
			_, _ = io.WriteString(w, `{"recipient":{"chat_id":-13549123},"body":{"mid":"`+messageID+`","text":"Привет"},"url":"https://max.ru/se13549123_biz/abc","stat":{"views":42}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/chats/"+chatID+"/pin":
			_, _ = io.WriteString(w, `{"message":{"recipient":{"chat_id":-13549123},"body":{"mid":"`+messageID+`"},"url":"https://max.ru/se13549123_biz/abc","stat":{"views":43}}}`)
		case r.Method == http.MethodPut && r.URL.Path == "/chats/"+chatID+"/pin":
			pinCalls++
			var body struct {
				MessageID string `json:"message_id"`
				Notify    *bool  `json:"notify"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.MessageID != messageID || body.Notify == nil || *body.Notify {
				t.Errorf("pin body = %#v", body)
			}
			_, _ = io.WriteString(w, `{"success":true}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/chats/"+chatID+"/pin":
			unpinCalls++
			_, _ = io.WriteString(w, `{"success":true}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := mustClient(t, server.URL, "token", server.Client())
	message, err := client.GetMessage(context.Background(), messageID)
	if err != nil {
		t.Fatal(err)
	}
	if message.MessageID != messageID || message.ChatID != chatID || message.URL != "https://max.ru/se13549123_biz/abc" ||
		message.Views == nil || *message.Views != 42 {
		t.Fatalf("message = %#v", message)
	}
	pinned, err := client.GetPinnedMessage(context.Background(), chatID)
	if err != nil {
		t.Fatal(err)
	}
	if pinned == nil || pinned.MessageID != messageID || pinned.ChatID != chatID || pinned.Views == nil || *pinned.Views != 43 {
		t.Fatalf("pinned = %#v", pinned)
	}
	if err := client.PinMessage(context.Background(), chatID, messageID); err != nil {
		t.Fatal(err)
	}
	if err := client.UnpinMessage(context.Background(), chatID); err != nil {
		t.Fatal(err)
	}
	if pinCalls != 1 || unpinCalls != 1 {
		t.Fatalf("pin calls = %d, unpin calls = %d", pinCalls, unpinCalls)
	}
}

func TestGetPinnedMessageAcceptsOfficialNullResponse(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"message":null}`)
	}))
	defer server.Close()
	client := mustClient(t, server.URL, "token", server.Client())
	pinned, err := client.GetPinnedMessage(context.Background(), "-1")
	if err != nil || pinned != nil {
		t.Fatalf("GetPinnedMessage() = %#v, %v; want nil, nil", pinned, err)
	}
}

func TestGetPinnedMessageAcceptsLegacyDirectResponseDuringMigration(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"recipient":{"chat_id":-1},"body":{"mid":"mid.legacy"}}`)
	}))
	defer server.Close()
	client := mustClient(t, server.URL, "token", server.Client())
	pinned, err := client.GetPinnedMessage(context.Background(), "-1")
	if err != nil || pinned == nil || pinned.MessageID != "mid.legacy" || pinned.ChatID != "-1" {
		t.Fatalf("GetPinnedMessage() = %#v, %v", pinned, err)
	}
}

func TestPublicationMetadataPreservesNotFoundAndRejectsUnsafeIDs(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"code":"message.not.found","message":"Message not found"}`)
	}))
	defer server.Close()
	client := mustClient(t, server.URL, "token", server.Client())
	_, err := client.GetPinnedMessage(context.Background(), "-1")
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound || apiErr.Code != "message.not.found" {
		t.Fatalf("GetPinnedMessage() error = %#v", err)
	}

	for _, messageID := range []string{"", "../me", "mid/value", "mid?token=x", " mid.1"} {
		if _, err := client.GetMessage(context.Background(), messageID); err == nil {
			t.Errorf("GetMessage(%q) accepted unsafe ID", messageID)
		}
		if err := client.PinMessage(context.Background(), "-1", messageID); err == nil {
			t.Errorf("PinMessage(%q) accepted unsafe ID", messageID)
		}
	}
	for _, chatID := range []string{"", "-1/pin", "1.5"} {
		if _, err := client.GetPinnedMessage(context.Background(), chatID); err == nil {
			t.Errorf("GetPinnedMessage(%q) accepted unsafe chat ID", chatID)
		}
		if err := client.UnpinMessage(context.Background(), chatID); err == nil {
			t.Errorf("UnpinMessage(%q) accepted unsafe chat ID", chatID)
		}
	}
}

func TestGetMessageRejectsMismatchedOrNegativeMetadata(t *testing.T) {
	t.Parallel()
	tests := []string{
		`{"recipient":{"chat_id":-1},"body":{"mid":"mid.other"},"stat":{"views":1}}`,
		`{"recipient":{"chat_id":-1},"body":{"mid":"mid.requested"},"stat":{"views":-1}}`,
		`{"recipient":{"chat_id":"not-numeric"},"body":{"mid":"mid.requested"},"stat":{"views":1}}`,
	}
	for _, response := range tests {
		response := response
		t.Run(response, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, response)
			}))
			defer server.Close()
			client := mustClient(t, server.URL, "token", server.Client())
			if _, err := client.GetMessage(context.Background(), "mid.requested"); err == nil {
				t.Fatal("malformed MAX metadata was accepted")
			}
		})
	}
}
