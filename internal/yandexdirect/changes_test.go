package yandexdirect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCurrentChangesTimestampBootstrapsFromProviderClock(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if request.Method != "checkDictionaries" || len(request.Params) != 0 {
			t.Errorf("bootstrap request = %#v", request)
		}
		_, _ = fmt.Fprint(w, `{"result":{"Timestamp":"2026-08-02T10:06:00Z"}}`)
	}))
	defer server.Close()
	client, err := New(
		server.URL+"/json/v501", "client", "secret", CallbackRedirectURI, server.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}
	timestamp, err := client.CurrentChangesTimestamp(
		context.Background(), "token", "client-login",
	)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, time.August, 2, 10, 6, 0, 0, time.UTC); !timestamp.Equal(want) {
		t.Fatalf("timestamp = %s, want %s", timestamp, want)
	}
}

func TestCheckCampaignChangesUsesOverlapAndDetectsChildren(t *testing.T) {
	t.Parallel()
	const campaignID int64 = 7004
	since := time.Date(2026, time.August, 2, 10, 5, 30, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Method string `json:"method"`
			Params struct {
				CampaignIDs []int64  `json:"CampaignIds"`
				Timestamp   string   `json:"Timestamp"`
				FieldNames  []string `json:"FieldNames"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if request.Method != "check" || len(request.Params.CampaignIDs) != 1 ||
			request.Params.CampaignIDs[0] != campaignID ||
			request.Params.Timestamp != "2026-08-02T10:04:30Z" ||
			len(request.Params.FieldNames) != 4 {
			t.Errorf("changes request = %#v", request)
		}
		w.Header().Set("Units", "10/990/1000")
		_, _ = fmt.Fprint(w, `{"result":{"Modified":{"AdGroupIds":[44]},"Timestamp":"2026-08-02T10:06:00Z"}}`)
	}))
	defer server.Close()
	client, err := New(
		server.URL+"/json/v501", "client", "secret", CallbackRedirectURI, server.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := client.CheckCampaignChanges(
		context.Background(), "token", "", campaignID, since,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !changes.Modified || !changes.GraphModified || changes.StatisticsModified ||
		!changes.Timestamp.Equal(time.Date(
			2026, time.August, 2, 10, 6, 0, 0, time.UTC,
		)) {
		t.Fatalf("changes = %#v", changes)
	}
}

func TestCheckCampaignChangesRejectsNotFoundAndMalformedCursor(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		response string
		wantCode string
	}{
		{
			name: "not found",
			response: `{"result":{"NotFound":{"CampaignIds":[7004]},` +
				`"Timestamp":"2026-08-02T10:06:00Z"}}`,
			wantCode: "campaign_not_found",
		},
		{
			name:     "invalid timestamp",
			response: `{"result":{"Timestamp":"not-a-time"}}`,
			wantCode: "invalid_changes_response",
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprint(w, test.response)
			}))
			defer server.Close()
			client, err := New(
				server.URL+"/json/v501", "client", "secret",
				CallbackRedirectURI, server.Client(),
			)
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.CheckCampaignChanges(
				context.Background(), "token", "", 7004,
				time.Date(2026, time.August, 2, 10, 5, 30, 0, time.UTC),
			)
			var providerErr *Error
			if !errors.As(err, &providerErr) || providerErr.Code != test.wantCode {
				t.Fatalf("error = %#v, want %q", err, test.wantCode)
			}
		})
	}
}
