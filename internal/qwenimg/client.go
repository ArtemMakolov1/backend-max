// Package qwenimg generates PNG images through Alibaba Cloud Model Studio.
package qwenimg

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"maxpilot/backend/internal/mediafetch"
	"maxpilot/backend/internal/openaiimg"
)

const (
	maxResponseBytes = 256 << 10
	maxImageBytes    = 50 << 20
	minImagePixels   = 512 * 512
	maxImagePixels   = 2048 * 2048
	imageTimeout     = 45 * time.Second
)

var (
	// Generated links are signed URLs on Model Studio's public OSS buckets.
	resultHostPattern = regexp.MustCompile(`^(?:dashscope-result(?:-[a-z0-9]+)*\.oss-[a-z0-9-]+|dashscope-[a-f0-9]+\.oss-accelerate)\.aliyuncs\.com$`)
	requestIDPattern  = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
)

type imageFetcher interface {
	Fetch(context.Context, string, mediafetch.Limits) (*mediafetch.Download, error)
}

type Client struct {
	baseURL    string
	apiKey     string
	model      string
	httpClient *http.Client
	images     imageFetcher
}

type generationPayload struct {
	Model string `json:"model"`
	Input struct {
		Messages []generationMessage `json:"messages"`
	} `json:"input"`
	Parameters struct {
		Size      string `json:"size,omitempty"`
		N         int    `json:"n"`
		Watermark bool   `json:"watermark"`
	} `json:"parameters"`
}

type generationMessage struct {
	Role    string `json:"role"`
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
}

type generationEnvelope struct {
	RequestID string `json:"request_id"`
	Code      string `json:"code"`
	Output    struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Role    string `json:"role"`
				Content []struct {
					Image string `json:"image"`
				} `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	} `json:"output"`
}

// Error contains only local messages and codes. Provider response bodies and
// signed image URLs may contain request data and never become error text.
type Error struct {
	StatusCode int
	Code       string
	Message    string
	RequestID  string
	cause      error
}

func (e *Error) Error() string {
	return fmt.Sprintf("Qwen image API error (%s, status %d): %s", e.Code, e.StatusCode, e.Message)
}

func (e *Error) Unwrap() error { return e.cause }

// New accepts a deployment-owned origin. Production config pins it to official
// regional Model Studio origins; HTTP origins support offline test servers.
func New(baseURL, apiKey, model string, httpClient *http.Client) (*Client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" ||
		(parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Opaque != "" || parsed.Path != "" {
		return nil, errors.New("Qwen API base URL must be an absolute HTTP(S) origin")
	}
	apiKey, model = strings.TrimSpace(apiKey), strings.TrimSpace(model)
	if apiKey == "" || model == "" || httpClient == nil || strings.ContainsAny(apiKey, "\r\n") {
		return nil, errors.New("Qwen API key, model and HTTP client are required")
	}
	return &Client{baseURL: baseURL, apiKey: apiKey, model: model, httpClient: httpClient,
		// This fetcher has its own transport, public DNS/IP validation and no
		// access to the authenticated API client's headers, cookies or proxy.
		images: mediafetch.New()}, nil
}

func (c *Client) ImageAIProvider() string { return "qwen" }

func (c *Client) Validate(request openaiimg.GenerateRequest) error {
	prompt := strings.TrimSpace(request.Prompt)
	if prompt == "" {
		return errors.New("image prompt is required")
	}
	if !utf8.ValidString(prompt) || utf8.RuneCountInString(prompt) > 2000 {
		return errors.New("image prompt must not exceed 2000 valid Unicode characters")
	}
	switch request.Size {
	case "", "auto", "1024x1024", "1536x1024", "1024x1536":
	default:
		return errors.New("Qwen image size must be auto, 1024x1024, 1536x1024 or 1024x1536")
	}
	// Quality is an existing MaxPosty billing option, not a Qwen parameter.
	// Use one fixed tier so unsupported levels cannot imply an upstream effect.
	if request.Quality != "" && request.Quality != "medium" {
		return errors.New("Qwen image quality must be medium")
	}
	return nil
}

func (c *Client) Generate(ctx context.Context, request openaiimg.GenerateRequest) (openaiimg.Result, error) {
	request.Prompt = strings.TrimSpace(request.Prompt)
	if err := c.Validate(request); err != nil {
		return openaiimg.Result{}, err
	}
	if request.Size == "" {
		request.Size = "1024x1024"
	}
	// The official native synchronous protocol is also available on legacy
	// regional origins where the OpenAI-compatible Images route returns 404.
	// A generation is submitted once, without retrying on another API protocol.
	payload := generationPayload{Model: c.model}
	message := generationMessage{Role: "user"}
	message.Content = append(message.Content, struct {
		Text string `json:"text"`
	}{Text: request.Prompt})
	payload.Input.Messages = []generationMessage{message}
	payload.Parameters.N = 1
	if request.Size != "auto" {
		payload.Parameters.Size = strings.Replace(request.Size, "x", "*", 1)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return openaiimg.Result{}, failure(ctx, "request_encoding", "Could not encode Qwen image request", 0, "")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/api/v1/services/aigc/multimodal-generation/generation", bytes.NewReader(body))
	if err != nil {
		return openaiimg.Result{}, failure(ctx, "request_creation", "Could not create Qwen image request", 0, "")
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	client := *c.httpClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	// #nosec G704 -- baseURL is deployment-owned and validated in New; production
	// config pins official HTTPS origins. Redirects never forward the API key.
	response, err := client.Do(req)
	if err != nil {
		return openaiimg.Result{}, failure(ctx, "request_failed", "Qwen image request failed", 0, "")
	}
	defer func() { _ = response.Body.Close() }()
	requestID := response.Header.Get("x-request-id")
	if !requestIDPattern.MatchString(requestID) {
		requestID = ""
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return openaiimg.Result{}, failure(ctx, "response_unreadable", "Could not read Qwen image response", response.StatusCode, requestID)
	}
	if len(raw) > maxResponseBytes {
		return openaiimg.Result{}, failure(ctx, "response_too_large", "Qwen image response exceeds its size bound", response.StatusCode, requestID)
	}
	var envelope generationEnvelope
	decodeErr := json.Unmarshal(raw, &envelope)
	if requestID == "" && decodeErr == nil && requestIDPattern.MatchString(envelope.RequestID) {
		requestID = envelope.RequestID
	}
	if response.StatusCode != http.StatusOK {
		return openaiimg.Result{}, failure(ctx, "provider_error", "Qwen image provider is unavailable", response.StatusCode, requestID)
	}
	if decodeErr != nil {
		return openaiimg.Result{}, failure(ctx, "invalid_response", "Qwen image response is invalid", response.StatusCode, requestID)
	}
	if envelope.Code != "" {
		return openaiimg.Result{}, failure(ctx, "provider_error", "Qwen image generation failed", response.StatusCode, requestID)
	}
	if len(envelope.Output.Choices) != 1 || envelope.Output.Choices[0].FinishReason != "stop" ||
		envelope.Output.Choices[0].Message.Role != "assistant" || len(envelope.Output.Choices[0].Message.Content) != 1 ||
		!validImageURL(envelope.Output.Choices[0].Message.Content[0].Image) {
		return openaiimg.Result{}, failure(ctx, "invalid_image_url", "Qwen response must contain one official image URL", response.StatusCode, requestID)
	}
	imageBytes, err := c.download(ctx, envelope.Output.Choices[0].Message.Content[0].Image, requestID)
	if err != nil {
		return openaiimg.Result{}, err
	}
	return openaiimg.Result{Bytes: imageBytes, MIMEType: "image/png", Model: c.model, RequestID: requestID}, nil
}

func validImageURL(raw string) bool {
	if raw == "" || len(raw) > 4096 || strings.ContainsAny(raw, "\r\n\t\\") {
		return false
	}
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme == "https" && parsed.Opaque == "" && parsed.User == nil &&
		parsed.Fragment == "" && (parsed.Port() == "" || parsed.Port() == "443") &&
		resultHostPattern.MatchString(parsed.Hostname()) && parsed.Path != "" && parsed.Path != "/"
}

func (c *Client) download(ctx context.Context, rawURL, requestID string) ([]byte, error) {
	download, err := c.images.Fetch(ctx, rawURL, mediafetch.Limits{
		MaxBytes: maxImageBytes, Timeout: imageTimeout, MaxRedirects: 0})
	if err != nil {
		return nil, failure(ctx, "image_download_failed", "Could not download Qwen image", 0, requestID)
	}
	defer func() { _ = download.Close() }()
	if download.MIMEType != "image/png" || download.ContentLength > maxImageBytes {
		return nil, failure(ctx, "invalid_image", "Qwen image must be a bounded PNG", 0, requestID)
	}
	imageBytes, err := io.ReadAll(io.LimitReader(download.Body, maxImageBytes+1))
	if err != nil || len(imageBytes) == 0 || len(imageBytes) > maxImageBytes {
		return nil, failure(ctx, "invalid_image", "Qwen image download is unreadable or exceeds its size bound", 0, requestID)
	}
	if http.DetectContentType(imageBytes) != "image/png" {
		return nil, failure(ctx, "invalid_image", "Qwen image must be PNG", 0, requestID)
	}
	config, err := png.DecodeConfig(bytes.NewReader(imageBytes))
	if err != nil || config.Width <= 0 || config.Height <= 0 || config.Width > 8192 || config.Height > 8192 {
		return nil, failure(ctx, "invalid_image", "Qwen image dimensions are invalid", 0, requestID)
	}
	pixels := int64(config.Width) * int64(config.Height)
	shortEdge, longEdge := min(config.Width, config.Height), max(config.Width, config.Height)
	if pixels < minImagePixels || pixels > maxImagePixels || longEdge > shortEdge*8 {
		return nil, failure(ctx, "invalid_image", "Qwen image dimensions exceed model bounds", 0, requestID)
	}
	// Decode after checking dimensions to reject corrupt/truncated PNGs while
	// bounding decompression memory by the documented output pixel limit.
	if _, err := png.Decode(bytes.NewReader(imageBytes)); err != nil {
		return nil, failure(ctx, "invalid_image", "Qwen image PNG is invalid", 0, requestID)
	}
	return imageBytes, nil
}

func failure(ctx context.Context, code, message string, status int, requestID string) *Error {
	return &Error{Code: code, Message: message, StatusCode: status, RequestID: requestID, cause: ctx.Err()}
}
