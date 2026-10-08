package contentsearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

const fakeExaKey = "synthetic-exa-key"
const fakeTavilyKey = "synthetic-tavily-key"

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Transport injection rewrites only fixed provider URLs to local test servers.
// Production Config never accepts an endpoint or arbitrary URL from the caller.
func testClient(t *testing.T, config Config, exa, tavily http.HandlerFunc) *Client {
	t.Helper()
	servers := map[string]*httptest.Server{}
	if exa != nil {
		servers["api.exa.ai"] = httptest.NewServer(exa)
	}
	if tavily != nil {
		servers["api.tavily.com"] = httptest.NewServer(tavily)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	t.Cleanup(func() {
		transport.CloseIdleConnections()
		for _, server := range servers {
			server.CloseClientConnections()
			server.Close()
		}
	})
	client, err := newClient(config, &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Path != "/search" {
			return nil, errors.New("unexpected test endpoint")
		}
		server, ok := servers[r.URL.Host]
		if !ok {
			return nil, errors.New("unexpected test provider")
		}
		target, err := url.Parse(server.URL)
		if err != nil {
			return nil, err
		}
		copyRequest := r.Clone(r.Context())
		copyURL := *r.URL
		copyRequest.URL = &copyURL
		copyRequest.URL.Scheme, copyRequest.URL.Host = target.Scheme, target.Host
		copyRequest.Host = target.Host
		return transport.RoundTrip(copyRequest)
	})})
	if err != nil {
		t.Fatal(err)
	}
	client.lookupNetIP = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	return client
}

func jsonResponse(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, body)
}

func exaGood(w http.ResponseWriter, _ *http.Request) {
	jsonResponse(w, `{"results":[{"title":"Exa источник","url":"https://example.com/article","publishedDate":"2026-10-04T12:00:00Z","highlights":["Проверенный фрагмент"],"image":"https://cdn.example.com/image.jpg"}]}`)
}

func tavilyGood(w http.ResponseWriter, _ *http.Request) {
	jsonResponse(w, `{"results":[{"title":"Tavily источник","url":"https://example.org/article","content":"Проверенный фрагмент","raw_content":null,"images":[{"url":"https://cdn.example.com/image.jpg"}]}]}`)
}

func TestRoutingAndProviderRequestSchema(t *testing.T) {
	for _, kind := range []string{"idea", "article", "auto", "meme", "video", ""} {
		t.Run(kind, func(t *testing.T) {
			var order []string
			var mu sync.Mutex
			handler := func(provider string, next http.HandlerFunc) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					order = append(order, provider)
					mu.Unlock()
					if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
						t.Error("incorrect provider method or content type")
					}
					var payload map[string]json.RawMessage
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Error(err)
					}
					var query string
					_ = json.Unmarshal(payload["query"], &query)
					if query != "смешные коты" {
						t.Errorf("query normalization: %q", query)
					}
					media := kind == "auto" || kind == "meme" || kind == ""
					if provider == "exa" {
						if r.Header.Get("x-api-key") != fakeExaKey || r.Header.Get("Authorization") != "" {
							t.Error("incorrect Exa authentication")
						}
						if len(payload) != 3 || string(payload["type"]) != `"auto"` {
							t.Error("Exa request decorated beyond query/type/contents")
						}
						var contents map[string]json.RawMessage
						_ = json.Unmarshal(payload["contents"], &contents)
						if string(contents["highlights"]) != "true" || (contents["extras"] != nil) != media {
							t.Error("incorrect Exa contents options")
						}
					} else {
						if r.Header.Get("Authorization") != "Bearer "+fakeTavilyKey || r.Header.Get("x-api-key") != "" {
							t.Error("incorrect Tavily authentication")
						}
						if string(payload["search_depth"]) != `"basic"` || string(payload["language"]) != `"ru"` || string(payload["max_results"]) != "10" || payload["filter_by_language"] != nil || payload["include_raw_content"] != nil {
							t.Error("incorrect Tavily extraction/ranking options")
						}
						includeImages := media || kind == "video"
						if (string(payload["include_images"]) == "true") != includeImages || (string(payload["include_image_descriptions"]) == "true") != includeImages {
							t.Error("incorrect Tavily image options")
						}
					}
					next(w, r)
				}
			}
			client := testClient(t, Config{ExaAPIKey: fakeExaKey, TavilyAPIKey: fakeTavilyKey}, handler("exa", exaGood), handler("tavily", tavilyGood))
			result, err := client.Search(context.Background(), Request{Query: "  смешные\nкоты  ", ContentKind: kind})
			if err != nil {
				t.Fatal(err)
			}
			want := "tavily"
			if kind == "idea" || kind == "article" {
				want = "exa"
			}
			if result.Provider != want || !reflect.DeepEqual(order, []string{want}) || len(result.Sources) != 1 {
				t.Fatalf("wrong routing result: provider=%s, calls=%v", result.Provider, order)
			}
		})
	}
}

func TestFallbackIsBoundedAndOnlyOnFailureOrEmptyUsableSources(t *testing.T) {
	for _, failure := range []string{"http_error", "empty", "unsafe", "invalid_schema", "missing_schema", "invalid_json"} {
		t.Run(failure, func(t *testing.T) {
			var primary, fallback atomic.Int32
			client := testClient(t, Config{ExaAPIKey: fakeExaKey, TavilyAPIKey: fakeTavilyKey}, func(w http.ResponseWriter, r *http.Request) {
				primary.Add(1)
				switch failure {
				case "http_error":
					w.WriteHeader(http.StatusTooManyRequests)
				case "empty":
					jsonResponse(w, `{"results":[]}`)
				case "unsafe":
					jsonResponse(w, `{"results":[{"title":"Local","url":"https://localhost/private","highlights":["internal"]}]}`)
				case "invalid_schema":
					jsonResponse(w, `{"results":[{"highlights":"wrong shape"}]}`)
				case "missing_schema":
					jsonResponse(w, `{"answer":"no results field"}`)
				case "invalid_json":
					jsonResponse(w, `{"results":[]}{"another":"document"}`)
				}
			}, func(w http.ResponseWriter, r *http.Request) { fallback.Add(1); tavilyGood(w, r) })
			result, err := client.Search(context.Background(), Request{Query: "Новости", ContentKind: "article"})
			if err != nil || result.Provider != "tavily" || primary.Load() != 1 || fallback.Load() != 1 {
				t.Fatalf("fallback failed: %v provider=%s calls=%d/%d", err, result.Provider, primary.Load(), fallback.Load())
			}
		})
	}
	var calls atomic.Int32
	client := testClient(t, Config{ExaAPIKey: fakeExaKey, TavilyAPIKey: fakeTavilyKey}, func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); jsonResponse(w, `{"results":[]}`) }, func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); jsonResponse(w, `{"results":[]}`) })
	_, err := client.Search(context.Background(), Request{Query: "Новости"})
	if !errors.Is(err, ErrNoSources) || calls.Load() != 2 {
		t.Fatalf("empty results must stop after two requests: %v, calls=%d", err, calls.Load())
	}
}

func TestUnconfiguredProviderIsSkipped(t *testing.T) {
	client := testClient(t, Config{ExaAPIKey: fakeExaKey}, exaGood, nil)
	result, err := client.Search(context.Background(), Request{Query: "Видео", ContentKind: "video"})
	if err != nil || result.Provider != "exa" {
		t.Fatalf("one configured provider should work: %v", err)
	}
	empty, err := NewClient(Config{})
	if err != nil || empty.Configured() {
		t.Fatalf("unconfigured client: %v", err)
	}
	_, err = empty.Search(context.Background(), Request{Query: "Новости"})
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatal(err)
	}
}

func TestCancellationNeverStartsFallback(t *testing.T) {
	started := make(chan struct{})
	var fallback atomic.Int32
	client := testClient(t, Config{ExaAPIKey: fakeExaKey, TavilyAPIKey: fakeTavilyKey}, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
	}, func(w http.ResponseWriter, r *http.Request) { fallback.Add(1); tavilyGood(w, r) })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.Search(ctx, Request{Query: "Новости", ContentKind: "article"})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("primary request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || fallback.Load() != 0 {
			t.Fatalf("cancel triggered fallback: %v calls=%d", err, fallback.Load())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("search did not honor cancellation")
	}
	_, err := client.Search(ctx, Request{Query: "Новости"})
	if !errors.Is(err, context.Canceled) || fallback.Load() != 0 {
		t.Fatal("pre-canceled request reached a provider")
	}
}

func TestCallerDeadlineNeverStartsFallback(t *testing.T) {
	var fallback atomic.Int32
	client := testClient(t, Config{ExaAPIKey: fakeExaKey, TavilyAPIKey: fakeTavilyKey}, func(_ http.ResponseWriter, r *http.Request) { _, _ = io.Copy(io.Discard, r.Body); <-r.Context().Done() }, func(w http.ResponseWriter, r *http.Request) { fallback.Add(1); tavilyGood(w, r) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := client.Search(ctx, Request{Query: "Новости", ContentKind: "article"})
	if !errors.Is(err, context.DeadlineExceeded) || fallback.Load() != 0 {
		t.Fatalf("deadline triggered fallback: %v calls=%d", err, fallback.Load())
	}
}

func TestResponseLimitsAndSafeProviderErrors(t *testing.T) {
	for _, mode := range []string{"length", "stream", "reject", "content_type"} {
		t.Run(mode, func(t *testing.T) {
			client := testClient(t, Config{TavilyAPIKey: fakeTavilyKey}, nil, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch mode {
				case "length":
					w.Header().Set("Content-Length", fmt.Sprint(maxResponseBytes+1))
					w.WriteHeader(http.StatusOK)
				case "stream":
					w.(http.Flusher).Flush()
					_, _ = io.WriteString(w, strings.Repeat(" ", maxResponseBytes+1))
				case "reject":
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = io.WriteString(w, fakeExaKey+" "+fakeTavilyKey+" private upstream error")
				case "content_type":
					w.Header().Set("Content-Type", "text/html")
					_, _ = io.WriteString(w, fakeTavilyKey)
				}
			})
			_, err := client.Search(context.Background(), Request{Query: "Новости"})
			if err == nil {
				t.Fatal("unsafe response accepted")
			}
			if strings.Contains(err.Error(), fakeExaKey) || strings.Contains(err.Error(), fakeTavilyKey) || strings.Contains(err.Error(), "private upstream error") {
				t.Fatal("provider body or credentials leaked")
			}
			var providerErr *Error
			if !errors.As(err, &providerErr) {
				t.Fatalf("expected safe structured error: %T", err)
			}
			if (mode == "length" || mode == "stream") && providerErr.Code != "response_too_large" {
				t.Fatalf("wrong size error: %s", providerErr.Code)
			}
		})
	}
}

func TestRedirectNeverForwardsCredentials(t *testing.T) {
	var foreignCalls atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignCalls.Add(1)
		jsonResponse(w, `{"results":[]}`)
	}))
	defer foreign.Close()
	client := testClient(t, Config{TavilyAPIKey: fakeTavilyKey}, nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", foreign.URL+"/leak")
		w.WriteHeader(http.StatusTemporaryRedirect)
	})
	_, err := client.Search(context.Background(), Request{Query: "Новости"})
	if err == nil || foreignCalls.Load() != 0 {
		t.Fatal("redirect was followed")
	}
}

func TestTransportErrorAndSuccessfulTextRedactCredentials(t *testing.T) {
	client, err := newClient(Config{ExaAPIKey: fakeExaKey}, &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("transport echoed %s", fakeExaKey)
	})})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Search(context.Background(), Request{Query: "Новости"})
	if err == nil || strings.Contains(err.Error(), fakeExaKey) {
		t.Fatal("transport leaked credentials")
	}
	client = testClient(t, Config{TavilyAPIKey: fakeTavilyKey}, nil, func(w http.ResponseWriter, r *http.Request) {
		var payload tavilyRequest
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if strings.Contains(payload.Query, fakeTavilyKey) {
			t.Error("query echoed the provider credential")
		}
		jsonResponse(w, `{"results":[{"title":"`+fakeTavilyKey+`","url":"https://example.com/article","content":"`+fakeTavilyKey+`","images":[{"url":"https://example.com/image.png","description":"`+fakeTavilyKey+`"}]}]}`)
	})
	result, err := client.Search(context.Background(), Request{Query: fakeTavilyKey, ContentKind: "meme"})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), fakeTavilyKey) || !strings.Contains(string(encoded), "[redacted]") {
		t.Fatal("successful metadata leaked credentials")
	}
}

func TestInvalidInputCannotReachProvider(t *testing.T) {
	var calls atomic.Int32
	client := testClient(t, Config{TavilyAPIKey: fakeTavilyKey}, nil, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); tavilyGood(w, r) })
	for _, request := range []Request{{Query: " "}, {Query: strings.Repeat("я", 2001)}, {Query: string([]byte{0xff})}, {Query: "abc\x00def"}, {Query: "Новости", ContentKind: "arbitrary"}} {
		if _, err := client.Search(context.Background(), request); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("invalid input accepted: %v", err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid input incurred a provider request")
	}
	invalidKey := fakeExaKey + "\nembedded-control"
	if _, err := NewClient(Config{ExaAPIKey: invalidKey}); err == nil || strings.Contains(err.Error(), fakeExaKey) {
		t.Fatal("invalid credential configuration accepted or echoed")
	}
}

func TestProviderContentIsBoundedWithoutInventingVideoFiles(t *testing.T) {
	client := testClient(t, Config{TavilyAPIKey: fakeTavilyKey}, nil, func(w http.ResponseWriter, _ *http.Request) {
		body, _ := json.Marshal(map[string]any{"results": []map[string]any{{"title": strings.Repeat("я", 300), "url": "https://www.youtube.com/watch?v=real", "content": strings.Repeat("я", maxContentRunes+100), "published_date": "not a date", "images": []any{map[string]any{"url": "https://example.com/poster.jpg"}}}}})
		jsonResponse(w, string(body))
	})
	result, err := client.Search(context.Background(), Request{Query: "Видео", ContentKind: "video"})
	if err != nil {
		t.Fatal(err)
	}
	source := result.Sources[0]
	if utf8.RuneCountInString(source.Content) != maxContentRunes || utf8.RuneCountInString(source.Title) != 250 || !utf8.ValidString(source.Content) || source.PublishedAt != "" || len(source.Images) != 1 || source.URL != "https://www.youtube.com/watch?v=real" {
		t.Fatalf("incorrect content/video bounds: title=%d content=%d", utf8.RuneCountInString(source.Title), utf8.RuneCountInString(source.Content))
	}
}
