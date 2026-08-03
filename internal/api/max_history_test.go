package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sync"
	"testing"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/maxclient"
	"maxpilot/backend/internal/store"
)

type maxHistoryAPIScript struct {
	before   int64
	messages []maxclient.HistoryMessage
	err      error
}

type maxHistoryAPIRequest struct {
	chatID string
	before int64
	count  int
}

type maxHistoryAPIFake struct {
	*claimWebhookMAX

	mu      sync.Mutex
	scripts []maxHistoryAPIScript
	calls   []maxHistoryAPIRequest
}

func (f *maxHistoryAPIFake) GetMessagesPage(
	_ context.Context,
	chatID string,
	before int64,
	count int,
) ([]maxclient.HistoryMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, maxHistoryAPIRequest{chatID: chatID, before: before, count: count})
	if len(f.scripts) == 0 {
		return nil, errors.New("unexpected MAX history request")
	}
	current := f.scripts[0]
	f.scripts = f.scripts[1:]
	if before != current.before {
		return nil, fmt.Errorf("MAX history before = %d, want %d", before, current.before)
	}
	if current.err != nil {
		return nil, current.err
	}
	return append([]maxclient.HistoryMessage(nil), current.messages...), nil
}

func (f *maxHistoryAPIFake) historyCalls() []maxHistoryAPIRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]maxHistoryAPIRequest(nil), f.calls...)
}

func newMAXHistoryAPIFake(
	channel store.Channel,
	expectedCount int,
	scripts ...maxHistoryAPIScript,
) *maxHistoryAPIFake {
	return &maxHistoryAPIFake{
		claimWebhookMAX: &claimWebhookMAX{
			chat: maxclient.ChatInfo{
				ChatID: channel.MAXChatID, OwnerID: "max-owner", Type: "channel", Status: "active",
				Title: channel.Title, MessagesCount: expectedCount,
			},
			membership: maxclient.Membership{
				UserID: 42, IsBot: true, IsAdmin: true,
				Permissions: []maxclient.Permission{maxclient.PermissionReadAllMessages},
			},
		},
		scripts: append([]maxHistoryAPIScript(nil), scripts...),
	}
}

func maxHistoryAPIHandler(t *testing.T, fixture workspaceAPIFixture, fake *maxHistoryAPIFake) http.Handler {
	t.Helper()
	application := app.New(fixture.storage, fixture.app.Media(), fake, nil, nil, fixture.logger)
	return New(application, fixture.logger, "http://localhost:4321", "webhook-secret",
		AuthOptions{YandexClient: &fakeYandexOAuth{}}).Handler()
}

func maxHistoryImportPath(fixture workspaceAPIFixture) string {
	return "/api/v1/workspaces/" + fixture.workspace.ID + "/channels/" +
		postID(fixture.channel.ID) + "/history/import"
}

func maxHistoryTextMessage(id, text, senderID string, senderIsBot bool, timestamp int64) maxclient.HistoryMessage {
	return maxclient.HistoryMessage{
		MessageID: id, ChatID: "-990001", Text: text, TimestampMillis: timestamp,
		SenderUserID: senderID, SenderIsBot: senderIsBot,
		Raw: json.RawMessage(`{"body":{"markup":[]}}`),
	}
}

func TestWorkspaceMAXHistoryImportOwnerAndEditorReturnExactProgress(t *testing.T) {
	for _, userID := range []string{"ws-owner", "ws-editor"} {
		t.Run(userID, func(t *testing.T) {
			fixture := newWorkspaceAPIFixture(t)
			message := maxHistoryTextMessage("mid.contract", "Импортированный текст", "42", true, 1_785_700_000_123)
			fake := newMAXHistoryAPIFake(fixture.channel, 1, maxHistoryAPIScript{messages: []maxclient.HistoryMessage{message}})
			handler := withTestSession(t, fixture.storage, maxHistoryAPIHandler(t, fixture, fake), userID)

			response := performJSONRequest(handler, http.MethodPost, maxHistoryImportPath(fixture), "")
			if response.Code != http.StatusOK {
				t.Fatalf("import status = %d; body = %s", response.Code, response.Body.String())
			}
			wantBody := fmt.Sprintf("{\"channel_id\":%d,\"status\":\"complete\",\"processed_count\":1,"+
				"\"imported_count\":1,\"existing_count\":0,\"skipped_count\":0,\"expected_count\":1,"+
				"\"has_more\":false,\"partial\":false}\n", fixture.channel.ID)
			if response.Body.String() != wantBody {
				t.Fatalf("import body = %s, want %s", response.Body.String(), wantBody)
			}
			if response.Header().Get("Cache-Control") != "no-store" ||
				response.Header().Get("Content-Type") != "application/json; charset=utf-8" {
				t.Fatalf("import headers = %v", response.Header())
			}

			posts, err := fixture.storage.ListPostsForWorkspace(
				t.Context(), userID, fixture.workspace.ID, store.PostStatusPublished, &fixture.channel.ID,
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(posts) != 1 || posts[0].Origin != store.PostOriginMAXHistory ||
				!posts[0].MAXHistoryAttachmentsComplete || posts[0].MAXSenderIsBot == nil ||
				!*posts[0].MAXSenderIsBot || posts[0].Content != message.Text {
				t.Fatalf("persisted imported post = %#v", posts)
			}
		})
	}
}

func TestWorkspaceMAXHistoryImportAuthorizationAndTenantBoundary(t *testing.T) {
	fixture := newWorkspaceAPIFixture(t)
	fake := newMAXHistoryAPIFake(fixture.channel, 0)
	rawHandler := maxHistoryAPIHandler(t, fixture, fake)
	path := maxHistoryImportPath(fixture)

	for _, userID := range []string{"ws-viewer", "ws-approver"} {
		response := performJSONRequest(
			withTestSession(t, fixture.storage, rawHandler, userID), http.MethodPost, path, "",
		)
		assertProblemCode(t, response, http.StatusForbidden, "workspace_forbidden")
	}
	response := performJSONRequest(
		withTestSession(t, fixture.storage, rawHandler, "ws-outsider"), http.MethodPost, path, "",
	)
	assertProblemCode(t, response, http.StatusNotFound, "not_found")

	foreignWorkspace, err := fixture.storage.CreateWorkspace(
		t.Context(), "ws-outsider", store.Workspace{Name: "Foreign workspace"},
	)
	if err != nil {
		t.Fatal(err)
	}
	activatePaidWorkspaceForAPITest(t, fixture.storage, "ws-outsider", foreignWorkspace.ID, "pro")
	foreignChannel, err := fixture.storage.CreateChannelForWorkspace(
		t.Context(), "ws-outsider", foreignWorkspace.ID, store.Channel{
			VerifiedMAXOwnerID: "foreign-owner", MAXChatID: "-990002", Title: "Foreign channel",
			IsChannel: true, Active: true,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	owner := withTestSession(t, fixture.storage, rawHandler, "ws-owner")
	foreignPath := "/api/v1/workspaces/" + fixture.workspace.ID + "/channels/" +
		postID(foreignChannel.ID) + "/history/import"
	response = performJSONRequest(owner, http.MethodPost, foreignPath, "")
	assertProblemCode(t, response, http.StatusNotFound, "not_found")
	response = performJSONRequest(owner, http.MethodPost,
		"/api/v1/workspaces/"+fixture.workspace.ID+"/channels/not-an-id/history/import", "")
	assertProblemCode(t, response, http.StatusBadRequest, "validation_error")

	if calls := fake.historyCalls(); len(calls) != 0 || len(fake.getChatIDs) != 0 {
		t.Fatalf("forbidden or foreign requests reached MAX: history=%#v chats=%#v", calls, fake.getChatIDs)
	}
}

func TestWorkspaceMAXHistoryImportPersistsEditSafetyFlags(t *testing.T) {
	fixture := newWorkspaceAPIFixture(t)
	timestamp := int64(1_785_700_000_123)
	supported := maxHistoryTextMessage("mid.supported", "Редактируемый текст", "42", true, timestamp)
	senderMismatch := maxHistoryTextMessage("mid.sender-mismatch", "Чужой текст", "77", true, timestamp-1)
	formatted := maxHistoryTextMessage("mid.formatted", "Форматированный текст", "42", true, timestamp-2)
	formatted.Raw = json.RawMessage(`{"body":{"markup":[{"type":"strong","from":0,"length":23}]}}`)
	unsupported := maxHistoryTextMessage("mid.unsupported", "Публикация с аудио", "42", true, timestamp-3)
	unsupported.Attachments = []maxclient.HistoryAttachment{{
		Type: "audio", Complete: false, Raw: json.RawMessage(`{"type":"audio","payload":{"token":"secret"}}`),
	}}
	messages := []maxclient.HistoryMessage{supported, senderMismatch, formatted, unsupported}
	fake := newMAXHistoryAPIFake(fixture.channel, len(messages), maxHistoryAPIScript{messages: messages})
	handler := withTestSession(t, fixture.storage, maxHistoryAPIHandler(t, fixture, fake), "ws-owner")

	response := performJSONRequest(handler, http.MethodPost, maxHistoryImportPath(fixture), "")
	if response.Code != http.StatusOK {
		t.Fatalf("import status = %d; body = %s", response.Code, response.Body.String())
	}
	posts, err := fixture.storage.ListPostsForWorkspace(
		t.Context(), "ws-owner", fixture.workspace.ID, store.PostStatusPublished, &fixture.channel.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) != len(messages) {
		t.Fatalf("imported posts = %d, want %d: %#v", len(posts), len(messages), posts)
	}
	byMessageID := make(map[string]store.Post, len(posts))
	for _, post := range posts {
		if post.Origin != store.PostOriginMAXHistory {
			t.Fatalf("post %q origin = %q", post.MAXMessageID, post.Origin)
		}
		byMessageID[post.MAXMessageID] = post
	}
	assertFlags := func(messageID string, attachmentsComplete bool, senderIsBot bool) {
		t.Helper()
		post, ok := byMessageID[messageID]
		if !ok || post.MAXHistoryAttachmentsComplete != attachmentsComplete ||
			post.MAXSenderIsBot == nil || *post.MAXSenderIsBot != senderIsBot {
			t.Fatalf("post %q safety flags = %#v", messageID, post)
		}
	}
	assertFlags(supported.MessageID, true, true)
	assertFlags(senderMismatch.MessageID, true, false)
	assertFlags(formatted.MessageID, false, true)
	assertFlags(unsupported.MessageID, false, true)
	if len(byMessageID[unsupported.MessageID].Attachments) != 0 {
		t.Fatalf("unsupported attachment subset was persisted: %#v", byMessageID[unsupported.MessageID].Attachments)
	}
}

func TestWorkspaceMAXHistoryImportPageCompletionAndInclusiveCursor(t *testing.T) {
	t.Run("ninety nine messages complete", func(t *testing.T) {
		fixture := newWorkspaceAPIFixture(t)
		messages := maxHistoryTextPage(99, 1_785_700_100_000)
		fake := newMAXHistoryAPIFake(fixture.channel, len(messages), maxHistoryAPIScript{messages: messages})
		handler := withTestSession(t, fixture.storage, maxHistoryAPIHandler(t, fixture, fake), "ws-owner")

		response := performJSONRequest(handler, http.MethodPost, maxHistoryImportPath(fixture), "")
		if response.Code != http.StatusOK {
			t.Fatalf("import status = %d; body = %s", response.Code, response.Body.String())
		}
		var progress app.ChannelHistoryImportResult
		if err := json.Unmarshal(response.Body.Bytes(), &progress); err != nil {
			t.Fatal(err)
		}
		if progress.Status != store.MAXHistoryImportStatusComplete || progress.HasMore || progress.Partial ||
			progress.ProcessedCount != 99 || progress.ImportedCount != 99 {
			t.Fatalf("99-message progress = %#v", progress)
		}
		if calls := fake.historyCalls(); !reflect.DeepEqual(calls, []maxHistoryAPIRequest{{
			chatID: fixture.channel.MAXChatID, before: 0, count: 100,
		}}) {
			t.Fatalf("99-message calls = %#v", calls)
		}
	})

	t.Run("full page repeats inclusive boundary without duplicate post", func(t *testing.T) {
		fixture := newWorkspaceAPIFixture(t)
		firstPage := maxHistoryTextPage(100, 1_785_700_100_000)
		oldest := firstPage[len(firstPage)-1]
		older := maxHistoryTextMessage("mid.page.100", "Публикация 100", "42", true, oldest.TimestampMillis-1_000)
		fake := newMAXHistoryAPIFake(fixture.channel, 101,
			maxHistoryAPIScript{messages: firstPage},
			maxHistoryAPIScript{before: oldest.TimestampMillis, messages: []maxclient.HistoryMessage{oldest, older}},
		)
		handler := withTestSession(t, fixture.storage, maxHistoryAPIHandler(t, fixture, fake), "ws-owner")
		path := maxHistoryImportPath(fixture)

		first := performJSONRequest(handler, http.MethodPost, path, "")
		if first.Code != http.StatusOK {
			t.Fatalf("first import status = %d; body = %s", first.Code, first.Body.String())
		}
		var firstProgress app.ChannelHistoryImportResult
		if err := json.Unmarshal(first.Body.Bytes(), &firstProgress); err != nil {
			t.Fatal(err)
		}
		if firstProgress.Status != store.MAXHistoryImportStatusInProgress || !firstProgress.HasMore ||
			firstProgress.Partial || firstProgress.ProcessedCount != 100 || firstProgress.ImportedCount != 100 {
			t.Fatalf("first progress = %#v", firstProgress)
		}

		second := performJSONRequest(handler, http.MethodPost, path, "")
		if second.Code != http.StatusOK {
			t.Fatalf("second import status = %d; body = %s", second.Code, second.Body.String())
		}
		var secondProgress app.ChannelHistoryImportResult
		if err := json.Unmarshal(second.Body.Bytes(), &secondProgress); err != nil {
			t.Fatal(err)
		}
		if secondProgress.Status != store.MAXHistoryImportStatusComplete || secondProgress.HasMore ||
			secondProgress.Partial || secondProgress.ProcessedCount != 101 || secondProgress.ImportedCount != 101 ||
			secondProgress.ExistingCount != 0 || secondProgress.SkippedCount != 0 {
			t.Fatalf("second progress = %#v", secondProgress)
		}
		wantCalls := []maxHistoryAPIRequest{
			{chatID: fixture.channel.MAXChatID, before: 0, count: 100},
			{chatID: fixture.channel.MAXChatID, before: oldest.TimestampMillis, count: 100},
		}
		if calls := fake.historyCalls(); !reflect.DeepEqual(calls, wantCalls) {
			t.Fatalf("history calls = %#v, want %#v", calls, wantCalls)
		}
		posts, err := fixture.storage.ListPostsForWorkspace(
			t.Context(), "ws-owner", fixture.workspace.ID, store.PostStatusPublished, &fixture.channel.ID,
		)
		if err != nil {
			t.Fatal(err)
		}
		seen := make(map[string]bool, len(posts))
		for _, post := range posts {
			if seen[post.MAXMessageID] {
				t.Fatalf("duplicate imported post for %q", post.MAXMessageID)
			}
			seen[post.MAXMessageID] = true
		}
		if len(posts) != 101 || len(seen) != 101 || !seen[oldest.MessageID] || !seen[older.MessageID] {
			t.Fatalf("persisted inclusive pages = posts:%d unique:%d", len(posts), len(seen))
		}
	})
}

func TestWorkspaceMAXHistoryImportProviderFailureReleasesForRetry(t *testing.T) {
	fixture := newWorkspaceAPIFixture(t)
	message := maxHistoryTextMessage("mid.after-retry", "После повтора", "42", true, 1_785_700_000_123)
	fake := newMAXHistoryAPIFake(fixture.channel, 1,
		maxHistoryAPIScript{err: &maxclient.Error{StatusCode: http.StatusServiceUnavailable, Message: "temporary"}},
		maxHistoryAPIScript{messages: []maxclient.HistoryMessage{message}},
	)
	handler := withTestSession(t, fixture.storage, maxHistoryAPIHandler(t, fixture, fake), "ws-owner")
	path := maxHistoryImportPath(fixture)

	failed := performJSONRequest(handler, http.MethodPost, path, "")
	assertProblemCode(t, failed, http.StatusBadGateway, "max_api_error")
	retried := performJSONRequest(handler, http.MethodPost, path, "")
	if retried.Code != http.StatusOK {
		t.Fatalf("retry status = %d; body = %s", retried.Code, retried.Body.String())
	}
	var progress app.ChannelHistoryImportResult
	if err := json.Unmarshal(retried.Body.Bytes(), &progress); err != nil {
		t.Fatal(err)
	}
	if progress.Status != store.MAXHistoryImportStatusComplete || progress.ImportedCount != 1 || progress.HasMore {
		t.Fatalf("retry progress = %#v", progress)
	}
	wantCalls := []maxHistoryAPIRequest{
		{chatID: fixture.channel.MAXChatID, before: 0, count: 100},
		{chatID: fixture.channel.MAXChatID, before: 0, count: 100},
	}
	if calls := fake.historyCalls(); !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("retry calls = %#v, want %#v", calls, wantCalls)
	}
}

func maxHistoryTextPage(count int, newestTimestamp int64) []maxclient.HistoryMessage {
	messages := make([]maxclient.HistoryMessage, count)
	for index := range messages {
		messages[index] = maxHistoryTextMessage(
			fmt.Sprintf("mid.page.%03d", index), fmt.Sprintf("Публикация %d", index), "42", true,
			newestTimestamp-int64(index)*1_000,
		)
	}
	return messages
}
