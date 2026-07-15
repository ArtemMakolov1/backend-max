package maxclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxJSONResponseBytes = 2 << 20
	maxPublicPageBytes   = 2 << 20
	maxImageBytes        = 50 << 20
	maxUploadRedirects   = 5
)

var (
	publicChannelIDPattern = regexp.MustCompile(`\bchannelId\b(?:\\?["'])?\s*:\s*(?:\\?["'])?([0-9]{1,19})\b`)
	messageIDPattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,255}$`)
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

// GetChatByLink resolves only a validated public MAX slug. Normalization strips
// URL query/fragment data and rejects alternate hosts, userinfo and nested or
// escaped paths before anything is appended to the API URL.
func (c *Client) GetChatByLink(ctx context.Context, publicLink string) (ChatInfo, error) {
	slug, err := NormalizeChatLink(publicLink)
	if err != nil {
		return ChatInfo{}, err
	}
	chat, err := c.getChat(ctx, slug)
	if err == nil || !isChatNotFound(err) {
		return chat, err
	}
	chat, fallbackErr := c.getChatFromPublicPage(ctx, slug)
	if fallbackErr != nil {
		// Preserve the structured upstream error for the API layer while keeping
		// the fallback reason available to operators and tests.
		return ChatInfo{}, fmt.Errorf("resolve MAX public channel page: %w", errors.Join(fallbackErr, err))
	}
	return chat, nil
}

func (c *Client) getChatFromPublicPage(ctx context.Context, slug string) (ChatInfo, error) {
	publicSlug := strings.TrimPrefix(slug, "@")
	if !validChatSlug(publicSlug) {
		return ChatInfo{}, errors.New("MAX public channel slug is invalid")
	}
	publicURL := (&url.URL{Scheme: "https", Host: "max.ru", Path: "/" + publicSlug}).String()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, publicURL, nil)
	if err != nil {
		return ChatInfo{}, fmt.Errorf("create MAX public channel request: %w", err)
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; MaxPosty/1.0; +https://maxposty.ru)")
	req.Header.Del("Authorization")
	req.Header.Del("Cookie")

	publicClient := *c.httpClient
	publicClient.Jar = nil
	publicClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	// #nosec G704 -- publicURL is constructed locally from the fixed HTTPS max.ru origin and a slug restricted to ASCII letters, digits, underscores and hyphens; redirects are disabled and no credentials are attached.
	resp, err := publicClient.Do(req)
	if err != nil {
		return ChatInfo{}, fmt.Errorf("fetch MAX public channel page: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ChatInfo{}, fmt.Errorf("MAX public channel page returned status %d", resp.StatusCode)
	}
	mediaType, _, mediaErr := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mediaErr != nil || (mediaType != "text/html" && mediaType != "application/xhtml+xml") {
		return ChatInfo{}, errors.New("MAX public channel page did not return HTML")
	}
	body, err := readBoundedBody(resp.Body, maxPublicPageBytes, "MAX public channel page")
	if err != nil {
		return ChatInfo{}, err
	}
	chatID, err := parsePublicChannelID(body)
	if err != nil {
		return ChatInfo{}, err
	}
	chat, err := c.getChat(ctx, chatID)
	if err != nil {
		return ChatInfo{}, fmt.Errorf("get MAX chat discovered from public page: %w", err)
	}
	canonicalSlug, err := NormalizeChatLink(chat.Link)
	if err != nil || !strings.EqualFold(strings.TrimPrefix(canonicalSlug, "@"), publicSlug) {
		return ChatInfo{}, errors.New("MAX public channel page did not match the canonical API link")
	}
	return chat, nil
}

func parsePublicChannelID(body []byte) (string, error) {
	var discovered int64
	remaining := body
	for len(remaining) != 0 {
		match := publicChannelIDPattern.FindSubmatchIndex(remaining)
		if match == nil {
			break
		}
		value, err := strconv.ParseInt(string(remaining[match[2]:match[3]]), 10, 64)
		if err != nil || value <= 0 {
			return "", errors.New("MAX public channel page contains an invalid channelId")
		}
		if discovered != 0 && discovered != value {
			return "", errors.New("MAX public channel page contains ambiguous channelId values")
		}
		discovered = value
		remaining = remaining[match[1]:]
	}
	if discovered == 0 {
		return "", errors.New("MAX public channel page does not contain channelId")
	}
	return strconv.FormatInt(-discovered, 10), nil
}

func isChatNotFound(err error) bool {
	var apiErr *Error
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound && apiErr.Code == "chat.not.found"
}

func (c *Client) getChat(ctx context.Context, identifier string) (ChatInfo, error) {
	var response struct {
		ChatID            json.RawMessage `json:"chat_id"`
		OwnerID           json.RawMessage `json:"owner_id"`
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
		ChatID: jsonCode(response.ChatID), OwnerID: jsonCode(response.OwnerID), Type: response.Type, Status: response.Status,
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

// GetMessage returns the current message metadata exposed by MAX, including
// the canonical public URL and view count. It is intentionally read-only.
func (c *Client) GetMessage(ctx context.Context, messageID string) (Message, error) {
	if !validMessageID(messageID) {
		return Message{}, errors.New("get MAX message: message ID is invalid")
	}
	var response apiMessage
	if err := c.doJSON(ctx, http.MethodGet, "/messages/"+url.PathEscape(messageID), nil, nil, &response); err != nil {
		return Message{}, err
	}
	message := response.publicMessage()
	if !validMessageID(message.MessageID) || message.MessageID != messageID {
		return Message{}, errors.New("MAX message response does not match the requested message ID")
	}
	if message.ChatID != "" && !numericID(message.ChatID) {
		return Message{}, errors.New("MAX message response contains an invalid chat ID")
	}
	if message.Views != nil && *message.Views < 0 {
		return Message{}, errors.New("MAX message response contains a negative view count")
	}
	return message, nil
}

// GetPinnedMessage returns the message currently pinned in a chat. MAX 404
// responses are deliberately preserved as structured upstream errors so a
// caller never mistakes an inaccessible chat or message for "not pinned".
func (c *Client) GetPinnedMessage(ctx context.Context, chatID string) (*Message, error) {
	if !numericID(chatID) {
		return nil, errors.New("get pinned MAX message: chat ID must be numeric")
	}
	// The official MAX contract wraps the nullable message in a top-level
	// `message` property. Keep accepting the former direct-message shape during
	// the API transition, but prefer and validate the documented envelope.
	var response json.RawMessage
	if err := c.doJSON(ctx, http.MethodGet, "/chats/"+url.PathEscape(chatID)+"/pin", nil, nil, &response); err != nil {
		return nil, err
	}
	wireMessage, err := decodePinnedMessage(response)
	if err != nil {
		return nil, err
	}
	if wireMessage == nil {
		return nil, nil
	}
	message := wireMessage.publicMessage()
	if !validMessageID(message.MessageID) {
		return nil, errors.New("MAX pinned message response does not contain a valid message ID")
	}
	if message.ChatID != "" && !numericID(message.ChatID) {
		return nil, errors.New("MAX pinned message response contains an invalid chat ID")
	}
	if message.Views != nil && *message.Views < 0 {
		return nil, errors.New("MAX pinned message response contains a negative view count")
	}
	return &message, nil
}

func decodePinnedMessage(response json.RawMessage) (*apiMessage, error) {
	trimmed := bytes.TrimSpace(response)
	if bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}

	var envelope struct {
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return nil, fmt.Errorf("decode MAX pinned message response: %w", err)
	}
	if len(envelope.Message) != 0 {
		if bytes.Equal(bytes.TrimSpace(envelope.Message), []byte("null")) {
			return nil, nil
		}
		var message apiMessage
		if err := json.Unmarshal(envelope.Message, &message); err != nil {
			return nil, fmt.Errorf("decode MAX pinned message: %w", err)
		}
		return &message, nil
	}

	// Older MAX responses exposed Message as the root object. An empty object
	// is also a valid representation of the documented optional result.
	var message apiMessage
	if err := json.Unmarshal(trimmed, &message); err != nil {
		return nil, fmt.Errorf("decode legacy MAX pinned message: %w", err)
	}
	if !validMessageID(message.publicMessage().MessageID) {
		return nil, nil
	}
	return &message, nil
}

// PinMessage pins a message without notifying channel subscribers.
func (c *Client) PinMessage(ctx context.Context, chatID, messageID string) error {
	if !numericID(chatID) {
		return errors.New("pin MAX message: chat ID must be numeric")
	}
	if !validMessageID(messageID) {
		return errors.New("pin MAX message: message ID is invalid")
	}
	body := struct {
		MessageID string `json:"message_id"`
		Notify    bool   `json:"notify"`
	}{MessageID: messageID, Notify: false}
	var response operationResponse
	if err := c.doJSON(ctx, http.MethodPut, "/chats/"+url.PathEscape(chatID)+"/pin", nil, body, &response); err != nil {
		return err
	}
	return response.asError(http.StatusOK)
}

// UnpinMessage removes the current pin from a chat.
func (c *Client) UnpinMessage(ctx context.Context, chatID string) error {
	if !numericID(chatID) {
		return errors.New("unpin MAX message: chat ID must be numeric")
	}
	var response operationResponse
	if err := c.doJSON(ctx, http.MethodDelete, "/chats/"+url.PathEscape(chatID)+"/pin", nil, nil, &response); err != nil {
		return err
	}
	return response.asError(http.StatusOK)
}

// SendClaimConfirmation asks the MAX account that opened the deep link to
// explicitly approve or cancel connecting the named channel. Callback payloads
// are opaque one-time values and are never included in application logs.
func (c *Client) SendClaimConfirmation(ctx context.Context, userID, channelTitle, channelLink, requesterLabel, comparisonCode, confirmPayload, cancelPayload string) error {
	if !numericID(userID) {
		return errors.New("send claim confirmation: MAX user ID must be numeric")
	}
	for _, payload := range []string{confirmPayload, cancelPayload} {
		if payload == "" || len(payload) > 128 {
			return errors.New("send claim confirmation: callback payload must contain 1 to 128 bytes")
		}
	}
	title := strings.TrimSpace(channelTitle)
	if title == "" {
		title = "канал MAX"
	}
	requesterLabel = strings.TrimSpace(requesterLabel)
	if requesterLabel == "" || len(comparisonCode) != 6 {
		return errors.New("send claim confirmation: requester label and six-digit comparison code are required")
	}
	text := "Подключить канал «" + title + "» к аккаунту MaxPosty «" + requesterLabel + "»?\n\n" +
		"Код проверки: " + comparisonCode + "\nПодтвердите только если такой же код показан в MaxPosty."
	if link := strings.TrimSpace(channelLink); link != "" {
		text += "\n" + link
	}
	body := struct {
		Text        string `json:"text"`
		Attachments []any  `json:"attachments"`
	}{
		Text: text,
		Attachments: []any{map[string]any{
			"type": "inline_keyboard",
			"payload": map[string]any{"buttons": [][]map[string]string{{
				{"type": "callback", "text": "Подключить", "payload": confirmPayload},
				{"type": "callback", "text": "Отмена", "payload": cancelPayload},
			}}},
		}},
	}
	return c.doJSON(ctx, http.MethodPost, "/messages", url.Values{"user_id": {userID}}, body, nil)
}

// SendIdentityLinkConfirmation asks a MAX user to explicitly bind that MAX
// identity to the named, authenticated MaxPosty account.
func (c *Client) SendIdentityLinkConfirmation(ctx context.Context, userID, requesterLabel, comparisonCode, confirmPayload, cancelPayload string) error {
	if !numericID(userID) {
		return errors.New("send identity confirmation: MAX user ID must be numeric")
	}
	for _, payload := range []string{confirmPayload, cancelPayload} {
		if payload == "" || len(payload) > 128 {
			return errors.New("send identity confirmation: callback payload must contain 1 to 128 bytes")
		}
	}
	requesterLabel = strings.TrimSpace(requesterLabel)
	if requesterLabel == "" || len(comparisonCode) != 6 {
		return errors.New("send identity confirmation: requester label and six-digit comparison code are required")
	}
	text := "Связать этот профиль MAX с аккаунтом MaxPosty «" + requesterLabel + "»?\n\n" +
		"Код проверки: " + comparisonCode + "\nПодтвердите только если такой же код показан в MaxPosty."
	body := struct {
		Text        string `json:"text"`
		Attachments []any  `json:"attachments"`
	}{
		Text: text,
		Attachments: []any{map[string]any{
			"type": "inline_keyboard",
			"payload": map[string]any{"buttons": [][]map[string]string{{
				{"type": "callback", "text": "Связать", "payload": confirmPayload},
				{"type": "callback", "text": "Отмена", "payload": cancelPayload},
			}}},
		}},
	}
	return c.doJSON(ctx, http.MethodPost, "/messages", url.Values{"user_id": {userID}}, body, nil)
}

type callbackAnswerMessage struct {
	Text        string `json:"text"`
	Attachments []any  `json:"attachments"`
}

type callbackAnswerRequest struct {
	Notification string                 `json:"notification,omitempty"`
	Message      *callbackAnswerMessage `json:"message,omitempty"`
}

// AnswerCallback acknowledges a button press and can replace the source
// message in the same atomic MAX API call. An empty attachments array removes
// the now-obsolete inline keyboard so a completed action cannot be repeated.
func (c *Client) AnswerCallback(ctx context.Context, callbackID, notification, messageText string) error {
	if strings.TrimSpace(callbackID) == "" {
		return errors.New("answer callback: callback ID is required")
	}
	notification = strings.TrimSpace(notification)
	messageText = strings.TrimSpace(messageText)
	if notification == "" && messageText == "" {
		return errors.New("answer callback: notification or replacement message is required")
	}
	if utf8.RuneCountInString(messageText) > 4000 {
		return errors.New("answer callback: replacement message exceeds 4000 characters")
	}
	body := callbackAnswerRequest{Notification: notification}
	if messageText != "" {
		body.Message = &callbackAnswerMessage{Text: messageText, Attachments: []any{}}
	}
	var response operationResponse
	if err := c.doJSON(ctx, http.MethodPost, "/answers", url.Values{"callback_id": {callbackID}}, body, &response); err != nil {
		return err
	}
	return response.asError(http.StatusOK)
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

	// #nosec G704 -- validateUploadURL requires an absolute HTTPS URL without userinfo/fragment, every redirect is revalidated, and Authorization is removed.
	resp, err := uploadClient.Do(req)
	if err != nil {
		return UploadResult{}, fmt.Errorf("upload image to MAX storage: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

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

	token := imageUploadToken(responseBody, reservation.Token, uploadURL.Query().Get("token"))
	if token == "" {
		// A syntactically successful storage response without an attachment
		// token is still an upstream protocol failure. Keep it typed so the API
		// returns max_api_error instead of hiding the problem as internal_error.
		return UploadResult{}, &Error{
			StatusCode: resp.StatusCode,
			Code:       "invalid_upload_response",
			Message:    "MAX image upload response does not contain a token",
			RequestID:  firstHeader(resp.Header, "X-Request-Id", "X-Request-ID", "X-Max-Request-Id"),
		}
	}

	return UploadResult{Token: token}, nil
}

// imageUploadToken accepts both upload response shapes currently used by MAX:
// the documented top-level token and the photo-token map returned by the
// official MAX Go client/storage endpoint. Reservation and signed-URL tokens
// remain valid fallbacks for deployments where MAX returns the image token
// before the multipart upload.
func imageUploadToken(responseBody []byte, fallbacks ...string) string {
	var response struct {
		Token  string `json:"token"`
		Photos map[string]struct {
			Token string `json:"token"`
		} `json:"photos"`
	}
	if json.Unmarshal(responseBody, &response) == nil {
		if strings.TrimSpace(response.Token) != "" {
			return response.Token
		}
		keys := make([]string, 0, len(response.Photos))
		for key := range response.Photos {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if token := response.Photos[key].Token; strings.TrimSpace(token) != "" {
				return token
			}
		}
	}
	for _, token := range fallbacks {
		if strings.TrimSpace(token) != "" {
			return token
		}
	}
	return ""
}

// Publish creates a post in a MAX chat or channel.
func (c *Client) Publish(ctx context.Context, request PublishRequest) (Message, error) {
	if request.ChatID == "" {
		return Message{}, errors.New("publish MAX post: chat ID is required")
	}
	if !validFormat(request.Format) {
		return Message{}, fmt.Errorf("publish MAX post: unsupported format %q", request.Format)
	}

	attachments, err := messageAttachments(request.ImageTokens, request.LinkButtons)
	if err != nil {
		return Message{}, fmt.Errorf("publish MAX post: %w", err)
	}
	body := messageBody{
		Text:        normalizeMessageText(request.Text, request.Format),
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
	if !validMessageID(request.MessageID) {
		return errors.New("edit MAX post: message ID is invalid")
	}
	if !validFormat(request.Format) {
		return fmt.Errorf("edit MAX post: unsupported format %q", request.Format)
	}

	attachments, err := messageAttachments(request.ImageTokens, request.LinkButtons)
	if err != nil {
		return fmt.Errorf("edit MAX post: %w", err)
	}
	body := messageBody{
		Text:        normalizeMessageText(request.Text, request.Format),
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
	if !validMessageID(messageID) {
		return errors.New("delete MAX post: message ID is invalid")
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

	// MAX API calls carry the shared bot credential. Do not follow redirects:
	// even a same-host HTTPS redirect can later be misconfigured into a
	// cross-origin or scheme-downgrade credential leak. Signed media uploads use
	// their own redirect policy in UploadImage and never carry Authorization.
	apiClient := *c.httpClient
	apiClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	// #nosec G704 -- New validates the absolute API base URL and rejects userinfo/query/fragment; production config pins the official MAX origin and redirects are disabled here.
	resp, err := apiClient.Do(req)
	if err != nil {
		return fmt.Errorf("MAX API %s %s: %w", method, endpointPath, err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

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
	return readBoundedBody(reader, maxJSONResponseBytes, "JSON response")
}

func readBoundedBody(reader io.Reader, limit int64, label string) ([]byte, error) {
	limited := io.LimitReader(reader, limit+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return body, err
	}
	if int64(len(body)) > limit {
		return body[:limit], fmt.Errorf("%s exceeds %d bytes", label, limit)
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

func validMessageID(value string) bool {
	return messageIDPattern.MatchString(value)
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
	value = strings.TrimPrefix(value, "@")
	if value == "" || len(value) > 128 || !asciiLetter(value[0]) {
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
