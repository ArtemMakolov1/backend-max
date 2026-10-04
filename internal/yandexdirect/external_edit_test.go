package yandexdirect

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestExternalEditsSendOnlyNameOrSelectedCopyAndRequiredResponsivePeers(t *testing.T) {
	t.Parallel()
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" || r.Header.Get("Client-Login") != "account" {
			t.Errorf("missing auth/account")
		}
		var request map[string]any
		_ = json.NewDecoder(r.Body).Decode(&request)
		requests = append(requests, request)
		id := float64(12)
		if strings.HasSuffix(r.URL.Path, "/campaigns") {
			id = 77
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"UpdateResults": []any{map[string]any{"Id": id}}}})
	}))
	defer server.Close()
	client, err := New(server.URL+"/json/v501", "client", "secret", CallbackRedirectURI, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err = client.UpdateExternalCampaignName(context.Background(), "token", "account", 77, "Название"); err != nil {
		t.Fatal(err)
	}
	name := requests[0]["params"].(map[string]any)["Campaigns"].([]any)[0].(map[string]any)
	if !reflect.DeepEqual(name, map[string]any{"Id": float64(77), "Name": "Название"}) {
		t.Fatalf("unexpected provider fields: %#v", name)
	}
	business := int64(456)
	href := "https://example.test/"
	title := "Новый заголовок"
	textAd := ExternalAd{ID: 12, Type: "TEXT_AD", Title: "Старый", Text: "Старый текст", Href: &href, BusinessID: &business}
	if err = client.UpdateExternalAd(context.Background(), "token", "account", textAd, ExternalAdChanges{Title: &title, Title2Set: true}); err != nil {
		t.Fatal(err)
	}
	scalar := requests[1]["params"].(map[string]any)["Ads"].([]any)[0].(map[string]any)["TextAd"].(map[string]any)
	if !reflect.DeepEqual(scalar, map[string]any{"Title": title, "Title2": nil}) {
		t.Fatalf("unchanged fields were sent: %#v", scalar)
	}
	responsive := ExternalAd{ID: 12, Type: "RESPONSIVE_AD", Titles: []string{"Первый", "Второй"}, Texts: []string{"Первый текст", "Второй текст"}, BusinessID: &business}
	titles := []string{"Обновлённый", "Второй"}
	if err = client.UpdateExternalAd(context.Background(), "token", "account", responsive, ExternalAdChanges{Titles: &titles}); err != nil {
		t.Fatal(err)
	}
	peers := requests[2]["params"].(map[string]any)["Ads"].([]any)[0].(map[string]any)["ResponsiveAd"].(map[string]any)
	if !reflect.DeepEqual(peers, map[string]any{"Titles": []any{"Обновлённый", "Второй"}, "Texts": []any{"Первый текст", "Второй текст"}, "BusinessId": float64(456)}) {
		t.Fatalf("required peers/destination lost or unrelated fields sent: %#v", peers)
	}
	if len(requests) != 3 {
		t.Fatal("mutation was retried")
	}
}
func TestExternalInspectionRejectsTruncatedOrCrossCampaignAds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		limited  any
		campaign int64
	}{{"truncated", 1000, 77}, {"wrong campaign", nil, 78}} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/campaigns") {
					_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"Campaigns": []any{validCampaignListItem(77, "Campaign")}}})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"Ads": []any{map[string]any{"Id": 12, "CampaignId": tc.campaign, "AdGroupId": 13, "Type": "TEXT_AD", "Status": "ACCEPTED", "State": "ON", "TextAd": map[string]any{"Title": "Copy", "Text": "Current text", "Href": "https://example.test/"}}}, "LimitedBy": tc.limited}})
			}))
			defer server.Close()
			client, err := New(server.URL+"/json/v501", "client", "secret", CallbackRedirectURI, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = client.GetExternalCampaignDetail(context.Background(), "token", "account", 77); err == nil {
				t.Fatal("unsafe partial detail accepted")
			}
		})
	}
}
func TestExternalCopyLimitsAndUnsupportedFieldsFailBeforeProviderWrite(t *testing.T) {
	t.Parallel()
	empty := []string{}
	invalidURL := "http://example.test/"
	oversized := strings.Repeat("я", 23)
	for _, patch := range []ExternalAdChanges{{}, {Titles: &empty}, {Href: &invalidURL}, {Title: &oversized}} {
		if patch.Validate() == nil {
			t.Fatalf("invalid patch passed: %#v", patch)
		}
	}
	title := "Новый"
	if _, err := (ExternalAdChanges{Title: &title}).Merge(ExternalAd{Type: "IMAGE_AD"}); err == nil {
		t.Fatal("unsupported ad allowed")
	}
	texts := []string{"Новый текст"}
	if _, err := (ExternalAdChanges{Texts: &texts}).Merge(ExternalAd{Type: "RESPONSIVE_AD", Titles: []string{"Заголовок"}}); err == nil {
		t.Fatal("missing destination allowed")
	}
}

func TestExternalFingerprintIgnoresLifecycleOrderingButFencesPeersAndDestination(t *testing.T) {
	t.Parallel()
	business := int64(44)
	detail := ExternalCampaignDetail{Campaign: CampaignSummary{ID: 77, Name: "Name", State: "ON"}, Ads: []ExternalAd{{ID: 13, CampaignID: 77, Type: "RESPONSIVE_AD", State: "ON", Titles: []string{"First", "Second"}, Texts: []string{"First text", "Second text"}, BusinessID: &business}, {ID: 12, CampaignID: 77, Type: "TEXT_AD", Title: "Headline", Text: "Body"}}}
	before, err := detail.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	reordered := detail
	reordered.Ads = append([]ExternalAd(nil), detail.Ads...)
	reordered.Ads[0], reordered.Ads[1] = reordered.Ads[1], reordered.Ads[0]
	reordered.Campaign.State = "OFF"
	reordered.Ads[0].Status = "MODERATION"
	after, err := reordered.Fingerprint()
	if err != nil || before != after {
		t.Fatal("provider response order or lifecycle invalidated creative", err)
	}
	destination := int64(45)
	reordered.Ads[1].BusinessID = &destination
	changed, err := reordered.Fingerprint()
	if err != nil || changed == before {
		t.Fatal("business destination missing from revision", err)
	}
	if detail.Ads[0].ID != 13 {
		t.Fatal("fingerprint reordered caller's snapshot")
	}
}
