package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"maxpilot/backend/internal/maxclient"
	"maxpilot/backend/internal/store"
)

func TestPublishedPostWithoutGalleryReturnsEmptyArrays(t *testing.T) {
	fixture := newWorkspaceAPIFixture(t)
	message := maxHistoryTextMessage("mid.empty-gallery", "Текст без вложений", "52", false, 1_785_700_000_123)
	fake := newMAXHistoryAPIFake(fixture.channel, 1, maxHistoryAPIScript{messages: []maxclient.HistoryMessage{message}})
	handler := withTestSession(t, fixture.storage, maxHistoryAPIHandler(t, fixture, fake), "ws-owner")
	response := performJSONRequest(handler, http.MethodPost, maxHistoryImportPath(fixture), "")
	if response.Code != http.StatusOK {
		t.Fatalf("import status = %d", response.Code)
	}
	posts, err := fixture.storage.ListPostsForWorkspace(t.Context(), "ws-owner", fixture.workspace.ID, store.PostStatusPublished, &fixture.channel.ID)
	if err != nil || len(posts) != 1 {
		t.Fatalf("imported posts count = %d, error = %v", len(posts), err)
	}
	// Viewer access must remain valid for an imported publication authored by
	// someone other than the bot; viewing it does not require posts.write.
	viewer := fixture.handler(t, "ws-viewer")
	base := "/api/v1/workspaces/" + fixture.workspace.ID + "/posts"
	for _, path := range []string{base + "/" + postID(posts[0].ID), base + "?status=published"} {
		response = performJSONRequest(viewer, http.MethodGet, path, "")
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d", path, response.Code)
		}
		var payload map[string]json.RawMessage
		if path == base+"?status=published" {
			var items []map[string]json.RawMessage
			if err := json.Unmarshal(response.Body.Bytes(), &items); err != nil || len(items) != 1 {
				t.Fatalf("post list length = %d, error = %v", len(items), err)
			}
			payload = items[0]
		} else if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"attachments", "link_buttons"} {
			if string(payload[field]) != "[]" {
				t.Fatalf("%s for %s = %s, want []", field, path, payload[field])
			}
		}
	}
}

func TestPostJSONNormalizesNilAndKeepsPopulatedGalleryPrivate(t *testing.T) {
	for _, attachments := range [][]store.PostAttachment{nil, {}} {
		post := store.Post{Attachments: attachments}
		data, err := json.Marshal(post)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Fatal(err)
		}
		if string(payload["attachments"]) != "[]" || string(payload["link_buttons"]) != "[]" {
			t.Fatalf("empty post collections = %s/%s", payload["attachments"], payload["link_buttons"])
		}
		if attachments == nil && post.Attachments != nil {
			t.Fatal("serialization mutated the caller's nil slice")
		}
	}
	post := store.Post{Attachments: []store.PostAttachment{{ID: 7, Type: store.PostAttachmentImage, URL: "https://media.max.ru/image.jpg", ProviderToken: "private-provider-token", StorageKey: "private-storage-key"}}}
	data, err := json.Marshal(post)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	var attachments []map[string]json.RawMessage
	if err := json.Unmarshal(payload["attachments"], &attachments); err != nil || len(attachments) != 1 {
		t.Fatalf("gallery length = %d, error = %v", len(attachments), err)
	}
	if string(attachments[0]["id"]) != "7" || attachments[0]["provider_token"] != nil || attachments[0]["storage_key"] != nil {
		t.Fatalf("public attachment projection changed: %v", attachments[0])
	}
}
