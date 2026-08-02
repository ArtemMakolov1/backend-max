package yandexdirect

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestParseUnitsUsage(t *testing.T) {
	t.Parallel()
	usage, ok := parseUnitsUsage("10/20828/64000")
	if !ok || usage.Spent != 10 || usage.Remaining != 20828 || usage.DailyLimit != 64000 {
		t.Fatalf("usage = %#v, ok=%v", usage, ok)
	}
	for _, value := range []string{"", "10/20", "x/20/30", "-1/20/30", "1/31/30", "1/2/0"} {
		if _, ok := parseUnitsUsage(value); ok {
			t.Fatalf("invalid Units header %q was accepted", value)
		}
	}
}

func TestDirectTransportLimitsConcurrencyAndTracksUnits(t *testing.T) {
	t.Parallel()
	var current atomic.Int64
	var maximum atomic.Int64
	started := make(chan struct{}, 16)
	release := make(chan struct{})
	transport := newDirectTransport(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		active := current.Add(1)
		defer current.Add(-1)
		for {
			observed := maximum.Load()
			if active <= observed || maximum.CompareAndSwap(observed, active) {
				break
			}
		}
		started <- struct{}{}
		<-release
		header := make(http.Header)
		header.Set("Units", "7/93/100")
		header.Set("Units-Used-Login", "advertiser")
		header.Set("RequestId", "request-1")
		return &http.Response{
			StatusCode: http.StatusOK, Header: header,
			Body:    io.NopCloser(strings.NewReader(`{"result":{}}`)),
			Request: request,
		}, nil
	}))

	const requests = 8
	var group sync.WaitGroup
	errorsSeen := make(chan error, requests)
	for index := 0; index < requests; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			request, err := http.NewRequestWithContext(
				context.Background(), http.MethodPost,
				"https://api.direct.yandex.com/json/v501/campaigns", nil,
			)
			if err == nil {
				var response *http.Response
				response, err = transport.RoundTrip(request)
				if response != nil {
					_ = response.Body.Close()
				}
			}
			errorsSeen <- err
		}()
	}
	for index := 0; index < directMaxConcurrentPerAdvertiser; index++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("four Direct requests did not enter the transport")
		}
	}
	select {
	case <-started:
		t.Fatal("more than four Direct requests ran concurrently")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	group.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := maximum.Load(); got != directMaxConcurrentPerAdvertiser {
		t.Fatalf("maximum concurrency = %d", got)
	}
	usage, ok := transport.usage("")
	if !ok || usage.Spent != 7 || usage.Remaining != 93 ||
		usage.DailyLimit != 100 || usage.UsedLogin != "advertiser" ||
		usage.RequestID != "request-1" {
		t.Fatalf("tracked usage = %#v, ok=%v", usage, ok)
	}
}

func TestDirectTransportSpacesReportRequestsPerAdvertiser(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)
	var waits []time.Duration
	var calls atomic.Int64
	transport := newDirectTransport(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader("Date\\tCampaignId\\n")), Request: request,
		}, nil
	}))
	transport.now = func() time.Time { return now }
	transport.wait = func(_ context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		now = now.Add(delay)
		return nil
	}

	for index := 0; index < 3; index++ {
		request, err := http.NewRequest(
			http.MethodPost, "https://api.direct.yandex.com/json/v501/reports", nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		response, err := transport.RoundTrip(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
	}
	regularRequest, err := http.NewRequest(
		http.MethodPost, "https://api.direct.yandex.com/json/v501/campaigns", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	regularResponse, err := transport.RoundTrip(regularRequest)
	if err != nil {
		t.Fatal(err)
	}
	_ = regularResponse.Body.Close()

	if calls.Load() != 4 {
		t.Fatalf("provider calls = %d, want 4", calls.Load())
	}
	if len(waits) != 2 || waits[0] != time.Second || waits[1] != time.Second {
		t.Fatalf("report waits = %v, want [1s 1s]", waits)
	}
}

func TestDirectTransportSerializesReportRequestsPerAdvertiser(t *testing.T) {
	t.Parallel()
	var current atomic.Int64
	var maximum atomic.Int64
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	transport := newDirectTransport(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		active := current.Add(1)
		defer current.Add(-1)
		for {
			observed := maximum.Load()
			if active <= observed || maximum.CompareAndSwap(observed, active) {
				break
			}
		}
		started <- struct{}{}
		<-release
		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader("Date\\tCampaignId\\n")), Request: request,
		}, nil
	}))
	transport.reportMinInterval = 0

	var group sync.WaitGroup
	errorsSeen := make(chan error, 2)
	for index := 0; index < 2; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			request, err := http.NewRequest(
				http.MethodPost, "https://api.direct.yandex.com/json/v501/reports", nil,
			)
			if err == nil {
				var response *http.Response
				response, err = transport.RoundTrip(request)
				if response != nil {
					_ = response.Body.Close()
				}
			}
			errorsSeen <- err
		}()
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first report request did not enter the transport")
	}
	select {
	case <-started:
		t.Fatal("report requests were not serialized")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	group.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	if maximum.Load() != 1 {
		t.Fatalf("maximum report concurrency = %d, want 1", maximum.Load())
	}
}

func TestDirectTransportFailsFastDuringProviderCooldown(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)
	called := false
	transport := newDirectTransport(roundTripperFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, errors.New("unexpected call")
	}))
	transport.now = func() time.Time { return now }
	transport.noteAPIError("", 152)
	request, err := http.NewRequest(
		http.MethodPost, "https://api.direct.yandex.com/json/v501/campaigns", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.RoundTrip(request)
	if response != nil {
		_ = response.Body.Close()
	}
	var providerErr *Error
	if !errors.As(err, &providerErr) || providerErr.Code != "direct_request_cooldown" ||
		providerErr.RetryAfter != time.Minute || providerErr.APIErrorCode != 152 ||
		providerErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("cooldown error = %#v", err)
	}
	if called {
		t.Fatal("cooldown request reached the provider")
	}
}

func TestDirectTransportPreservesConnectionLimitCooldownClass(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)
	transport := newDirectTransport(roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("cooldown request reached provider")
	}))
	transport.now = func() time.Time { return now }
	transport.noteAPIError("client", 506)
	request, err := http.NewRequest(
		http.MethodPost, "https://api.direct.yandex.com/json/v501/campaigns", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Client-Login", "client")
	response, err := transport.RoundTrip(request)
	if response != nil {
		_ = response.Body.Close()
	}
	var providerErr *Error
	if !errors.As(err, &providerErr) || providerErr.APIErrorCode != 506 ||
		providerErr.StatusCode != http.StatusServiceUnavailable ||
		providerErr.RetryAfter != 2*time.Second {
		t.Fatalf("cooldown error = %#v", err)
	}
}

func TestDirectReadRetriesTransientHTTPButMutationDoesNot(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name          string
		method        string
		failureStatus int
		failureCode   int
		wantCalls     int64
		wantError     bool
	}{
		{name: "read HTTP failure", method: "get", failureStatus: http.StatusBadGateway,
			failureCode: 500, wantCalls: 2},
		{name: "read API auth service failure", method: "get", failureStatus: http.StatusOK,
			failureCode: 52, wantCalls: 2},
		{name: "mutation", method: "update", failureStatus: http.StatusBadGateway,
			failureCode: 500, wantCalls: 1, wantError: true},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 {
					w.WriteHeader(test.failureStatus)
					_, _ = fmt.Fprintf(w,
						`{"error":{"error_code":%d,"error_string":"server"}}`, test.failureCode)
					return
				}
				w.Header().Set("Units", "1/99/100")
				_, _ = w.Write([]byte(`{"result":{"Campaigns":[]}}`))
			}))
			defer server.Close()
			client, err := New(
				server.URL+"/json/v501", "client", "secret",
				CallbackRedirectURI, server.Client(),
			)
			if err != nil {
				t.Fatal(err)
			}
			var result struct {
				Campaigns []any `json:"Campaigns"`
			}
			err = client.call(context.Background(), "campaigns", "token", "", map[string]any{
				"method": test.method, "params": map[string]any{},
			}, &result)
			if test.wantError && err == nil {
				t.Fatal("mutation unexpectedly retried and succeeded")
			}
			if !test.wantError && err != nil {
				t.Fatalf("read retry: %v", err)
			}
			if got := calls.Load(); got != test.wantCalls {
				t.Fatalf("provider calls = %d, want %d", got, test.wantCalls)
			}
		})
	}
}
