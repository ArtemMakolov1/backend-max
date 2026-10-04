package contentsearch

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
)

func TestMemeRequiresAssociatedImageAfterDNSAndFallsBack(t *testing.T) {
	for _, mode := range []string{"article_about_memes", "private_image", "unattributed_global_image"} {
		t.Run(mode, func(t *testing.T) {
			var fallback atomic.Int32
			client := testClient(t, Config{ExaAPIKey: fakeExaKey, TavilyAPIKey: fakeTavilyKey}, func(w http.ResponseWriter, r *http.Request) { fallback.Add(1); exaGood(w, r) }, func(w http.ResponseWriter, _ *http.Request) {
				switch mode {
				case "article_about_memes":
					jsonResponse(w, `{"results":[{"title":"Как делать мемы","url":"https://example.com/tutorial","content":"Статья о мемах"}]}`)
				case "private_image":
					jsonResponse(w, `{"results":[{"title":"Мем","url":"https://example.com/meme","content":"Подпись","images":[{"url":"https://private.example.com/meme.jpg"}]}]}`)
				case "unattributed_global_image":
					jsonResponse(w, `{"images":[{"url":"https://example.com/global.jpg"}],"results":[{"title":"Мем","url":"https://example.com/meme","content":"Подпись"}]}`)
				}
			})
			client.lookupNetIP = func(_ context.Context, _, host string) ([]netip.Addr, error) {
				if host == "private.example.com" {
					return []netip.Addr{netip.MustParseAddr("192.168.1.1")}, nil
				}
				return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
			}
			result, err := client.Search(context.Background(), Request{Query: "Мемы", ContentKind: "meme"})
			if err != nil || result.Provider != "exa" || fallback.Load() != 1 || len(result.Sources[0].Images) != 1 {
				t.Fatalf("unusable meme did not fall back: %v provider=%s calls=%d", err, result.Provider, fallback.Load())
			}
		})
	}
}

func TestImageOnlySourceRequiresSurvivingAttributedImage(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(map[bool]string{true: "public", false: "private"}[valid], func(t *testing.T) {
			client := testClient(t, Config{TavilyAPIKey: fakeTavilyKey}, nil, func(w http.ResponseWriter, _ *http.Request) {
				jsonResponse(w, `{"results":[{"title":"Смешной кот","url":"https://example.com/meme","content":"","images":[{"url":"https://cdn.example.com/cat.jpg","description":"Кот в коробке"}]}]}`)
			})
			if !valid {
				client.lookupNetIP = func(context.Context, string, string) ([]netip.Addr, error) {
					return []netip.Addr{netip.MustParseAddr("10.1.2.3")}, nil
				}
			}
			result, err := client.Search(context.Background(), Request{Query: "Мемы", ContentKind: "meme"})
			if valid {
				if err != nil || len(result.Sources) != 1 || result.Sources[0].Content != "" || result.Sources[0].Images[0].Description != "Кот в коробке" {
					t.Fatalf("valid image-only source rejected or text invented: %v", err)
				}
			} else if !errors.Is(err, ErrNoSources) {
				t.Fatalf("private image-only source accepted: %v", err)
			}
		})
	}
}

func TestSourceFilterRejectsVideoListingAndTriggersFallback(t *testing.T) {
	var primary, fallback atomic.Int32
	client := testClient(t, Config{ExaAPIKey: fakeExaKey, TavilyAPIKey: fakeTavilyKey}, func(w http.ResponseWriter, _ *http.Request) {
		fallback.Add(1)
		jsonResponse(w, `{"results":[{"title":"Настоящий ролик","url":"https://www.youtube.com/watch?v=real","highlights":["Описание ролика"],"image":"https://cdn.example.com/clip.jpg"}]}`)
	}, func(w http.ResponseWriter, _ *http.Request) {
		primary.Add(1)
		jsonResponse(w, `{"results":[{"title":"Список смешных видео","url":"https://example.com/videos","content":"Перечень роликов"}]}`)
	})
	request := Request{Query: "Смешное видео", ContentKind: "video", SourceFilter: func(source Source) bool { return strings.HasPrefix(source.URL, "https://www.youtube.com/watch?") }}
	result, err := client.Search(context.Background(), request)
	if err != nil || result.Provider != "exa" || primary.Load() != 1 || fallback.Load() != 1 || len(result.Sources) != 1 || !result.Sources[0].Images[0].PreviewOnly {
		t.Fatalf("video listing did not fall back: %v provider=%s calls=%d/%d", err, result.Provider, primary.Load(), fallback.Load())
	}
	// The default policy deliberately remains generic: app-specific URL rules
	// stay in the server-owned callback rather than duplicated in the client.
	request.SourceFilter = nil
	result, err = client.Search(context.Background(), request)
	if err != nil || result.Provider != "tavily" || primary.Load() != 2 || fallback.Load() != 1 {
		t.Fatalf("nil source policy changed generic retrieval: %v", err)
	}
}

func TestExaArticleCarriesMetadataPreviewWithoutExtraExtraction(t *testing.T) {
	client := testClient(t, Config{ExaAPIKey: fakeExaKey}, exaGood, nil)
	result, err := client.Search(context.Background(), Request{Query: "Новости", ContentKind: "article"})
	if err != nil || len(result.Sources[0].Images) != 1 || !result.Sources[0].Images[0].PreviewOnly {
		t.Fatalf("article preview metadata was lost: %v", err)
	}
}
