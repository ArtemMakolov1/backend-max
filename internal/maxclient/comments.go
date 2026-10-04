package maxclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

const MaxCommentTextRunes = 4000

// CommentsQuery selects a bounded page of channel-post comments. MAX ignores
// time pagination when CommentIDs is provided. Nil timestamps are omitted;
// a pointer to zero sends the documented zero boundary explicitly.
type CommentsQuery struct {
	Before     *int64
	After      *int64
	Count      int
	CommentIDs []string
}

// CommentsPage preserves MAX's newest-first order. The official response has
// no total count or pagination marker; a short page does not prove completeness.
type CommentsPage struct {
	Messages []CommentMessage
}

// CommentRequest supports plain text, optional Markdown/HTML, and a reply to
// another comment. MAX comments cannot have attachments, forwards, formatted
// hyperlinks or user mentions. MAX remains the authority on markup validity.
type CommentRequest struct {
	Text    string
	Format  Format
	ReplyTo string
}

// CommentMessage is a normalized channel-post comment. Numeric identifiers
// remain strings. PostID is populated only from an actual recipient.post_id;
// ReplyTo is a parent comment ID and must never be used to infer the root post.
// TimestampMillis is creation time; the API does not expose an edit timestamp.
type CommentMessage struct {
	MessageID       string
	ChatID          string
	PostID          string
	Text            string
	TimestampMillis int64
	SenderUserID    string
	SenderName      string
	SenderUsername  string
	SenderIsBot     bool
	ReplyTo         string
	Raw             json.RawMessage `json:"-"`
}

// HasCommentPermission uses channel permission semantics. In particular,
// write grants publication/replies, not editing/deleting others' comments.
// Keep the legacy channel aliases without inheriting group-only privileges.
func (m Membership) HasCommentPermission(required Permission) bool {
	for _, permission := range m.Permissions {
		if permission == required {
			return true
		}
		if (required == PermissionWrite && permission == "post_edit_delete_message") ||
			(required == PermissionEdit && permission == "edit_message") ||
			(required == PermissionDelete && permission == "delete_message") {
			return true
		}
	}
	return false
}

func (c *Client) GetComments(ctx context.Context, postID string, input CommentsQuery) (CommentsPage, error) {
	path, err := commentsPath(postID)
	if err != nil {
		return CommentsPage{}, err
	}
	query, limit, err := commentsQuery(input)
	if err != nil {
		return CommentsPage{}, err
	}
	var response struct {
		Messages json.RawMessage `json:"messages"`
	}
	if err := c.doJSON(ctx, http.MethodGet, path, query, nil, &response); err != nil {
		return CommentsPage{}, err
	}
	if rawJSONNull(response.Messages) {
		return CommentsPage{}, errors.New("MAX comments response does not contain a messages array")
	}
	var items []json.RawMessage
	if err := json.Unmarshal(response.Messages, &items); err != nil {
		return CommentsPage{}, fmt.Errorf("decode MAX comments array: %w", err)
	}
	if len(items) > limit {
		return CommentsPage{}, errors.New("MAX comments response exceeds the requested bound")
	}
	page := CommentsPage{Messages: make([]CommentMessage, 0, len(items))}
	seen := make(map[string]struct{}, len(items))
	requestedIDs := make(map[string]struct{}, len(input.CommentIDs))
	for _, id := range input.CommentIDs {
		requestedIDs[id] = struct{}{}
	}
	for index, raw := range items {
		message, err := decodeRequestedComment(raw, postID, "")
		if err != nil {
			return CommentsPage{}, fmt.Errorf("decode MAX comment %d: %w", index, err)
		}
		if _, duplicate := seen[message.MessageID]; duplicate {
			return CommentsPage{}, errors.New("MAX comments response contains a duplicate comment ID")
		}
		if len(requestedIDs) > 0 {
			if _, requested := requestedIDs[message.MessageID]; !requested {
				return CommentsPage{}, errors.New("MAX comments response contains an unrequested comment ID")
			}
		}
		seen[message.MessageID] = struct{}{}
		page.Messages = append(page.Messages, message)
	}
	return page, nil
}

func (c *Client) GetComment(ctx context.Context, postID, commentID string) (CommentMessage, error) {
	path, err := commentsPath(postID)
	if err != nil {
		return CommentMessage{}, err
	}
	if !validMessageID(commentID) {
		return CommentMessage{}, errors.New("MAX comment ID is invalid")
	}
	var raw json.RawMessage
	if err := c.doJSON(ctx, http.MethodGet, path+"/"+url.PathEscape(commentID), nil, nil, &raw); err != nil {
		return CommentMessage{}, err
	}
	return decodeRequestedComment(raw, postID, commentID)
}

func (c *Client) SendComment(ctx context.Context, postID string, input CommentRequest) (CommentMessage, error) {
	path, err := commentsPath(postID)
	if err != nil {
		return CommentMessage{}, err
	}
	body, err := commentBody(input)
	if err != nil {
		return CommentMessage{}, err
	}
	var response publishResponse
	if err := c.doJSON(ctx, http.MethodPost, path, nil, body, &response); err != nil {
		return CommentMessage{}, err
	}
	if response.Success != nil && !*response.Success {
		return CommentMessage{}, (operationResponse{Code: response.Code, Message: rawJSONMessage(response.Message)}).asError(http.StatusOK)
	}
	// A malformed successful response is ambiguous. Never retry a comment
	// mutation here: MAX may have accepted it before the response was lost.
	return decodeRequestedComment(response.Message, postID, "")
}

func (c *Client) EditComment(ctx context.Context, postID, commentID string, input CommentRequest) error {
	body, err := commentBody(input)
	if err != nil {
		return err
	}
	return c.mutateComment(ctx, http.MethodPut, postID, commentID, body)
}

func (c *Client) DeleteComment(ctx context.Context, postID, commentID string) error {
	return c.mutateComment(ctx, http.MethodDelete, postID, commentID, nil)
}

func (c *Client) mutateComment(ctx context.Context, method, postID, commentID string, body any) error {
	path, err := commentsPath(postID)
	if err != nil {
		return err
	}
	if !validMessageID(commentID) {
		return errors.New("MAX comment ID is invalid")
	}
	var response struct {
		Success *bool           `json:"success"`
		Code    json.RawMessage `json:"code,omitempty"`
		Message string          `json:"message,omitempty"`
	}
	if err := c.doJSON(ctx, method, path, url.Values{"comment_id": {commentID}}, body, &response); err != nil {
		return err
	}
	if response.Success == nil {
		// An absent acknowledgement is not an explicit provider rejection.
		// The write may already have succeeded; callers must retain uncertainty.
		return errors.New("MAX comment mutation response does not contain a success acknowledgement")
	}
	return (operationResponse{Success: *response.Success, Code: response.Code, Message: response.Message}).asError(http.StatusOK)
}

func commentsPath(postID string) (string, error) {
	if !validMessageID(postID) {
		return "", errors.New("MAX commented post ID is invalid")
	}
	return "/messages/" + url.PathEscape(postID) + "/comments", nil
}

func commentsQuery(input CommentsQuery) (url.Values, int, error) {
	count := input.Count
	if count == 0 {
		count = 50
	}
	if count < 1 || count > 100 {
		return nil, 0, errors.New("MAX comments count must be between 1 and 100")
	}
	if (input.Before != nil && *input.Before < 0) || (input.After != nil && *input.After < 0) {
		return nil, 0, errors.New("MAX comments timestamp must not be negative")
	}
	query := make(url.Values)
	if len(input.CommentIDs) > 0 {
		// Bound the explicit-ID request independently of pagination, which
		// MAX ignores in this mode. This client accepts at most 100 IDs.
		if len(input.CommentIDs) > 100 {
			return nil, 0, errors.New("MAX comments request must contain at most 100 IDs")
		}
		seen := make(map[string]struct{}, len(input.CommentIDs))
		for _, id := range input.CommentIDs {
			if !validMessageID(id) {
				return nil, 0, errors.New("MAX requested comment ID is invalid")
			}
			if _, duplicate := seen[id]; duplicate {
				return nil, 0, errors.New("MAX requested comment IDs must be unique")
			}
			seen[id] = struct{}{}
		}
		query.Set("comment_ids", strings.Join(input.CommentIDs, ","))
		return query, len(input.CommentIDs), nil
	}
	query.Set("count", strconv.Itoa(count))
	if input.Before != nil {
		query.Set("before", strconv.FormatInt(*input.Before, 10))
	}
	if input.After != nil {
		query.Set("after", strconv.FormatInt(*input.After, 10))
	}
	return query, count, nil
}

type commentRequestBody struct {
	Text   string `json:"text"`
	Format Format `json:"format,omitempty"`
	Link   *struct {
		Type string `json:"type"`
		MID  string `json:"mid"`
	} `json:"link,omitempty"`
}

func commentBody(input CommentRequest) (commentRequestBody, error) {
	if !utf8.ValidString(input.Text) || strings.TrimSpace(input.Text) == "" || utf8.RuneCountInString(input.Text) > MaxCommentTextRunes {
		return commentRequestBody{}, errors.New("MAX comment text must contain 1 to 4000 Unicode characters")
	}
	if !validFormat(input.Format) {
		return commentRequestBody{}, errors.New("MAX comment format must be markdown or html")
	}
	result := commentRequestBody{Text: input.Text, Format: input.Format}
	if input.ReplyTo != "" {
		if !validMessageID(input.ReplyTo) {
			return commentRequestBody{}, errors.New("MAX reply comment ID is invalid")
		}
		result.Link = &struct {
			Type string `json:"type"`
			MID  string `json:"mid"`
		}{Type: "reply", MID: input.ReplyTo}
	}
	return result, nil
}

type commentMessageWire struct {
	Recipient *struct {
		ChatID   json.RawMessage `json:"chat_id"`
		ChatType string          `json:"chat_type"`
		PostID   string          `json:"post_id"`
	} `json:"recipient"`
	Sender *struct {
		UserID    json.RawMessage `json:"user_id"`
		FirstName string          `json:"first_name"`
		LastName  string          `json:"last_name"`
		Username  string          `json:"username"`
		IsBot     bool            `json:"is_bot"`
	} `json:"sender"`
	Timestamp int64 `json:"timestamp"`
	Body      *struct {
		MID         string          `json:"mid"`
		Text        string          `json:"text"`
		Attachments json.RawMessage `json:"attachments"`
	} `json:"body"`
	Link *struct {
		Type    string `json:"type"`
		Message *struct {
			MID string `json:"mid"`
		} `json:"message"`
	} `json:"link"`
}

// DecodeCommentMessage also normalizes the message in comment-created/edited
// updates. It preserves an omitted root PostID rather than deriving one from
// the reply parent or an unrelated channel post.
func DecodeCommentMessage(raw json.RawMessage) (CommentMessage, error) {
	if len(raw) > maxJSONResponseBytes {
		return CommentMessage{}, errors.New("MAX comment exceeds the response size bound")
	}
	var wire commentMessageWire
	if err := decodeJSON(raw, &wire); err != nil {
		return CommentMessage{}, fmt.Errorf("decode MAX comment message: %w", err)
	}
	if wire.Recipient == nil || wire.Recipient.ChatType != "channel" {
		return CommentMessage{}, errors.New("MAX comment recipient is not a channel")
	}
	chatID := jsonCode(wire.Recipient.ChatID)
	parsedChatID, chatIDErr := strconv.ParseInt(chatID, 10, 64)
	if !numericID(chatID) || chatIDErr != nil || parsedChatID == 0 {
		return CommentMessage{}, errors.New("MAX comment contains an invalid channel ID")
	}
	if wire.Recipient.PostID != "" && !validMessageID(wire.Recipient.PostID) {
		return CommentMessage{}, errors.New("MAX comment contains an invalid post ID")
	}
	if wire.Body == nil || !validMessageID(wire.Body.MID) {
		return CommentMessage{}, errors.New("MAX comment does not contain a valid comment ID")
	}
	if wire.Timestamp <= 0 {
		return CommentMessage{}, errors.New("MAX comment contains a non-positive timestamp")
	}
	if !rawJSONNull(wire.Body.Attachments) {
		var attachments []json.RawMessage
		if err := json.Unmarshal(wire.Body.Attachments, &attachments); err != nil || len(attachments) > 0 {
			return CommentMessage{}, errors.New("MAX comment unexpectedly contains attachments")
		}
	}
	message := CommentMessage{MessageID: wire.Body.MID, ChatID: chatID, PostID: wire.Recipient.PostID,
		Text: wire.Body.Text, TimestampMillis: wire.Timestamp, Raw: append(json.RawMessage(nil), raw...)}
	if wire.Sender != nil {
		id := jsonCode(wire.Sender.UserID)
		parsed, err := strconv.ParseInt(id, 10, 64)
		if err != nil || parsed <= 0 {
			return CommentMessage{}, errors.New("MAX comment contains an invalid sender user ID")
		}
		message.SenderUserID, message.SenderIsBot = id, wire.Sender.IsBot
		message.SenderName = strings.TrimSpace(wire.Sender.FirstName + " " + wire.Sender.LastName)
		message.SenderUsername = wire.Sender.Username
	}
	if wire.Link != nil {
		if wire.Link.Type != "reply" || wire.Link.Message == nil || !validMessageID(wire.Link.Message.MID) {
			return CommentMessage{}, errors.New("MAX comment contains an invalid reply link")
		}
		message.ReplyTo = wire.Link.Message.MID
	}
	return message, nil
}

func decodeRequestedComment(raw json.RawMessage, postID, commentID string) (CommentMessage, error) {
	message, err := DecodeCommentMessage(raw)
	if err != nil {
		return CommentMessage{}, err
	}
	if message.PostID != "" && message.PostID != postID {
		return CommentMessage{}, errors.New("MAX comment does not belong to the requested post")
	}
	if commentID != "" && message.MessageID != commentID {
		return CommentMessage{}, errors.New("MAX comment response does not match the requested comment ID")
	}
	return message, nil
}
