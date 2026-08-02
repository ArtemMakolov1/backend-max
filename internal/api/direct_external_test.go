package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/store"
)

func TestPublicDirectExternalCampaignUsesJavaScriptSafeProviderIDAndNullableEnd(t *testing.T) {
	t.Parallel()
	response := publicDirectExternalCampaign(store.DirectExternalCampaign{
		ProviderCampaignID: 9_007_199_254_740_993,
		Name:               "Existing campaign", CampaignType: "TEXT_CAMPAIGN",
		ProviderStatus: "ACCEPTED", ProviderState: "ON",
		ProviderStatusPayment: "ALLOWED",
		StartsAt:              time.Date(2043, time.June, 7, 12, 30, 0, 0, time.FixedZone("test", 3600)),
		Timezone:              "Europe/Moscow",
		SyncedAt:              time.Date(2043, time.June, 7, 12, 30, 0, 0, time.UTC),
	})
	if response.ProviderCampaignID != "9007199254740993" {
		t.Fatalf("provider_campaign_id = %q", response.ProviderCampaignID)
	}
	if response.StartsAt != "2043-06-07" || response.EndsAt != nil {
		t.Fatalf("date projection = starts %q ends %#v", response.StartsAt, response.EndsAt)
	}
	payload, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded["provider_campaign_id"].(string); !ok {
		t.Fatalf("provider_campaign_id JSON type = %T", decoded["provider_campaign_id"])
	}
	if value, exists := decoded["ends_at"]; !exists || value != nil {
		t.Fatalf("ends_at JSON = %#v, exists=%v", value, exists)
	}
}

func TestDirectExternalCampaignEnvelopeIncludesSnapshotStateWhenEmpty(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		syncedAt *time.Time
	}{
		{name: "never synchronized"},
		{name: "successfully synchronized empty snapshot", syncedAt: func() *time.Time {
			value := time.Date(2043, time.June, 7, 12, 30, 0, 0, time.UTC)
			return &value
		}()},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			response := httptest.NewRecorder()
			(&Server{}).writeDirectExternalCampaigns(
				response,
				app.DirectExternalCampaignSnapshot{
					Items: []store.DirectExternalCampaign{}, SyncedAt: test.syncedAt,
				},
			)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d", response.Code)
			}
			var envelope struct {
				Items    []directExternalCampaignResponse `json:"items"`
				SyncedAt *time.Time                       `json:"synced_at"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Items == nil || len(envelope.Items) != 0 {
				t.Fatalf("items = %#v", envelope.Items)
			}
			if test.syncedAt == nil && envelope.SyncedAt != nil {
				t.Fatalf("synced_at = %v, want null", envelope.SyncedAt)
			}
			if test.syncedAt != nil &&
				(envelope.SyncedAt == nil || !envelope.SyncedAt.Equal(*test.syncedAt)) {
				t.Fatalf("synced_at = %v, want %v", envelope.SyncedAt, test.syncedAt)
			}
		})
	}
}
