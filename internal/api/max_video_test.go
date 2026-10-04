package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/maxclient"
	"maxpilot/backend/internal/store"
)

type maxVideoAPIFake struct {
	*maxHistoryAPIFake
	video      maxclient.VideoInfo
	videoErr   error
	videoCalls int
	readVideo  func(string) (maxclient.VideoInfo, error)
	afterVideo func()
}

func (f *maxVideoAPIFake) GetVideo(_ context.Context, token string) (maxclient.VideoInfo, error) {
	f.videoCalls++
	if f.readVideo != nil {
		return f.readVideo(token)
	}
	if token != "private-video-token" {
		return maxclient.VideoInfo{}, context.Canceled
	}
	if f.afterVideo != nil {
		f.afterVideo()
	}
	return f.video, f.videoErr
}

func newMAXVideoAPIFixture(t *testing.T) (workspaceAPIFixture, *maxVideoAPIFake, store.Post, http.Handler) {
	t.Helper()
	fixture := newWorkspaceAPIFixture(t)
	message := maxHistoryTextMessage("mid.video", "Видео из MAX", "52", false, 1_785_700_000_123)
	message.Attachments = []maxclient.HistoryAttachment{{Type: "video", Token: "private-video-token", URL: "https://media.max.ru/original.mp4", Complete: true}}
	fake := &maxVideoAPIFake{maxHistoryAPIFake: newMAXHistoryAPIFake(fixture.channel, 1, maxHistoryAPIScript{messages: []maxclient.HistoryMessage{message}}),
		video: maxclient.VideoInfo{Token: "private-video-token", URLs: &maxclient.VideoURLs{MP4480: "https://media.max.ru/current.mp4", HLS: "https://v.oneme.ru/hls.m3u8"},
			ThumbnailURL: "https://media.max.ru/thumb.jpg", Width: 640, Height: 480, DurationMS: 37000}}
	application := app.New(fixture.storage, fixture.app.Media(), fake, nil, nil, fixture.logger)
	raw := New(application, fixture.logger, "http://localhost:4321", "webhook-secret", AuthOptions{YandexClient: &fakeYandexOAuth{}}).Handler()
	response := performJSONRequest(withTestSession(t, fixture.storage, raw, "ws-owner"), http.MethodPost, maxHistoryImportPath(fixture), "")
	if response.Code != http.StatusOK {
		t.Fatalf("import status = %d", response.Code)
	}
	posts, err := fixture.storage.ListPostsForWorkspace(t.Context(), "ws-owner", fixture.workspace.ID, store.PostStatusPublished, &fixture.channel.ID)
	if err != nil || len(posts) != 1 || len(posts[0].Attachments) != 1 {
		t.Fatalf("imported video count = %d, error = %v", len(posts), err)
	}
	return fixture, fake, posts[0], raw
}

func maxVideoPath(workspaceID string, post store.Post) string {
	return "/api/v1/workspaces/" + workspaceID + "/posts/" + postID(post.ID) + "/attachments/" + postID(post.Attachments[0].ID) + "/video"
}

func TestMAXVideoPreviewReadOnlyViewerReceivesFreshSafeSources(t *testing.T) {
	fixture, fake, post, raw := newMAXVideoAPIFixture(t)
	fake.video.URLs.MP41080 = "https://127.0.0.1/private"
	response := performJSONRequest(withTestSession(t, fixture.storage, raw, "ws-viewer"), http.MethodGet, maxVideoPath(fixture.workspace.ID, post), "")
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" || fake.videoCalls != 1 {
		t.Fatalf("video status = %d, calls = %d", response.Code, fake.videoCalls)
	}
	var preview app.MAXVideoPreview
	if err := json.Unmarshal(response.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if !preview.Available || preview.URLs == nil || preview.URLs.MP4480 != "https://media.max.ru/current.mp4" || preview.URLs.MP41080 != "" || preview.DurationMS == nil || *preview.DurationMS != 37000 || preview.AttachmentID != post.Attachments[0].ID {
		t.Fatalf("preview = %#v", preview)
	}
	if strings.Contains(response.Body.String(), "private-video-token") || strings.Contains(response.Body.String(), "provider_token") {
		t.Fatal("opaque provider token escaped to the browser")
	}
	stored, err := fixture.storage.GetPostForWorkspace(t.Context(), "ws-owner", fixture.workspace.ID, post.ID)
	if err != nil || stored.Attachments[0].URL != "https://media.max.ru/original.mp4" || stored.UpdatedAt != post.UpdatedAt {
		t.Fatal("read-only metadata lookup rewrote the imported post")
	}
	// The imported post is authored by someone else and must remain immutable.
	response = performJSONRequest(withTestSession(t, fixture.storage, raw, "ws-owner"), http.MethodPatch,
		"/api/v1/workspaces/"+fixture.workspace.ID+"/posts/"+postID(post.ID), `{"content":"replace"}`)
	assertProblemCode(t, response, http.StatusConflict, "state_conflict")
}

func TestMAXVideoPreviewUnavailableAndTenantPermissionBoundaries(t *testing.T) {
	fixture, fake, post, raw := newMAXVideoAPIFixture(t)
	path := maxVideoPath(fixture.workspace.ID, post)
	viewer := withTestSession(t, fixture.storage, raw, "ws-viewer")
	for _, path := range []string{path + "?video_token=arbitrary", strings.Replace(path, "/attachments/"+postID(post.Attachments[0].ID)+"/", "/attachments/999999/", 1), strings.Replace(path, "/posts/"+postID(post.ID)+"/", "/posts/999999/", 1)} {
		response := performJSONRequest(viewer, http.MethodGet, path, "")
		if response.Code != http.StatusBadRequest && response.Code != http.StatusNotFound {
			t.Fatalf("invalid target status = %d", response.Code)
		}
	}
	response := performJSONRequest(withTestSession(t, fixture.storage, raw, "ws-outsider"), http.MethodGet, path, "")
	assertProblemCode(t, response, http.StatusNotFound, "not_found")
	if fake.videoCalls != 0 {
		t.Fatal("unauthorized or arbitrary target reached MAX")
	}
	fake.membership.Permissions = nil
	response = performJSONRequest(viewer, http.MethodGet, path, "")
	if response.Code != http.StatusUnprocessableEntity || fake.videoCalls != 0 {
		t.Fatal("missing read_all_messages was ignored")
	}
	fake.membership.Permissions = []maxclient.Permission{maxclient.PermissionReadAllMessages}
	fake.video.URLs = nil
	response = performJSONRequest(viewer, http.MethodGet, path, "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"available":false`) || !strings.Contains(response.Body.String(), `"urls":null`) || strings.Contains(response.Body.String(), "processing") {
		t.Fatal("unavailable video was represented as a fabricated processing state")
	}
}

func TestMAXVideoPreviewRechecksRevocationAfterProviderRead(t *testing.T) {
	fixture, fake, post, raw := newMAXVideoAPIFixture(t)
	fake.afterVideo = func() {
		if err := fixture.storage.RemoveWorkspaceMember(t.Context(), "ws-owner", fixture.workspace.ID, "ws-viewer"); err != nil {
			t.Fatal(err)
		}
	}
	response := performJSONRequest(withTestSession(t, fixture.storage, raw, "ws-viewer"), http.MethodGet, maxVideoPath(fixture.workspace.ID, post), "")
	assertProblemCode(t, response, http.StatusNotFound, "not_found")
	if fake.videoCalls != 1 || strings.Contains(response.Body.String(), "current.mp4") {
		t.Fatal("revoked member received a late video URL")
	}
}
