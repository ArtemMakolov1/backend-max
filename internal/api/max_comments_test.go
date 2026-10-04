package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/maxclient"
	"maxpilot/backend/internal/media"
	"maxpilot/backend/internal/store"
)

type commentsAPIMAX struct {
	*claimWebhookMAX
	items                        []maxclient.CommentMessage
	lists, sends, edits, deletes int
	getChatErr                   error
	afterSend                    func()
}

func (f *commentsAPIMAX) GetChat(ctx context.Context, id string) (maxclient.ChatInfo, error) {
	if f.getChatErr != nil {
		return maxclient.ChatInfo{}, f.getChatErr
	}
	return f.claimWebhookMAX.GetChat(ctx, id)
}

func (f *commentsAPIMAX) GetComments(context.Context, string, maxclient.CommentsQuery) (maxclient.CommentsPage, error) {
	f.lists++
	return maxclient.CommentsPage{Messages: f.items}, nil
}
func (f *commentsAPIMAX) GetComment(_ context.Context, _, mid string) (maxclient.CommentMessage, error) {
	for _, c := range f.items {
		if c.MessageID == mid {
			return c, nil
		}
	}
	return maxclient.CommentMessage{}, &maxclient.Error{StatusCode: 404}
}
func (f *commentsAPIMAX) SendComment(_ context.Context, _ string, r maxclient.CommentRequest) (maxclient.CommentMessage, error) {
	f.sends++
	if f.afterSend != nil {
		defer f.afterSend()
	}
	c := maxclient.CommentMessage{MessageID: "mid.api-reply", ChatID: f.chat.ChatID, PostID: "mid.api-root", Text: r.Text, ReplyTo: r.ReplyTo, SenderUserID: "42", SenderIsBot: true, TimestampMillis: time.Now().UTC().UnixMilli(), Raw: json.RawMessage(`{"body":{"markup":[]}}`)}
	f.items = append(f.items, c)
	return c, nil
}
func (f *commentsAPIMAX) EditComment(context.Context, string, string, maxclient.CommentRequest) error {
	f.edits++
	return nil
}
func (f *commentsAPIMAX) DeleteComment(context.Context, string, string) error {
	f.deletes++
	return nil
}

func maxCommentsAPIFixture(t *testing.T) (workspaceAPIFixture, *commentsAPIMAX, string) {
	t.Helper()
	fixture := newWorkspaceAPIFixture(t)
	ctx := t.Context()
	if _, err := fixture.storage.ClaimForPublishing(ctx, fixture.post.ID); err != nil {
		t.Fatal(err)
	}
	p, err := fixture.storage.MarkPublished(ctx, fixture.post.ID, "mid.api-root", "https://max.ru/post/api-root")
	if err != nil {
		t.Fatal(err)
	}
	fixture.post = p
	f := &commentsAPIMAX{claimWebhookMAX: &claimWebhookMAX{chat: maxclient.ChatInfo{ChatID: fixture.channel.MAXChatID, Type: "channel", Status: "active", OwnerID: fixture.channel.VerifiedMAXOwnerID}, membership: maxclient.Membership{UserID: 42, IsBot: true, IsAdmin: true, Permissions: []maxclient.Permission{maxclient.PermissionReadAllMessages, maxclient.PermissionWrite, maxclient.PermissionDelete}}}}
	f.items = []maxclient.CommentMessage{{MessageID: "mid.api-reader", ChatID: f.chat.ChatID, PostID: p.MAXMessageID, Text: "Question", SenderUserID: "71", SenderName: "Reader", TimestampMillis: time.Now().Add(-time.Hour).UnixMilli(), Raw: json.RawMessage(`{"body":{"markup":[]}}`)}}
	m, err := media.New(t.TempDir(), "http://localhost:8080")
	if err != nil {
		t.Fatal(err)
	}
	fixture.app = app.New(fixture.storage, m, f, nil, nil, fixture.logger)
	return fixture, f, "/api/v1/workspaces/" + fixture.workspace.ID + "/posts/" + postID(p.ID) + "/max-comments"
}

func TestMAXCommentAPIAuthScopeRootFenceAndNoEditorialMixing(t *testing.T) {
	t.Parallel()
	fixture, f, path := maxCommentsAPIFixture(t)
	owner := fixture.handler(t, "ws-owner")
	viewer := fixture.handler(t, "ws-viewer")
	outsider := fixture.handler(t, "ws-outsider")
	unauth := New(fixture.app, fixture.logger, "http://localhost:4321", "webhook-secret", AuthOptions{YandexClient: &fakeYandexOAuth{}}).Handler()
	response := performJSONRequest(unauth, http.MethodGet, path, "")
	assertProblemCode(t, response, 401, "authentication_required")
	response = performJSONRequest(outsider, http.MethodGet, path, "")
	assertProblemCode(t, response, 404, "not_found")
	response = performJSONRequest(viewer, http.MethodPost, path, `{"expected_root_message_id":"mid.api-root","client_request_id":"api_send_request_01","text":"Reply"}`)
	assertProblemCode(t, response, 403, "workspace_forbidden")
	response = performJSONRequest(owner, http.MethodPost, path, `{"expected_root_message_id":"mid.stale","client_request_id":"api_send_request_01","text":"Reply"}`)
	assertProblemCode(t, response, 409, "max_comment_not_written")
	if f.sends != 0 || len(f.getChatIDs) != 0 {
		t.Fatal("denied/stale mutation reached MAX")
	}
	response = performJSONRequest(owner, http.MethodPost, path+"/sync", "")
	assertProblemCode(t, response, 400, "validation_error")
	response = performJSONRequest(owner, http.MethodPost, path+"/sync?expected_root_message_id=mid.api-root", "")
	if response.Code != 200 {
		t.Fatalf("sync: %d %s", response.Code, response.Body.String())
	}
	var result app.MAXCommentsView
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.RootMessageID != "mid.api-root" || result.Metrics.Total != nil || result.Coverage.Complete || len(result.Comments) != 1 {
		t.Fatalf("invalid MAX DTO: %+v %v", result, err)
	}
	response = performJSONRequest(owner, http.MethodGet, strings.TrimSuffix(path, "max-comments")+"comments", "")
	if response.Code != 200 || strings.Contains(response.Body.String(), "Question") {
		t.Fatalf("MAX comments leaked into editorial discussion: %d %s", response.Code, response.Body.String())
	}
	response = performJSONRequest(owner, http.MethodPost, path, `{"expected_root_message_id":"mid.api-root","client_request_id":"api_send_request_01","text":"Reply","reply_to_message_id":"mid.api-reader"}`)
	if response.Code != 200 || f.sends != 1 {
		t.Fatalf("send: %d %s", response.Code, response.Body.String())
	}
	response = performJSONRequest(owner, http.MethodPost, path, `{"expected_root_message_id":"mid.api-root","client_request_id":"api_send_request_01","text":"Reply","reply_to_message_id":"mid.api-reader"}`)
	if response.Code != 200 || f.sends != 1 {
		t.Fatalf("same key replayed: %d %s", response.Code, response.Body.String())
	}
}

func TestMAXCommentWebhookExactRootKnownFallbackAndRemovalAreIdempotent(t *testing.T) {
	t.Parallel()
	fixture, _, path := maxCommentsAPIFixture(t)
	handler := New(fixture.app, fixture.logger, "http://localhost:4321", "webhook-secret", AuthOptions{YandexClient: &fakeYandexOAuth{}}).Handler()
	now := time.Now().UTC().Truncate(time.Millisecond)
	message := func(root, text string) string {
		return fmt.Sprintf(`{"sender":{"user_id":71,"first_name":"Reader","is_bot":false},"recipient":{"chat_id":%s,"chat_type":"channel"%s},"timestamp":%d,"body":{"mid":"mid.webhook","text":%q}}`, fixture.channel.MAXChatID, root, now.Add(-time.Hour).UnixMilli(), text)
	}
	send := func(secret, event, body string, at time.Time) *httptest.ResponseRecorder {
		payload := fmt.Sprintf(`{"update_type":%q,"timestamp":%d,%s}`, event, at.UnixMilli(), body)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/max", strings.NewReader(payload))
		req.Header.Set("X-Max-Bot-Api-Secret", secret)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	response := send("wrong", "comment_created", `"message":`+message(`,"post_id":"mid.api-root"`, "Rejected secret"), now)
	if response.Code != 401 {
		t.Fatalf("webhook secret bypass: %d", response.Code)
	}
	response = send("webhook-secret", "comment_created", `"message":`+message("", "Unmapped reply"), now)
	if response.Code != 200 {
		t.Fatalf("unknown mapping ack: %d %s", response.Code, response.Body.String())
	}
	owner := fixture.handler(t, "ws-owner")
	response = performJSONRequest(owner, http.MethodGet, path, "")
	var view app.MAXCommentsView
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil || len(view.Comments) != 0 {
		t.Fatalf("unknown root inferred: %+v %v", view, err)
	}
	response = send("webhook-secret", "comment_created", `"message":`+message(`,"post_id":"mid.api-root"`, "Created"), now)
	if response.Code != 200 {
		t.Fatalf("mapped create: %d %s", response.Code, response.Body.String())
	}
	response = send("webhook-secret", "comment_edited", `"message":`+message("", "Edited via known MID"), now.Add(time.Second))
	if response.Code != 200 {
		t.Fatalf("known MID edit: %d %s", response.Code, response.Body.String())
	}
	removal := fmt.Sprintf(`"chat_id":%s,"post_id":"mid.api-root","message_id":"mid.webhook"`, fixture.channel.MAXChatID)
	for i := 0; i < 2; i++ {
		response = send("webhook-secret", "comment_removed", removal, now.Add(2*time.Second))
		if response.Code != 200 {
			t.Fatalf("remove: %d %s", response.Code, response.Body.String())
		}
	}
	response = send("webhook-secret", "comment_created", `"message":`+message(`,"post_id":"mid.api-root"`, "Late create"), now.Add(3*time.Second))
	if response.Code != 200 {
		t.Fatalf("late create: %d %s", response.Code, response.Body.String())
	}
	response = performJSONRequest(owner, http.MethodGet, path, "")
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil || len(view.Comments) != 1 || view.Comments[0].DeletedAt == nil || view.Comments[0].Version != 3 || view.Comments[0].Text == "Late create" {
		t.Fatalf("event/tombstone state: %+v %v response%d", view, err, response.Code)
	}
}

func TestHealthMAXCommentsAvailabilityReflectsOptionalClient(t *testing.T) {
	t.Parallel()
	fixture, _, _ := maxCommentsAPIFixture(t)
	response := performJSONRequest(fixture.handler(t, "ws-owner"), http.MethodGet, "/api/v1/health", "")
	var payload struct {
		MAXCommentsConfigured bool `json:"max_comments_configured"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil || response.Code != 200 || !payload.MAXCommentsConfigured {
		t.Fatalf("availability: %d %s %v", response.Code, response.Body.String(), err)
	}
}

func TestMAXCommentAPIGuaranteedPrewriteFailureIsDistinguishableFromAcceptedWrite(t *testing.T) {
	t.Parallel()
	fixture, f, path := maxCommentsAPIFixture(t)
	owner := fixture.handler(t, "ws-owner")
	body := `{"expected_root_message_id":"mid.api-root","client_request_id":"api_send_request_01","text":"Reply"}`
	f.getChatErr = &maxclient.Error{StatusCode: 502, Code: "temporary"}
	response := performJSONRequest(owner, http.MethodPost, path, body)
	assertProblemCode(t, response, 502, "max_comment_not_written")
	if f.sends != 0 {
		t.Fatal("pre-write failure sent reply")
	}
	f.getChatErr = nil
	f.afterSend = func() {
		if err := fixture.storage.RemoveWorkspaceMember(t.Context(), "ws-owner", fixture.workspace.ID, "ws-editor"); err != nil {
			t.Error(err)
		}
	}
	editor := fixture.handler(t, "ws-editor")
	response = performJSONRequest(editor, http.MethodPost, path, body)
	assertProblemCode(t, response, 409, "max_comment_uncertain")
	if f.sends != 1 {
		t.Fatal("accepted reply not sent exactly once")
	}
	f.afterSend = nil
	if _, err := fixture.storage.AddWorkspaceMember(t.Context(), "ws-owner", store.WorkspaceMember{WorkspaceID: fixture.workspace.ID, UserID: "ws-editor", Role: store.WorkspaceRoleEditor}); err != nil {
		t.Fatal(err)
	}
	response = performJSONRequest(editor, http.MethodPost, path, body)
	if response.Code != 200 || f.sends != 1 {
		t.Fatalf("restored actor repeated known send: %d %s sends%d", response.Code, response.Body.String(), f.sends)
	}
}
