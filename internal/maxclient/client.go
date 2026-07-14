package maxclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	maxJSONResponseBytes = 2 << 20
	maxImageBytes        = 50 << 20
	maxUploadRedirects   = 5
)

// Client is safe for concurrent use as long as its supplied http.Client is not
// mutated concurrently.
type Client struct {
	baseURL    *url.URL
	token      string
	httpClient *http.Client
}

// New constructs a MAX API client. baseURL is explicit so configuration owns
// the choice of MAX endpoint; httpClient owns transport and timeout policy.
func New(baseURL, token string, httpClient *http.Client) (*Client, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, errors.New("MAX API base URL is required")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("MAX API token is required")
	}
	if httpClient == nil {
		return nil, errors.New("MAX API http client is required")
	}

	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse MAX API base URL: %w", err)
	}
	if !parsed.IsAbs() || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, errors.New("MAX API base URL must be an absolute HTTP(S) URL")
	}
	if parsed.User != nil {
		return nil, errors.New("MAX API base URL must not contain user info")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("MAX API base URL must not contain a query or fragment")
	}

	copyURL := *parsed
	copyURL.Path = strings.TrimRight(copyURL.Path, "/")
	return &Client{baseURL: &copyURL, token: token, httpClient: httpClient}, nil
}

// Test validates the configured credentials with GET /me.
func (c *Client) Test(ctx context.Context) error {
	_, err := c.GetMe(ctx)
	return err
}

// GetMe returns information about the bot identified by the configured token.
func (c *Client) GetMe(ctx context.Context) (BotInfo, error) {
	var bot BotInfo
	if err := c.doJSON(ctx, http.MethodGet, "/me", nil, nil, &bot); err != nil {
		return BotInfo{}, err
	}
	return bot, nil
}

// GetChat returns channel metadata and is safe to use as a read-only access
// check. MAX chat IDs are int64 values but remain strings in the application to
// avoid precision loss in browser clients.
func (c *Client) GetChat(ctx context.Context, chatID string) (ChatInfo, error) {
	if !numericID(chatID) {
		return ChatInfo{}, errors.New("get MAX chat: chat ID must be numeric")
	}
	return c.getChat(ctx, chatID)
}

// ResolveChat resolves a public max.ru link or channel slug without changing
// any MAX state.
func (c *Client) ResolveChat(ctx context.Context, publicLink string) (ChatInfo, error) {
	slug, err := NormalizeChatLink(publicLink)
	if err != nil {
		return ChatInfo{}, err
	}
	return c.getChat(ctx, slug)
}

func (c *Client) getChat(ctx context.Context, identifier string) (ChatInfo, error) {
	var response struct {
		ChatID            json.RawMessage `json:"chat_id"`
		Type              string          `json:"type"`
		Status            string          `json:"status"`
		Title             string          `json:"title"`
		Link              string          `json:"link,omitempty"`
		Icon              ChatIcon        `json:"icon,omitempty"`
		ParticipantsCount int             `json:"participants_count,omitempty"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/chats/"+url.PathEscape(identifier), nil, nil, &response); err != nil {
		return ChatInfo{}, err
	}
	chat := ChatInfo{
		ChatID: jsonCode(response.ChatID), Type: response.Type, Status: response.Status,
		Title: response.Title, Link: response.Link, Icon: response.Icon,
		ParticipantsCount: response.ParticipantsCount,
	}
	if !numericID(chat.ChatID) {
		return ChatInfo{}, errors.New("MAX chat response does not contain a numeric chat_id")
	}
	return chat, nil
}

// GetMembership returns the bot's current membership and admin permissions.
func (c *Client) GetMembership(ctx context.Context, chatID string) (Membership, error) {
	if !numericID(chatID) {
		return Membership{}, errors.New("get MAX membership: chat ID must be numeric")
	}
	var membership Membership
	if err := c.doJSON(ctx, http.MethodGet, "/chats/"+url.PathEscape(chatID)+"/members/me", nil, nil, &membership); err != nil {
		return Membership{}, err
	}
	return membership, nil
}

// UploadImage reserves an image upload, sends multipart field "data" to the
// returned HTTPS URL, and returns the resulting attachment token.
func (c *Client) UploadImage(ctx context.Context, filename string, image io.Reader) (UploadResult, error) {
	if ctx == nil {
		return UploadResult{}, errors.New("upload image: nil context")
	}
	if filename == "" {
		return UploadResult{}, errors.New("upload image: filename is required")
	}
	if image == nil {
		return UploadResult{}, errors.New("upload image: reader is required")
	}

	query := url.Values{"type": []string{"image"}}
	var reservation struct {
		URL   string `json:"url"`
		Token string `json:"token,omitempty"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/uploads", query, nil, &reservation); err != nil {
		return UploadResult{}, err
	}

	uploadURL, err := validateUploadURL(reservation.URL)
	if err != nil {
		return UploadResult{}, fmt.Errorf("MAX image upload URL: %w", err)
	}

	var body bytes.Buffer
	multipartWriter := multipart.NewWriter(&body)
	part, err := multipartWriter.CreateFormFile("data", filename)
	if err != nil {
		return UploadResult{}, fmt.Errorf("create image multipart body: %w", err)
	}

	written, copyErr := io.CopyN(part, image, maxImageBytes+1)
	if copyErr != nil && !errors.Is(copyErr, io.EOF) {
		return UploadResult{}, fmt.Errorf("read image: %w", copyErr)
	}
	if written > maxImageBytes {
		return UploadResult{}, fmt.Errorf("image is larger than %d bytes", maxImageBytes)
	}
	if err := multipartWriter.Close(); err != nil {
		return UploadResult{}, fmt.Errorf("finish image multipart body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL.String(), bytes.NewReader(body.Bytes()))
	if err != nil {
		return UploadResult{}, fmt.Errorf("create image upload request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", multipartWriter.FormDataContentType())
	// The signed upload URL is a separate origin and must never receive the bot
	// credential, even if a caller's custom transport adds defaults elsewhere.
	req.Header.Del("Authorization")

	uploadClient := *c.httpClient
	callerRedirectPolicy := uploadClient.CheckRedirect
	uploadClient.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if len(via) >= maxUploadRedirects {
			return errors.New("too many image upload redirects")
		}
		if _, err := validateUploadURL(next.URL.String()); err != nil {
			return fmt.Errorf("unsafe image upload redirect: %w", err)
		}
		if next.Method != http.MethodPost {
			return fmt.Errorf("unsafe image upload redirect changed method to %s", next.Method)
		}
		if callerRedirectPolicy != nil {
			if err := callerRedirectPolicy(next, via); err != nil {
				return err
			}
		}
		// Run this after a caller policy as well, so that policy cannot
		// accidentally reintroduce the bot credential on the storage origin.
		next.Header.Del("Authorization")
		return nil
	}

	resp, err := uploadClient.Do(req)
	if err != nil {
		return UploadResult{}, fmt.Errorf("upload image to MAX storage: %w", err)
	}
	defer resp.Body.Close()

	responseBody, readErr := readJSONBody(resp.Body)
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		if readErr != nil {
			return UploadResult{}, responseReadError(resp, responseBody, readErr)
		}
		return UploadResult{}, apiError(resp, responseBody)
	}
	if readErr != nil {
		return UploadResult{}, fmt.Errorf("read MAX image upload response: %w", readErr)
	}

	var result struct {
		Token string `json:"token"`
	}
	if err := decodeJSON(responseBody, &result); err != nil {
		return UploadResult{}, fmt.Errorf("decode MAX image upload response: %w", err)
	}
	if result.Token == "" {
		result.Token = reservation.Token
	}
	if result.Token == "" {
		return UploadResult{}, errors.New("MAX image upload response does not contain a token")
	}

	return UploadResult{Token: result.Token}, nil
}

// Publish creates a post in a MAX chat or channel.
func (c *Client) Publish(ctx context.Context, request PublishRequest) (Message, error) {
	if request.ChatID == "" {
		return Message{}, errors.New("publish MAX post: chat ID is required")
	}
	if !validFormat(request.Format) {
		return Message{}, fmt.Errorf("publish MAX post: unsupported format %q", request.Format)
	}

	attachments, err := imageAttachments(request.ImageTokens)
	if err != nil {
		return Message{}, fmt.Errorf("publish MAX post: %w", err)
	}
	body := messageBody{
		Text:        request.Text,
		Attachments: attachments,
		Notify:      request.Notify,
		Format:      request.Format,
	}
	query := url.Values{
		"chat_id":              []string{request.ChatID},
		"disable_link_preview": []string{strconv.FormatBool(request.DisableLinkPreview)},
	}

	var response struct {
		Message apiMessage `json:"message"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/messages", query, body, &response); err != nil {
		return Message{}, err
	}
	return response.Message.publicMessage(), nil
}

// Edit updates a post previously sent by the bot.
func (c *Client) Edit(ctx context.Context, request EditRequest) error {
	if request.MessageID == "" {
		return errors.New("edit MAX post: message ID is required")
	}
	if !validFormat(request.Format) {
		return fmt.Errorf("edit MAX post: unsupported format %q", request.Format)
	}

	attachments, err := imageAttachments(request.ImageTokens)
	if err != nil {
		return fmt.Errorf("edit MAX post: %w", err)
	}
	body := messageBody{
		Text:        request.Text,
		Attachments: attachments,
		Notify:      request.Notify,
		Format:      request.Format,
	}
	query := url.Values{"message_id": []string{request.MessageID}}

	var response operationResponse
	if err := c.doJSON(ctx, http.MethodPut, "/messages", query, body, &response); err != nil {
		return err
	}
	return response.asError(http.StatusOK)
}

// Delete removes a post previously sent by the bot.
func (c *Client) Delete(ctx context.Context, messageID string) error {
	if messageID == "" {
		return errors.New("delete MAX post: message ID is required")
	}

	query := url.Values{"message_id": []string{messageID}}
	var response operationResponse
	if err := c.doJSON(ctx, http.MethodDelete, "/messages", query, nil, &response); err != nil {
		return err
	}
	return response.asError(http.StatusOK)
}

type operationResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
}

func (r operationResponse) asError(status int) error {
	if r.Success {
		return nil
	}
	return &Error{
		StatusCode: status,
		Code:       "operation_failed",
		Message:    r.Message,
	}
}

func (c *Client) doJSON(ctx context.Context, method, endpointPath string, query url.Values, body, output any) error {
	if ctx == nil {
		return errors.New("MAX API request: nil context")
	}

	requestURL := *c.baseURL
	requestURL.Path = strings.TrimRight(requestURL.Path, "/") + "/" + strings.TrimLeft(endpointPath, "/")
	requestURL.RawQuery = query.Encode()

	var requestBody io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode MAX API request: %w", err)
		}
		requestBody = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, requestURL.String(), requestBody)
	if err != nil {
		return fmt.Errorf("create MAX API request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("MAX API %s %s: %w", method, endpointPath, err)
	}
	defer resp.Body.Close()

	responseBody, readErr := readJSONBody(resp.Body)
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		if readErr != nil {
			return responseReadError(resp, responseBody, readErr)
		}
		return apiError(resp, responseBody)
	}
	if readErr != nil {
		return fmt.Errorf("read MAX API response: %w", readErr)
	}
	if output == nil {
		return nil
	}
	if err := decodeJSON(responseBody, output); err != nil {
		return fmt.Errorf("decode MAX API response: %w", err)
	}
	return nil
}

func validateUploadURL(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, errors.New("URL is empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if !u.IsAbs() || u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("URL must be an absolute HTTPS URL")
	}
	if u.User != nil {
		return nil, errors.New("URL must not contain user info")
	}
	if u.Fragment != "" {
		return nil, errors.New("URL must not contain a fragment")
	}
	return u, nil
}

func readJSONBody(reader io.Reader) ([]byte, error) {
	limited := io.LimitReader(reader, maxJSONResponseBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return body, err
	}
	if len(body) > maxJSONResponseBytes {
		return body[:maxJSONResponseBytes], fmt.Errorf("JSON response exceeds %d bytes", maxJSONResponseBytes)
	}
	return body, nil
}

func decodeJSON(body []byte, output any) error {
	if len(bytes.TrimSpace(body)) == 0 {
		return errors.New("empty JSON response")
	}
	if err := json.Unmarshal(body, output); err != nil {
		return err
	}
	return nil
}

func apiError(resp *http.Response, body []byte) *Error {
	parsed := struct {
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
		Error   string          `json:"error"`
	}{}
	_ = json.Unmarshal(body, &parsed)

	message := parsed.Message
	if message == "" {
		message = parsed.Error
	}
	return &Error{
		StatusCode: resp.StatusCode,
		Code:       jsonCode(parsed.Code),
		Message:    message,
		RetryAfter: retryAfter(resp.Header.Get("Retry-After"), time.Now()),
		RequestID:  firstHeader(resp.Header, "X-Request-Id", "X-Request-ID", "X-Max-Request-Id"),
		Body:       string(body),
	}
}

func responseReadError(resp *http.Response, body []byte, readErr error) *Error {
	return &Error{
		StatusCode: resp.StatusCode,
		Message:    readErr.Error(),
		RetryAfter: retryAfter(resp.Header.Get("Retry-After"), time.Now()),
		RequestID:  firstHeader(resp.Header, "X-Request-Id", "X-Request-ID", "X-Max-Request-Id"),
		Body:       string(body),
	}
}

func retryAfter(value string, now time.Time) time.Duration {
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if date, err := http.ParseTime(value); err == nil && date.After(now) {
		return date.Sub(now)
	}
	return 0
}

func firstHeader(headers http.Header, names ...string) string {
	for _, name := range names {
		if value := headers.Get(name); value != "" {
			return value
		}
	}
	return ""
}

func numericID(value string) bool {
	if value == "" || value[0] == '+' {
		return false
	}
	_, err := strconv.ParseInt(value, 10, 64)
	return err == nil
}

// NormalizeChatLink converts https://max.ru/<slug>, max.ru/<slug>, @slug and
// slug into the chatLink path parameter accepted by MAX.
func NormalizeChatLink(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("MAX public_link is required")
	}
	if strings.HasPrefix(strings.ToLower(value), "max.ru/") || strings.HasPrefix(strings.ToLower(value), "www.max.ru/") {
		value = "https://" + value
	}
	if strings.Contains(value, "://") {
		parsed, err := url.Parse(value)
		host := ""
		if parsed != nil {
			host = strings.ToLower(parsed.Hostname())
		}
		if err != nil || parsed.Scheme != "https" || (host != "max.ru" && host != "www.max.ru") || parsed.Port() != "" || parsed.User != nil {
			return "", errors.New("MAX public_link must be an https://max.ru channel URL")
		}
		value = strings.Trim(parsed.EscapedPath(), "/")
		decoded, err := url.PathUnescape(value)
		if err != nil {
			return "", errors.New("MAX public_link contains invalid escaping")
		}
		value = decoded
	}
	value = strings.Trim(value, "/")
	if !validChatSlug(value) {
		return "", errors.New("MAX public_link must contain a single channel slug")
	}
	return value, nil
}

func validChatSlug(value string) bool {
	if strings.HasPrefix(value, "@") {
		value = strings.TrimPrefix(value, "@")
	}
	if value == "" || !asciiLetter(value[0]) {
		return false
	}
	for i := 1; i < len(value); i++ {
		char := value[i]
		if !asciiLetter(char) && (char < '0' || char > '9') && char != '_' && char != '-' {
			return false
		}
	}
	return true
}

func asciiLetter(char byte) bool {
	return (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z')
}
