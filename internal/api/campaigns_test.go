package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"maxpilot/backend/internal/store"
)

func TestCampaignAPIAllChannelCalendarApprovalAndOptimisticReschedule(t *testing.T) {
	fixture := newWorkspaceAPIFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	server := New(fixture.app, fixture.logger, "http://localhost:4321", "", AuthOptions{YandexClient: &fakeYandexOAuth{}})
	server.now = func() time.Time { return now }
	router := chi.NewRouter()
	router.Route("/api/v1", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(server.requireSession)
			r.Route("/workspaces/{workspace_id}", func(r chi.Router) {
				server.registerCampaignRoutes(r)
				r.Delete("/posts/{post_id}/schedule", server.cancelWorkspaceSchedule)
			})
		})
	})
	owner := withTestSession(t, fixture.storage, router, "ws-owner")
	viewer := withTestSession(t, fixture.storage, router, "ws-viewer")
	base := "/api/v1/workspaces/" + fixture.workspace.ID
	plannedAt := now.Add(6 * time.Hour).Format(time.RFC3339)
	body := `{"name":"Multi-channel launch","description":"Release","variants":[` +
		`{"channel_id":` + postID(fixture.channel.ID) + `,"title":"Launch","content":"Ready body","format":"markdown","planned_at":"` + plannedAt + `"}]}`

	response := performJSONRequest(viewer, http.MethodPost, base+"/campaigns", body)
	assertProblemCode(t, response, http.StatusForbidden, "workspace_forbidden")
	response = performJSONRequest(owner, http.MethodPost, base+"/campaigns", body)
	if response.Code != http.StatusCreated {
		t.Fatalf("create campaign=%d %s", response.Code, response.Body.String())
	}
	var campaign store.Campaign
	if err := json.Unmarshal(response.Body.Bytes(), &campaign); err != nil || len(campaign.Variants) != 1 {
		t.Fatalf("campaign=%#v err=%v", campaign, err)
	}

	response = performJSONRequest(viewer, http.MethodGet,
		base+"/calendar?from="+now.Add(-time.Hour).Format(time.RFC3339)+"&to="+now.Add(24*time.Hour).Format(time.RFC3339), "")
	if response.Code != http.StatusOK {
		t.Fatalf("viewer calendar=%d %s", response.Code, response.Body.String())
	}
	var calendar []store.CalendarItem
	if err := json.Unmarshal(response.Body.Bytes(), &calendar); err != nil || len(calendar) != 1 || calendar[0].ChannelID != fixture.channel.ID {
		t.Fatalf("calendar=%#v err=%v", calendar, err)
	}

	response = performJSONRequest(owner, http.MethodPost, base+"/campaigns/"+campaign.ID+"/materialize", `{}`)
	if response.Code != http.StatusOK {
		t.Fatalf("materialize=%d %s", response.Code, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), &campaign); err != nil || campaign.Variants[0].PostID == nil || campaign.Variants[0].PostUpdatedAt == nil {
		t.Fatalf("materialized=%#v err=%v", campaign, err)
	}
	variant := campaign.Variants[0]
	scheduleBody := `{"items":[{"variant_id":"` + variant.ID + `","expected_updated_at":"` +
		variant.PostUpdatedAt.Format(time.RFC3339Nano) + `"}]}`
	response = performJSONRequest(owner, http.MethodPost, base+"/campaigns/"+campaign.ID+"/schedule", scheduleBody)
	assertProblemCode(t, response, http.StatusConflict, "campaign_schedule_conflict")

	revision, err := fixture.storage.SubmitPostForReview(t.Context(), "ws-owner", fixture.workspace.ID, *variant.PostID, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.storage.DecidePostReview(t.Context(), "ws-approver", fixture.workspace.ID,
		*variant.PostID, revision.ID, store.ReviewDecisionApproved, "Approved", now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	response = performJSONRequest(owner, http.MethodGet, base+"/campaigns/"+campaign.ID, "")
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &campaign) != nil {
		t.Fatalf("reload campaign=%d %s", response.Code, response.Body.String())
	}
	variant = campaign.Variants[0]
	staleExpected := *variant.PostUpdatedAt
	scheduleBody = `{"items":[{"variant_id":"` + variant.ID + `","expected_updated_at":"` +
		variant.PostUpdatedAt.Format(time.RFC3339Nano) + `"}]}`
	response = performJSONRequest(owner, http.MethodPost, base+"/campaigns/"+campaign.ID+"/schedule", scheduleBody)
	if response.Code != http.StatusOK {
		t.Fatalf("schedule approved campaign=%d %s", response.Code, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), &campaign); err != nil {
		t.Fatal(err)
	}
	scheduled := campaign.Variants[0]

	reschedulePath := base + "/calendar/posts/" + postID(*scheduled.PostID)
	staleBody := `{"scheduled_at":"` + now.Add(8*time.Hour).Format(time.RFC3339) +
		`","expected_updated_at":"` + staleExpected.Format(time.RFC3339Nano) + `"}`
	response = performJSONRequest(owner, http.MethodPut, reschedulePath, staleBody)
	assertProblemCode(t, response, http.StatusConflict, "calendar_reschedule_conflict")
	validBody := `{"scheduled_at":"` + now.Add(8*time.Hour).Format(time.RFC3339) +
		`","expected_updated_at":"` + scheduled.PostUpdatedAt.Format(time.RFC3339Nano) + `"}`
	response = performJSONRequest(owner, http.MethodPut, reschedulePath, validBody)
	if response.Code != http.StatusOK {
		t.Fatalf("reschedule=%d %s", response.Code, response.Body.String())
	}
	var post store.Post
	if err := json.Unmarshal(response.Body.Bytes(), &post); err != nil || post.ScheduledAt == nil || !post.ScheduledAt.Equal(now.Add(8*time.Hour)) {
		t.Fatalf("rescheduled post=%#v err=%v", post, err)
	}
	response = performJSONRequest(owner, http.MethodDelete,
		base+"/posts/"+postID(post.ID)+"/schedule", "")
	post = store.Post{}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &post) != nil ||
		post.Status != store.PostStatusDraft || post.ScheduledAt != nil {
		t.Fatalf("cancel campaign schedule=%d post=%#v body=%s", response.Code, post, response.Body.String())
	}
	response = performJSONRequest(owner, http.MethodGet, base+"/campaigns/"+campaign.ID, "")
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &campaign) != nil ||
		campaign.Variants[0].Status != "materialized" {
		t.Fatalf("campaign after API cancellation=%d campaign=%#v body=%s", response.Code, campaign, response.Body.String())
	}
}
