package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"maxpilot/backend/internal/store"
)

func TestWorkspaceAnalyticsContentIsScopedNormalizedTimezoneAwareAndSafe(t *testing.T) {
	fixture := newWorkspaceAPIFixture(t)
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)

	primary, err := fixture.storage.CreateChannelForWorkspace(
		t.Context(), "ws-owner", fixture.workspace.ID, store.Channel{
			VerifiedMAXOwnerID: "max-owner", MAXChatID: "-analytics-primary",
			Title: "Primary", ParticipantsCount: 1000, IsChannel: true, Active: true,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.storage.SyncChannelParticipantStatsForUser(
		t.Context(), fixture.workspace.CompatOwnerUserID, primary.ID, primary.MAXChatID,
		"", 800, time.Date(2026, time.July, 12, 20, 0, 0, 0, time.UTC),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.storage.SyncChannelParticipantStatsForUser(
		t.Context(), fixture.workspace.CompatOwnerUserID, primary.ID, primary.MAXChatID,
		"", 1000, now,
	); err != nil {
		t.Fatal(err)
	}
	secondary, err := fixture.storage.CreateChannelForWorkspace(
		t.Context(), "ws-owner", fixture.workspace.ID, store.Channel{
			VerifiedMAXOwnerID: "max-owner", MAXChatID: "-analytics-secondary",
			Title: "Secondary", ParticipantsCount: 2000, IsChannel: true, Active: true,
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	// Sunday 23:30 UTC is Monday 02:30 at UTC+03:00. The heatmap must use
	// the requested local offset and report it back to the client.
	primaryPublishedAt := time.Date(2026, time.July, 12, 23, 30, 0, 0, time.UTC)
	primaryViews := int64(100)
	primaryPost, err := fixture.storage.CreatePostForWorkspace(
		t.Context(), "ws-owner", fixture.workspace.ID, store.Post{
			Title: "Primary winner", Content: "Source body", Format: store.FormatMarkdown,
			Status: store.PostStatusPublished, ChannelID: &primary.ID,
			MAXMessageID: "primary-message", MAXMessageURL: "https://max.ru/primary",
			MAXViews: &primaryViews, MAXStatsSyncedAt: &now, PublishedAt: &primaryPublishedAt,
			Notify: true,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	secondaryPublishedAt := time.Date(2026, time.July, 11, 8, 0, 0, 0, time.UTC)
	secondaryViews := int64(40)
	if _, err := fixture.storage.CreatePostForWorkspace(
		t.Context(), "ws-owner", fixture.workspace.ID, store.Post{
			Title: "Secondary post", Content: "Other body", Format: store.FormatMarkdown,
			Status: store.PostStatusPublished, ChannelID: &secondary.ID,
			MAXMessageID: "secondary-message", MAXViews: &secondaryViews,
			MAXStatsSyncedAt: &now, PublishedAt: &secondaryPublishedAt, Notify: true,
		},
	); err != nil {
		t.Fatal(err)
	}

	server := New(fixture.app, fixture.logger, "http://localhost:4321", "", AuthOptions{YandexClient: &fakeYandexOAuth{}})
	server.now = func() time.Time { return now }
	router := chi.NewRouter()
	router.Route("/api/v1", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(server.requireSession)
			r.Route("/workspaces/{workspace_id}", func(r chi.Router) {
				server.RegisterAnalyticsContentRoutes(r)
			})
		})
	})
	viewer := withTestSession(t, fixture.storage, router, "ws-viewer")
	editor := withTestSession(t, fixture.storage, router, "ws-editor")
	outsider := withTestSession(t, fixture.storage, router, "ws-outsider")
	base := "/api/v1/workspaces/" + fixture.workspace.ID
	undiscoverable := performJSONRequest(outsider, http.MethodGet,
		base+"/analytics/content?channel_id=all&tz_offset_minutes=180", "")
	assertProblemCode(t, undiscoverable, http.StatusNotFound, "not_found")

	response := performJSONRequest(viewer, http.MethodGet,
		base+"/analytics/content?channel_id=all&from=2026-07-01&to=2026-07-13&tz_offset_minutes=180", "")
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("all-channel analytics = %d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	var payload struct {
		Analytics store.AnalyticsContentReport `json:"analytics"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	report := payload.Analytics
	if report.Scope.Kind != "workspace" || report.Summary.PublishedPosts != 2 ||
		report.Summary.ViewsPer1KAudience == nil || report.Summary.AverageViewsPerHour == nil ||
		report.Summary.AverageReach == nil || *report.Summary.AverageReach != 70 ||
		report.Summary.ERR30D == nil || *report.Summary.ERR30D != 7.25 || report.Summary.ERR30DSample != 2 ||
		report.Summary.PublishedLast24H != 1 || report.Summary.PostsPerDay == nil ||
		report.TimezoneOffsetMinutes != 180 || len(report.Heatmap) != 7*24 ||
		len(report.Posts) != 2 || report.Posts[0].Audience != 800 ||
		report.Posts[0].Score == nil || *report.Posts[0].Score != 31.62 {
		t.Fatalf("analytics report = %#v", report)
	}
	var rolloverCell *store.AnalyticsHeatmapCell
	for index := range report.Heatmap {
		cell := &report.Heatmap[index]
		if cell.Weekday == 0 && cell.Hour == 2 {
			rolloverCell = cell
			break
		}
	}
	if rolloverCell == nil || rolloverCell.Posts != 1 || rolloverCell.ViewsPer1KAudience == nil ||
		*rolloverCell.ViewsPer1KAudience != 125 || rolloverCell.Score == nil {
		t.Fatalf("UTC-to-local rollover cell = %#v", rolloverCell)
	}
	if report.BestTime == nil || report.BestTime.Weekday != 0 || report.BestTime.Hour != 2 ||
		report.BestTime.NextAt.Before(now) {
		t.Fatalf("best time = %#v", report.BestTime)
	}

	current := performJSONRequest(viewer, http.MethodGet,
		base+"/analytics?channel_id="+postID(primary.ID)+"&from=2026-07-01&to=2026-07-13&tz_offset_minutes=180", "")
	if current.Code != http.StatusOK {
		t.Fatalf("current-channel analytics = %d %s", current.Code, current.Body.String())
	}
	if err := json.Unmarshal(current.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Analytics.Scope.Kind != "channel" || len(payload.Analytics.Posts) != 1 ||
		payload.Analytics.Posts[0].ChannelID != primary.ID {
		t.Fatalf("current channel report = %#v", payload.Analytics)
	}

	invalidTimezone := performJSONRequest(viewer, http.MethodGet,
		base+"/analytics/content?channel_id=all&tz_offset_minutes=841", "")
	assertProblemCode(t, invalidTimezone, http.StatusBadRequest, "validation_error")

	forbidden := performJSONRequest(viewer, http.MethodPost,
		base+"/analytics/content/posts/"+postID(primaryPost.ID)+"/variation", "")
	assertProblemCode(t, forbidden, http.StatusForbidden, "workspace_forbidden")

	variation := performJSONRequest(editor, http.MethodPost,
		base+"/analytics/content/posts/"+postID(primaryPost.ID)+"/variation", "")
	if variation.Code != http.StatusCreated {
		t.Fatalf("variation = %d %s", variation.Code, variation.Body.String())
	}
	var variationPayload struct {
		Post store.Post `json:"post"`
	}
	if err := json.Unmarshal(variation.Body.Bytes(), &variationPayload); err != nil {
		t.Fatal(err)
	}
	if variationPayload.Post.Status != store.PostStatusDraft || variationPayload.Post.ScheduledAt != nil ||
		variationPayload.Post.PublishedAt != nil {
		t.Fatalf("variation bypassed draft workflow: %#v", variationPayload.Post)
	}

	plannedAt := now.Add(7 * 24 * time.Hour).Format(time.RFC3339)
	repeat := performJSONRequest(editor, http.MethodPost,
		base+"/analytics/content/posts/"+postID(primaryPost.ID)+"/repeat",
		`{"planned_at":"`+plannedAt+`"}`)
	if repeat.Code != http.StatusCreated {
		t.Fatalf("repeat = %d %s", repeat.Code, repeat.Body.String())
	}
	var repeatPayload struct {
		Post             store.Post     `json:"post"`
		PlannedAt        time.Time      `json:"planned_at"`
		RequiresApproval bool           `json:"requires_approval"`
		CampaignID       string         `json:"campaign_id"`
		VariantID        string         `json:"variant_id"`
		Campaign         store.Campaign `json:"campaign"`
	}
	if err := json.Unmarshal(repeat.Body.Bytes(), &repeatPayload); err != nil {
		t.Fatal(err)
	}
	if repeatPayload.Post.Status != store.PostStatusDraft || repeatPayload.Post.ScheduledAt != nil ||
		!repeatPayload.RequiresApproval || !repeatPayload.PlannedAt.Equal(now.Add(7*24*time.Hour)) ||
		repeatPayload.CampaignID == "" || repeatPayload.VariantID == "" ||
		len(repeatPayload.Campaign.Variants) != 1 || repeatPayload.Campaign.Variants[0].PostID == nil ||
		*repeatPayload.Campaign.Variants[0].PostID != repeatPayload.Post.ID {
		t.Fatalf("repeat bypassed approval workflow: %#v", repeatPayload)
	}
	persisted, err := fixture.storage.GetCampaign(
		t.Context(), "ws-editor", fixture.workspace.ID, repeatPayload.CampaignID,
	)
	if err != nil || len(persisted.Variants) != 1 ||
		!persisted.Variants[0].PlannedAt.Equal(now.Add(7*24*time.Hour)) {
		t.Fatalf("durable repeat plan = %#v err=%v", persisted, err)
	}
}

func TestWorkspacePostAnalyticsIsDetailedReadOnlyAndCapabilityScoped(t *testing.T) {
	fixture := newWorkspaceAPIFixture(t)
	ctx := t.Context()
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	publishedAt := time.Date(2026, time.July, 11, 20, 0, 0, 0, time.UTC)
	if _, err := fixture.storage.SyncChannelParticipantStatsForUser(
		ctx, fixture.workspace.CompatOwnerUserID, fixture.channel.ID, fixture.channel.MAXChatID,
		"", 800, publishedAt.Add(-time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	post, err := fixture.storage.CreatePostForWorkspace(ctx, "ws-owner", fixture.workspace.ID, store.Post{
		Title: "Detailed post", Content: "body", Format: store.FormatMarkdown,
		Status: store.PostStatusPublished, ChannelID: &fixture.channel.ID,
		MAXMessageID: "detailed-message", PublishedAt: &publishedAt, Notify: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	anchorViews, firstViews, lastViews := int64(90), int64(100), int64(130)
	if _, err := fixture.storage.SyncPublicationMetadataForUser(
		ctx, fixture.workspace.CompatOwnerUserID, post.ID, fixture.channel.ID, post.MAXMessageID,
		"https://max.ru/detailed", &anchorViews, publishedAt.Add(3*time.Hour), false,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.storage.SyncPublicationMetadataForUser(
		ctx, fixture.workspace.CompatOwnerUserID, post.ID, fixture.channel.ID, post.MAXMessageID,
		"https://max.ru/detailed", &firstViews, publishedAt.Add(5*time.Hour), false,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.storage.SyncPublicationMetadataForUser(
		ctx, fixture.workspace.CompatOwnerUserID, post.ID, fixture.channel.ID, post.MAXMessageID,
		"https://max.ru/detailed", &lastViews, publishedAt.Add(7*time.Hour), false,
	); err != nil {
		t.Fatal(err)
	}

	server := New(fixture.app, fixture.logger, "http://localhost:4321", "", AuthOptions{YandexClient: &fakeYandexOAuth{}})
	server.now = func() time.Time { return now }
	router := chi.NewRouter()
	router.Route("/api/v1", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(server.requireSession)
			r.Route("/workspaces/{workspace_id}", func(r chi.Router) {
				server.RegisterAnalyticsContentRoutes(r)
			})
		})
	})
	base := "/api/v1/workspaces/" + fixture.workspace.ID + "/analytics/content/posts/" + postID(post.ID)
	path := base + "?from=2026-07-12&to=2026-07-13"
	handlers := map[string]http.Handler{
		"ws-owner":    withTestSession(t, fixture.storage, router, "ws-owner"),
		"ws-editor":   withTestSession(t, fixture.storage, router, "ws-editor"),
		"ws-approver": withTestSession(t, fixture.storage, router, "ws-approver"),
		"ws-viewer":   withTestSession(t, fixture.storage, router, "ws-viewer"),
		"ws-outsider": withTestSession(t, fixture.storage, router, "ws-outsider"),
	}
	for _, userID := range []string{"ws-owner", "ws-editor", "ws-approver", "ws-viewer"} {
		response := performJSONRequest(handlers[userID], http.MethodGet, path, "")
		if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s post analytics = %d headers=%v body=%s", userID, response.Code, response.Header(), response.Body.String())
		}
	}

	viewer := handlers["ws-viewer"]
	response := performJSONRequest(viewer, http.MethodGet, path, "")
	var payload struct {
		Analytics store.PostAnalyticsReport `json:"analytics"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	report := payload.Analytics
	if report.Post.ID != post.ID || report.Post.PublicationState != store.PostAnalyticsPublicationPublished ||
		report.Post.MAXMessageURL != "https://max.ru/detailed" || report.Summary.Views == nil ||
		*report.Summary.Views != lastViews || report.Summary.ViewsChange == nil ||
		*report.Summary.ViewsChange != 40 || report.Summary.Audience == nil ||
		*report.Summary.Audience != 800 || report.Summary.AudienceSource != store.PostAnalyticsAudienceSnapshotAtPublish ||
		report.Summary.ViewsPer1KAudience == nil || *report.Summary.ViewsPer1KAudience != 162.5 ||
		report.Summary.LifetimeViewsPerHour == nil || *report.Summary.LifetimeViewsPerHour != 18.57 ||
		report.Summary.ObservedViewsPerHour == nil || *report.Summary.ObservedViewsPerHour != 10 ||
		report.Summary.SeriesTruncated ||
		len(report.Series) != 3 || report.Series[0].Views != anchorViews || report.Series[0].Delta != nil ||
		report.Series[1].Delta == nil || *report.Series[1].Delta != 10 || report.Series[2].Delta == nil ||
		*report.Series[2].Delta != 30 {
		t.Fatalf("detailed post analytics = %#v", report)
	}

	outsider := handlers["ws-outsider"]
	undiscoverable := performJSONRequest(outsider, http.MethodGet, path, "")
	assertProblemCode(t, undiscoverable, http.StatusNotFound, "not_found")
	invalidPeriod := performJSONRequest(viewer, http.MethodGet, base+"?from=2026-07-14&to=2026-07-13", "")
	assertProblemCode(t, invalidPeriod, http.StatusBadRequest, "validation_error")
	invalidID := performJSONRequest(viewer, http.MethodGet,
		"/api/v1/workspaces/"+fixture.workspace.ID+"/analytics/content/posts/not-a-number", "")
	assertProblemCode(t, invalidID, http.StatusBadRequest, "validation_error")
}
