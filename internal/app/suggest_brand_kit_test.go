package app

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"maxpilot/backend/internal/media"
	"maxpilot/backend/internal/openairesearch"
	"maxpilot/backend/internal/store"
)

type fakeBrandKitSuggester struct {
	mu       sync.Mutex
	requests []openairesearch.SuggestBrandKitRequest
	result   openairesearch.SuggestBrandKitResult
	err      error
}

func (f *fakeBrandKitSuggester) Generate(context.Context, openairesearch.Request) (openairesearch.Result, error) {
	return openairesearch.Result{}, errors.New("research generation is not expected in brand kit tests")
}

func (f *fakeBrandKitSuggester) SuggestBrandKit(_ context.Context, request openairesearch.SuggestBrandKitRequest) (openairesearch.SuggestBrandKitResult, error) {
	f.mu.Lock()
	f.requests = append(f.requests, request)
	f.mu.Unlock()
	return f.result, f.err
}

func newBrandKitSuggestionFixture(t *testing.T, research ResearchClient) (*App, *store.Store, string) {
	t.Helper()
	ctx := context.Background()
	storage, err := store.Open(ctx, filepath.Join(t.TempDir(), "brand-kit-suggest.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	mediaStore, err := media.New(t.TempDir(), "http://localhost:8080")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	application := New(storage, mediaStore, nil, nil, research, logger)
	if err := storage.UpsertUser(ctx, store.User{ID: "bk-owner", DisplayName: "Owner"}); err != nil {
		t.Fatal(err)
	}
	workspace, err := storage.CreateWorkspace(ctx, "bk-owner", store.Workspace{Name: "Brand"})
	if err != nil {
		t.Fatal(err)
	}
	return application, storage, workspace.ID
}

func createBrandKitPost(t *testing.T, storage *store.Store, workspaceID string, post store.Post) store.Post {
	t.Helper()
	if post.Format == "" {
		post.Format = store.FormatMarkdown
	}
	created, err := storage.CreatePostForWorkspace(context.Background(), "bk-owner", workspaceID, post)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func brandKitTestPNG(t *testing.T, shade uint8) []byte {
	t.Helper()
	canvas := image.NewRGBA(image.Rect(0, 0, 2, 2))
	canvas.Set(0, 0, color.RGBA{R: shade, G: shade, B: shade, A: 255})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, canvas); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func TestSuggestBrandKitOrdersPublishedFirstAndSkipsEmptyPosts(t *testing.T) {
	fake := &fakeBrandKitSuggester{result: openairesearch.SuggestBrandKitResult{Tone: "Дружелюбный"}}
	application, storage, workspaceID := newBrandKitSuggestionFixture(t, fake)
	createBrandKitPost(t, storage, workspaceID, store.Post{Title: "Черновик", Content: "Старый черновик о команде."})
	createBrandKitPost(t, storage, workspaceID, store.Post{
		Title: "Запуск", Content: "Запустили интеграцию с каналами.", Status: store.PostStatusPublished,
	})
	createBrandKitPost(t, storage, workspaceID, store.Post{
		Title: "Разбор", Content: "Разбираем контент-план на месяц.", Status: store.PostStatusPublished, Format: store.FormatHTML,
	})
	createBrandKitPost(t, storage, workspaceID, store.Post{Title: "Пустой", Content: "  \n\t"})
	createBrandKitPost(t, storage, workspaceID, store.Post{Title: "Свежий черновик", Content: "Свежий черновик про запуск."})

	result, err := application.SuggestBrandKit(context.Background(), "bk-owner", workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Tone != "Дружелюбный" {
		t.Fatalf("SuggestBrandKit() = %#v", result)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) != 1 {
		t.Fatalf("suggest requests = %#v", fake.requests)
	}
	wantTexts := []string{
		"Разбираем контент-план на месяц.",
		"Запустили интеграцию с каналами.",
		"Свежий черновик про запуск.",
		"Старый черновик о команде.",
	}
	sent := fake.requests[0]
	if len(sent.Posts) != len(wantTexts) {
		t.Fatalf("sampled posts = %#v", sent.Posts)
	}
	for index, want := range wantTexts {
		if sent.Posts[index].Text != want {
			t.Fatalf("sampled post %d = %#v, want text %q", index, sent.Posts[index], want)
		}
	}
	if sent.Posts[0].Format != store.FormatHTML || sent.Posts[1].Format != store.FormatMarkdown {
		t.Fatalf("sampled formats = %#v", sent.Posts)
	}
	if len(sent.Images) != 0 {
		t.Fatalf("imageless workspace produced images: %#v", sent.Images)
	}
}

func TestSuggestBrandKitTruncatesMaterialToTotalBudget(t *testing.T) {
	fake := &fakeBrandKitSuggester{result: openairesearch.SuggestBrandKitResult{Tone: "Тон"}}
	application, storage, workspaceID := newBrandKitSuggestionFixture(t, fake)
	for range 4 {
		createBrandKitPost(t, storage, workspaceID, store.Post{
			Title: "Длинный", Content: strings.Repeat("я", 4000),
		})
	}
	if _, err := application.SuggestBrandKit(context.Background(), "bk-owner", workspaceID); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	sent := fake.requests[0]
	if len(sent.Posts) != 3 {
		t.Fatalf("budgeted sample size = %d", len(sent.Posts))
	}
	total := 0
	for _, post := range sent.Posts {
		total += utf8.RuneCountInString(post.Text)
	}
	if total > openairesearch.MaxSuggestBrandKitTotalRunes {
		t.Fatalf("sample exceeds the rune budget: %d", total)
	}
}

func TestSuggestBrandKitRequiresEnoughPostsWithText(t *testing.T) {
	fake := &fakeBrandKitSuggester{}
	application, storage, workspaceID := newBrandKitSuggestionFixture(t, fake)
	createBrandKitPost(t, storage, workspaceID, store.Post{Title: "Один", Content: "Первый пост."})
	createBrandKitPost(t, storage, workspaceID, store.Post{Title: "Два", Content: "Второй пост."})
	createBrandKitPost(t, storage, workspaceID, store.Post{Title: "Пустой", Content: "   "})

	_, err := application.SuggestBrandKit(context.Background(), "bk-owner", workspaceID)
	if !errors.Is(err, ErrNotEnoughPostsForBrandKit) {
		t.Fatalf("SuggestBrandKit() error = %v, want ErrNotEnoughPostsForBrandKit", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) != 0 {
		t.Fatalf("insufficient material reached suggester: %#v", fake.requests)
	}
}

func TestSuggestBrandKitCollectsPostImagesAndSkipsBrokenOnes(t *testing.T) {
	fake := &fakeBrandKitSuggester{result: openairesearch.SuggestBrandKitResult{Tone: "Тон"}}
	application, storage, workspaceID := newBrandKitSuggestionFixture(t, fake)
	ctx := context.Background()

	imageContents := [][]byte{
		brandKitTestPNG(t, 10), brandKitTestPNG(t, 20), brandKitTestPNG(t, 30), brandKitTestPNG(t, 40),
	}
	for _, content := range imageContents {
		file, err := application.Media().Save(ctx, "cover.png", bytes.NewReader(content))
		if err != nil {
			t.Fatal(err)
		}
		createBrandKitPost(t, storage, workspaceID, store.Post{
			Title: "С картинкой", Content: "Пост с обложкой номер " + file.Filename,
			ImageURL: file.URL, ImagePath: file.Path,
		})
	}
	// A dangling image path must be skipped silently without failing the call.
	createBrandKitPost(t, storage, workspaceID, store.Post{
		Title: "Битый", Content: "Пост с удалённой обложкой.",
		ImagePath: strings.Repeat("0", 64) + ".png",
	})

	if _, err := application.SuggestBrandKit(ctx, "bk-owner", workspaceID); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	sent := fake.requests[0]
	if len(sent.Posts) != 5 {
		t.Fatalf("sampled posts = %d", len(sent.Posts))
	}
	if len(sent.Images) != openairesearch.MaxSuggestBrandKitImages {
		t.Fatalf("collected images = %d, want %d", len(sent.Images), openairesearch.MaxSuggestBrandKitImages)
	}
	for _, sentImage := range sent.Images {
		if sentImage.MIME != "image/png" || len(sentImage.Data) == 0 {
			t.Fatalf("collected image = %#v", sentImage)
		}
	}
}

func TestSuggestBrandKitRequiresConfiguredSuggester(t *testing.T) {
	application, storage, workspaceID := newBrandKitSuggestionFixture(t, nil)
	createBrandKitPost(t, storage, workspaceID, store.Post{Title: "Один", Content: "Первый пост."})
	if _, err := application.SuggestBrandKit(context.Background(), "bk-owner", workspaceID); !errors.Is(err, ErrResearchNotConfigured) {
		t.Fatalf("SuggestBrandKit() without research error = %v", err)
	}
	if application.BrandKitSuggestionConfigured() {
		t.Fatal("BrandKitSuggestionConfigured() = true without a research client")
	}
}
