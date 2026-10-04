package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"maxpilot/backend/internal/openaiimg"
	"maxpilot/backend/internal/store"
)

func TestDirectSuggestionValidationDoesNotConsumePaidUsageOrRateQuota(t *testing.T) {
	fake := &fakeResearchClient{}
	options := testAILimitOptions()
	options.MonthlyPlanEnforcement = true
	options.ResearchPerMinute = 1
	server, storage, raw := newAIQuotaTestServer(t, nil, fake, options, "direct-validation-usage")
	server.now = func() time.Time { return time.Now().UTC() }
	handler := withTestSession(t, storage, raw, "direct-validation-usage")
	workspaceID := personalWorkspaceIDForTest(t, storage, "direct-validation-usage")
	path := "/api/v1/workspaces/" + workspaceID + "/advertising/direct/campaigns/suggest"
	for _, body := range []string{
		strings.Replace(validDirectSuggestionBody, `"objective":"traffic"`, `"objective":""`, 1),
		strings.Replace(validDirectSuggestionBody, "Promote practical channel content to a relevant business audience.", "short", 1),
		strings.Replace(validDirectSuggestionBody, "https://maxposty.ru/", "not-a-url", 1),
	} {
		response := performJSONRequest(handler, http.MethodPost, path, body)
		assertProblemCode(t, response, http.StatusBadRequest, "validation_error")
	}
	fake.mu.Lock()
	calls := len(fake.directRequests)
	fake.mu.Unlock()
	billing := readWorkspaceBillingForTest(t, handler, workspaceID)
	if calls != 0 || billingUsageQuantity(t, billing.Usage, store.UsageMetricAIResearchRequests) != 0 {
		t.Fatalf("invalid suggestions reached provider or consumed usage: calls=%d usage=%#v", calls, billing.Usage)
	}
	response := performJSONRequest(handler, http.MethodPost, path, validDirectSuggestionBody)
	if response.Code != http.StatusOK {
		t.Fatalf("invalid suggestions consumed minute quota: %d %s", response.Code, response.Body.String())
	}
	billing = readWorkspaceBillingForTest(t, handler, workspaceID)
	if billingUsageQuantity(t, billing.Usage, store.UsageMetricAIResearchRequests) != 1 {
		t.Fatalf("valid suggestion was not charged once: %#v", billing.Usage)
	}
}

func TestPublishingPostImageDoesNotConsumePaidUsage(t *testing.T) {
	for _, nested := range []bool{false, true} {
		name := "legacy"
		if nested {
			name = "workspace"
		}
		t.Run(name, func(t *testing.T) {
			image := &quotaImageClient{}
			options := testAILimitOptions()
			options.MonthlyPlanEnforcement = true
			options.ImagePerMinute = 1
			userID := "publishing-image-" + name
			_, storage, raw := newAIQuotaTestServer(t, image, nil, options, userID)
			handler := withTestSession(t, storage, raw, userID)
			workspaceID := personalWorkspaceIDForTest(t, storage, userID)
			post, err := storage.CreatePost(t.Context(), store.Post{
				UserID: userID, Title: "Publishing", Content: "Body", Format: store.FormatMarkdown, Status: store.PostStatusDraft,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := storage.ClaimForPublishing(t.Context(), post.ID); err != nil {
				t.Fatal(err)
			}
			path := "/api/v1/posts/" + formatInt64(post.ID) + "/generate-image"
			if nested {
				path = "/api/v1/workspaces/" + workspaceID + "/posts/" + formatInt64(post.ID) + "/generate-image"
			}
			response := performJSONRequest(handler, http.MethodPost, path, `{"prompt":"Illustration","quality":"medium"}`)
			assertProblemCode(t, response, http.StatusConflict, "state_conflict")
			billing := readWorkspaceBillingForTest(t, handler, workspaceID)
			if image.callCount() != 0 || billingUsageQuantity(t, billing.Usage, store.UsageMetricAIImageCredits) != 0 {
				t.Fatalf("publishing target consumed AI usage: calls=%d usage=%#v", image.callCount(), billing.Usage)
			}
			response = performJSONRequest(handler, http.MethodPost, "/api/v1/images/generate", `{"prompt":"Valid image","quality":"medium"}`)
			if response.Code != http.StatusCreated {
				t.Fatalf("publishing rejection consumed minute quota: %d %s", response.Code, response.Body.String())
			}
			billing = readWorkspaceBillingForTest(t, handler, workspaceID)
			if image.callCount() != 1 || billingUsageQuantity(t, billing.Usage, store.UsageMetricAIImageCredits) != 9 {
				t.Fatalf("successful generation was not charged once: calls=%d usage=%#v", image.callCount(), billing.Usage)
			}
		})
	}
}

func TestImageGenerationConflictAfterProviderRetainsPaidUsage(t *testing.T) {
	image := &quotaImageClient{}
	options := testAILimitOptions()
	options.MonthlyPlanEnforcement = true
	_, storage, raw := newAIQuotaTestServer(t, image, nil, options, "image-generation-race")
	handler := withTestSession(t, storage, raw, "image-generation-race")
	workspaceID := personalWorkspaceIDForTest(t, storage, "image-generation-race")
	post, err := storage.CreatePost(t.Context(), store.Post{
		UserID: "image-generation-race", Title: "Race", Content: "Body", Format: store.FormatMarkdown, Status: store.PostStatusDraft,
	})
	if err != nil {
		t.Fatal(err)
	}
	image.generate = func(ctx context.Context, _ openaiimg.GenerateRequest) (openaiimg.Result, error) {
		if _, err := storage.ClaimForPublishing(ctx, post.ID); err != nil {
			return openaiimg.Result{}, err
		}
		return openaiimg.Result{Bytes: quotaTestPNG(), MIMEType: "image/png", Model: "fake"}, nil
	}
	response := performJSONRequest(handler, http.MethodPost,
		"/api/v1/workspaces/"+workspaceID+"/posts/"+formatInt64(post.ID)+"/generate-image",
		`{"prompt":"Illustration","quality":"medium"}`)
	assertProblemCode(t, response, http.StatusConflict, "state_conflict")
	billing := readWorkspaceBillingForTest(t, handler, workspaceID)
	if image.callCount() != 1 || billingUsageQuantity(t, billing.Usage, store.UsageMetricAIImageCredits) != 9 {
		t.Fatalf("AI work performed during a concurrent publication must remain charged: calls=%d usage=%#v", image.callCount(), billing.Usage)
	}
}
