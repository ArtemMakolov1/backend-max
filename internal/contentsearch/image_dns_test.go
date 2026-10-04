package contentsearch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestImageDNSDropsAnyPrivateAnswerAndCachesHosts(t *testing.T) {
	var mu sync.Mutex
	calls := make(map[string]int)
	client := &Client{lookupNetIP: func(_ context.Context, network, host string) ([]netip.Addr, error) {
		if network != "ip" {
			t.Error("DNS must inspect both address families")
		}
		mu.Lock()
		calls[host]++
		mu.Unlock()
		switch host {
		case "mixed.example.com":
			return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("127.0.0.1")}, nil
		case "unavailable.example.com":
			return nil, errors.New("not available")
		case "mapped.example.com":
			return []netip.Addr{netip.MustParseAddr("::ffff:127.0.0.1")}, nil
		default:
			return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("2606:4700:4700::1111")}, nil
		}
	}}
	sources := []Source{{Content: "First", Images: []Image{{URL: "https://public.example.com/a.jpg"}, {URL: "https://mixed.example.com/a.jpg"}, {URL: "https://unavailable.example.com/a.jpg"}, {URL: "https://mapped.example.com/a.jpg"}}}, {Content: "Second", Images: []Image{{URL: "https://public.example.com/b.jpg"}}}}
	result := client.validateImageDNS(context.Background(), sources)
	if len(result) != 2 || result[0].Content != "First" || len(result[0].Images) != 1 || len(result[1].Images) != 1 {
		t.Fatalf("DNS image filtering lost text or kept unsafe images: %#v", result)
	}
	for host, count := range calls {
		if count != 1 {
			t.Errorf("DNS cache failed for %s: %d lookups", host, count)
		}
	}
}

func TestImageDNSFanoutAndSharedBudget(t *testing.T) {
	var active, peak atomic.Int32
	client := &Client{lookupNetIP: func(ctx context.Context, _, _ string) ([]netip.Addr, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for previous := peak.Load(); current > previous; previous = peak.Load() {
			if peak.CompareAndSwap(previous, current) {
				break
			}
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	sources := make([]Source, maxSources)
	for i := range sources {
		sources[i].Content = "Keep text"
		for j := range maxSourceImages {
			sources[i].Images = append(sources[i].Images, Image{URL: fmt.Sprintf("https://host%d-%d.example.com/a.jpg", i, j)})
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	result := client.validateImageDNS(ctx, sources)
	if time.Since(started) > time.Second || peak.Load() > imageDNSWorkers || active.Load() != 0 {
		t.Fatalf("DNS fanout/budget leaked: peak=%d active=%d", peak.Load(), active.Load())
	}
	for _, source := range result {
		if len(source.Images) != 0 || source.Content != "Keep text" {
			t.Fatal("unresolved images accepted or text lost")
		}
	}
}

func TestCancellationDuringImageDNSDoesNotFallback(t *testing.T) {
	started := make(chan struct{})
	var fallback atomic.Int32
	client := testClient(t, Config{ExaAPIKey: fakeExaKey, TavilyAPIKey: fakeTavilyKey}, func(w http.ResponseWriter, _ *http.Request) {
		jsonResponse(w, `{"results":[{"title":"Article","url":"https://example.com/page","highlights":["Useful text"],"image":"https://cdn.example.com/preview.jpg"}]}`)
	}, func(w http.ResponseWriter, r *http.Request) { fallback.Add(1); tavilyGood(w, r) })
	client.lookupNetIP = func(ctx context.Context, _, _ string) ([]netip.Addr, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
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
		t.Fatal("DNS lookup did not start")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) || fallback.Load() != 0 {
		t.Fatalf("DNS cancel incorrectly started another provider: %v", err)
	}
}

func TestPublicAddressRejectsNonPublicRanges(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "169.254.169.254", "10.1.2.3", "100.64.0.1", "192.0.2.1", "198.18.0.1", "203.0.113.1", "224.1.1.1", "::1", "::ffff:8.8.8.8", "fc00::1", "fe80::1", "2001:db8::1", "2002:0808:0808::1"} {
		if publicAddress(netip.MustParseAddr(raw)) {
			t.Errorf("nonpublic address accepted: %s", raw)
		}
	}
	for _, raw := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if !publicAddress(netip.MustParseAddr(raw)) {
			t.Errorf("public address rejected: %s", raw)
		}
	}
}

func TestProviderQuotaFailureIsNotHiddenByEmptyFallback(t *testing.T) {
	for _, primary := range []string{"exa", "tavily"} {
		t.Run(primary, func(t *testing.T) {
			fail := func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte("private provider quota detail"))
			}
			empty := func(w http.ResponseWriter, _ *http.Request) { jsonResponse(w, `{"results":[]}`) }
			exa, tavily := http.HandlerFunc(fail), http.HandlerFunc(empty)
			kind := "article"
			if primary == "tavily" {
				exa, tavily = empty, fail
				kind = "auto"
			}
			client := testClient(t, Config{ExaAPIKey: fakeExaKey, TavilyAPIKey: fakeTavilyKey}, exa, tavily)
			_, err := client.Search(context.Background(), Request{Query: "Новости", ContentKind: kind})
			var providerErr *Error
			if !errors.As(err, &providerErr) || errors.Is(err, ErrNoSources) || providerErr.Status != http.StatusForbidden || providerErr.Provider != primary || strings.Contains(err.Error(), "quota detail") {
				t.Fatalf("provider rejection became no results: %v", err)
			}
		})
	}
}
