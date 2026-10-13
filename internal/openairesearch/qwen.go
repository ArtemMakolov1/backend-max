package openairesearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"
)

// NewQwen uses Alibaba Cloud Model Studio's OpenAI-compatible Chat Completions
// API. baseURL is the regional origin, without /compatible-mode/v1. The public
// feature contracts, editorial prompts and strict result validators are shared.
func NewQwen(baseURL, apiKey, model string, httpClient *http.Client) (*Client, error) {
	return newClient("qwen", "/compatible-mode/v1/chat/completions", baseURL, apiKey, model, httpClient)
}

func usesExternalResearch(provider string) bool {
	return provider == "deepseek" || provider == "qwen"
}

type qwenChatPayload struct {
	Model          string              `json:"model"`
	Messages       []inputMessage      `json:"messages"`
	EnableThinking bool                `json:"enable_thinking"`
	Stream         bool                `json:"stream"`
	MaxTokens      int                 `json:"max_tokens"`
	Temperature    float64             `json:"temperature"`
	ResponseFormat *qwenResponseFormat `json:"response_format,omitempty"`
}

type qwenResponseFormat struct {
	Type string `json:"type"`
}

type qwenContentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *qwenImageURL `json:"image_url,omitempty"`
}

type qwenImageURL struct {
	URL string `json:"url"`
}

func qwenPayload(payload responsePayload) (qwenChatPayload, error) {
	result := qwenChatPayload{Model: payload.Model, MaxTokens: payload.MaxOutputTokens,
		EnableThinking: false, Stream: false, Temperature: 0.2,
		Messages: make([]inputMessage, 0, len(payload.Input)+1)}
	if len(payload.Input) == 0 || payload.MaxOutputTokens <= 0 {
		return qwenChatPayload{}, errors.New("invalid Qwen completion input")
	}
	for _, message := range payload.Input {
		if message.Role != "system" && message.Role != "user" && message.Role != "assistant" {
			return qwenChatPayload{}, errors.New("unsupported Qwen message role")
		}
		converted := inputMessage{Role: message.Role}
		switch content := message.Content.(type) {
		case string:
			converted.Content = content
		case []inputContentPart:
			parts := make([]qwenContentPart, 0, len(content))
			for _, part := range content {
				switch part.Type {
				case "input_text":
					parts = append(parts, qwenContentPart{Type: "text", Text: part.Text})
				case "input_image":
					parts = append(parts, qwenContentPart{Type: "image_url", ImageURL: &qwenImageURL{URL: part.ImageURL}})
				default:
					return qwenChatPayload{}, errors.New("unsupported Qwen content part")
				}
			}
			converted.Content = parts
		default:
			return qwenChatPayload{}, errors.New("unsupported Qwen message content")
		}
		result.Messages = append(result.Messages, converted)
	}
	if payload.Text != nil {
		if payload.Text.Format.Type != "json_schema" || payload.Text.Format.Schema == nil {
			return qwenChatPayload{}, errors.New("unsupported Qwen structured output format")
		}
		schema, err := json.Marshal(payload.Text.Format.Schema)
		if err != nil {
			return qwenChatPayload{}, errors.New("invalid Qwen structured output schema")
		}
		// json_object guarantees JSON syntax, while the explicit shared schema
		// and server decoders enforce every feature's fields and constraints.
		result.ResponseFormat = &qwenResponseFormat{Type: "json_object"}
		instruction := "\nВерни ровно один объект JSON, строго по схеме ниже. Не добавляй Markdown, блоки кода, пояснения или текст до/после JSON. JSON Schema:\n" + string(schema)
		if result.Messages[0].Role == "system" {
			if text, ok := result.Messages[0].Content.(string); ok {
				result.Messages[0].Content = text + instruction
				return result, nil
			}
		}
		result.Messages = append([]inputMessage{{Role: "system", Content: instruction}}, result.Messages...)
	}
	return result, nil
}

func (c *Client) callQwen(ctx context.Context, payload responsePayload) (responseEnvelope, error) {
	// Research always comes from the independently configured search provider.
	// Never silently drop a requested tool and turn its output into assertions.
	if len(payload.Tools) != 0 || payload.ToolChoice != "" || payload.MaxToolCalls != 0 || len(payload.Include) != 0 {
		return responseEnvelope{}, c.contentSearchNotConfigured()
	}
	completion, err := qwenPayload(payload)
	if err != nil {
		return responseEnvelope{}, err
	}
	body, err := json.Marshal(completion)
	if err != nil {
		return responseEnvelope{}, errors.New("encode Qwen completion request failed")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+c.responsePath, bytes.NewReader(body))
	if err != nil {
		return responseEnvelope{}, errors.New("create Qwen completion request failed")
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	requestClient := *c.httpClient
	requestClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	// #nosec G704 -- baseURL is deployment-owned configuration validated by NewQwen; no user URL input and redirects are disabled above.
	resp, err := requestClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return responseEnvelope{}, ctx.Err()
		}
		// Transport errors may contain endpoint URLs or credential-bearing text
		// supplied by a proxy. Expose only a fixed diagnostic to API logs.
		return responseEnvelope{}, &Error{Provider: c.provider, Code: "provider_transport_error", Message: "Qwen completion request failed"}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return responseEnvelope{}, ctx.Err()
		}
		return responseEnvelope{}, &Error{Provider: c.provider, Code: "invalid_provider_response", Message: "Qwen completion response could not be read"}
	}
	if len(raw) > maxResponseBytes {
		return responseEnvelope{}, &Error{Provider: c.provider, Code: "invalid_provider_response", Message: "Qwen completion response is too large"}
	}
	requestID := strings.TrimSpace(resp.Header.Get("x-request-id"))
	if len(requestID) > 128 || !safeRequestIDPattern.MatchString(requestID) || strings.Contains(requestID, c.apiKey) {
		requestID = ""
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return responseEnvelope{}, &Error{Provider: c.provider, StatusCode: resp.StatusCode, Code: "provider_http_error", Message: http.StatusText(resp.StatusCode), RequestID: requestID}
	}
	return decodeQwenCompletion(raw, requestID, completion.ResponseFormat != nil)
}

func decodeQwenCompletion(raw []byte, requestID string, structured bool) (responseEnvelope, error) {
	var result struct {
		Error   json.RawMessage `json:"error"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Role         string            `json:"role"`
				Content      string            `json:"content"`
				Refusal      json.RawMessage   `json:"refusal"`
				ToolCalls    []json.RawMessage `json:"tool_calls"`
				FunctionCall json.RawMessage   `json:"function_call"`
			} `json:"message"`
		} `json:"choices"`
	}
	envelope := responseEnvelope{provider: "qwen", structuredJSON: structured, Status: "completed", RequestID: requestID}
	if !utf8.Valid(raw) || json.Unmarshal(raw, &result) != nil {
		return responseEnvelope{}, responseError(envelope, "invalid_provider_response", "Qwen completion response is invalid")
	}
	if presentQwenValue(result.Error) {
		return responseEnvelope{}, responseError(envelope, "response_failed", "Qwen could not complete the response")
	}
	if len(result.Choices) != 1 {
		return responseEnvelope{}, responseError(envelope, "invalid_provider_response", "Qwen completion requires one choice")
	}
	choice := result.Choices[0]
	if presentQwenValue(choice.Message.Refusal) {
		return responseEnvelope{}, responseError(envelope, "response_refused", "Qwen refused the request")
	}
	if choice.FinishReason != "stop" {
		code := "response_not_completed"
		if choice.FinishReason == "length" || choice.FinishReason == "content_filter" {
			code = "response_incomplete"
		}
		return responseEnvelope{}, responseError(envelope, code, "Qwen completion was not completed")
	}
	if choice.Message.Role != "assistant" || len(choice.Message.ToolCalls) != 0 || presentQwenValue(choice.Message.FunctionCall) {
		return responseEnvelope{}, responseError(envelope, "invalid_provider_response", "Qwen completion is not an assistant text response")
	}
	if strings.TrimSpace(choice.Message.Content) == "" {
		return responseEnvelope{}, responseError(envelope, "missing_output_text", "Qwen completion does not contain text")
	}
	envelope.OutputText = choice.Message.Content
	return envelope, nil
}

func presentQwenValue(raw json.RawMessage) bool {
	value := strings.TrimSpace(string(raw))
	return value != "" && value != "null" && value != `""`
}
