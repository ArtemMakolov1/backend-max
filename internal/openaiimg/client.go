package openaiimg

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const maxResponseBytes = 80 << 20

const (
	minGPTImage2Pixels = 655_360
	maxGPTImage2Pixels = 8_294_400
	maxGPTImage2Edge   = 3_840
)

type Client struct {
	baseURL    string
	apiKey     string
	model      string
	httpClient *http.Client
}

type GenerateRequest struct {
	Prompt  string `json:"prompt"`
	Size    string `json:"size,omitempty"`
	Quality string `json:"quality,omitempty"`
}

type Result struct {
	Bytes     []byte
	MIMEType  string
	Model     string
	RequestID string
}

type Error struct {
	StatusCode int
	Code       string
	Message    string
	RequestID  string
}

func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("OpenAI API error (%s): %s", e.Code, e.Message)
	}
	return fmt.Sprintf("OpenAI API error (status %d): %s", e.StatusCode, e.Message)
}

func New(baseURL, apiKey, model string, httpClient *http.Client) (*Client, error) {
	parsed, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || !parsed.IsAbs() || parsed.Host == "" {
		return nil, errors.New("OpenAI API base URL must be absolute")
	}
	if strings.TrimSpace(apiKey) == "" || strings.TrimSpace(model) == "" || httpClient == nil {
		return nil, errors.New("OpenAI API key, model and HTTP client are required")
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, model: model, httpClient: httpClient}, nil
}

func (c *Client) Generate(ctx context.Context, request GenerateRequest) (Result, error) {
	request.Prompt = strings.TrimSpace(request.Prompt)
	if err := c.Validate(request); err != nil {
		return Result{}, err
	}
	if request.Size == "" {
		request.Size = "1024x1024"
	}
	if request.Quality == "" {
		request.Quality = "medium"
	}

	payload := struct {
		Model        string `json:"model"`
		Prompt       string `json:"prompt"`
		Size         string `json:"size"`
		Quality      string `json:"quality"`
		N            int    `json:"n"`
		OutputFormat string `json:"output_format"`
	}{c.model, request.Prompt, request.Size, request.Quality, 1, "png"}
	body, err := json.Marshal(payload)
	if err != nil {
		return Result{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/images/generations", bytes.NewReader(body))
	if err != nil {
		return Result{}, fmt.Errorf("create OpenAI image request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	requestClient := *c.httpClient
	requestClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	// #nosec G704 -- baseURL is deployment-owned configuration validated as an absolute URL in New, never HTTP request input; redirects are disabled above.
	resp, err := requestClient.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("call OpenAI image API: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return Result{}, fmt.Errorf("read OpenAI image response: %w", err)
	}
	if len(responseBody) > maxResponseBytes {
		return Result{}, errors.New("OpenAI image response is too large")
	}
	requestID := resp.Header.Get("x-request-id")
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var envelope struct {
			Error struct {
				Message string          `json:"message"`
				Code    json.RawMessage `json:"code"`
				Type    string          `json:"type"`
			} `json:"error"`
		}
		_ = json.Unmarshal(responseBody, &envelope)
		code := strings.Trim(string(envelope.Error.Code), `"`)
		if code == "" {
			code = envelope.Error.Type
		}
		message := envelope.Error.Message
		if message == "" {
			message = http.StatusText(resp.StatusCode)
		}
		return Result{}, &Error{StatusCode: resp.StatusCode, Code: code, Message: message, RequestID: requestID}
	}

	var envelope struct {
		Data []struct {
			Base64 string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.Unmarshal(responseBody, &envelope); err != nil {
		return Result{}, fmt.Errorf("decode OpenAI image response: %w", err)
	}
	if len(envelope.Data) == 0 || envelope.Data[0].Base64 == "" {
		return Result{}, errors.New("OpenAI image response does not contain image data")
	}
	imageBytes, err := base64.StdEncoding.DecodeString(envelope.Data[0].Base64)
	if err != nil {
		return Result{}, fmt.Errorf("decode OpenAI image: %w", err)
	}
	if len(imageBytes) == 0 {
		return Result{}, errors.New("OpenAI returned an empty image")
	}
	return Result{Bytes: imageBytes, MIMEType: "image/png", Model: c.model, RequestID: requestID}, nil
}

func validateSize(model, size string) error {
	if size == "auto" {
		return nil
	}
	if !strings.HasPrefix(model, "gpt-image-2") {
		switch size {
		case "1024x1024", "1536x1024", "1024x1536":
			return nil
		default:
			return errors.New("image size must be auto, 1024x1024, 1536x1024 or 1024x1536")
		}
	}

	parts := strings.Split(size, "x")
	if len(parts) != 2 {
		return errors.New("GPT Image 2 size must use WIDTHxHEIGHT, for example 1024x1024")
	}
	width, widthErr := strconv.Atoi(parts[0])
	height, heightErr := strconv.Atoi(parts[1])
	if widthErr != nil || heightErr != nil || width <= 0 || height <= 0 {
		return errors.New("GPT Image 2 width and height must be positive integers")
	}
	if width%16 != 0 || height%16 != 0 {
		return errors.New("GPT Image 2 width and height must be divisible by 16")
	}
	if width > maxGPTImage2Edge || height > maxGPTImage2Edge {
		return fmt.Errorf("GPT Image 2 width and height must not exceed %d pixels", maxGPTImage2Edge)
	}
	shortEdge, longEdge := width, height
	if shortEdge > longEdge {
		shortEdge, longEdge = longEdge, shortEdge
	}
	if longEdge > shortEdge*3 {
		return errors.New("GPT Image 2 aspect ratio must be between 1:3 and 3:1")
	}
	pixels := int64(width) * int64(height)
	if pixels < minGPTImage2Pixels || pixels > maxGPTImage2Pixels {
		return fmt.Errorf("GPT Image 2 size must contain between %d and %d pixels", minGPTImage2Pixels, maxGPTImage2Pixels)
	}
	return nil
}
