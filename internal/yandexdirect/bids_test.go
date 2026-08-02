package yandexdirect

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCampaignBidConfigurationReadsAutomaticCeiling(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/v501/campaigns" || r.Header.Get("Authorization") != "Bearer token" ||
			r.Header.Get("Client-Login") != "login" {
			t.Errorf("request = %s headers=%v", r.URL.Path, r.Header)
		}
		var request map[string]any
		if json.NewDecoder(r.Body).Decode(&request) != nil || request["method"] != "get" {
			t.Errorf("payload = %#v", request)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"Campaigns":[{"Id":42,"Type":"UNIFIED_CAMPAIGN","UnifiedCampaign":{"BiddingStrategy":{"Search":{"BiddingStrategyType":"SERVING_OFF"},"Network":{"BiddingStrategyType":"WB_MAXIMUM_CLICKS","WbMaximumClicks":{"WeeklySpendLimit":300000000,"BidCeiling":12000000}}}}}]}}`))
	}))
	defer server.Close()
	client := newUnifiedTestClient(t, server.URL+"/json/v501")
	result, err := client.GetCampaignBidConfiguration(t.Context(), "token", "login", 42)
	if err != nil {
		t.Fatal(err)
	}
	if result.CampaignID != 42 || result.WeeklyBudgetMinor != 30_000 ||
		result.BidCeilingMinor != 1_200 {
		t.Fatalf("configuration = %#v", result)
	}
}

func TestCurrencyBidLimitsUseDocumentedCurrencyProperties(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/v501/dictionaries" {
			t.Errorf("path = %s", r.URL.Path)
		}
		var request struct {
			Method string `json:"method"`
			Params struct {
				DictionaryNames []string `json:"DictionaryNames"`
			} `json:"params"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.Method != "get" ||
			len(request.Params.DictionaryNames) != 1 || request.Params.DictionaryNames[0] != "Currencies" {
			t.Errorf("payload = %#v", request)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"Currencies":[{"Currency":"RUB","Properties":[{"Name":"FullName","Value":"российские рубли"},{"Name":"MinimumBid","Value":"300000"},{"Name":"MaximumBid","Value":"25000000000"},{"Name":"BidIncrement","Value":"100000"}]}]}}`))
	}))
	defer server.Close()
	client := newUnifiedTestClient(t, server.URL+"/json/v501")
	result, err := client.GetCurrencyBidLimits(t.Context(), "token", "login", "rub")
	if err != nil {
		t.Fatal(err)
	}
	if result.CurrencyCode != "RUB" || result.MinimumBidMinor != 30 ||
		result.MaximumBidMinor != 2_500_000 || result.BidIncrementMinor != 10 {
		t.Fatalf("limits = %#v", result)
	}
}

func TestCurrencyBidLimitsCacheIsScopedAndExpires(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = fmt.Fprint(w, `{"result":{"Currencies":[{"Currency":"RUB","Properties":[`+
			`{"Name":"MinimumBid","Value":"300000"},`+
			`{"Name":"MaximumBid","Value":"25000000000"},`+
			`{"Name":"BidIncrement","Value":"100000"}]}]}}`)
	}))
	defer server.Close()
	client := newUnifiedTestClient(t, server.URL+"/json/v501")
	now := time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)
	client.currencyCache.now = func() time.Time { return now }

	for _, call := range []struct{ token, login string }{
		{token: "token-a", login: " Client-Login "},
		{token: "rotated-token", login: "client-login"},
	} {
		if _, err := client.GetCurrencyBidLimits(
			context.Background(), call.token, call.login, "rub",
		); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("same advertiser dictionary calls = %d, want 1", calls.Load())
	}
	if _, err := client.GetCurrencyBidLimits(
		context.Background(), "token-a", "other-login", "RUB",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetCurrencyBidLimits(
		context.Background(), "token-without-login", "", "RUB",
	); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("scoped dictionary calls = %d, want 3", calls.Load())
	}
	now = now.Add(directCurrencyCacheTTL)
	if _, err := client.GetCurrencyBidLimits(
		context.Background(), "rotated-again", "client-login", "RUB",
	); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 4 {
		t.Fatalf("expired dictionary calls = %d, want 4", calls.Load())
	}
}

func TestCurrencyBidLimitsCacheCollapsesConcurrentLoads(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		_, _ = fmt.Fprint(w, `{"result":{"Currencies":[{"Currency":"RUB","Properties":[`+
			`{"Name":"MinimumBid","Value":"300000"},`+
			`{"Name":"MaximumBid","Value":"25000000000"},`+
			`{"Name":"BidIncrement","Value":"100000"}]}]}}`)
	}))
	defer server.Close()
	client := newUnifiedTestClient(t, server.URL+"/json/v501")

	const workers = 12
	var group sync.WaitGroup
	errorsSeen := make(chan error, workers)
	for index := 0; index < workers; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := client.GetCurrencyBidLimits(
				context.Background(), "token", "client-login", "RUB",
			)
			errorsSeen <- err
		}()
	}
	<-started
	close(release)
	group.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("concurrent dictionary calls = %d, want 1", calls.Load())
	}
}

func TestCurrencyBidLimitsCacheIsBounded(t *testing.T) {
	t.Parallel()
	cache := newDirectCurrencyCache()
	now := time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)
	cache.now = func() time.Time { return now }
	values := map[string]CurrencyBidLimits{
		"RUB": {CurrencyCode: "RUB", MinimumBidMinor: 30, MaximumBidMinor: 2_500_000},
	}
	for index := 0; index < directCurrencyCacheMaxEntries+20; index++ {
		key := fmt.Sprintf("login:%d", index)
		if _, err := cache.get(context.Background(), key, func(context.Context) (map[string]CurrencyBidLimits, error) {
			return values, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	cache.mu.Lock()
	entries := len(cache.entries)
	cache.mu.Unlock()
	if entries != directCurrencyCacheMaxEntries {
		t.Fatalf("cache entries=%d, want %d", entries, directCurrencyCacheMaxEntries)
	}
}

func TestCampaignBidConfigurationRejectsManualOrMalformedStrategy(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"Campaigns":[{"Id":42,"Type":"UNIFIED_CAMPAIGN","UnifiedCampaign":{"BiddingStrategy":{"Search":{"BiddingStrategyType":"SERVING_OFF"},"Network":{"BiddingStrategyType":"HIGHEST_POSITION","HighestPosition":{}}}}}]}}`))
	}))
	defer server.Close()
	client := newUnifiedTestClient(t, server.URL+"/json/v501")
	if _, err := client.GetCampaignBidConfiguration(t.Context(), "token", "", 42); err == nil {
		t.Fatal("unsupported strategy was accepted")
	}
}

func newUnifiedTestClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	client, err := New(
		baseURL, "client-id", "client-secret", CallbackRedirectURI,
		&http.Client{},
	)
	if err != nil {
		t.Fatal(err)
	}
	return client
}
