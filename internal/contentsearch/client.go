// Package contentsearch retrieves attributed web sources without generating posts.
// Provider content is untrusted editorial data; callers must keep it separate
// from system instructions and must validate media again before downloading it.
package contentsearch

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	exaEndpoint      = "https://api.exa.ai/search"
	tavilyEndpoint   = "https://api.tavily.com/search"
	maxResponseBytes = 2 << 20
	maxSources       = 20
	maxSourceImages  = 4
	maxContentRunes  = 4000
	providerTimeout  = 18 * time.Second
	searchTimeout    = 35 * time.Second
)

var (
	ErrNotConfigured  = errors.New("content search is not configured")
	ErrInvalidRequest = errors.New("invalid content search request")
	ErrNoSources      = errors.New("no usable content search sources")
)

type Config struct {
	ExaAPIKey    string
	TavilyAPIKey string
}

type Request struct {
	Query       string
	ContentKind string
	// SourceFilter is server-owned policy, never accepted from a JSON request.
	// It runs after normalization and image DNS checks, before fallback decisions.
	SourceFilter func(Source) bool `json:"-"`
}

type Result struct {
	Provider string
	Sources  []Source
}

type Source struct {
	Title       string
	URL         string
	Content     string
	PublishedAt string
	Images      []Image
}

type Image struct {
	URL         string
	Description string
	PreviewOnly bool
}

// Error deliberately excludes remote bodies, transport errors and credentials.
type Error struct {
	Provider string
	Code     string
	Status   int
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("content search %s: %s (HTTP %d)", e.Provider, e.Code, e.Status)
	}
	return "content search " + e.Provider + ": " + e.Code
}

type Client struct {
	exaKey      string
	tavilyKey   string
	http        *http.Client
	lookupNetIP func(context.Context, string, string) ([]netip.Addr, error)
}

func NewClient(config Config) (*Client, error) {
	transport := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:   true,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     90 * time.Second,
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 4,
	}
	// Do not disclose provider credentials through an environment-selected proxy.
	transport.Proxy = nil
	transport.ResponseHeaderTimeout = providerTimeout
	return newClient(config, &http.Client{Transport: transport})
}

// Internal injection keeps tests offline without allowing endpoint overrides in
// production configuration. Every outgoing URL is still a fixed provider URL.
func newClient(config Config, client *http.Client) (*Client, error) {
	config.ExaAPIKey = strings.TrimSpace(config.ExaAPIKey)
	config.TavilyAPIKey = strings.TrimSpace(config.TavilyAPIKey)
	for _, key := range []string{config.ExaAPIKey, config.TavilyAPIKey} {
		if len(key) > 4096 {
			return nil, errors.New("invalid content search credentials")
		}
		for _, r := range key {
			if r <= 32 || r > 126 {
				return nil, errors.New("invalid content search credentials")
			}
		}
	}
	if client == nil {
		return nil, errors.New("content search HTTP client is required")
	}
	copyClient := *client
	copyClient.Timeout = providerTimeout
	copyClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &Client{exaKey: config.ExaAPIKey, tavilyKey: config.TavilyAPIKey, http: &copyClient, lookupNetIP: net.DefaultResolver.LookupNetIP}, nil
}

func (c *Client) Configured() bool {
	return c != nil && (c.exaKey != "" || c.tavilyKey != "")
}

// Search issues at most two requests. It tries the preferred configured provider
// first, then the other only after an error or an empty usable result. Parent
// cancellation or the overall deadline never starts another paid request.
func (c *Client) Search(ctx context.Context, request Request) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if !c.Configured() {
		return Result{}, ErrNotConfigured
	}
	request.Query = strings.Join(strings.Fields(request.Query), " ")
	request.ContentKind = strings.TrimSpace(request.ContentKind)
	if request.ContentKind == "" {
		request.ContentKind = "auto"
	}
	if !utf8.ValidString(request.Query) || len(request.Query) == 0 || utf8.RuneCountInString(request.Query) > 2000 {
		return Result{}, ErrInvalidRequest
	}
	for _, r := range request.Query {
		if unicode.IsControl(r) {
			return Result{}, ErrInvalidRequest
		}
	}
	switch request.ContentKind {
	case "auto", "meme", "video", "idea", "article":
	default:
		return Result{}, ErrInvalidRequest
	}
	request.Query = c.redact(request.Query)
	providers := []string{"tavily", "exa"}
	if request.ContentKind == "idea" || request.ContentKind == "article" {
		providers = []string{"exa", "tavily"}
	}
	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()
	var lastErr error
	for _, provider := range providers {
		if (provider == "exa" && c.exaKey == "") || (provider == "tavily" && c.tavilyKey == "") {
			continue
		}
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		result, err := c.searchProvider(ctx, provider, request)
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		if err == nil && len(result.Sources) > 0 {
			result.Sources = c.validateImageDNS(ctx, result.Sources)
			if ctx.Err() != nil {
				return Result{}, ctx.Err()
			}
			result.Sources = usableSources(result.Sources, request.ContentKind)
			if request.SourceFilter != nil {
				filtered := make([]Source, 0, len(result.Sources))
				for _, source := range result.Sources {
					if request.SourceFilter(source) {
						filtered = append(filtered, source)
					}
				}
				result.Sources = filtered
			}
			if len(result.Sources) > 0 {
				return result, nil
			}
		}
		if err != nil {
			// A rejected/quota-limited provider remains an availability error
			// even when the second provider legitimately returns no results.
			lastErr = err
		} else if lastErr == nil {
			lastErr = ErrNoSources
		}
	}
	return Result{}, lastErr
}

func (c *Client) searchProvider(ctx context.Context, provider string, request Request) (Result, error) {
	media := request.ContentKind == "auto" || request.ContentKind == "meme"
	if provider == "exa" {
		payload := exaRequest{Query: request.Query, Type: "auto", Contents: exaContents{Highlights: true}}
		if media {
			payload.Contents.Extras = &exaExtrasRequest{ImageLinks: maxSourceImages}
		}
		var response exaResponse
		if err := c.post(ctx, provider, exaEndpoint, payload, &response); err != nil {
			return Result{}, err
		}
		if response.Results == nil {
			return Result{}, &Error{Provider: provider, Code: "invalid_response"}
		}
		return Result{Provider: provider, Sources: c.exaSources(*response.Results, media)}, nil
	}
	includeImages := media || request.ContentKind == "video"
	payload := tavilyRequest{Query: request.Query, SearchDepth: "basic", MaxResults: 10, Language: "ru", IncludeImages: includeImages, IncludeImageDescriptions: includeImages}
	var response tavilyResponse
	if err := c.post(ctx, provider, tavilyEndpoint, payload, &response); err != nil {
		return Result{}, err
	}
	if response.Results == nil {
		return Result{}, &Error{Provider: provider, Code: "invalid_response"}
	}
	return Result{Provider: provider, Sources: c.tavilySources(*response.Results, includeImages)}, nil
}

func (c *Client) post(ctx context.Context, provider, endpoint string, payload, target any) error {
	// The injection hook must not become a configurable proxy or endpoint surface.
	if (provider != "exa" || endpoint != exaEndpoint) && (provider != "tavily" || endpoint != tavilyEndpoint) {
		return &Error{Provider: provider, Code: "invalid_endpoint"}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return &Error{Provider: provider, Code: "invalid_request"}
	}
	ctx, cancel := context.WithTimeout(ctx, providerTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return &Error{Provider: provider, Code: "invalid_request"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if provider == "exa" {
		req.Header.Set("x-api-key", c.exaKey)
	} else {
		req.Header.Set("Authorization", "Bearer "+c.tavilyKey)
	}
	response, err := c.http.Do(req)
	if err != nil {
		return &Error{Provider: provider, Code: "request_failed"}
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return &Error{Provider: provider, Code: "provider_rejected", Status: response.StatusCode}
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || (mediaType != "application/json" && !strings.HasSuffix(mediaType, "+json")) {
		return &Error{Provider: provider, Code: "invalid_response"}
	}
	if response.ContentLength > maxResponseBytes {
		return &Error{Provider: provider, Code: "response_too_large"}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return &Error{Provider: provider, Code: "response_unreadable"}
	}
	if len(data) > maxResponseBytes {
		return &Error{Provider: provider, Code: "response_too_large"}
	}
	if err := json.Unmarshal(data, target); err != nil {
		return &Error{Provider: provider, Code: "invalid_response"}
	}
	return nil
}

func (c *Client) redact(value string) string {
	for _, key := range []string{c.exaKey, c.tavilyKey} {
		if key != "" {
			value = strings.ReplaceAll(value, key, "[redacted]")
		}
	}
	return value
}

func (c *Client) text(value string, maxRunes int) string {
	value = strings.ToValidUTF8(c.redact(value), "")
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, value)
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) > maxRunes {
		value = string([]rune(value)[:maxRunes])
	}
	return value
}
