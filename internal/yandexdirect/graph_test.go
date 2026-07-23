package yandexdirect

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestResolveRegionNamesUsesExactNamesAndPreservesOrder(t *testing.T) {
	t.Parallel()
	client, closeServer := newGraphTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/v501/dictionaries" {
			t.Errorf("path = %q", r.URL.Path)
		}
		var request struct {
			Method string `json:"method"`
			Params struct {
				SelectionCriteria struct {
					ExactNames []string `json:"ExactNames"`
				} `json:"SelectionCriteria"`
				FieldNames []string `json:"FieldNames"`
				Page       struct {
					Limit  int64 `json:"Limit"`
					Offset int64 `json:"Offset"`
				} `json:"Page"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if request.Method != "getGeoRegions" ||
			!reflect.DeepEqual(request.Params.SelectionCriteria.ExactNames,
				[]string{"Москва", "Санкт-Петербург"}) {
			t.Errorf("request = %#v", request)
		}
		if !reflect.DeepEqual(request.Params.FieldNames,
			[]string{"GeoRegionId", "GeoRegionName", "ParentGeoRegionNames"}) ||
			request.Params.Page.Limit != 1000 || request.Params.Page.Offset != 0 {
			t.Errorf("params = %#v", request.Params)
		}
		_, _ = fmt.Fprint(w, `{"result":{"GeoRegions":[
{"GeoRegionId":2,"GeoRegionName":"Санкт-Петербург",
 "ParentGeoRegionNames":{"Items":["Россия","Северо-Западный федеральный округ"]}},
{"GeoRegionId":213,"GeoRegionName":"Москва",
 "ParentGeoRegionNames":{"Items":["Россия","Центральный федеральный округ"]}}
]}}`)
	})
	defer closeServer()

	regions, err := client.ResolveRegionNames(
		context.Background(), "token", "login",
		[]string{" Москва ", "Санкт-Петербург"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := []int64{regions[0].ID, regions[1].ID}; !reflect.DeepEqual(got, []int64{213, 2}) {
		t.Fatalf("region IDs = %v", got)
	}
	if !reflect.DeepEqual(regions[0].ParentNames,
		[]string{"Россия", "Центральный федеральный округ"}) {
		t.Fatalf("parents = %#v", regions[0].ParentNames)
	}
}

func TestResolveRegionNamesFailsOnAmbiguousOrMissingExactMatch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		response string
		wantCode string
	}{
		{
			name: "ambiguous",
			response: `{"result":{"GeoRegions":[
{"GeoRegionId":10,"GeoRegionName":"Киров","ParentGeoRegionNames":{"Items":["A"]}},
{"GeoRegionId":11,"GeoRegionName":"Киров","ParentGeoRegionNames":{"Items":["B"]}}
]}}`,
			wantCode: "region_name_ambiguous",
		},
		{
			name:     "missing",
			response: `{"result":{"GeoRegions":[]}}`,
			wantCode: "region_name_not_found",
		},
		{
			name: "truncated",
			response: `{"result":{"LimitedBy":1,"GeoRegions":[
{"GeoRegionId":10,"GeoRegionName":"Киров","ParentGeoRegionNames":null}
]}}`,
			wantCode: "region_resolution_incomplete",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			client, closeServer := newGraphTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprint(w, test.response)
			})
			defer closeServer()
			_, err := client.ResolveRegionNames(
				context.Background(), "token", "", []string{"Киров"},
			)
			requireGraphErrorCode(t, err, test.wantCode)
		})
	}
}

func TestCreateUnifiedAdGroupUsesCurrentV501ShapeAndPreservesWarnings(t *testing.T) {
	t.Parallel()
	client, closeServer := newGraphTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/v501/adgroups" {
			t.Errorf("path = %q", r.URL.Path)
		}
		var payload map[string]any
		decoder := json.NewDecoder(r.Body)
		decoder.UseNumber()
		if err := decoder.Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		if payload["method"] != "add" {
			t.Errorf("method = %#v", payload["method"])
		}
		params := payload["params"].(map[string]any)
		group := params["AdGroups"].([]any)[0].(map[string]any)
		if group["CampaignId"].(json.Number).String() != "7001" ||
			group["Name"] != UnifiedAdGroupOperationName("op_abc-123") {
			t.Errorf("group = %#v", group)
		}
		if _, exists := group["TrackingParams"]; exists {
			t.Error("unsupported TrackingParams was sent for UnifiedAdGroup")
		}
		if got := jsonNumbers(group["RegionIds"].([]any)); !reflect.DeepEqual(got, []string{"-219", "225"}) {
			t.Errorf("region IDs = %v", got)
		}
		negative := group["NegativeKeywords"].(map[string]any)["Items"].([]any)
		if !reflect.DeepEqual(negative, []any{"бесплатно", "скачать"}) {
			t.Errorf("negative keywords = %#v", negative)
		}
		unified := group["UnifiedAdGroup"].(map[string]any)
		if !reflect.DeepEqual(unified, map[string]any{"OfferRetargeting": "NO"}) {
			t.Errorf("UnifiedAdGroup = %#v", unified)
		}
		_, _ = fmt.Fprint(w, `{"result":{"AddResults":[{
"Id":7101,"Warnings":[{"Code":100,"Message":"warning","Details":"normalized"}]
}]}}`)
	})
	defer closeServer()

	result, err := client.CreateUnifiedAdGroup(
		context.Background(), "token", "login", UnifiedAdGroupDraft{
			CampaignID:       7001,
			Name:             " Основная группа ",
			RegionIDs:        []int64{225, -219},
			NegativeKeywords: []string{"скачать", "бесплатно"},
			TrackingMarker:   "op_abc-123",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != 7101 || len(result.Warnings) != 1 ||
		result.Warnings[0].Code != 100 || result.Warnings[0].Details != "normalized" {
		t.Fatalf("result = %#v", result)
	}
}

func TestUnifiedAdGroupValidationFailsBeforeProviderCall(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	client, closeServer := newGraphTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = fmt.Fprint(w, `{"result":{"AddResults":[{"Id":1}]}}`)
	})
	defer closeServer()
	tests := []UnifiedAdGroupDraft{
		{CampaignID: 0, Name: "x", RegionIDs: []int64{225}, TrackingMarker: "m"},
		{CampaignID: 1, Name: "", RegionIDs: []int64{225}, TrackingMarker: "m"},
		{CampaignID: 1, Name: "x", RegionIDs: []int64{0, 225}, TrackingMarker: "m"},
		{CampaignID: 1, Name: "x", RegionIDs: []int64{-225}, TrackingMarker: "m"},
		{CampaignID: 1, Name: "x", RegionIDs: []int64{225}, TrackingMarker: "bad marker"},
		{
			CampaignID: 1, Name: "x", RegionIDs: []int64{225}, TrackingMarker: "m",
			NegativeKeywords: []string{"-неверно"},
		},
	}
	for _, draft := range tests {
		if _, err := client.CreateUnifiedAdGroup(
			context.Background(), "token", "", draft,
		); err == nil {
			t.Fatalf("invalid draft accepted: %#v", draft)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("provider calls = %d", calls.Load())
	}
}

func TestCreateResponsiveAdUsesResponsiveShapeAndNormalizesHTTPSURL(t *testing.T) {
	t.Parallel()
	client, closeServer := newGraphTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/v501/ads" {
			t.Errorf("path = %q", r.URL.Path)
		}
		var payload map[string]any
		decoder := json.NewDecoder(r.Body)
		decoder.UseNumber()
		if err := decoder.Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		params := payload["params"].(map[string]any)
		ad := params["Ads"].([]any)[0].(map[string]any)
		if _, exists := ad["TextAd"]; exists {
			t.Error("legacy TextAd was sent")
		}
		responsive := ad["ResponsiveAd"].(map[string]any)
		if ad["AdGroupId"].(json.Number).String() != "7101" ||
			responsive["Href"] != "https://example.com/path?a=1&b=2" ||
			!reflect.DeepEqual(responsive["Titles"], []any{"Первый заголовок", "Второй"}) ||
			!reflect.DeepEqual(responsive["Texts"], []any{"Первый текст"}) {
			t.Errorf("ad = %#v", ad)
		}
		_, _ = fmt.Fprint(w, `{"result":{"AddResults":[{"Id":7201}]}}`)
	})
	defer closeServer()

	result, err := client.CreateResponsiveAd(
		context.Background(), "token", "", ResponsiveAdDraft{
			AdGroupID: 7101,
			Titles:    []string{" Первый заголовок ", "Второй"},
			Texts:     []string{"Первый текст"},
			Href:      "HTTPS://Example.COM:443/path?b=2&a=1",
		},
	)
	if err != nil || result.ID != 7201 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestResponsiveAdValidationCoversCountsWordsHTTPSAndIDs(t *testing.T) {
	t.Parallel()
	valid := ResponsiveAdDraft{
		AdGroupID: 1, Titles: []string{"Заголовок"}, Texts: []string{"Текст"},
		Href: "https://example.com",
	}
	tests := []ResponsiveAdDraft{
		func() ResponsiveAdDraft { value := valid; value.AdGroupID = 0; return value }(),
		func() ResponsiveAdDraft { value := valid; value.Titles = nil; return value }(),
		func() ResponsiveAdDraft {
			value := valid
			value.Titles = []string{"1", "2", "3", "4", "5", "6", "7", "8"}
			return value
		}(),
		func() ResponsiveAdDraft {
			value := valid
			value.Titles = []string{strings.Repeat("я", maxResponsiveTitleWord+1)}
			return value
		}(),
		func() ResponsiveAdDraft { value := valid; value.Texts = nil; return value }(),
		func() ResponsiveAdDraft {
			value := valid
			value.Texts = []string{strings.Repeat("я", maxResponsiveTextRunes+1)}
			return value
		}(),
		func() ResponsiveAdDraft { value := valid; value.Href = "http://example.com"; return value }(),
		func() ResponsiveAdDraft { value := valid; value.Href = "https://user:pass@example.com"; return value }(),
	}
	for _, draft := range tests {
		if _, err := normalizeResponsiveAdDraft(draft); err == nil {
			t.Fatalf("invalid draft accepted: %#v", draft)
		}
	}
}

func TestAddKeywordsPreservesSuccessWarningsAndPartialErrors(t *testing.T) {
	t.Parallel()
	client, closeServer := newGraphTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		decoder := json.NewDecoder(r.Body)
		decoder.UseNumber()
		if err := decoder.Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		params := payload["params"].(map[string]any)
		keywords := params["Keywords"].([]any)
		if len(keywords) != 2 {
			t.Fatalf("keywords = %#v", keywords)
		}
		for _, raw := range keywords {
			item := raw.(map[string]any)
			if item["StrategyPriority"] != "NORMAL" {
				t.Errorf("priority = %#v", item)
			}
			if _, exists := item["Bid"]; exists {
				t.Error("automatic strategy keyword unexpectedly contains Bid")
			}
			if _, exists := item["ContextBid"]; exists {
				t.Error("automatic strategy keyword unexpectedly contains ContextBid")
			}
		}
		_, _ = fmt.Fprint(w, `{"result":{"AddResults":[
{"Id":7301,"Warnings":[{"Code":10,"Message":"saved","Details":"normalized"}]},
{"Errors":[{"Code":5000,"Message":"invalid","Details":"duplicate"}]}
]}}`)
	})
	defer closeServer()

	results, err := client.AddKeywords(context.Background(), "token", "", []KeywordDraft{
		{AdGroupID: 7101, Keyword: "купить сервис"},
		{AdGroupID: 7101, Keyword: "ведение канала"},
	})
	var partial *PartialMutationError
	if !errors.As(err, &partial) {
		t.Fatalf("error = %v, want *PartialMutationError", err)
	}
	if len(results) != 2 || results[0].ID != 7301 ||
		len(results[0].Warnings) != 1 || results[1].Errors[0].Code != 5000 {
		t.Fatalf("results = %#v", results)
	}
	if !reflect.DeepEqual(partial.Results, results) {
		t.Fatalf("partial results = %#v, want %#v", partial.Results, results)
	}
}

func TestKeywordValidationRejectsImplicitAutotargetingAndBadWords(t *testing.T) {
	t.Parallel()
	tests := [][]KeywordDraft{
		nil,
		{{AdGroupID: 0, Keyword: "фраза"}},
		{{AdGroupID: 1, Keyword: "---autotargeting"}},
		{{AdGroupID: 1, Keyword: strings.Repeat("я", maxKeywordWordRunes+1)}},
		{{AdGroupID: 1, Keyword: "один два три четыре пять шесть семь восемь"}},
		{{AdGroupID: 1, Keyword: "фраза"}, {AdGroupID: 1, Keyword: " ФРАЗА "}},
	}
	for _, drafts := range tests {
		if _, err := normalizeKeywordDrafts(drafts); err == nil {
			t.Fatalf("invalid keywords accepted: %#v", drafts)
		}
	}
}

func TestModerateAdsUsesIdsCriteriaAndValidatesResponseOrder(t *testing.T) {
	t.Parallel()
	client, closeServer := newGraphTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Method string `json:"method"`
			Params struct {
				SelectionCriteria struct {
					IDs []int64 `json:"Ids"`
				} `json:"SelectionCriteria"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if request.Method != "moderate" ||
			!reflect.DeepEqual(request.Params.SelectionCriteria.IDs, []int64{7201, 7202}) {
			t.Errorf("request = %#v", request)
		}
		_, _ = fmt.Fprint(w, `{"result":{"ModerateResults":[
{"Id":7201},{"Id":7202,"Warnings":[{"Code":1,"Message":"queued"}]}
]}}`)
	})
	defer closeServer()
	results, err := client.ModerateAds(
		context.Background(), "token", "", []int64{7201, 7202},
	)
	if err != nil || len(results) != 2 || results[1].Warnings[0].Code != 1 {
		t.Fatalf("results=%#v err=%v", results, err)
	}
	if _, err := client.ModerateAds(
		context.Background(), "token", "", []int64{7201, 7201},
	); err == nil {
		t.Fatal("duplicate moderation ID was accepted")
	}
}

func TestReconcileCampaignMarkerPaginatesAndReturnsExactMatch(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	client, closeServer := newGraphTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Params struct {
				SelectionCriteria struct {
					Types []string `json:"Types"`
				} `json:"SelectionCriteria"`
				UnifiedCampaignFieldNames []string `json:"UnifiedCampaignFieldNames"`
				Page                      struct {
					Limit  int64 `json:"Limit"`
					Offset int64 `json:"Offset"`
				} `json:"Page"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if !reflect.DeepEqual(request.Params.SelectionCriteria.Types,
			[]string{"UNIFIED_CAMPAIGN"}) ||
			!reflect.DeepEqual(request.Params.UnifiedCampaignFieldNames,
				[]string{"TrackingParams"}) ||
			request.Params.Page.Limit != 1000 {
			t.Errorf("request = %#v", request)
		}
		switch calls.Add(1) {
		case 1:
			if request.Params.Page.Offset != 0 {
				t.Errorf("first offset = %d", request.Params.Page.Offset)
			}
			_, _ = fmt.Fprint(w, `{"result":{"LimitedBy":1,"Campaigns":[{
"Id":10,"Type":"UNIFIED_CAMPAIGN","UnifiedCampaign":{"TrackingParams":"utm_source=yandex"}
}]}}`)
		case 2:
			if request.Params.Page.Offset != 1 {
				t.Errorf("second offset = %d", request.Params.Page.Offset)
			}
			_, _ = fmt.Fprint(w, `{"result":{"Campaigns":[{
"Id":11,"Type":"UNIFIED_CAMPAIGN",
"UnifiedCampaign":{"TrackingParams":"utm_source=yandex&mp_op=op_123"}
}]}}`)
		default:
			t.Fatalf("unexpected request %d", calls.Load())
		}
	})
	defer closeServer()

	id, err := client.FindUnifiedCampaignByOperationMarker(
		context.Background(), "token", "login", "op_123",
	)
	if err != nil || id != 11 || calls.Load() != 2 {
		t.Fatalf("id=%d calls=%d err=%v", id, calls.Load(), err)
	}
}

func TestReconcileCampaignMarkerFailsOnAmbiguityDuplicatesAndUnknownObjects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		response string
		wantCode string
	}{
		{
			name: "ambiguous marker",
			response: `{"result":{"Campaigns":[
{"Id":1,"Type":"UNIFIED_CAMPAIGN","UnifiedCampaign":{"TrackingParams":"mp_op=op_1"}},
{"Id":2,"Type":"UNIFIED_CAMPAIGN","UnifiedCampaign":{"TrackingParams":"mp_op=op_1"}}
]}}`,
			wantCode: "ambiguous_campaign_operation_marker",
		},
		{
			name: "duplicate provider object",
			response: `{"result":{"Campaigns":[
{"Id":1,"Type":"UNIFIED_CAMPAIGN","UnifiedCampaign":{"TrackingParams":""}},
{"Id":1,"Type":"UNIFIED_CAMPAIGN","UnifiedCampaign":{"TrackingParams":""}}
]}}`,
			wantCode: "duplicate_campaign_reconcile_response",
		},
		{
			name: "unknown object",
			response: `{"result":{"Campaigns":[
{"Id":1,"Type":"FUTURE_CAMPAIGN","UnifiedCampaign":{"TrackingParams":"mp_op=op_1"}}
]}}`,
			wantCode: "unsupported_campaign_in_reconcile",
		},
		{
			name: "duplicate marker parameter",
			response: `{"result":{"Campaigns":[
{"Id":1,"Type":"UNIFIED_CAMPAIGN","UnifiedCampaign":{"TrackingParams":"mp_op=op_1&mp_op=op_1"}}
]}}`,
			wantCode: "invalid_campaign_reconcile_tracking_params",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			client, closeServer := newGraphTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprint(w, test.response)
			})
			defer closeServer()
			_, err := client.FindUnifiedCampaignByOperationMarker(
				context.Background(), "token", "", "op_1",
			)
			requireGraphErrorCode(t, err, test.wantCode)
		})
	}
}

func TestReconcileAdGroupMarkerPaginatesAndFailsOnAmbiguity(t *testing.T) {
	t.Parallel()
	t.Run("pagination", func(t *testing.T) {
		t.Parallel()
		var calls atomic.Int32
		client, closeServer := newGraphTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			var request struct {
				Params struct {
					SelectionCriteria struct {
						CampaignIDs []int64 `json:"CampaignIds"`
					} `json:"SelectionCriteria"`
					Page struct {
						Offset int64 `json:"Offset"`
					} `json:"Page"`
				} `json:"params"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
				return
			}
			if !reflect.DeepEqual(request.Params.SelectionCriteria.CampaignIDs, []int64{7}) {
				t.Errorf("criteria = %#v", request.Params.SelectionCriteria)
			}
			switch calls.Add(1) {
			case 1:
				_, _ = fmt.Fprint(w, `{"result":{"LimitedBy":4,"AdGroups":[{
"Id":8,"CampaignId":7,"Type":"UNIFIED_AD_GROUP","Name":"other"
}]}}`)
			case 2:
				if request.Params.Page.Offset != 4 {
					t.Errorf("second offset = %d", request.Params.Page.Offset)
				}
				_, _ = fmt.Fprint(w, `{"result":{"AdGroups":[{
"Id":9,"CampaignId":7,"Type":"UNIFIED_AD_GROUP","Name":"MaxPosty · group_1"
}]}}`)
			default:
				t.Fatalf("unexpected request %d", calls.Load())
			}
		})
		defer closeServer()
		id, err := client.FindUnifiedAdGroupByTrackingMarker(
			context.Background(), "token", "", 7, "group_1",
		)
		if err != nil || id != 9 || calls.Load() != 2 {
			t.Fatalf("id=%d calls=%d err=%v", id, calls.Load(), err)
		}
	})

	t.Run("ambiguous", func(t *testing.T) {
		t.Parallel()
		client, closeServer := newGraphTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, `{"result":{"AdGroups":[
{"Id":8,"CampaignId":7,"Type":"UNIFIED_AD_GROUP","Name":"MaxPosty · group_1"},
{"Id":9,"CampaignId":7,"Type":"UNIFIED_AD_GROUP","Name":"MaxPosty · group_1"}
]}}`)
		})
		defer closeServer()
		_, err := client.FindUnifiedAdGroupByTrackingMarker(
			context.Background(), "token", "", 7, "group_1",
		)
		requireGraphErrorCode(t, err, "ambiguous_adgroup_operation_name")
	})
}

func TestEnsureNoBidModifiersUsesBothLevelsAndFailsOnAnyObject(t *testing.T) {
	t.Parallel()
	t.Run("empty paginated result", func(t *testing.T) {
		t.Parallel()
		var calls atomic.Int32
		client, closeServer := newGraphTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/json/v501/bidmodifiers" {
				t.Errorf("path = %q", r.URL.Path)
			}
			var request struct {
				Params struct {
					SelectionCriteria struct {
						CampaignIDs []int64  `json:"CampaignIds"`
						Levels      []string `json:"Levels"`
					} `json:"SelectionCriteria"`
					FieldNames []string `json:"FieldNames"`
					Page       struct {
						Offset int64 `json:"Offset"`
					} `json:"Page"`
				} `json:"params"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
				return
			}
			if !reflect.DeepEqual(request.Params.SelectionCriteria.CampaignIDs, []int64{7}) ||
				!reflect.DeepEqual(request.Params.SelectionCriteria.Levels,
					[]string{"CAMPAIGN", "AD_GROUP"}) {
				t.Errorf("criteria = %#v", request.Params.SelectionCriteria)
			}
			requireContainsAll(t, request.Params.FieldNames,
				[]string{"Id", "CampaignId", "AdGroupId", "Level", "Type"})
			switch calls.Add(1) {
			case 1:
				_, _ = fmt.Fprint(w,
					`{"result":{"LimitedBy":1,"BidModifiers":[]}}`)
			case 2:
				if request.Params.Page.Offset != 1 {
					t.Errorf("second offset = %d", request.Params.Page.Offset)
				}
				_, _ = fmt.Fprint(w, `{"result":{"BidModifiers":[]}}`)
			default:
				t.Fatalf("unexpected request %d", calls.Load())
			}
		})
		defer closeServer()
		if err := client.EnsureNoBidModifiers(
			context.Background(), "token", "", 7,
		); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("unknown modifier still blocks", func(t *testing.T) {
		t.Parallel()
		client, closeServer := newGraphTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, `{"result":{"BidModifiers":[{
"Id":1,"CampaignId":7,"Level":"CAMPAIGN","Type":"FUTURE_ADJUSTMENT"
}]}}`)
		})
		defer closeServer()
		err := client.EnsureNoBidModifiers(context.Background(), "token", "", 7)
		requireGraphErrorCode(t, err, "unsupported_bid_modifiers")
	})
}

func TestEnsureNoAudienceTargetsFailsClosedOnAnyObject(t *testing.T) {
	t.Parallel()
	t.Run("empty paginated result", func(t *testing.T) {
		t.Parallel()
		var calls atomic.Int32
		client, closeServer := newGraphTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/json/v501/audiencetargets" {
				t.Errorf("path = %q", r.URL.Path)
			}
			var request struct {
				Params struct {
					SelectionCriteria struct {
						CampaignIDs []int64 `json:"CampaignIds"`
					} `json:"SelectionCriteria"`
					FieldNames []string `json:"FieldNames"`
					Page       struct {
						Offset int64 `json:"Offset"`
					} `json:"Page"`
				} `json:"params"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
				return
			}
			if !reflect.DeepEqual(request.Params.SelectionCriteria.CampaignIDs, []int64{7}) {
				t.Errorf("criteria = %#v", request.Params.SelectionCriteria)
			}
			requireContainsAll(t, request.Params.FieldNames, []string{
				"Id", "AdGroupId", "CampaignId", "RetargetingListId",
				"InterestId", "ContextBid", "StrategyPriority", "State",
			})
			switch calls.Add(1) {
			case 1:
				_, _ = fmt.Fprint(w,
					`{"result":{"LimitedBy":1,"AudienceTargets":[]}}`)
			case 2:
				if request.Params.Page.Offset != 1 {
					t.Errorf("second offset = %d", request.Params.Page.Offset)
				}
				_, _ = fmt.Fprint(w, `{"result":{"AudienceTargets":[]}}`)
			default:
				t.Fatalf("unexpected request %d", calls.Load())
			}
		})
		defer closeServer()
		if err := client.EnsureNoAudienceTargets(
			context.Background(), "token", "", 7,
		); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("unknown target still blocks", func(t *testing.T) {
		t.Parallel()
		client, closeServer := newGraphTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, `{"result":{"AudienceTargets":[{
"Id":1,"CampaignId":7,"AdGroupId":8,"FutureTarget":{"Kind":"UNKNOWN"}
}]}}`)
		})
		defer closeServer()
		err := client.EnsureNoAudienceTargets(context.Background(), "token", "", 7)
		requireGraphErrorCode(t, err, "unsupported_audience_targets")
	})

	t.Run("missing collection is incomplete response", func(t *testing.T) {
		t.Parallel()
		client, closeServer := newGraphTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, `{"result":{}}`)
		})
		defer closeServer()
		err := client.EnsureNoAudienceTargets(context.Background(), "token", "", 7)
		requireGraphErrorCode(t, err, "invalid_audience_targets_response")
	})
}

func TestListKeywordsRejectsHrefUserParameters(t *testing.T) {
	t.Parallel()
	client, closeServer := newGraphTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"result":{"Keywords":[{
"Id":91,"CampaignId":7,"AdGroupId":8,"Keyword":"max posty",
"UserParam1":"redirect","UserParam2":"","StrategyPriority":"NORMAL",
"Status":"ACCEPTED","State":"ON","ServingStatus":"ELIGIBLE"
}]}}`)
	})
	defer closeServer()
	_, err := client.ListKeywords(context.Background(), "token", "", 7)
	requireGraphErrorCode(t, err, "unsupported_keyword_user_params")
}

func TestUpdateUnifiedCampaignsUsesNarrowSnapshotAndPreservesPartialResults(t *testing.T) {
	t.Parallel()
	fixture := graphCampaignFixture(
		1, "Campaign", 30_000,
		time.Date(2044, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2044, 2, 1, 0, 0, 0, 0, time.UTC),
	)
	client, closeServer := newGraphTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/v501/campaigns" {
			t.Errorf("path = %q", r.URL.Path)
		}
		var payload map[string]any
		decoder := json.NewDecoder(r.Body)
		decoder.UseNumber()
		if err := decoder.Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		if payload["method"] != "update" {
			t.Errorf("method = %#v", payload["method"])
		}
		items := payload["params"].(map[string]any)["Campaigns"].([]any)
		if len(items) != 2 {
			t.Fatalf("campaigns = %#v", items)
		}
		first := items[0].(map[string]any)
		if first["Id"].(json.Number).String() != "1" ||
			first["Name"] != "Campaign A" ||
			first["StartDate"] != "2044-01-01" ||
			first["EndDate"] != "2044-02-01" {
			t.Errorf("campaign = %#v", first)
		}
		for _, forbidden := range []string{
			"DailyBudget", "TimeZone", "TimeTargeting", "NegativeKeywords",
			"BlockedIps", "ExcludedSites", "Notification", "ClientInfo",
		} {
			if _, exists := first[forbidden]; exists {
				t.Errorf("forbidden campaign field %q was sent", forbidden)
			}
		}
		unified := first["UnifiedCampaign"].(map[string]any)
		if unified["TrackingParams"] != "mp_op=op_1" {
			t.Errorf("unified = %#v", unified)
		}
		if _, exists := unified["PackageBiddingStrategy"]; exists {
			t.Error("package strategy membership was sent")
		}
		strategy := unified["BiddingStrategy"].(map[string]any)
		network := strategy["Network"].(map[string]any)
		spend := network["WbMaximumClicks"].(map[string]any)["WeeklySpendLimit"]
		if spend.(json.Number).String() != "300000000" {
			t.Errorf("weekly spend = %#v", spend)
		}
		_, _ = fmt.Fprint(w, `{"result":{"UpdateResults":[
{"Id":1,"Warnings":[{"Code":10,"Message":"saved","Details":"normalized"}]},
{"Errors":[{"Code":5000,"Message":"invalid","Details":"locked"}]}
]}}`)
	})
	defer closeServer()

	updates := []UnifiedCampaignUpdate{
		{
			ID: 1, Name: " Campaign A ", WeeklyBudgetMinor: 30_000,
			StartsAt: fixture.StartsAt, EndsAt: fixture.EndsAt,
			BiddingStrategy: fixture.BiddingStrategy, Settings: fixture.Settings,
			TrackingParams: "mp_op=op_1",
		},
		{
			ID: 2, Name: "Campaign B", WeeklyBudgetMinor: 30_000,
			StartsAt: fixture.StartsAt, EndsAt: fixture.EndsAt,
			BiddingStrategy: fixture.BiddingStrategy, Settings: fixture.Settings,
			TrackingParams: "mp_op=op_2",
		},
	}
	results, err := client.UpdateUnifiedCampaigns(
		context.Background(), "token", "", updates,
	)
	var partial *PartialMutationError
	if !errors.As(err, &partial) {
		t.Fatalf("error = %v, want partial", err)
	}
	if len(results) != 2 || results[0].ID != 1 ||
		len(results[0].Warnings) != 1 || results[1].Errors[0].Code != 5000 ||
		!reflect.DeepEqual(partial.Results, results) {
		t.Fatalf("results = %#v partial = %#v", results, partial)
	}
}

func TestUpdateUnifiedAdGroupsUsesOnlyManagedFields(t *testing.T) {
	t.Parallel()
	client, closeServer := newGraphTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		decoder := json.NewDecoder(r.Body)
		decoder.UseNumber()
		if err := decoder.Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		item := payload["params"].(map[string]any)["AdGroups"].([]any)[0].(map[string]any)
		if item["Id"].(json.Number).String() != "10" ||
			item["Name"] != UnifiedAdGroupOperationName("group_1") ||
			!reflect.DeepEqual(jsonNumbers(item["RegionIds"].([]any)), []string{"-219", "225"}) ||
			!reflect.DeepEqual(item["NegativeKeywords"].(map[string]any)["Items"],
				[]any{"free", "spam"}) {
			t.Errorf("group = %#v", item)
		}
		if _, exists := item["TrackingParams"]; exists {
			t.Error("unsupported TrackingParams was sent for UnifiedAdGroup")
		}
		if _, exists := item["UnifiedAdGroup"]; exists {
			t.Error("OfferRetargeting was unexpectedly changed")
		}
		_, _ = fmt.Fprint(w, `{"result":{"UpdateResults":[{
"Id":10,"Warnings":[{"Code":1,"Message":"saved"}]
}]}}`)
	})
	defer closeServer()
	results, err := client.UpdateUnifiedAdGroups(
		context.Background(), "token", "", []UnifiedAdGroupUpdate{{
			ID: 10, Name: " Group ", RegionIDs: []int64{225, -219},
			NegativeKeywords: []string{"spam", "free"}, TrackingMarker: "group_1",
		}},
	)
	if err != nil || len(results) != 1 || results[0].ID != 10 ||
		len(results[0].Warnings) != 1 {
		t.Fatalf("results=%#v err=%v", results, err)
	}
}

func TestUpdateResponsiveAdsUsesOnlySupportedCreativeFields(t *testing.T) {
	t.Parallel()
	client, closeServer := newGraphTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		decoder := json.NewDecoder(r.Body)
		decoder.UseNumber()
		if err := decoder.Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		item := payload["params"].(map[string]any)["Ads"].([]any)[0].(map[string]any)
		responsive := item["ResponsiveAd"].(map[string]any)
		if item["Id"].(json.Number).String() != "20" ||
			!reflect.DeepEqual(responsive["Titles"], []any{"Title 1", "Title 2"}) ||
			!reflect.DeepEqual(responsive["Texts"], []any{"Text"}) ||
			responsive["Href"] != "https://example.com/path?a=1&b=2" {
			t.Errorf("ad = %#v", item)
		}
		if len(responsive) != 3 {
			t.Errorf("unsupported responsive fields were sent: %#v", responsive)
		}
		_, _ = fmt.Fprint(w, `{"result":{"UpdateResults":[{"Id":20}]}}`)
	})
	defer closeServer()
	results, err := client.UpdateResponsiveAds(
		context.Background(), "token", "", []ResponsiveAdUpdate{{
			ID: 20, Titles: []string{" Title 1 ", "Title 2"}, Texts: []string{"Text"},
			Href: "HTTPS://EXAMPLE.COM:443/path?b=2&a=1",
		}},
	)
	if err != nil || len(results) != 1 || results[0].ID != 20 {
		t.Fatalf("results=%#v err=%v", results, err)
	}
}

func TestUpdateAndDeleteKeywordsPreserveProviderResults(t *testing.T) {
	t.Parallel()
	t.Run("update may replace ID", func(t *testing.T) {
		t.Parallel()
		client, closeServer := newGraphTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			var payload map[string]any
			decoder := json.NewDecoder(r.Body)
			decoder.UseNumber()
			if err := decoder.Decode(&payload); err != nil {
				t.Error(err)
				return
			}
			item := payload["params"].(map[string]any)["Keywords"].([]any)[0].(map[string]any)
			if item["Id"].(json.Number).String() != "30" ||
				item["Keyword"] != "new phrase" || len(item) != 2 {
				t.Errorf("keyword = %#v", item)
			}
			_, _ = fmt.Fprint(w, `{"result":{"UpdateResults":[{
"Id":31,"Warnings":[{"Code":2,"Message":"replacement created"}]
}]}}`)
		})
		defer closeServer()
		results, err := client.UpdateKeywords(
			context.Background(), "token", "", []KeywordUpdate{{
				ID: 30, Keyword: " new phrase ",
			}},
		)
		if err != nil || results[0].ID != 31 || len(results[0].Warnings) != 1 {
			t.Fatalf("results=%#v err=%v", results, err)
		}
	})

	t.Run("delete partial", func(t *testing.T) {
		t.Parallel()
		client, closeServer := newGraphTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			var request struct {
				Method string `json:"method"`
				Params struct {
					SelectionCriteria struct {
						IDs []int64 `json:"Ids"`
					} `json:"SelectionCriteria"`
				} `json:"params"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
				return
			}
			if request.Method != "delete" ||
				!reflect.DeepEqual(request.Params.SelectionCriteria.IDs, []int64{40, 41}) {
				t.Errorf("request = %#v", request)
			}
			_, _ = fmt.Fprint(w, `{"result":{"DeleteResults":[
{"Id":40},{"Errors":[{"Code":3,"Message":"not found"}]}
]}}`)
		})
		defer closeServer()
		results, err := client.DeleteKeywords(
			context.Background(), "token", "", []int64{40, 41},
		)
		var partial *PartialMutationError
		if !errors.As(err, &partial) || len(results) != 2 ||
			results[0].ID != 40 || results[1].Errors[0].Code != 3 {
			t.Fatalf("results=%#v partial=%#v err=%v", results, partial, err)
		}
	})
}

func TestUpdatePrimitiveValidationFailsBeforeProviderCall(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	client, closeServer := newGraphTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = fmt.Fprint(w, `{"result":{"UpdateResults":[]}}`)
	})
	defer closeServer()
	fixture := graphCampaignFixture(
		1, "x", 30_000,
		time.Date(2044, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2044, 2, 1, 0, 0, 0, 0, time.UTC),
	)
	_, _ = client.UpdateUnifiedCampaigns(context.Background(), "token", "",
		[]UnifiedCampaignUpdate{{
			ID: 1, Name: "x", WeeklyBudgetMinor: 1,
			StartsAt: fixture.StartsAt, EndsAt: fixture.EndsAt,
			BiddingStrategy: fixture.BiddingStrategy,
		}})
	_, _ = client.UpdateUnifiedAdGroups(context.Background(), "token", "",
		[]UnifiedAdGroupUpdate{{ID: 1, Name: "x", RegionIDs: []int64{-1}, TrackingMarker: "m"}})
	_, _ = client.UpdateResponsiveAds(context.Background(), "token", "",
		[]ResponsiveAdUpdate{{ID: 1, Titles: nil, Texts: []string{"x"}, Href: "https://example.com"}})
	_, _ = client.UpdateKeywords(context.Background(), "token", "",
		[]KeywordUpdate{{ID: 1, Keyword: "---autotargeting"}})
	if calls.Load() != 0 {
		t.Fatalf("provider calls = %d", calls.Load())
	}
}

func TestListGraphPrimitivesNormalizeAndRejectUnsupportedTypes(t *testing.T) {
	t.Parallel()
	t.Run("normal graph", func(t *testing.T) {
		t.Parallel()
		client, closeServer := newGraphTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/json/v501/adgroups":
				_, _ = fmt.Fprint(w, `{"result":{"AdGroups":[{
"Id":7101,"CampaignId":7001,"Name":" Group ","RegionIds":[225,-219],
"NegativeKeywords":{"Items":["скачать","бесплатно"]},
"TrackingParams":"mp_group=op_1","Status":"DRAFT","ServingStatus":"ELIGIBLE",
"Type":"UNIFIED_AD_GROUP","Subtype":"NONE","NegativeKeywordSharedSetIds":{"Items":[]},
"UnifiedAdGroup":{"OfferRetargeting":"NO"}
}]}}`)
			case "/json/v501/ads":
				_, _ = fmt.Fprint(w, `{"result":{"Ads":[{
"Id":7201,"CampaignId":7001,"AdGroupId":7101,"Status":"ACCEPTED","State":"ON",
"Type":"RESPONSIVE_AD","ResponsiveAd":{
"Titles":[{"Title":"B","Status":"ACCEPTED"},{"Title":"A","Status":"ACCEPTED"}],
"Texts":[{"Text":"Text","Status":"ACCEPTED"}],
"Href":"https://EXAMPLE.com:443/path?b=2&a=1"}
}]}}`)
			case "/json/v501/keywords":
				_, _ = fmt.Fprint(w, `{"result":{"Keywords":[{
"Id":7301,"CampaignId":7001,"AdGroupId":7101,"Keyword":" купить сервис ",
"StrategyPriority":"NORMAL","Status":"ACCEPTED","State":"ON","ServingStatus":"ELIGIBLE"
}]}}`)
			default:
				t.Fatalf("unexpected path %q", r.URL.Path)
			}
		})
		defer closeServer()
		groups, err := client.ListUnifiedAdGroups(context.Background(), "token", "", 7001)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(groups[0].RegionIDs, []int64{-219, 225}) ||
			!reflect.DeepEqual(groups[0].NegativeKeywords, []string{"бесплатно", "скачать"}) ||
			groups[0].TrackingMarker != "op_1" {
			t.Fatalf("group = %#v", groups[0])
		}
		ads, err := client.ListResponsiveAds(context.Background(), "token", "", 7001)
		if err != nil {
			t.Fatal(err)
		}
		if ads[0].Href != "https://example.com/path?a=1&b=2" ||
			ads[0].Titles[0].Value != "B" {
			t.Fatalf("ad = %#v", ads[0])
		}
		keywords, err := client.ListKeywords(context.Background(), "token", "", 7001)
		if err != nil {
			t.Fatal(err)
		}
		if keywords[0].Keyword != "купить сервис" {
			t.Fatalf("keyword = %#v", keywords[0])
		}
	})

	t.Run("unsupported ad is not silently filtered", func(t *testing.T) {
		t.Parallel()
		client, closeServer := newGraphTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, `{"result":{"Ads":[{
"Id":9,"CampaignId":7,"AdGroupId":8,"Type":"TEXT_AD","TextAd":{}
}]}}`)
		})
		defer closeServer()
		_, err := client.ListResponsiveAds(context.Background(), "token", "", 7)
		requireGraphErrorCode(t, err, "unsupported_ad_in_campaign")
	})

	t.Run("unsupported group subtype is not silently accepted", func(t *testing.T) {
		t.Parallel()
		client, closeServer := newGraphTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, `{"result":{"AdGroups":[{
"Id":8,"CampaignId":7,"Name":"Group","RegionIds":[225],
"Type":"UNIFIED_AD_GROUP","Subtype":"FUTURE_SUBTYPE",
"UnifiedAdGroup":{"OfferRetargeting":"NO"}
}]}}`)
		})
		defer closeServer()
		_, err := client.ListUnifiedAdGroups(context.Background(), "token", "", 7)
		requireGraphErrorCode(t, err, "unsupported_adgroup_subtype")
	})

	t.Run("shared negative set changes targeting", func(t *testing.T) {
		t.Parallel()
		client, closeServer := newGraphTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, `{"result":{"AdGroups":[{
"Id":8,"CampaignId":7,"Name":"Group","RegionIds":[225],
"Type":"UNIFIED_AD_GROUP","Subtype":"NONE",
"NegativeKeywordSharedSetIds":{"Items":[91]},
"UnifiedAdGroup":{"OfferRetargeting":"NO"}
}]}}`)
		})
		defer closeServer()
		_, err := client.ListUnifiedAdGroups(context.Background(), "token", "", 7)
		requireGraphErrorCode(t, err, "unsupported_adgroup_negative_shared_sets")
	})
}

func TestGetGraphCampaignRequestsAndNormalizesConsentSensitiveFields(t *testing.T) {
	t.Parallel()
	client, closeServer := newGraphTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/v501/campaigns" {
			t.Errorf("path = %q", r.URL.Path)
		}
		var request struct {
			Method string `json:"method"`
			Params struct {
				FieldNames                []string `json:"FieldNames"`
				UnifiedCampaignFieldNames []string `json:"UnifiedCampaignFieldNames"`
				SearchPlacementFieldNames []string `json:"UnifiedCampaignSearchStrategyPlacementTypesFieldNames"`
				PackagePlatformFieldNames []string `json:"UnifiedCampaignPackageBiddingStrategyPlatformsFieldNames"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if request.Method != "get" {
			t.Errorf("method = %q", request.Method)
		}
		requireContainsAll(t, request.Params.FieldNames, []string{
			"Id", "Name", "Type", "TimeZone", "NegativeKeywords", "BlockedIps",
			"ExcludedSites", "TimeTargeting", "DailyBudget",
		})
		requireContainsAll(t, request.Params.UnifiedCampaignFieldNames, []string{
			"BiddingStrategy", "Settings", "CounterIds", "PriorityGoals",
			"NegativeKeywordSharedSetIds", "TrackingParams", "AttributionModel", "PackageBiddingStrategy",
		})
		requireContainsAll(t, request.Params.SearchPlacementFieldNames, []string{
			"SearchResults", "ProductGallery", "DynamicPlaces", "Maps",
			"SearchOrganizationList",
		})
		requireContainsAll(t, request.Params.PackagePlatformFieldNames, []string{
			"SearchResult", "ProductGallery", "Maps", "SearchOrganizationList",
			"Network", "DynamicPlaces",
		})
		_, _ = fmt.Fprint(w, `{"result":{"Campaigns":[{
"Id":7001,"Name":" Campaign ","Status":"DRAFT","State":"OFF",
"Type":"UNIFIED_CAMPAIGN","StartDate":"2044-01-02","EndDate":"2044-02-02",
"TimeZone":"Europe/Moscow",
"NegativeKeywords":{"Items":["download","free"]},
"BlockedIps":{"Items":["203.0.113.9","198.51.100.7"]},
"ExcludedSites":{"Items":["B.EXAMPLE","a.example"]},
"TimeTargeting":{"Schedule":{"Items":["2,100","1,100"]},"ConsiderWorkingWeekends":"NO",
"HolidaysSchedule":{"SuspendOnHolidays":"YES","BidPercent":100,"StartHour":8,"EndHour":23}},
"DailyBudget":null,
"UnifiedCampaign":{
 "BiddingStrategy":{
  "Search":{"BiddingStrategyType":"SERVING_OFF",
   "PlacementTypes":{"SearchResults":"YES","ProductGallery":"NO","DynamicPlaces":"NO","Maps":"NO","SearchOrganizationList":"NO"}},
  "Network":{"BiddingStrategyType":"WB_MAXIMUM_CLICKS","WbMaximumClicks":{"WeeklySpendLimit":300000000}}
 },
 "Settings":[{"Option":"ENABLE_AREA_OF_INTEREST_TARGETING","Value":"NO"},{"Option":"ADD_METRICA_TAG","Value":"YES"}],
 "CounterIds":{"Items":[200,100]},
 "NegativeKeywordSharedSetIds":null,
 "PriorityGoals":{"Items":[{"GoalId":5,"Value":100000,"IsMetrikaSourceOfValue":"NO"}]},
 "TrackingParams":"utm_source=yandex","AttributionModel":"LAST_YANDEX_DIRECT_CLICK",
 "PackageBiddingStrategy":null
}
}]}}`)
	})
	defer closeServer()

	campaign, err := client.GetGraphCampaign(context.Background(), "token", "login", 7001)
	if err != nil {
		t.Fatal(err)
	}
	if campaign.ID != 7001 || campaign.WeeklyBudgetMinor != 30_000 ||
		campaign.Type != "UNIFIED_CAMPAIGN" ||
		campaign.TimeZone != "Europe/Moscow" ||
		campaign.TrackingParams != "utm_source=yandex" {
		t.Fatalf("campaign = %#v", campaign)
	}
	if !reflect.DeepEqual(campaign.CounterIDs, []int64{100, 200}) ||
		!reflect.DeepEqual(campaign.BlockedIPs, []string{"198.51.100.7", "203.0.113.9"}) ||
		!reflect.DeepEqual(campaign.ExcludedSites, []string{"a.example", "b.example"}) ||
		!reflect.DeepEqual(campaign.TimeTargeting.Schedule, []string{"1,100", "2,100"}) {
		t.Fatalf("normalized campaign = %#v", campaign)
	}
	if !bytes.Contains(campaign.BiddingStrategy, []byte(`"PlacementTypes"`)) {
		t.Fatalf("strategy lost placement types: %s", campaign.BiddingStrategy)
	}
}

func TestGetGraphCampaignFailsClosedOnUnsupportedBudgetOrPackageStrategy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		response string
		wantCode string
	}{
		{
			name: "daily budget",
			response: `{"result":{"Campaigns":[{
"Id":1,"DailyBudget":{"Amount":1000000},"UnifiedCampaign":{}
}]}}`,
			wantCode: "unsupported_campaign_daily_budget",
		},
		{
			name: "package strategy",
			response: `{"result":{"Campaigns":[{
"Id":1,"DailyBudget":null,
"UnifiedCampaign":{"PackageBiddingStrategy":{"StrategyId":77}}
}]}}`,
			wantCode: "unsupported_package_bidding_strategy",
		},
		{
			name: "missing requested field",
			response: `{"result":{"Campaigns":[{
"Id":1,"Name":"x","Status":"DRAFT","State":"OFF","Type":"UNIFIED_CAMPAIGN",
"StartDate":"2044-01-01","EndDate":"2044-02-01","TimeZone":"Europe/Moscow",
"NegativeKeywords":null,"BlockedIps":null,"ExcludedSites":null,
"TimeTargeting":{"Schedule":{"Items":[]},"ConsiderWorkingWeekends":"NO"},
"DailyBudget":null,
"UnifiedCampaign":{"BiddingStrategy":{},"Settings":[],"CounterIds":null,
"TrackingParams":null,"AttributionModel":"AUTO",
"PackageBiddingStrategy":null}
}]}}`,
			wantCode: "incomplete_campaign_response",
		},
		{
			name: "campaign shared negative set",
			response: `{"result":{"Campaigns":[{
"Id":1,"Name":"x","Status":"DRAFT","State":"OFF","Type":"UNIFIED_CAMPAIGN",
"StartDate":"2044-01-01","EndDate":"2044-02-01","TimeZone":"Europe/Moscow",
"NegativeKeywords":null,"BlockedIps":null,"ExcludedSites":null,
"TimeTargeting":{"Schedule":{"Items":["1,100"]},"ConsiderWorkingWeekends":"NO"},
"DailyBudget":null,
"UnifiedCampaign":{
 "BiddingStrategy":{
  "Search":{"BiddingStrategyType":"SERVING_OFF","PlacementTypes":{"SearchResults":"NO","ProductGallery":"NO","DynamicPlaces":"NO","Maps":"NO","SearchOrganizationList":"NO"}},
  "Network":{"BiddingStrategyType":"WB_MAXIMUM_CLICKS","PlacementTypes":{"Network":"YES","Maps":"NO"},"WbMaximumClicks":{"WeeklySpendLimit":300000000}}
 },
 "Settings":[],"CounterIds":null,"PriorityGoals":null,
 "NegativeKeywordSharedSetIds":{"Items":[77]},
 "TrackingParams":"mp_op=marker","AttributionModel":"AUTO",
 "PackageBiddingStrategy":null
}
}]}}`,
			wantCode: "unsupported_campaign_negative_shared_sets",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			client, closeServer := newGraphTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprint(w, test.response)
			})
			defer closeServer()
			_, err := client.GetGraphCampaign(context.Background(), "token", "", 1)
			requireGraphErrorCode(t, err, test.wantCode)
		})
	}
}

func TestListResponsiveAdsRequestsAllOptionalFieldsAndFailsClosed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		topLevel       string
		responsivePart string
	}{
		{name: "image", responsivePart: `"AdImages":{"Items":[{"ImageHash":"hash"}]}`},
		{name: "extension", responsivePart: `"AdExtensions":{"Items":[{"AdExtensionId":1}]}`},
		{name: "business", responsivePart: `"BusinessId":55`},
		{name: "erir", responsivePart: `"ErirAdDescription":"advertisement"`},
		{name: "display path", responsivePart: `"DisplayUrlPath":"sale"`},
		{name: "age", topLevel: `"AgeLabel":"AGE_18"`},
		{name: "category", topLevel: `"AdCategories":{"Items":["MEDICINE"]}`},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			client, closeServer := newGraphTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Params struct {
						FieldNames             []string `json:"FieldNames"`
						ResponsiveAdFieldNames []string `json:"ResponsiveAdFieldNames"`
					} `json:"params"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				requireContainsAll(t, request.Params.FieldNames,
					[]string{"Subtype", "AdCategories", "AgeLabel"})
				requireContainsAll(t, request.Params.ResponsiveAdFieldNames, []string{
					"Titles", "Texts", "Href", "DisplayDomain", "DisplayUrlPath",
					"AdImages", "SitelinkSetId", "DisplayUrlPathModeration",
					"SitelinksModeration", "AdExtensions", "VideoExtensions",
					"PriceExtension", "BusinessId", "ErirAdDescription",
				})
				topComma := ""
				if test.topLevel != "" {
					topComma = "," + test.topLevel
				}
				responsiveComma := ""
				if test.responsivePart != "" {
					responsiveComma = "," + test.responsivePart
				}
				_, _ = fmt.Fprintf(w, `{"result":{"Ads":[{
"Id":9,"CampaignId":7,"AdGroupId":8,"Type":"RESPONSIVE_AD","Subtype":"NONE"%s,
"ResponsiveAd":{"Titles":[{"Title":"Title"}],"Texts":[{"Text":"Text"}],
"Href":"https://example.com"%s}
}]}}`, topComma, responsiveComma)
			})
			defer closeServer()
			_, err := client.ListResponsiveAds(context.Background(), "token", "", 7)
			requireGraphErrorCode(t, err, "unsupported_responsive_ad_features")
		})
	}
}

func TestCampaignGraphFingerprintIsCanonicalAndStatusIndependent(t *testing.T) {
	t.Parallel()
	startsAt := time.Date(2044, 1, 2, 13, 0, 0, 0, time.FixedZone("x", 7200))
	endsAt := time.Date(2044, 2, 2, 1, 0, 0, 0, time.UTC)
	graph := CampaignGraph{
		Campaign: graphCampaignFixture(
			7001, " Campaign ", 30_000, startsAt, endsAt,
		),
		AdGroups: []UnifiedAdGroup{{
			ID: 7101, CampaignID: 7001, Name: "Group",
			RegionIDs:        []int64{225, -219},
			NegativeKeywords: []string{"скачать", "бесплатно"},
			TrackingParams:   "mp_group=op_1", TrackingMarker: "op_1",
			OfferRetargeting: "NO", Status: "DRAFT",
		}},
		Ads: []ResponsiveAd{{
			ID: 7201, CampaignID: 7001, AdGroupID: 7101,
			Titles: []ModeratedText{
				{Value: "Второй", Status: "DRAFT"},
				{Value: "Первыи\u0306", Status: "DRAFT"},
			},
			Texts:  []ModeratedText{{Value: "Текст", Status: "DRAFT"}},
			Href:   "https://EXAMPLE.com:443/path?b=2&a=1",
			Status: "DRAFT", State: "OFF",
		}},
		Keywords: []Keyword{{
			ID: 7301, CampaignID: 7001, AdGroupID: 7101,
			Keyword: "купить сервис", StrategyPriority: "NORMAL",
			Status: "DRAFT", State: "OFF",
		}},
	}
	first, err := CampaignGraphFingerprint(graph)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != sha256HexLength {
		t.Fatalf("fingerprint length = %d: %q", len(first), first)
	}
	if _, err := hex.DecodeString(first); err != nil {
		t.Fatalf("fingerprint is not hex: %v", err)
	}

	reordered := graph
	reordered.Campaign.Status = "ACCEPTED"
	reordered.Campaign.State = "ON"
	reordered.Campaign.Settings = []GraphCampaignSetting{
		{Option: "ENABLE_AREA_OF_INTEREST_TARGETING", Value: "NO"},
		{Option: "ADD_METRICA_TAG", Value: "YES"},
	}
	reordered.Campaign.CounterIDs = []int64{100, 200}
	reordered.Campaign.TimeTargeting.Schedule = []string{"2,100", "1,100"}
	reordered.Campaign.BiddingStrategy = json.RawMessage(
		`{"Search":{"BiddingStrategyType":"SERVING_OFF"},"Network":{"BiddingStrategyType":"WB_MAXIMUM_CLICKS","WbMaximumClicks":{"WeeklySpendLimit":300000000}}}`,
	)
	reordered.AdGroups = append([]UnifiedAdGroup(nil), graph.AdGroups...)
	reordered.AdGroups[0].RegionIDs = []int64{-219, 225}
	reordered.AdGroups[0].NegativeKeywords = []string{"бесплатно", "скачать"}
	reordered.Ads = append([]ResponsiveAd(nil), graph.Ads...)
	reordered.Ads[0].Titles = []ModeratedText{
		{Value: "Первый", Status: "ACCEPTED"},
		{Value: "Второй", Status: "ACCEPTED"},
	}
	reordered.Ads[0].Href = "https://example.com/path?a=1&b=2"
	reordered.Keywords = append([]Keyword(nil), graph.Keywords...)
	reordered.Keywords[0].Status = "ACCEPTED"
	reordered.Keywords[0].State = "ON"
	second, err := reordered.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("canonical fingerprints differ:\n%s\n%s", first, second)
	}

	changed := reordered
	changed.Ads = append([]ResponsiveAd(nil), reordered.Ads...)
	changed.Ads[0].Href = "https://example.com/other?a=1&b=2"
	third, err := changed.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if third == first {
		t.Fatal("creative URL change did not change fingerprint")
	}

	changedCampaign := reordered
	changedCampaign.Campaign.TrackingParams = "utm_source=changed"
	fourth, err := changedCampaign.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if fourth == first {
		t.Fatal("campaign tracking change did not change fingerprint")
	}

	changedStrategy := reordered
	changedStrategy.Campaign.BiddingStrategy = json.RawMessage(
		`{"Network":{"BiddingStrategyType":"WB_MAXIMUM_CLICKS","WbMaximumClicks":{"WeeklySpendLimit":300000000}},"Search":{"BiddingStrategyType":"SERVING_OFF","PlacementTypes":{"Maps":"YES"}}}`,
	)
	fifth, err := changedStrategy.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if fifth == first {
		t.Fatal("strategy placement change did not change fingerprint")
	}
}

func TestCampaignGraphFingerprintRejectsOrphanObjects(t *testing.T) {
	t.Parallel()
	graph := CampaignGraph{
		Campaign: graphCampaignFixture(
			1, "Campaign", 30_000,
			time.Date(2044, 1, 1, 0, 0, 0, 0, time.UTC),
			time.Date(2044, 2, 1, 0, 0, 0, 0, time.UTC),
		),
		Ads: []ResponsiveAd{{
			ID: 3, CampaignID: 1, AdGroupID: 2,
			Titles: []ModeratedText{{Value: "Title"}},
			Texts:  []ModeratedText{{Value: "Text"}},
			Href:   "https://example.com",
		}},
	}
	_, err := graph.Fingerprint()
	requireGraphErrorCode(t, err, "orphan_responsive_ad")
}

func TestGraphPrimitivesRequireV501(t *testing.T) {
	t.Parallel()
	client, err := New(
		"https://api-sandbox.direct.yandex.com/json/v5", "client", "secret",
		CallbackRedirectURI, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if client.SupportsUnifiedGraph() {
		t.Fatal("v5 client unexpectedly reports unified graph support")
	}
	_, err = client.ResolveRegionNames(context.Background(), "token", "", []string{"Москва"})
	requireGraphErrorCode(t, err, "unified_graph_requires_v501")
	_, err = client.CreateUnifiedAdGroup(context.Background(), "token", "", UnifiedAdGroupDraft{})
	requireGraphErrorCode(t, err, "unified_graph_requires_v501")
	_, err = client.CreateResponsiveAd(context.Background(), "token", "", ResponsiveAdDraft{})
	requireGraphErrorCode(t, err, "unified_graph_requires_v501")
}

func TestSupportsUnifiedGraphForV501Client(t *testing.T) {
	t.Parallel()
	client, closeServer := newGraphTestClient(t, func(http.ResponseWriter, *http.Request) {
		t.Fatal("unexpected provider request")
	})
	defer closeServer()
	if !client.SupportsUnifiedGraph() {
		t.Fatal("v501 client does not report unified graph support")
	}
	var nilClient *Client
	if nilClient.SupportsUnifiedGraph() {
		t.Fatal("nil client reports unified graph support")
	}
}

const sha256HexLength = 64

func newGraphTestClient(
	t *testing.T, handler func(http.ResponseWriter, *http.Request),
) (*Client, func()) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		handler(w, r)
	}))
	client, err := New(
		server.URL+"/json/v501", "client-id", "secret",
		CallbackRedirectURI, server.Client(),
	)
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	return client, server.Close
}

func requireGraphErrorCode(t *testing.T, err error, code string) {
	t.Helper()
	var providerErr *Error
	if !errors.As(err, &providerErr) {
		t.Fatalf("error = %v, want *Error with code %q", err, code)
	}
	if providerErr.Code != code {
		t.Fatalf("error code = %q, want %q", providerErr.Code, code)
	}
}

func jsonNumbers(values []any) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, value.(json.Number).String())
	}
	return result
}

func requireContainsAll(t *testing.T, got, want []string) {
	t.Helper()
	set := make(map[string]struct{}, len(got))
	for _, item := range got {
		set[item] = struct{}{}
	}
	for _, item := range want {
		if _, ok := set[item]; !ok {
			t.Fatalf("%q is missing from %v", item, got)
		}
	}
}

func graphCampaignFixture(
	id int64, name string, weeklyBudgetMinor int64, startsAt, endsAt time.Time,
) GraphCampaign {
	weeklyBudgetMicros, err := MinorToMicros(weeklyBudgetMinor)
	if err != nil {
		panic(err)
	}
	return GraphCampaign{
		ID: id, Name: name, Status: "DRAFT", State: "OFF",
		Type: "UNIFIED_CAMPAIGN", WeeklyBudgetMinor: weeklyBudgetMinor,
		StartsAt: startsAt, EndsAt: endsAt, TimeZone: "Europe/Moscow",
		TimeTargeting: GraphTimeTargeting{
			Present: true, Schedule: []string{"1,100", "2,100"},
			ConsiderWorkingWeekends: "NO",
		},
		BiddingStrategy: json.RawMessage(fmt.Sprintf(
			`{"Network":{"WbMaximumClicks":{"WeeklySpendLimit":%d},"BiddingStrategyType":"WB_MAXIMUM_CLICKS"},"Search":{"BiddingStrategyType":"SERVING_OFF"}}`,
			weeklyBudgetMicros,
		)),
		Settings: []GraphCampaignSetting{
			{Option: "ADD_METRICA_TAG", Value: "YES"},
			{Option: "ENABLE_AREA_OF_INTEREST_TARGETING", Value: "NO"},
		},
		CounterIDs:       []int64{200, 100},
		PriorityGoals:    []GraphPriorityGoal{{GoalID: 5, Value: 100_000}},
		TrackingParams:   "utm_source=yandex",
		AttributionModel: "LAST_YANDEX_DIRECT_CLICK",
	}
}
