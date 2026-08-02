package yandexwordstat

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const DefaultAPIBaseURL = "https://searchapi.api.cloud.yandex.net"

const (
	maxResponseBytes = 4 << 20
	// MaxResponsePhraseRunes bounds provider-controlled suggestion text while
	// allowing associations longer than the 400-rune input phrase limit.
	MaxResponsePhraseRunes = 1024
)

type Client struct {
	baseURL  *url.URL
	apiKey   string
	folderID string
	http     *http.Client
}

type Error struct {
	StatusCode int
	Code       string
	Message    string
	RequestID  string
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	if e == nil || e.Code == "" {
		return "yandex Wordstat request failed"
	}
	return "yandex Wordstat request failed: " + e.Code
}

type Phrase struct {
	Phrase string
	Count  int64
}

type TopRequest struct {
	Phrase  string
	Limit   int
	Regions []string
}

type TopResult struct {
	TotalCount   int64
	Results      []Phrase
	Associations []Phrase
}

type phraseEnvelope struct {
	Phrase string         `json:"phrase"`
	Count  *flexibleInt64 `json:"count"`
}

func New(
	apiBaseURL, apiKey, folderID string, httpClient *http.Client,
) (*Client, error) {
	baseURL, err := url.Parse(strings.TrimRight(strings.TrimSpace(apiBaseURL), "/"))
	if err != nil || !baseURL.IsAbs() || baseURL.User != nil || baseURL.RawQuery != "" ||
		baseURL.Fragment != "" || strings.TrimRight(baseURL.Path, "/") != "" {
		return nil, errors.New("invalid Yandex Wordstat API base URL")
	}
	host := strings.ToLower(baseURL.Hostname())
	ip := net.ParseIP(host)
	isLoopback := host == "localhost" || ip != nil && ip.IsLoopback()
	if baseURL.Scheme != "https" && (baseURL.Scheme != "http" || !isLoopback) {
		return nil, errors.New("yandex Wordstat API must use HTTPS outside localhost")
	}
	if !isLoopback && host != "searchapi.api.cloud.yandex.net" {
		return nil, errors.New("yandex Wordstat API host is not allowed")
	}
	apiKey = strings.TrimSpace(apiKey)
	folderID = strings.TrimSpace(folderID)
	if apiKey == "" || len(apiKey) > 4096 {
		return nil, errors.New("yandex Wordstat API key is required")
	}
	if folderID == "" || len(folderID) > 50 {
		return nil, errors.New("yandex Wordstat folder ID is required")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 20 * time.Second}
	}
	safeHTTP := *httpClient
	safeHTTP.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &Client{
		baseURL: baseURL, apiKey: apiKey, folderID: folderID, http: &safeHTTP,
	}, nil
}

func (c *Client) GetTop(ctx context.Context, request TopRequest) (TopResult, error) {
	request.Phrase = strings.TrimSpace(request.Phrase)
	if request.Phrase == "" || utf8.RuneCountInString(request.Phrase) > 400 {
		return TopResult{}, errors.New("wordstat phrase must contain 1 to 400 characters")
	}
	if request.Limit < 1 || request.Limit > 50 {
		return TopResult{}, errors.New("wordstat limit must be between 1 and 50")
	}
	regions, err := normalizeRegionIDs(request.Regions)
	if err != nil {
		return TopResult{}, err
	}
	payload, err := json.Marshal(map[string]any{
		"phrase": request.Phrase, "numPhrases": strconv.Itoa(request.Limit),
		"regions": regions, "devices": []string{"DEVICE_ALL"},
		"folderId": c.folderID,
	})
	if err != nil {
		return TopResult{}, err
	}
	endpoint := *c.baseURL
	endpoint.Path = "/v2/wordstat/topRequests"
	body, statusCode, requestID, err := c.postTopRequest(ctx, endpoint.String(), payload)
	if err != nil {
		return TopResult{}, err
	}
	var envelope struct {
		TotalCount   *flexibleInt64   `json:"totalCount"`
		Results      []phraseEnvelope `json:"results"`
		Associations []phraseEnvelope `json:"associations"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.TotalCount == nil ||
		*envelope.TotalCount < 0 {
		return TopResult{}, &Error{
			StatusCode: statusCode, Code: "invalid_api_response", RequestID: requestID,
		}
	}
	result := TopResult{TotalCount: int64(*envelope.TotalCount)}
	result.Results, err = normalizePhrases(envelope.Results)
	if err != nil {
		return TopResult{}, &Error{
			StatusCode: statusCode, Code: "invalid_api_response", RequestID: requestID,
		}
	}
	result.Associations, err = normalizePhrases(envelope.Associations)
	if err != nil {
		return TopResult{}, &Error{
			StatusCode: statusCode, Code: "invalid_api_response", RequestID: requestID,
		}
	}
	return result, nil
}

func (c *Client) postTopRequest(
	ctx context.Context, endpoint string, payload []byte,
) ([]byte, int, string, error) {
	const maxRetries = 2
	for attempt := 0; attempt <= maxRetries; attempt++ {
		httpRequest, err := http.NewRequestWithContext(
			ctx, http.MethodPost, endpoint, bytes.NewReader(payload),
		)
		if err != nil {
			return nil, 0, "", err
		}
		httpRequest.Header.Set("Authorization", "Api-Key "+c.apiKey)
		httpRequest.Header.Set("Content-Type", "application/json")
		httpRequest.Header.Set("Accept", "application/json")
		response, err := c.http.Do(httpRequest)
		if err != nil {
			return nil, 0, "", fmt.Errorf("call Yandex Wordstat: %w", err)
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
		_ = response.Body.Close()
		requestID := strings.TrimSpace(response.Header.Get("x-request-id"))
		if requestID == "" {
			requestID = strings.TrimSpace(response.Header.Get("RequestId"))
		}
		if readErr != nil {
			return nil, response.StatusCode, requestID, readErr
		}
		if len(body) > maxResponseBytes {
			return nil, response.StatusCode, requestID, &Error{
				StatusCode: response.StatusCode, Code: "response_too_large", RequestID: requestID,
			}
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			return body, response.StatusCode, requestID, nil
		}
		providerErr := decodeProviderError(response, body, requestID)
		if attempt == maxRetries || !retryableStatus(response.StatusCode) {
			return nil, response.StatusCode, requestID, providerErr
		}
		delay, retry := retryDelay(providerErr, attempt)
		if !retry {
			return nil, response.StatusCode, requestID, providerErr
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, response.StatusCode, requestID, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, 0, "", &Error{Code: "retry_exhausted"}
}

func retryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func retryDelay(err error, attempt int) (time.Duration, bool) {
	const maximum = 2 * time.Second
	var providerErr *Error
	if errors.As(err, &providerErr) {
		if providerErr.RetryAfter > 0 {
			if providerErr.RetryAfter > maximum {
				return 0, false
			}
			return providerErr.RetryAfter, true
		}
		if providerErr.StatusCode == http.StatusTooManyRequests {
			return 0, false
		}
	}
	delay := 200 * time.Millisecond * time.Duration(1<<attempt)
	if delay > maximum {
		delay = maximum
	}
	// A small bounded jitter prevents replicas from repeating a transient
	// provider failure in lockstep.
	jitter := secureJitter(delay / 2)
	return delay + jitter, true
}

func secureJitter(maximum time.Duration) time.Duration {
	if maximum <= 0 {
		return 0
	}
	value, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(maximum)+1))
	if err != nil {
		// Jitter is optional: preserving the bounded base delay is safer than
		// failing or extending a provider request when system entropy is absent.
		return 0
	}
	return time.Duration(value.Int64())
}

func normalizePhrases(values []phraseEnvelope) ([]Phrase, error) {
	result := make([]Phrase, 0, len(values))
	for _, value := range values {
		value.Phrase = strings.Join(strings.Fields(norm.NFC.String(value.Phrase)), " ")
		if value.Count == nil || *value.Count < 0 {
			return nil, errors.New("invalid Wordstat phrase")
		}
		// The documented 400-rune limit applies to the request phrase, not
		// response associations. Keep longer bounded suggestions for the UI's
		// copy-only flow and ignore unusable empty or pathological values.
		if value.Phrase == "" || utf8.RuneCountInString(value.Phrase) > MaxResponsePhraseRunes {
			continue
		}
		result = append(result, Phrase{Phrase: value.Phrase, Count: int64(*value.Count)})
	}
	return result, nil
}

func normalizeRegionIDs(values []string) ([]string, error) {
	if len(values) == 0 || len(values) > 100 {
		return nil, errors.New("wordstat requires 1 to 100 region IDs")
	}
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed <= 0 || strconv.FormatInt(parsed, 10) != value {
			return nil, errors.New("wordstat region ID must be a positive canonical integer")
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result, nil
}

func decodeProviderError(response *http.Response, body []byte, requestID string) error {
	var envelope struct {
		Code    any    `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &envelope)
	code := "api_request_failed"
	switch value := envelope.Code.(type) {
	case string:
		if strings.TrimSpace(value) != "" {
			code = strings.TrimSpace(value)
		}
	case float64:
		code = strconv.FormatInt(int64(value), 10)
	}
	retryAfter := time.Duration(0)
	retryAfterHeader := strings.TrimSpace(response.Header.Get("Retry-After"))
	if seconds, err := strconv.Atoi(retryAfterHeader); err == nil && seconds > 0 {
		retryAfter = time.Duration(seconds) * time.Second
	} else if retryAt, err := http.ParseTime(retryAfterHeader); err == nil {
		retryAfter = time.Until(retryAt)
		if retryAfter < 0 {
			retryAfter = 0
		}
	}
	return &Error{
		StatusCode: response.StatusCode, Code: code,
		Message: strings.TrimSpace(envelope.Message), RequestID: requestID,
		RetryAfter: retryAfter,
	}
}

type flexibleInt64 int64

func (value *flexibleInt64) UnmarshalJSON(data []byte) error {
	text := strings.TrimSpace(string(data))
	if len(text) >= 2 && text[0] == '"' && text[len(text)-1] == '"' {
		var decoded string
		if err := json.Unmarshal(data, &decoded); err != nil {
			return err
		}
		text = decoded
	}
	parsed, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return err
	}
	*value = flexibleInt64(parsed)
	return nil
}
