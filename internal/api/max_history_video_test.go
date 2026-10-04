package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/maxclient"
	"maxpilot/backend/internal/store"
)

func TestMAXHistoryVideoPrefixResumesPreviouslyCompletedMissingPreviews(t *testing.T) {
	fixture := newWorkspaceAPIFixture(t)
	messages := make([]maxclient.HistoryMessage, 30)
	for index := range messages {
		messages[index] = maxHistoryTextMessage(fmt.Sprintf("mid.missing.%d", index), fmt.Sprintf("Сохранённый текст %d", index), "42", true, 1_785_700_000_123-int64(index))
		messages[index].Attachments = []maxclient.HistoryAttachment{{Type: "video", Token: fmt.Sprintf("video_%d", index), Complete: true}}
	}
	// This simulates already imported publications whose token-only videos did
	// not have previews before GET /videos was supported.
	oldFake := newMAXHistoryAPIFake(fixture.channel, 30, maxHistoryAPIScript{messages: messages})
	oldHandler := withTestSession(t, fixture.storage, maxHistoryAPIHandler(t, fixture, oldFake), "ws-owner")
	response := performJSONRequest(oldHandler, http.MethodPost, maxHistoryImportPath(fixture), "")
	if response.Code != http.StatusOK {
		t.Fatalf("initial import = %d: %s", response.Code, response.Body.String())
	}
	before, err := fixture.storage.ListPostsForWorkspace(t.Context(), "ws-owner", fixture.workspace.ID, store.PostStatusPublished, &fixture.channel.ID)
	if err != nil || len(before) != 30 {
		t.Fatalf("old import count=%d error=%v", len(before), err)
	}
	oldByMID := make(map[string]store.Post, 30)
	for _, post := range before {
		if len(post.Attachments) != 0 || post.MAXHistoryAttachmentsComplete {
			t.Fatal("token-only baseline unexpectedly had an editable preview")
		}
		oldByMID[post.MAXMessageID] = post
	}
	for index := range messages {
		messages[index].Text = "Provider changed text: must not replace saved text"
	}
	fake := &maxVideoAPIFake{
		maxHistoryAPIFake: newMAXHistoryAPIFake(fixture.channel, 30,
			maxHistoryAPIScript{messages: messages},
			maxHistoryAPIScript{before: messages[19].TimestampMillis, messages: messages[19:]}),
		readVideo: func(token string) (maxclient.VideoInfo, error) {
			return maxclient.VideoInfo{Token: token, URLs: &maxclient.VideoURLs{MP4480: "https://media.max.ru/" + token + ".mp4"}, Width: 640, Height: 480, DurationMS: 1000}, nil
		},
	}
	application := app.New(fixture.storage, fixture.app.Media(), fake, nil, nil, fixture.logger)
	handler := withTestSession(t, fixture.storage, New(application, fixture.logger, "http://localhost:4321", "webhook-secret", AuthOptions{YandexClient: &fakeYandexOAuth{}}).Handler(), "ws-owner")
	response = performJSONRequest(handler, http.MethodPost, maxHistoryImportPath(fixture), "")
	var progress app.ChannelHistoryImportResult
	if err := json.Unmarshal(response.Body.Bytes(), &progress); err != nil {
		t.Fatalf("first prefix response=%d body=%s error=%v", response.Code, response.Body.String(), err)
	}
	if response.Code != http.StatusOK || !progress.HasMore || progress.Status != store.MAXHistoryImportStatusInProgress || progress.ProcessedCount != 20 || progress.ExistingCount != 20 || progress.ImportedCount != 0 || fake.videoCalls != 20 {
		t.Fatalf("bounded prefix = %#v calls=%d", progress, fake.videoCalls)
	}
	first, err := fixture.storage.ListPostsForWorkspace(t.Context(), "ws-owner", fixture.workspace.ID, store.PostStatusPublished, &fixture.channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	firstIDs := make(map[string]int64)
	for _, post := range first {
		if len(post.Attachments) == 1 {
			firstIDs[post.MAXMessageID] = post.Attachments[0].ID
		}
	}
	if len(firstIDs) != 20 {
		t.Fatalf("prefix preview count=%d", len(firstIDs))
	}
	response = performJSONRequest(handler, http.MethodPost, maxHistoryImportPath(fixture), "")
	if err := json.Unmarshal(response.Body.Bytes(), &progress); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || progress.HasMore || progress.Partial || progress.ProcessedCount != 30 || progress.ExistingCount != 30 || progress.ImportedCount != 0 || fake.videoCalls != 31 {
		t.Fatalf("inclusive resumed progress=%#v calls=%d status=%d", progress, fake.videoCalls, response.Code)
	}
	after, err := fixture.storage.ListPostsForWorkspace(t.Context(), "ws-owner", fixture.workspace.ID, store.PostStatusPublished, &fixture.channel.ID)
	if err != nil || len(after) != 30 {
		t.Fatalf("resumed post count=%d error=%v", len(after), err)
	}
	for _, post := range after {
		old := oldByMID[post.MAXMessageID]
		if post.ID != old.ID || post.Content != old.Content || post.Title != old.Title || post.Status != old.Status || post.UpdatedAt != old.UpdatedAt || post.MAXHistoryAttachmentsComplete || len(post.Attachments) != 1 {
			t.Fatalf("resync replaced publication or lost read-only guard: post %d", post.ID)
		}
		if oldID := firstIDs[post.MAXMessageID]; oldID != 0 && oldID != post.Attachments[0].ID {
			t.Fatal("inclusive overlap replaced an existing preview ID")
		}
	}
	calls := fake.historyCalls()
	if len(calls) != 2 || calls[0].count != 100 || calls[1].count != 100 || calls[1].before != messages[19].TimestampMillis {
		t.Fatalf("provider cursor requests = %#v", calls)
	}
}

func TestMAXHistoryVideoMetadataFailureReleasesLeaseWithoutAcceptingPage(t *testing.T) {
	fixture := newWorkspaceAPIFixture(t)
	message := maxHistoryTextMessage("mid.retry-video", "Видео", "42", true, 1_785_700_000_123)
	message.Attachments = []maxclient.HistoryAttachment{{Type: "video", Token: "video_retry", Complete: true}}
	fake := &maxVideoAPIFake{
		maxHistoryAPIFake: newMAXHistoryAPIFake(fixture.channel, 1,
			maxHistoryAPIScript{messages: []maxclient.HistoryMessage{message}},
			maxHistoryAPIScript{messages: []maxclient.HistoryMessage{message}}),
		readVideo: func(string) (maxclient.VideoInfo, error) {
			return maxclient.VideoInfo{}, &maxclient.Error{StatusCode: 500}
		},
	}
	application := app.New(fixture.storage, fixture.app.Media(), fake, nil, nil, fixture.logger)
	handler := withTestSession(t, fixture.storage, New(application, fixture.logger, "http://localhost:4321", "webhook-secret", AuthOptions{YandexClient: &fakeYandexOAuth{}}).Handler(), "ws-owner")
	response := performJSONRequest(handler, http.MethodPost, maxHistoryImportPath(fixture), "")
	if response.Code != http.StatusBadGateway {
		t.Fatalf("transient metadata status=%d", response.Code)
	}
	posts, err := fixture.storage.ListPostsForWorkspace(t.Context(), "ws-owner", fixture.workspace.ID, store.PostStatusPublished, &fixture.channel.ID)
	if err != nil || len(posts) != 0 {
		t.Fatal("failed metadata page was silently persisted")
	}
	fake.readVideo = func(token string) (maxclient.VideoInfo, error) { return maxclient.VideoInfo{Token: token}, nil }
	response = performJSONRequest(handler, http.MethodPost, maxHistoryImportPath(fixture), "")
	if response.Code != http.StatusOK || fake.videoCalls != 2 {
		t.Fatalf("retry did not release and resume original page: status=%d calls=%d", response.Code, fake.videoCalls)
	}
	posts, err = fixture.storage.ListPostsForWorkspace(t.Context(), "ws-owner", fixture.workspace.ID, store.PostStatusPublished, &fixture.channel.ID)
	if err != nil || len(posts) != 1 || len(posts[0].Attachments) != 0 || posts[0].MAXHistoryAttachmentsComplete {
		t.Fatal("nullable unavailability invented a gallery or lost the publication")
	}
}
