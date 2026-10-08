package contentsearch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestUnsafeSourceAndImageURLs(t *testing.T) {
	client := &Client{exaKey: fakeExaKey, tavilyKey: fakeTavilyKey}
	for _, raw := range []string{
		"http://example.com/a", "https://localhost/a", "https://127.0.0.1/a", "https://169.254.169.254/latest",
		"https://192.168.1.1/a", "https://[::1]/a", "https://[2001:4860:4860::8888]/a", "https://2130706433/a",
		"https://0x7f000001/a", "https://service.local/a", "https://service.internal/a", "https://service.lan/a",
		"https://service.test/a", "https://service.home.arpa/a", "https://service.invalid/a", "https://service.onion/a",
		"https://user:pass@example.com/a", "https://example.com:8443/a", "https://example.com./a", "https://example.com\\@localhost/a",
		"https://example.com/a\n", "https://example.com/?key=" + fakeExaKey, "https://example.com/?key=%73ynthetic-exa-key",
		"https://example.com/" + strings.Repeat("a", 4096),
	} {
		// Whitespace at the very end is trimmed intentionally, so a real embedded
		// newline is used here rather than an otherwise clean pasted URL.
		if strings.HasSuffix(raw, "\n") {
			raw += "tail"
		}
		if safe := client.safeURL(raw); safe != "" {
			t.Errorf("unsafe URL accepted: %q", raw)
		}
		if images := client.appendImage(nil, Image{URL: raw}); len(images) != 0 {
			t.Errorf("unsafe image accepted: %q", raw)
		}
	}
	if safe := client.safeURL("https://EXAMPLE.com:443/a#section"); safe != "https://example.com/a" {
		t.Fatalf("URL normalization failed: %q", safe)
	}
}

func TestTavilyImagesRequirePerResultAttributionAndAreDeduplicated(t *testing.T) {
	client := testClient(t, Config{TavilyAPIKey: fakeTavilyKey}, nil, func(w http.ResponseWriter, _ *http.Request) {
		jsonResponse(w, `{"images":[{"url":"https://example.com/unattributed.jpg","description":"global image"}],"results":[
		{"title":"Первая","url":"https://example.com/page#first","content":"Фрагмент","published_date":"Tue, 11 Mar 2025 17:00:00 GMT","images":[{"url":"https://cdn.example.com/one.jpg","description":"Принадлежит первой"},{"url":"https://cdn.example.com/one.jpg","description":"Дубликат"},{"url":"https://localhost/private.jpg"}]},
		{"title":"Первая дубликат","url":"https://EXAMPLE.com:443/page#second","content":"Другой фрагмент","images":[{"url":"https://cdn.example.com/two.jpg"}]},
		{"title":"Вторая","url":"https://example.org/page","raw_content":"Извлечённый текст","images":[]}]}`)
	})
	result, err := client.Search(context.Background(), Request{Query: "Мемы", ContentKind: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Sources) != 2 || len(result.Sources[0].Images) != 2 || len(result.Sources[1].Images) != 0 || result.Sources[1].Content != "Извлечённый текст" || result.Sources[0].PublishedAt != "2025-03-11T17:00:00Z" {
		t.Fatalf("bad attribution/dedup: %#v", result.Sources)
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "unattributed") || strings.Contains(string(encoded), "localhost") {
		t.Fatal("unattributed or unsafe image escaped")
	}
}

func TestExaImageLinksAreOriginalAndMetadataImageIsPreview(t *testing.T) {
	client := testClient(t, Config{ExaAPIKey: fakeExaKey}, func(w http.ResponseWriter, _ *http.Request) {
		jsonResponse(w, `{"results":[{"title":"Мем","url":"https://example.com/meme","highlights":[],"image":"https://cdn.example.com/preview.jpg","extras":{"imageLinks":["https://cdn.example.com/original.jpg","https://cdn.example.com/original.jpg"],"richImageLinks":[{"url":"https://cdn.example.com/original.jpg","alt":"Кот и коробка"}]}}]}`)
	}, nil)
	result, err := client.Search(context.Background(), Request{Query: "Мемы", ContentKind: "meme"})
	if err != nil {
		t.Fatal(err)
	}
	images := result.Sources[0].Images
	if len(images) != 2 || images[0].URL != "https://cdn.example.com/original.jpg" || images[0].PreviewOnly || images[0].Description != "Кот и коробка" || !images[1].PreviewOnly {
		t.Fatalf("incorrect image authority: %#v", images)
	}
}

func TestEmptyContentAndImageOnlyGlobalResultsAreNotUsable(t *testing.T) {
	client := testClient(t, Config{TavilyAPIKey: fakeTavilyKey}, nil, func(w http.ResponseWriter, _ *http.Request) {
		jsonResponse(w, `{"images":["https://example.com/global.jpg"],"results":[{"title":"Only metadata","url":"https://example.com/page","content":""}]}`)
	})
	if _, err := client.Search(context.Background(), Request{Query: "Мемы", ContentKind: "meme"}); !errors.Is(err, ErrNoSources) {
		t.Fatalf("unusable metadata was treated as content: %v", err)
	}
}

func TestSourceAndImageCountLimits(t *testing.T) {
	client := &Client{}
	input := make([]tavilySource, maxSources+5)
	for i := range input {
		input[i].Title = "Source"
		input[i].URL = "https://example.com/" + strings.Repeat("a", i+1)
		input[i].Content = "Content"
		for j := 0; j < maxSourceImages+4; j++ {
			input[i].Images = append(input[i].Images, struct {
				URL         string `json:"url"`
				Description string `json:"description"`
			}{URL: "https://cdn.example.com/" + strings.Repeat("a", j+1) + ".jpg"})
		}
	}
	sources := client.tavilySources(input, true)
	if len(sources) != maxSources {
		t.Fatalf("source limit not enforced: %d", len(sources))
	}
	for _, source := range sources {
		if len(source.Images) != maxSourceImages {
			t.Fatalf("image limit not enforced: %d", len(source.Images))
		}
	}
}
