package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/openairesearch"
	"maxpilot/backend/internal/store"
)

func TestDiscoveryDraftAPIAuthorizesSavedCardsAndIsIdempotentWithoutAnotherAIRequest(t *testing.T) {
	fake := &fakeContentDiscoveryClient{fakeResearchClient: &fakeResearchClient{}}
	_, storage, raw := newAIQuotaTestServer(t, nil, fake, testAILimitOptions(), "discovery-draft-owner", "discovery-draft-outsider")
	owner := withTestSession(t, storage, raw, "discovery-draft-owner")
	workspace := personalWorkspaceIDForTest(t, storage, "discovery-draft-owner")
	path := "/api/v1/workspaces/" + workspace + "/research/content/drafts"
	response := performJSONRequest(owner, http.MethodPost, "/api/v1/workspaces/"+workspace+"/research/discover", `{"topic":"Смешные коты"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("discovery=%d %s", response.Code, response.Body.String())
	}
	var cards openairesearch.DiscoverContentResult
	if err := json.Unmarshal(response.Body.Bytes(), &cards); err != nil || len(cards.Cards) != 1 || cards.Cards[0].CandidateID == "" {
		t.Fatalf("card authority was not issued: %#v err=%v", cards, err)
	}
	requestID := "01234567-89ab-4cde-8f01-23456789abcd"
	body := fmt.Sprintf(`{"candidate_id":%q,"client_request_id":%q}`, cards.Cards[0].CandidateID, requestID)
	anonymous := httptest.NewRecorder()
	raw.ServeHTTP(anonymous, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous draft=%d", anonymous.Code)
	}
	outsider := withTestSession(t, storage, raw, "discovery-draft-outsider")
	response = performJSONRequest(outsider, http.MethodPost, path, body)
	if response.Code != http.StatusNotFound {
		t.Fatalf("foreign workspace exposed saved card: %d %s", response.Code, response.Body.String())
	}
	foreignWorkspace := personalWorkspaceIDForTest(t, storage, "discovery-draft-outsider")
	response = performJSONRequest(outsider, http.MethodPost, "/api/v1/workspaces/"+foreignWorkspace+"/research/content/drafts", body)
	if response.Code != http.StatusNotFound {
		t.Fatalf("candidate crossed workspace/actor scope: %d %s", response.Code, response.Body.String())
	}
	for _, invalid := range []string{
		`{}`, fmt.Sprintf(`{"candidate_id":%q,"client_request_id":"not-a-uuid"}`, cards.Cards[0].CandidateID),
		fmt.Sprintf(`{"candidate_id":%q,"client_request_id":%q,"media_url":"https://localhost/private"}`, cards.Cards[0].CandidateID, requestID),
	} {
		response = performJSONRequest(owner, http.MethodPost, path, invalid)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("untrusted transfer parameters accepted: %d %s", response.Code, response.Body.String())
		}
	}
	response = performJSONRequest(owner, http.MethodPost, path, body)
	var first app.CreateDiscoveryDraftResult
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" || json.Unmarshal(response.Body.Bytes(), &first) != nil || first.Post.ID <= 0 || first.MediaTransfer.Status != "complete" || first.MediaTransfer.Items == nil || first.MediaTransfer.Warnings == nil {
		t.Fatalf("draft response=%d %s", response.Code, response.Body.String())
	}
	response = performJSONRequest(owner, http.MethodPost, path, body)
	var replay app.CreateDiscoveryDraftResult
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &replay) != nil || replay.Post.ID != first.Post.ID {
		t.Fatalf("retry duplicated draft: %d %s", response.Code, response.Body.String())
	}
	response = performJSONRequest(owner, http.MethodPost, path, fmt.Sprintf(`{"candidate_id":%q,"client_request_id":%q,"include_media":false}`, cards.Cards[0].CandidateID, requestID))
	assertProblemCode(t, response, http.StatusConflict, "content_discovery_request_conflict")
	fake.mu.Lock()
	providerCalls := len(fake.discoveryRequests)
	fake.mu.Unlock()
	billing := readWorkspaceBillingForTest(t, owner, workspace)
	if providerCalls != 1 || billingUsageQuantity(t, billing.Usage, store.UsageMetricAIResearchRequests) != 1 {
		t.Fatal("draft transfer or retry triggered another AI charge")
	}
}

func TestDiscoveryDraftAPIReturnsExplicitBusyAndExpiredStates(t *testing.T) {
	_, storage, raw := newAIQuotaTestServer(t, nil, nil, testAILimitOptions(), "discovery-draft-states")
	owner := withTestSession(t, storage, raw, "discovery-draft-states")
	workspace := personalWorkspaceIDForTest(t, storage, "discovery-draft-states")
	path := "/api/v1/workspaces/" + workspace + "/research/content/drafts"
	now := time.Now().UTC().Truncate(time.Microsecond)
	payload := json.RawMessage(`{"card":{"draft":{"title":"Title","content":"Body","format":"markdown"}},"media":[]}`)
	candidates, err := storage.SaveDiscoveryCandidates(t.Context(), "discovery-draft-states", workspace, nil, []json.RawMessage{payload}, now)
	if err != nil {
		t.Fatal(err)
	}
	requestID := "01234567-89ab-4cde-8f01-23456789abcd"
	if _, err = storage.ClaimDiscoveryDraft(t.Context(), "discovery-draft-states", workspace, candidates[0].ID, requestID, nil, true, store.Post{Title: "Title", Content: "Body", Format: "markdown"}, now); err != nil {
		t.Fatal(err)
	}
	response := performJSONRequest(owner, http.MethodPost, path, fmt.Sprintf(`{"candidate_id":%q,"client_request_id":%q}`, candidates[0].ID, requestID))
	assertProblemCode(t, response, http.StatusConflict, "content_discovery_draft_in_progress")
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("busy request may be cached")
	}
	candidates, err = storage.SaveDiscoveryCandidates(t.Context(), "discovery-draft-states", workspace, nil, []json.RawMessage{payload}, now.Add(-25*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	response = performJSONRequest(owner, http.MethodPost, path, fmt.Sprintf(`{"candidate_id":%q,"client_request_id":%q}`, candidates[0].ID, "11234567-89ab-4cde-8f01-23456789abcd"))
	assertProblemCode(t, response, http.StatusGone, "content_discovery_candidate_expired")
}
