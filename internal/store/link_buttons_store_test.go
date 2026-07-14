package store

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPostLinkButtonsPersistUpdateAndDuplicate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "link-buttons.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })

	created, err := storage.CreatePost(ctx, Post{
		Title: "Buttons", Content: "Body", Format: FormatMarkdown,
		LinkButtons: []LinkButton{{Text: "Сайт", URL: "https://example.com"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(created.LinkButtons, []LinkButton{{Text: "Сайт", URL: "https://example.com"}}) {
		t.Fatalf("created link buttons = %#v", created.LinkButtons)
	}

	changedButtons := []LinkButton{
		{Text: "Каталог", URL: "https://example.com/catalog"},
		{Text: "Черновик", URL: "https://"},
	}
	updated, err := storage.UpdatePost(ctx, created.ID, PostChanges{LinkButtons: &changedButtons})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(updated.LinkButtons, changedButtons) {
		t.Fatalf("updated link buttons = %#v", updated.LinkButtons)
	}

	duplicate, err := storage.DuplicatePost(ctx, updated.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(duplicate.LinkButtons, changedButtons) {
		t.Fatalf("duplicate link buttons = %#v", duplicate.LinkButtons)
	}
	if duplicate.LinkButtons == nil {
		t.Fatal("duplicate returned nil link_buttons")
	}

	cleared := []LinkButton{}
	updated, err = storage.UpdatePost(ctx, updated.ID, PostChanges{LinkButtons: &cleared})
	if err != nil {
		t.Fatal(err)
	}
	if updated.LinkButtons == nil || len(updated.LinkButtons) != 0 {
		t.Fatalf("cleared link buttons = %#v, want non-nil empty array", updated.LinkButtons)
	}
}
