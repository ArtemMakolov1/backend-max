package yandexdirect

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestExternalControlsReadAuthoritativeStrategyGroupsKeywordsAndCurrency(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Method string                     `json:"method"`
			Params map[string]json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.Method != "get" {
			t.Error("read performed a mutation")
		}
		result := ""
		switch {
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			result = `{"Campaigns":[{"Id":77,"Type":"UNIFIED_CAMPAIGN","State":"SUSPENDED","Currency":"RUB","UnifiedCampaign":{"BiddingStrategy":{"Search":{"BiddingStrategyType":"HIGHEST_POSITION","HighestPosition":{"WeeklySpendLimit":700000000}},"Network":{"BiddingStrategyType":"WB_MAXIMUM_CLICKS","WbMaximumClicks":{"WeeklySpendLimit":900000000,"BidCeiling":9900000,"UnknownFutureSetting":"keep"}}}}}]}`
		case strings.HasSuffix(r.URL.Path, "/dictionaries"):
			result = `{"Currencies":[{"Currency":"RUB","Properties":[{"Name":"MinimumBid","Value":"300000"},{"Name":"MaximumBid","Value":"25000000000"},{"Name":"BidIncrement","Value":"100000"},{"Name":"MinimumDailyBudget","Value":"300000000"},{"Name":"MinimumWeeklySpendLimit","Value":"300000000"}]}]}`
		case strings.HasSuffix(r.URL.Path, "/adgroups"):
			result = `{"AdGroups":[{"Id":13,"CampaignId":77,"Name":"Current group","Type":"UNIFIED_AD_GROUP","RegionIds":[1,-219],"NegativeKeywords":{"Items":["cheap"]}}]}`
		case strings.HasSuffix(r.URL.Path, "/keywords"):
			result = `{"Keywords":[{"Id":14,"CampaignId":77,"AdGroupId":13,"Keyword":"курс английского","State":"ON","Bid":800000,"ContextBid":900000,"AutotargetingSearchBidIsAuto":"NO"},{"Id":15,"CampaignId":77,"AdGroupId":13,"Keyword":"---autotargeting","State":"ON","Bid":800000,"AutotargetingSearchBidIsAuto":"YES"}]}`
		default:
			t.Errorf("unknown service %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"result":`+result+`}`)
	}))
	defer server.Close()
	client := externalControlsTestClient(t, server)
	controls, err := client.GetExternalCampaignControls(t.Context(), "token", "client", 77)
	if err != nil {
		t.Fatal(err)
	}
	if len(controls.Budgets) != 1 || controls.Budgets[0].AmountMicros != "900000000" || controls.Budgets[0].Branch != "WbMaximumClicks" || controls.CurrencyCode != "RUB" || !controls.StateActions.CanResume || controls.StateActions.CanPause {
		t.Fatalf("wrong strategy controls: %#v", controls)
	}
	keyword := controls.Groups[0].Keywords[0]
	if !reflect.DeepEqual(keyword.EditableFields, []string{"keyword", "search_bid_micros"}) || len(controls.Groups[0].Keywords[1].EditableFields) != 0 || controls.Spend.AmountMicros != nil {
		t.Fatal("automatic/unknown spend or bids were presented as editable")
	}
}

func TestExternalControlMutationsSendOnlyReviewedFieldsAndNeverReplay(t *testing.T) {
	var body map[string]any
	calls := 0
	var fail atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if fail.Load() {
			w.WriteHeader(503)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		_, _ = io.WriteString(w, `{"result":{"UpdateResults":[{"Id":77}]}}`)
	}))
	defer server.Close()
	client := externalControlsTestClient(t, server)
	controls := ExternalCampaignControls{CampaignType: "UNIFIED_CAMPAIGN", CurrencyCode: "RUB", Budgets: []ExternalBudget{{Key: "network_weekly", AmountMicros: "700000000", StrategyType: "WB_MAXIMUM_CLICKS", MinimumMicros: "300000000", IncrementMicros: "10000", Editable: true, Branch: "WbMaximumClicks"}}}
	if err := client.UpdateExternalBudget(t.Context(), "token", "client", 77, controls, "network_weekly", "900000000", "RUB"); err != nil {
		t.Fatal(err)
	}
	params := body["params"].(map[string]any)
	item := params["Campaigns"].([]any)[0].(map[string]any)
	strategy := item["UnifiedCampaign"].(map[string]any)["BiddingStrategy"].(map[string]any)
	if len(item) != 2 || len(strategy) != 1 || len(strategy["Network"].(map[string]any)) != 2 {
		t.Fatal("budget update changed unrelated campaign or strategy peers")
	}
	if err := client.UpdateExternalBudget(t.Context(), "token", "client", 77, controls, "network_weekly", "900000001", "RUB"); err == nil || calls != 1 {
		t.Fatal("invalid currency precision reached Direct")
	}
	controls.Budgets[0].Editable = false
	if err := client.UpdateExternalBudget(t.Context(), "token", "client", 77, controls, "network_weekly", "900000000", "RUB"); err == nil || calls != 1 {
		t.Fatal("unsupported strategy reached Direct")
	}
	fail.Store(true)
	if _, err := client.AddExternalKeyword(t.Context(), "token", "client", 13, "английский онлайн"); err == nil || calls != 2 {
		t.Fatal("ambiguous keyword create was retried")
	}
}

func TestExternalControlsRejectTruncatedOrForeignGroupBeforeEdits(t *testing.T) {
	for _, body := range []string{`{"AdGroups":[],"LimitedBy":1000}`, `{"AdGroups":[{"Id":13,"CampaignId":999,"Name":"foreign","Type":"TEXT_AD_GROUP","RegionIds":[1]}]}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"result":`+body+`}`) }))
		client := externalControlsTestClient(t, server)
		if _, err := client.externalGroups(t.Context(), "token", "client", 77); err == nil {
			t.Fatal("partial or foreign graph became an edit fence")
		}
		server.Close()
	}
}

func TestExternalMicrosRejectFloatOverflowAndValidateLargeExactAmount(t *testing.T) {
	for _, value := range []string{"1.5", "1e6", "+100000", "00100000", "9223372036854775808", "-1", " 100000"} {
		if _, err := ParseExternalMicros(value); err == nil {
			t.Fatalf("invalid amount accepted: %s", value)
		}
	}
	value := "9007199254740000"
	if n, err := ParseExternalMicros(value); err != nil || n != 9007199254740000 {
		t.Fatal("large exact amount was rounded")
	}
	if err := ValidateExternalAmount(value, "300000000", "", "10000"); err != nil {
		t.Fatal(err)
	}
}

func externalControlsTestClient(t *testing.T, server *httptest.Server) *Client {
	t.Helper()
	client, err := New(server.URL+"/json/v501", "client", "secret", CallbackRedirectURI, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestExternalKeywordEditAcceptsProviderReplacementIDWithoutRetry(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var request struct {
			Method string `json:"method"`
			Params struct {
				Keywords []struct {
					ID      int64  `json:"Id"`
					Keyword string `json:"Keyword"`
				} `json:"Keywords"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.Method != "update" || len(request.Params.Keywords) != 1 || request.Params.Keywords[0].ID != 14 || request.Params.Keywords[0].Keyword != "английский онлайн" {
			t.Error("unexpected keyword update")
		}
		_, _ = io.WriteString(w, `{"result":{"UpdateResults":[{"Id":99,"Warnings":[{"Code":103,"Message":"keyword recreated"}]}]}}`)
	}))
	defer server.Close()
	client := externalControlsTestClient(t, server)
	id, err := client.UpdateExternalKeyword(t.Context(), "token", "client", ExternalKeyword{ID: 14, AdGroupID: 13, Keyword: "курс английского"}, "английский онлайн")
	if err != nil || id != 99 || calls != 1 {
		t.Fatal("authoritative replacement ID rejected or retried", id, err, calls)
	}
}
func TestExternalGroupPhraseAndRegionLimitsRejectUnsupportedInputs(t *testing.T) {
	for _, regions := range [][]int64{{-1}, {0, -1}, {1, 1}, {}} {
		if err := (ExternalGroupChanges{RegionIDs: &regions}).Validate(); err == nil {
			t.Fatal("invalid geography accepted", regions)
		}
	}
	for _, phrases := range [][]string{{"-дешево"}, {"один два три четыре пять шесть семь восемь"}, {strings.Repeat("я", 36)}, {"[!+\"()]|"}} {
		if err := (ExternalGroupChanges{NegativeKeywords: &phrases}).Validate(); err == nil {
			t.Fatal("invalid group phrase accepted")
		}
	}
	phrases := make([]string, 130)
	for i := range phrases {
		phrases[i] = strings.Repeat("я", 35)
	}
	if err := (ExternalGroupChanges{NegativeKeywords: &phrases}).Validate(); err == nil {
		t.Fatal("group aggregate4096 exceeded")
	}
}

func TestExternalGroupPhraseAggregateMatchesProviderCharactersAndKeepsLargeListsReadable(t *testing.T) {
	phrases := make([]string, 4096)
	for i := range phrases {
		phrases[i] = "[!я]"
	}
	if err := (ExternalGroupChanges{NegativeKeywords: &phrases}).Validate(); err != nil {
		t.Fatal("valid provider aggregate counted operators or imposed a 200-entry limit", err)
	}
	phrases = append(phrases, "я")
	if err := (ExternalGroupChanges{NegativeKeywords: &phrases}).Validate(); err == nil {
		t.Fatal("aggregate greater than 4096 accepted")
	}
	phrases = make([]string, 512)
	for i := range phrases {
		phrases[i] = "[!я-+я] \"я(я|я)я\" я я"
	}
	if err := (ExternalGroupChanges{NegativeKeywords: &phrases}).Validate(); err != nil {
		t.Fatal("spaces, hyphens or operators counted in provider aggregate", err)
	}
	items, err := json.Marshal(phrases)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"result":{"AdGroups":[{"Id":13,"CampaignId":77,"Name":"Existing","Type":"TEXT_AD_GROUP","RegionIds":[0],"NegativeKeywords":{"Items":`+string(items)+`}}]}}`)
	}))
	defer server.Close()
	groups, err := externalControlsTestClient(t, server).externalGroups(t.Context(), "token", "client", 77)
	if err != nil || len(groups) != 1 || len(groups[0].NegativeKeywords) != len(phrases) {
		t.Fatal("existing provider list became unreadable", err)
	}
}
