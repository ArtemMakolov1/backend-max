package yandexdirect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
)

var expectedCampaignSummaryFields = []string{
	"Id", "Name", "Type", "Status", "State", "StatusPayment",
	"StartDate", "EndDate", "TimeZone",
}

type campaignListTestRequest struct {
	Method string `json:"method"`
	Params struct {
		SelectionCriteria map[string]json.RawMessage `json:"SelectionCriteria"`
		FieldNames        []string                   `json:"FieldNames"`
		Page              map[string]int64           `json:"Page"`
	} `json:"params"`
}

func TestListCampaignsFreezesIDsHydratesProjectionAndPreservesIDOrder(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	client, closeServer := newCampaignListTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Client-Login") != "direct-client" {
			t.Errorf("Client-Login = %q", r.Header.Get("Client-Login"))
		}
		request := decodeCampaignListTestRequest(t, r)
		if request.Method != "get" {
			t.Errorf("method = %q", request.Method)
		}
		switch calls.Add(1) {
		case 1:
			if ids, present := campaignListTestRequestIDs(t, request); present || ids != nil {
				t.Errorf("ID snapshot SelectionCriteria = %#v", request.Params.SelectionCriteria)
			}
			if !reflect.DeepEqual(request.Params.FieldNames, []string{"Id"}) ||
				!reflect.DeepEqual(request.Params.Page, map[string]int64{"Limit": maxCampaignListObjects}) {
				t.Errorf("ID snapshot request = %#v", request)
			}
			writeCampaignListResponse(t, w, campaignIDListItems(103, 101, 102), nil)
		case 2:
			ids, present := campaignListTestRequestIDs(t, request)
			if !present || !reflect.DeepEqual(ids, []int64{103, 101, 102}) ||
				!reflect.DeepEqual(request.Params.FieldNames, expectedCampaignSummaryFields) ||
				request.Params.Page != nil {
				t.Errorf("detail request = %#v, IDs=%v", request, ids)
			}
			third := validCampaignListItem(103, "Третья")
			delete(third, "EndDate")
			// Provider order is deliberately different from the frozen ID order.
			writeCampaignListResponse(t, w, []map[string]any{
				validCampaignListItem(102, "Вторая"),
				third,
				validCampaignListItem(101, "Первая"),
			}, nil)
		default:
			t.Fatalf("unexpected request %d", calls.Load())
		}
	})
	defer closeServer()

	campaigns, err := client.ListCampaigns(
		context.Background(), "token", " direct-client ",
	)
	if err != nil {
		t.Fatal(err)
	}
	endDate := "2027-12-31"
	want := []CampaignSummary{
		{
			ID: 103, Name: "Третья", Type: "UNIFIED_CAMPAIGN", Status: "ACCEPTED",
			State: "ON", StatusPayment: "ALLOWED", StartDate: "2027-01-01",
			TimeZone: "Europe/Moscow",
		},
		{
			ID: 101, Name: "Первая", Type: "UNIFIED_CAMPAIGN", Status: "ACCEPTED",
			State: "ON", StatusPayment: "ALLOWED", StartDate: "2027-01-01",
			EndDate: &endDate, TimeZone: "Europe/Moscow",
		},
		{
			ID: 102, Name: "Вторая", Type: "UNIFIED_CAMPAIGN", Status: "ACCEPTED",
			State: "ON", StatusPayment: "ALLOWED", StartDate: "2027-01-01",
			EndDate: &endDate, TimeZone: "Europe/Moscow",
		},
	}
	if !reflect.DeepEqual(campaigns, want) || calls.Load() != 2 {
		t.Fatalf("campaigns=%#v calls=%d", campaigns, calls.Load())
	}
}

func TestListCampaignsReturnsNonNilEmptyListWithoutDetailRequest(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	client, closeServer := newCampaignListTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeCampaignListResponse(t, w, []map[string]any{}, nil)
	})
	defer closeServer()

	campaigns, err := client.ListCampaigns(context.Background(), "token", "")
	if err != nil {
		t.Fatal(err)
	}
	if campaigns == nil || len(campaigns) != 0 || calls.Load() != 1 {
		t.Fatalf("campaigns = %#v, calls=%d", campaigns, calls.Load())
	}
}

func TestListCampaignsHydratesFrozenIDsInBoundedBatches(t *testing.T) {
	t.Parallel()
	ids := make([]int64, campaignDetailBatchSize+1)
	for index := range ids {
		ids[index] = int64(index + 1)
	}
	var calls atomic.Int32
	client, closeServer := newCampaignListTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		request := decodeCampaignListTestRequest(t, r)
		switch calls.Add(1) {
		case 1:
			writeCampaignListResponse(t, w, campaignIDListItems(ids...), nil)
		case 2:
			requested, present := campaignListTestRequestIDs(t, request)
			if !present || !reflect.DeepEqual(requested, ids[:campaignDetailBatchSize]) ||
				request.Params.Page != nil {
				t.Errorf("first detail batch = %v, page=%#v", requested, request.Params.Page)
			}
			items := make([]map[string]any, 0, len(requested))
			for index := len(requested) - 1; index >= 0; index-- {
				id := requested[index]
				items = append(items, validCampaignListItem(id, fmt.Sprintf("Campaign %d", id)))
			}
			writeCampaignListResponse(t, w, items, nil)
		case 3:
			requested, present := campaignListTestRequestIDs(t, request)
			if !present || !reflect.DeepEqual(requested, ids[campaignDetailBatchSize:]) ||
				request.Params.Page != nil {
				t.Errorf("second detail batch = %v, page=%#v", requested, request.Params.Page)
			}
			writeCampaignListResponse(t, w, []map[string]any{
				validCampaignListItem(requested[0], fmt.Sprintf("Campaign %d", requested[0])),
			}, nil)
		default:
			t.Fatalf("unexpected request %d", calls.Load())
		}
	})
	defer closeServer()

	campaigns, err := client.ListCampaigns(context.Background(), "token", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(campaigns) != len(ids) || calls.Load() != 3 {
		t.Fatalf("campaign count=%d calls=%d", len(campaigns), calls.Load())
	}
	for index, campaign := range campaigns {
		if campaign.ID != ids[index] {
			t.Fatalf("campaign %d ID=%d, want %d", index, campaign.ID, ids[index])
		}
	}
}

func TestListCampaignsRejectsInvalidCampaignFields(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "zero ID", mutate: func(item map[string]any) { item["Id"] = int64(0) }},
		{name: "missing name", mutate: func(item map[string]any) { delete(item, "Name") }},
		{name: "missing type", mutate: func(item map[string]any) { delete(item, "Type") }},
		{name: "missing status", mutate: func(item map[string]any) { delete(item, "Status") }},
		{name: "missing state", mutate: func(item map[string]any) { delete(item, "State") }},
		{name: "missing payment status", mutate: func(item map[string]any) { delete(item, "StatusPayment") }},
		{name: "missing start date", mutate: func(item map[string]any) { delete(item, "StartDate") }},
		{name: "invalid start date", mutate: func(item map[string]any) { item["StartDate"] = "2027-02-30" }},
		{name: "missing time zone", mutate: func(item map[string]any) { delete(item, "TimeZone") }},
		{name: "empty end date", mutate: func(item map[string]any) { item["EndDate"] = "" }},
		{name: "invalid end date", mutate: func(item map[string]any) { item["EndDate"] = "31.12.2027" }},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			item := validCampaignListItem(1, "Кампания")
			test.mutate(item)
			var calls atomic.Int32
			client, closeServer := newCampaignListTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 {
					writeCampaignListResponse(t, w, campaignIDListItems(1), nil)
					return
				}
				writeCampaignListResponse(t, w, []map[string]any{item}, nil)
			})
			defer closeServer()
			_, err := client.ListCampaigns(context.Background(), "token", "")
			requireCampaignListErrorCode(t, err, "invalid_campaign_list_response")
		})
	}
}

func TestListCampaignsRejectsInvalidIDSnapshot(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		items    []map[string]any
		wantCode string
	}{
		{name: "missing ID", items: []map[string]any{{}}, wantCode: "invalid_campaign_list_response"},
		{name: "zero ID", items: campaignIDListItems(0), wantCode: "invalid_campaign_list_response"},
		{name: "negative ID", items: campaignIDListItems(-1), wantCode: "invalid_campaign_list_response"},
		{name: "duplicate ID", items: campaignIDListItems(1, 1), wantCode: "duplicate_campaign_list_response"},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			client, closeServer := newCampaignListTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				writeCampaignListResponse(t, w, test.items, nil)
			})
			defer closeServer()
			_, err := client.ListCampaigns(context.Background(), "token", "")
			requireCampaignListErrorCode(t, err, test.wantCode)
		})
	}
}

func TestListCampaignsRejectsInvalidIDSnapshotShapes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		response string
		wantCode string
	}{
		{name: "missing campaigns", response: `{"result":{}}`, wantCode: "invalid_campaign_list_response"},
		{name: "null campaigns", response: `{"result":{"Campaigns":null}}`, wantCode: "invalid_campaign_list_response"},
		{name: "object instead of array", response: `{"result":{"Campaigns":{}}}`, wantCode: "invalid_campaign_list_response"},
		{name: "invalid ID type", response: `{"result":{"Campaigns":[{"Id":"one"}]}}`, wantCode: "invalid_campaign_list_response"},
		{name: "invalid LimitedBy type", response: `{"result":{"Campaigns":[],"LimitedBy":"one"}}`, wantCode: "invalid_api_result"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			client, closeServer := newCampaignListTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprint(w, test.response)
			})
			defer closeServer()
			_, err := client.ListCampaigns(context.Background(), "token", "")
			requireCampaignListErrorCode(t, err, test.wantCode)
		})
	}
}

func TestListCampaignsRejectsInvalidDetailShapes(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		response string
		wantCode string
	}{
		{name: "missing campaigns", response: `{"result":{}}`, wantCode: "invalid_campaign_list_response"},
		{name: "null campaigns", response: `{"result":{"Campaigns":null}}`, wantCode: "invalid_campaign_list_response"},
		{name: "object instead of array", response: `{"result":{"Campaigns":{}}}`, wantCode: "invalid_campaign_list_response"},
		{name: "invalid campaign field type", response: `{"result":{"Campaigns":[{"Id":"one"}]}}`, wantCode: "invalid_campaign_list_response"},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			client, closeServer := newCampaignListTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 {
					writeCampaignListResponse(t, w, campaignIDListItems(1), nil)
					return
				}
				_, _ = fmt.Fprint(w, test.response)
			})
			defer closeServer()
			_, err := client.ListCampaigns(context.Background(), "token", "")
			requireCampaignListErrorCode(t, err, test.wantCode)
		})
	}
}

func TestListCampaignsRejectsProviderCollectionAboveLimit(t *testing.T) {
	t.Parallel()
	items := make([]map[string]any, 0, maxCampaignListObjects+1)
	for id := int64(1); id <= maxCampaignListObjects+1; id++ {
		items = append(items, map[string]any{"Id": id})
	}
	client, closeServer := newCampaignListTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeCampaignListResponse(t, w, items, nil)
	})
	defer closeServer()

	_, err := client.ListCampaigns(context.Background(), "token", "")
	requireCampaignListErrorCode(t, err, "campaign_list_limit_exceeded")
}

func TestListCampaignIDsAcceptsExactProviderLimit(t *testing.T) {
	t.Parallel()
	items := make([]map[string]any, 0, maxCampaignListObjects)
	for id := int64(1); id <= maxCampaignListObjects; id++ {
		items = append(items, map[string]any{"Id": id})
	}
	client, closeServer := newCampaignListTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeCampaignListResponse(t, w, items, nil)
	})
	defer closeServer()

	ids, err := client.listCampaignIDs(context.Background(), "token", "")
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(ids)) != maxCampaignListObjects || ids[0] != 1 ||
		ids[len(ids)-1] != maxCampaignListObjects {
		t.Fatalf("ID range=%d..%d length=%d", ids[0], ids[len(ids)-1], len(ids))
	}
}

func TestListCampaignsTreatsInitialLimitedByAsExplicitLimitError(t *testing.T) {
	t.Parallel()
	limitedBy := maxCampaignListObjects
	client, closeServer := newCampaignListTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeCampaignListResponse(t, w, campaignIDListItems(1), &limitedBy)
	})
	defer closeServer()

	_, err := client.ListCampaigns(context.Background(), "token", "")
	requireCampaignListErrorCode(t, err, "campaign_list_limit_exceeded")
}

func TestListCampaignsFailsClosedWhenFrozenSetChangesDuringHydration(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		frozen   []int64
		details  []map[string]any
		wantCode string
	}{
		{
			name: "campaign deleted after ID snapshot", frozen: []int64{1, 2},
			details:  []map[string]any{validCampaignListItem(1, "A")},
			wantCode: "invalid_campaign_list_response",
		},
		{
			name: "unexpected campaign added to filtered response", frozen: []int64{1},
			details:  []map[string]any{validCampaignListItem(2, "New")},
			wantCode: "invalid_campaign_list_response",
		},
		{
			name: "extra campaign returned", frozen: []int64{1},
			details: []map[string]any{
				validCampaignListItem(1, "A"), validCampaignListItem(2, "Extra"),
			},
			wantCode: "invalid_campaign_list_response",
		},
		{
			name: "duplicate detail", frozen: []int64{1, 2},
			details: []map[string]any{
				validCampaignListItem(1, "A"), validCampaignListItem(1, "Again"),
			},
			wantCode: "duplicate_campaign_list_response",
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			client, closeServer := newCampaignListTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 {
					writeCampaignListResponse(t, w, campaignIDListItems(test.frozen...), nil)
					return
				}
				writeCampaignListResponse(t, w, test.details, nil)
			})
			defer closeServer()
			_, err := client.ListCampaigns(context.Background(), "token", "")
			requireCampaignListErrorCode(t, err, test.wantCode)
		})
	}
}

func TestListCampaignsRejectsLimitedDetailResponse(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	limitedBy := int64(1)
	client, closeServer := newCampaignListTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			writeCampaignListResponse(t, w, campaignIDListItems(1), nil)
			return
		}
		writeCampaignListResponse(t, w, []map[string]any{
			validCampaignListItem(1, "A"),
		}, &limitedBy)
	})
	defer closeServer()

	_, err := client.ListCampaigns(context.Background(), "token", "")
	requireCampaignListErrorCode(t, err, "invalid_provider_pagination")
}

func TestListCampaignsValidatesTokenBeforeRequest(t *testing.T) {
	t.Parallel()
	client, closeServer := newCampaignListTestClient(t, func(http.ResponseWriter, *http.Request) {
		t.Fatal("unexpected provider request")
	})
	defer closeServer()

	_, err := client.ListCampaigns(context.Background(), "  ", "")
	requireCampaignListErrorCode(t, err, "missing_access_token")
}

func decodeCampaignListTestRequest(t *testing.T, r *http.Request) campaignListTestRequest {
	t.Helper()
	var request campaignListTestRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		t.Fatalf("decode campaign list request: %v", err)
	}
	return request
}

func campaignListTestRequestIDs(
	t *testing.T, request campaignListTestRequest,
) ([]int64, bool) {
	t.Helper()
	raw, present := request.Params.SelectionCriteria["Ids"]
	if !present {
		return nil, false
	}
	var ids []int64
	if err := json.Unmarshal(raw, &ids); err != nil {
		t.Fatalf("decode requested campaign IDs: %v", err)
	}
	return ids, true
}

func campaignIDListItems(ids ...int64) []map[string]any {
	items := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		items = append(items, map[string]any{"Id": id})
	}
	return items
}

func validCampaignListItem(id int64, name string) map[string]any {
	return map[string]any{
		"Id": id, "Name": name, "Type": "UNIFIED_CAMPAIGN", "Status": "ACCEPTED",
		"State": "ON", "StatusPayment": "ALLOWED", "StartDate": "2027-01-01",
		"EndDate": "2027-12-31", "TimeZone": "Europe/Moscow",
	}
}

func writeCampaignListResponse(
	t *testing.T, w http.ResponseWriter, campaigns []map[string]any, limitedBy *int64,
) {
	t.Helper()
	result := map[string]any{"Campaigns": campaigns}
	if limitedBy != nil {
		result["LimitedBy"] = *limitedBy
	}
	if err := json.NewEncoder(w).Encode(map[string]any{"result": result}); err != nil {
		t.Error(err)
	}
}

func newCampaignListTestClient(
	t *testing.T, handler func(http.ResponseWriter, *http.Request),
) (*Client, func()) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/v501/campaigns" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Accept-Language") != "ru" ||
			r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("headers = %#v", r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		handler(w, r)
	}))
	client, err := New(
		server.URL+"/json/v501", "client-id", "secret", CallbackRedirectURI, server.Client(),
	)
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	return client, server.Close
}

func requireCampaignListErrorCode(t *testing.T, err error, code string) {
	t.Helper()
	var providerErr *Error
	if !errors.As(err, &providerErr) {
		t.Fatalf("error = %v, want *Error with code %q", err, code)
	}
	if providerErr.Code != code {
		t.Fatalf("error code = %q, want %q", providerErr.Code, code)
	}
}
