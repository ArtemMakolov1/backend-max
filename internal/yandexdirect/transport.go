package yandexdirect

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	directMaxConcurrentPerAdvertiser = 4
	// Direct Reports allows 20 requests in any 10-second window per user.
	// Serializing report requests and spacing their starts by one second keeps
	// MaxPosty at no more than 10 requests in that window, including polling a
	// queued report, while leaving capacity for the regular Direct services.
	directReportRequestMinInterval = time.Second
)

// UnitsUsage is the provider quota snapshot returned in the Direct Units
// response header. Direct documents the values as spent/remaining/daily.
// The snapshot is deliberately kept in memory: it is operational telemetry,
// not tenant data and not a source of billing truth.
type UnitsUsage struct {
	Spent      int64
	Remaining  int64
	DailyLimit int64
	UsedLogin  string
	RequestID  string
	ObservedAt time.Time
}

type directAccountTransportState struct {
	semaphore            chan struct{}
	reportSemaphore      chan struct{}
	mu                   sync.Mutex
	usage                UnitsUsage
	hasUsage             bool
	cooldown             time.Time
	cooldownStatusCode   int
	cooldownAPIErrorCode int
	nextReportRequestAt  time.Time
}

type directTransport struct {
	base              http.RoundTripper
	now               func() time.Time
	wait              func(context.Context, time.Duration) error
	reportMinInterval time.Duration
	mu                sync.Mutex
	byID              map[string]*directAccountTransportState
}

func newDirectTransport(base http.RoundTripper) *directTransport {
	if base == nil {
		base = http.DefaultTransport
	}
	return &directTransport{
		base: base, now: time.Now, wait: waitDirectRetry,
		reportMinInterval: directReportRequestMinInterval,
		byID:              make(map[string]*directAccountTransportState),
	}
}

func (t *directTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil || !isDirectAPIRequest(request) {
		return t.base.RoundTrip(request)
	}
	key := directAdvertiserKey(request.Header.Get("Client-Login"))
	state := t.account(key)
	if isDirectReportsRequest(request) {
		if err := acquireDirectSemaphore(request.Context(), state.reportSemaphore); err != nil {
			return nil, err
		}
		defer func() { <-state.reportSemaphore }()
	}
	if err := t.acquire(request.Context(), state); err != nil {
		return nil, err
	}
	defer func() { <-state.semaphore }()
	if isDirectReportsRequest(request) {
		if err := t.waitForReportSlot(request.Context(), state); err != nil {
			return nil, err
		}
	}

	response, err := t.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	t.observe(state, response.Header)
	if response.StatusCode == http.StatusTooManyRequests ||
		response.StatusCode == http.StatusServiceUnavailable {
		delay := retryAfter(response.Header, t.now())
		if delay <= 0 {
			delay = time.Second
		}
		t.setCooldown(state, delay, response.StatusCode, 0)
	}
	return response, nil
}

func (t *directTransport) account(key string) *directAccountTransportState {
	t.mu.Lock()
	defer t.mu.Unlock()
	state := t.byID[key]
	if state == nil {
		state = &directAccountTransportState{
			semaphore:       make(chan struct{}, directMaxConcurrentPerAdvertiser),
			reportSemaphore: make(chan struct{}, 1),
		}
		t.byID[key] = state
	}
	return state
}

func (t *directTransport) acquire(
	ctx context.Context, state *directAccountTransportState,
) error {
	now := t.now().UTC()
	state.mu.Lock()
	cooldown := state.cooldown
	statusCode := state.cooldownStatusCode
	apiErrorCode := state.cooldownAPIErrorCode
	state.mu.Unlock()
	if cooldown.After(now) {
		if statusCode == 0 {
			statusCode = http.StatusTooManyRequests
		}
		return &Error{
			StatusCode: statusCode, APIErrorCode: apiErrorCode,
			Code: "direct_request_cooldown", RetryAfter: cooldown.Sub(now),
		}
	}
	return acquireDirectSemaphore(ctx, state.semaphore)
}

func acquireDirectSemaphore(ctx context.Context, semaphore chan struct{}) error {
	select {
	case semaphore <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (t *directTransport) waitForReportSlot(
	ctx context.Context, state *directAccountTransportState,
) error {
	if t.reportMinInterval <= 0 {
		return nil
	}
	for {
		now := t.now().UTC()
		state.mu.Lock()
		availableAt := state.nextReportRequestAt
		if !availableAt.After(now) {
			state.nextReportRequestAt = now.Add(t.reportMinInterval)
			state.mu.Unlock()
			return nil
		}
		state.mu.Unlock()
		wait := t.wait
		if wait == nil {
			wait = waitDirectRetry
		}
		if err := wait(ctx, availableAt.Sub(now)); err != nil {
			return err
		}
	}
}

func (t *directTransport) observe(
	state *directAccountTransportState, header http.Header,
) {
	usage, ok := parseUnitsUsage(header.Get("Units"))
	if !ok {
		return
	}
	usage.UsedLogin = strings.TrimSpace(header.Get("Units-Used-Login"))
	usage.RequestID = strings.TrimSpace(header.Get("RequestId"))
	usage.ObservedAt = t.now().UTC()
	state.mu.Lock()
	state.usage = usage
	state.hasUsage = true
	state.mu.Unlock()
}

func (t *directTransport) setCooldown(
	state *directAccountTransportState, delay time.Duration,
	statusCode, apiErrorCode int,
) {
	if delay <= 0 {
		return
	}
	until := t.now().UTC().Add(delay)
	state.mu.Lock()
	if until.After(state.cooldown) {
		state.cooldown = until
		state.cooldownStatusCode = statusCode
		state.cooldownAPIErrorCode = apiErrorCode
	}
	state.mu.Unlock()
}

func (t *directTransport) noteAPIError(clientLogin string, code int) {
	state := t.account(directAdvertiserKey(clientLogin))
	switch code {
	case 152: // Not enough units in the advertiser's rolling quota.
		t.setCooldown(state, time.Minute, http.StatusTooManyRequests, code)
	case 506: // Too many simultaneous connections for the advertiser.
		t.setCooldown(state, 2*time.Second, http.StatusServiceUnavailable, code)
	}
}

func (t *directTransport) usage(clientLogin string) (UnitsUsage, bool) {
	state := t.account(directAdvertiserKey(clientLogin))
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.usage, state.hasUsage
}

func directAdvertiserKey(clientLogin string) string {
	if clientLogin = strings.ToLower(strings.TrimSpace(clientLogin)); clientLogin != "" {
		return "login:" + clientLogin
	}
	// MaxPosty currently supports direct advertisers, not agency subclients.
	// A shared primary bucket is intentionally conservative: even when several
	// direct-advertiser tokens are active in this process, their combined
	// concurrency never exceeds the documented per-advertiser ceiling.
	return "primary"
}

func isDirectAPIRequest(request *http.Request) bool {
	if request == nil || request.URL == nil {
		return false
	}
	path := strings.ToLower(request.URL.Path)
	return strings.Contains(path, "/json/v5/") ||
		strings.Contains(path, "/json/v501/")
}

func isDirectReportsRequest(request *http.Request) bool {
	if !isDirectAPIRequest(request) {
		return false
	}
	path := strings.TrimRight(strings.ToLower(request.URL.Path), "/")
	return strings.HasSuffix(path, "/reports")
}

func parseUnitsUsage(value string) (UnitsUsage, bool) {
	parts := strings.Split(strings.TrimSpace(value), "/")
	if len(parts) != 3 {
		return UnitsUsage{}, false
	}
	values := make([]int64, len(parts))
	for index, part := range parts {
		parsed, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil || parsed < 0 {
			return UnitsUsage{}, false
		}
		values[index] = parsed
	}
	if values[2] == 0 || values[1] > values[2] {
		return UnitsUsage{}, false
	}
	return UnitsUsage{
		Spent: values[0], Remaining: values[1], DailyLimit: values[2],
	}, true
}

func retryAfter(header http.Header, now time.Time) time.Duration {
	value := strings.TrimSpace(header.Get("Retry-After"))
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	when, err := http.ParseTime(value)
	if err != nil || !when.After(now) {
		return 0
	}
	return when.Sub(now)
}

func directReadRetryDelay(err error, attempt int) (time.Duration, bool) {
	if attempt >= 2 || err == nil {
		return 0, false
	}
	var providerErr *Error
	if errors.As(err, &providerErr) {
		if providerErr.APIErrorCode == 152 || providerErr.Code == "152" {
			return 0, false
		}
		apiCode := providerErr.APIErrorCode
		if apiCode == 0 {
			apiCode, _ = strconv.Atoi(strings.TrimSpace(providerErr.Code))
		}
		retryableAPIError := apiCode == 52 || apiCode == 506 ||
			(apiCode >= 1000 && apiCode <= 1002)
		retryable := retryableAPIError ||
			providerErr.StatusCode == http.StatusTooManyRequests ||
			providerErr.StatusCode == http.StatusInternalServerError ||
			providerErr.StatusCode == http.StatusBadGateway ||
			providerErr.StatusCode == http.StatusServiceUnavailable ||
			providerErr.StatusCode == http.StatusGatewayTimeout
		if !retryable {
			return 0, false
		}
		if providerErr.RetryAfter > 0 {
			if providerErr.RetryAfter > 2*time.Second {
				return 0, false
			}
			return providerErr.RetryAfter, true
		}
	}
	return time.Duration(200*(1<<attempt)) * time.Millisecond, true
}

func waitDirectRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
