package maxclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEditChatDescriptionPreservesUnicodeAndCanClear(t *testing.T) {
	t.Parallel()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPatch || r.URL.Path != "/chats/-5" {
			t.Errorf("unexpected description request: %s %s", r.Method, r.URL.Path)
		}
		fields := map[string]json.RawMessage{}
		if err := json.NewDecoder(r.Body).Decode(&fields); err != nil {
			t.Error(err)
		}
		if len(fields) != 1 {
			t.Errorf("description-only patch changed other fields: %#v", fields)
		}
		var description string
		if err := json.Unmarshal(fields["description"], &description); err != nil {
			t.Errorf("empty description must remain an explicit field: %v", err)
		}
		want := strings.Repeat("🦊", 16000)
		if calls == 2 {
			want = ""
		}
		if description != want {
			t.Errorf("description request length=%d, want preserved input length=%d", len(description), len(want))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"chat_id": -5, "type": "channel", "status": "active", "description": description})
	}))
	defer server.Close()
	client := mustClient(t, server.URL, "token", server.Client())
	tooLong := strings.Repeat("🦊", 16001)
	if _, err := client.EditChat(context.Background(), "-5", ChatPatch{Description: &tooLong}); err == nil || calls != 0 {
		t.Fatalf("oversized Unicode description reached MAX: %v, calls=%d", err, calls)
	}
	for _, value := range []string{strings.Repeat("🦊", 16000), ""} {
		chat, err := client.EditChat(context.Background(), "-5", ChatPatch{Description: &value})
		if err != nil || chat.Description != value {
			t.Fatalf("description result length=%d, %v", len(chat.Description), err)
		}
	}
	if calls != 2 {
		t.Fatalf("description requests=%d, want2", calls)
	}
}
