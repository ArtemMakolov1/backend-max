package yandexdirect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOAuthRedirectAllowlistAndVerificationCodeFlow(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		redirect string
		flow     OAuthFlow
	}{
		{redirect: CallbackRedirectURI, flow: OAuthFlowCallback},
		{redirect: VerificationCodeRedirectURI, flow: OAuthFlowVerificationCode},
	} {
		client, err := New(
			DefaultSandboxAPIBaseURL, "client-id", "secret", test.redirect, nil,
		)
		if err != nil {
			t.Fatalf("New(%q): %v", test.redirect, err)
		}
		if client.OAuthFlow() != test.flow {
			t.Fatalf("flow for %q = %q, want %q", test.redirect, client.OAuthFlow(), test.flow)
		}
		authorizationURL, err := url.Parse(client.AuthorizationURL("opaque-state", "pkce-challenge"))
		if err != nil {
			t.Fatal(err)
		}
		if got := authorizationURL.Query().Get("redirect_uri"); got != test.redirect {
			t.Fatalf("authorization redirect_uri = %q, want %q", got, test.redirect)
		}
		if authorizationURL.Query().Get("state") != "opaque-state" ||
			authorizationURL.Query().Get("code_challenge") != "pkce-challenge" {
			t.Fatalf("authorization query = %v", authorizationURL.Query())
		}
	}
	for _, redirect := range []string{
		"http://localhost:8080/api/v1/advertising/direct/oauth/callback",
		"https://evil.example/api/v1/advertising/direct/oauth/callback",
		"https://oauth.yandex.ru/verification_code/",
	} {
		if _, err := New(
			DefaultSandboxAPIBaseURL, "client-id", "secret", redirect, nil,
		); err == nil {
			t.Fatalf("unsafe redirect %q was accepted", redirect)
		}
	}
}

func TestCreateCampaignDraftPreservesPerItemRejectionBeforeMissingID(
	t *testing.T,
) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter, _ *http.Request,
	) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"AddResults":[{
			"Id":0,
			"Errors":[{"Code":6000,"Message":"invalid","Details":"private detail"}]
		}]}}`))
	}))
	defer server.Close()
	client, err := New(
		server.URL+"/json/v501", "client-id", "secret",
		CallbackRedirectURI, server.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.CreateCampaignDraft(
		context.Background(), "token", "", CampaignDraft{
			Name: "Campaign", WeeklyBudgetMinor: 30_000,
			StartsAt: time.Date(2044, time.January, 2, 0, 0, 0, 0, time.UTC),
			EndsAt:   time.Date(2044, time.February, 2, 0, 0, 0, 0, time.UTC),
			TimeZone: "Europe/Moscow", OperationMarker: "test_marker",
		},
	)
	var providerErr *Error
	if !errors.As(err, &providerErr) || providerErr.Code != "6000" {
		t.Fatalf("campaign rejection = %#v, want numeric per-item code", err)
	}
	if strings.Contains(err.Error(), "private detail") {
		t.Fatalf("provider detail leaked through public error: %v", err)
	}
}

func TestResumeCampaignUsesV501ResumeResultsContract(t *testing.T) {
	t.Parallel()
	const campaignID int64 = 7004
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/v501/campaigns" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer access-token" ||
			r.Header.Get("Client-Login") != "client-login" {
			t.Errorf("auth headers were not set")
		}
		var payload struct {
			Method string `json:"method"`
			Params struct {
				SelectionCriteria struct {
					IDs []int64 `json:"Ids"`
				} `json:"SelectionCriteria"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		if payload.Method != "resume" ||
			!reflect.DeepEqual(payload.Params.SelectionCriteria.IDs, []int64{campaignID}) {
			t.Errorf("resume payload = %#v", payload)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"result":{"ResumeResults":[{
"Id":%d,
"Warnings":[{"Code":100,"Message":"already eligible","Details":"provider warning"}]
}]}}`, campaignID)
	}))
	defer server.Close()
	client, err := New(
		server.URL+"/json/v501", "client-id", "secret",
		CallbackRedirectURI, server.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.ResumeCampaign(
		context.Background(), "access-token", "client-login", campaignID,
	); err != nil {
		t.Fatalf("ResumeCampaign: %v", err)
	}
}

func TestResumeCampaignRejectsMalformedOrMismatchedActionResults(t *testing.T) {
	t.Parallel()
	const campaignID int64 = 7004
	tests := []struct {
		name     string
		response string
		wantCode string
	}{
		{
			name:     "legacy generic field is not accepted",
			response: `{"result":{"ActionResults":[{"Id":7004}]}}`,
			wantCode: "campaign_resume_failed",
		},
		{
			name:     "missing result",
			response: `{"result":{"ResumeResults":[]}}`,
			wantCode: "campaign_resume_failed",
		},
		{
			name: "multiple results",
			response: `{"result":{"ResumeResults":[
				{"Id":7004},{"Id":7004}
			]}}`,
			wantCode: "campaign_resume_failed",
		},
		{
			name:     "wrong campaign id",
			response: `{"result":{"ResumeResults":[{"Id":7005}]}}`,
			wantCode: "invalid_campaign_resume_response",
		},
		{
			name: "per item error is preserved before id validation",
			response: `{"result":{"ResumeResults":[{
				"Id":0,
				"Errors":[{"Code":8800,"Message":"cannot resume","Details":"provider detail"}]
			}]}}`,
			wantCode: "8800",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, test.response)
			}))
			defer server.Close()
			client, err := New(
				server.URL+"/json/v501", "client-id", "secret",
				CallbackRedirectURI, server.Client(),
			)
			if err != nil {
				t.Fatal(err)
			}
			err = client.ResumeCampaign(
				context.Background(), "access-token", "client-login", campaignID,
			)
			var providerErr *Error
			if !errors.As(err, &providerErr) {
				t.Fatalf("ResumeCampaign error = %#v, want *Error", err)
			}
			if providerErr.Code != test.wantCode {
				t.Fatalf("ResumeCampaign error code = %q, want %q",
					providerErr.Code, test.wantCode)
			}
		})
	}
}

func TestOAuthCodeExchangeAndRefreshParseRotatingTokenWithoutLeaks(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
			return
		}
		switch calls.Add(1) {
		case 1:
			if r.Form.Get("grant_type") != "authorization_code" ||
				r.Form.Get("code") != "A1b2C3d4E5f6G7h8" ||
				r.Form.Get("code_verifier") != "pkce-verifier" ||
				r.Form.Get("client_id") != "client-id" ||
				r.Form.Get("client_secret") != "client-secret" {
				t.Errorf("authorization form = %v", r.Form)
			}
			_, _ = w.Write([]byte(`{
				"token_type":"bearer",
				"access_token":"first-access",
				"refresh_token":"first-refresh",
				"expires_in":3600
			}`))
		case 2:
			if r.Form.Get("grant_type") != "refresh_token" ||
				r.Form.Get("refresh_token") != "first-refresh" ||
				r.Form.Get("client_id") != "client-id" ||
				r.Form.Get("client_secret") != "client-secret" {
				t.Errorf("refresh form = %v", r.Form)
			}
			_, _ = w.Write([]byte(`{
				"token_type":"Bearer",
				"access_token":"second-access",
				"refresh_token":"rotated-refresh",
				"expires_in":7200
			}`))
		default:
			t.Errorf("unexpected OAuth call")
		}
	}))
	defer oauth.Close()
	client, err := New(
		DefaultSandboxAPIBaseURL, "client-id", "client-secret",
		VerificationCodeRedirectURI, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	client.oauthTokenURL = oauth.URL
	issued, err := client.ExchangeCode(
		context.Background(), "A1b2C3d4E5f6G7h8", "pkce-verifier",
	)
	if err != nil {
		t.Fatal(err)
	}
	if issued.AccessToken != "first-access" ||
		issued.RefreshToken != "first-refresh" ||
		issued.ExpiresInSeconds != 3600 {
		t.Fatalf("issued token = %#v", issued)
	}
	refreshed, err := client.RefreshToken(context.Background(), issued.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.AccessToken != "second-access" ||
		refreshed.RefreshToken != "rotated-refresh" ||
		refreshed.ExpiresInSeconds != 7200 {
		t.Fatalf("refreshed token = %#v", refreshed)
	}
}

func TestOAuthTokenErrorsRejectIncompletePayloadAndNeverFollowRedirect(t *testing.T) {
	t.Parallel()
	targetCalls := atomic.Int32{}
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetCalls.Add(1)
	}))
	defer target.Close()
	for _, test := range []struct {
		name       string
		statusCode int
		body       string
		redirect   bool
		errorCode  string
	}{
		{
			name: "invalid grant", statusCode: http.StatusBadRequest,
			body:      `{"error":"invalid_grant","error_description":"secret refresh-token-value"}`,
			errorCode: "invalid_grant",
		},
		{
			name: "missing rotated refresh token", statusCode: http.StatusOK,
			body:      `{"token_type":"bearer","access_token":"access","expires_in":3600}`,
			errorCode: "invalid_oauth_response",
		},
		{
			name: "redirect", statusCode: http.StatusTemporaryRedirect,
			redirect: true, errorCode: "invalid_oauth_response",
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if test.redirect {
					w.Header().Set("Location", target.URL)
					w.WriteHeader(test.statusCode)
					return
				}
				w.WriteHeader(test.statusCode)
				_, _ = w.Write([]byte(test.body))
			}))
			defer oauth.Close()
			client, err := New(
				DefaultSandboxAPIBaseURL, "client-id", "client-secret",
				VerificationCodeRedirectURI, nil,
			)
			if err != nil {
				t.Fatal(err)
			}
			client.oauthTokenURL = oauth.URL
			_, err = client.RefreshToken(context.Background(), "refresh-token-value")
			var providerErr *Error
			if !errors.As(err, &providerErr) || providerErr.Code != test.errorCode {
				t.Fatalf("error = %#v, want code %q", err, test.errorCode)
			}
			for _, secret := range []string{
				"refresh-token-value", "client-secret", "secret refresh-token-value",
			} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error leaked %q: %v", secret, err)
				}
			}
		})
	}
	if targetCalls.Load() != 0 {
		t.Fatalf("OAuth redirect target calls = %d", targetCalls.Load())
	}
}

func TestCreateCampaignUsesEndpointSpecificDocumentedShape(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		basePath      string
		campaignField string
	}{
		{name: "unified production API", basePath: "/json/v501", campaignField: "UnifiedCampaign"},
		{name: "text sandbox-compatible API", basePath: "/json/v5", campaignField: "TextCampaign"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != test.basePath+"/campaigns" {
					t.Errorf("path = %q", r.URL.Path)
				}
				if r.Header.Get("Authorization") != "Bearer access-token" ||
					r.Header.Get("Client-Login") != "client-login" {
					t.Errorf("auth headers were not set")
				}
				var payload map[string]any
				decoder := json.NewDecoder(r.Body)
				decoder.UseNumber()
				if err := decoder.Decode(&payload); err != nil {
					t.Error(err)
					return
				}
				params := payload["params"].(map[string]any)
				campaign := params["Campaigns"].([]any)[0].(map[string]any)
				if _, ok := campaign[test.campaignField]; !ok {
					t.Errorf("campaign payload = %#v, missing %s", campaign, test.campaignField)
				}
				other := "TextCampaign"
				if test.campaignField == other {
					other = "UnifiedCampaign"
				}
				if _, ok := campaign[other]; ok {
					t.Errorf("campaign payload unexpectedly contains %s", other)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, `{"result":{"AddResults":[{"Id":7001}]}}`)
			}))
			defer server.Close()
			client, err := New(
				server.URL+test.basePath, "client-id", "secret",
				CallbackRedirectURI, server.Client(),
			)
			if err != nil {
				t.Fatal(err)
			}
			campaign, err := client.CreateCampaignDraft(
				context.Background(), "access-token", "client-login", CampaignDraft{
					Name: "Test", WeeklyBudgetMinor: 12_345,
					StartsAt: time.Date(2044, 1, 2, 0, 0, 0, 0, time.UTC),
					EndsAt:   time.Date(2044, 2, 2, 0, 0, 0, 0, time.UTC),
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			if campaign.ID != 7001 || campaign.WeeklyBudgetMinor != 12_345 {
				t.Fatalf("campaign = %#v", campaign)
			}
		})
	}
}

func TestCreateUnifiedCampaignPinsGraphSafeDefaultsAndPreservesWarnings(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Params struct {
				Campaigns []struct {
					TimeZone      string `json:"TimeZone"`
					TimeTargeting struct {
						Schedule struct {
							Items []string `json:"Items"`
						} `json:"Schedule"`
						ConsiderWorkingWeekends string `json:"ConsiderWorkingWeekends"`
					} `json:"TimeTargeting"`
					UnifiedCampaign struct {
						BiddingStrategy map[string]json.RawMessage `json:"BiddingStrategy"`
						Settings        []GraphCampaignSetting     `json:"Settings"`
						TrackingParams  string                     `json:"TrackingParams"`
					} `json:"UnifiedCampaign"`
				} `json:"Campaigns"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		if len(payload.Params.Campaigns) != 1 {
			t.Fatalf("campaign count = %d", len(payload.Params.Campaigns))
		}
		campaign := payload.Params.Campaigns[0]
		if campaign.TimeZone != "Asia/Yekaterinburg" {
			t.Errorf("time zone = %q", campaign.TimeZone)
		}
		if len(campaign.TimeTargeting.Schedule.Items) != 7 ||
			campaign.TimeTargeting.ConsiderWorkingWeekends != "NO" {
			t.Errorf("time targeting = %#v", campaign.TimeTargeting)
		}
		for index, schedule := range campaign.TimeTargeting.Schedule.Items {
			parts := strings.Split(schedule, ",")
			if len(parts) != 25 || parts[0] != strconv.Itoa(index+1) {
				t.Errorf("schedule %d = %q", index, schedule)
				continue
			}
			for _, percent := range parts[1:] {
				if percent != "100" {
					t.Errorf("schedule %d contains %q", index, percent)
				}
			}
		}
		tracking, err := url.ParseQuery(campaign.UnifiedCampaign.TrackingParams)
		if err != nil || tracking.Get("mp_op") != "submission_42" || len(tracking) != 1 {
			t.Errorf("tracking params = %q, err=%v", campaign.UnifiedCampaign.TrackingParams, err)
		}
		wantSettings := SafeUnifiedCampaignSettings()
		if fmt.Sprint(campaign.UnifiedCampaign.Settings) != fmt.Sprint(wantSettings) {
			t.Errorf("settings = %#v, want %#v", campaign.UnifiedCampaign.Settings, wantSettings)
		}
		for _, setting := range campaign.UnifiedCampaign.Settings {
			if setting.Value != "NO" {
				t.Errorf("unsafe setting = %#v", setting)
			}
		}
		var search struct {
			BiddingStrategyType string            `json:"BiddingStrategyType"`
			PlacementTypes      map[string]string `json:"PlacementTypes"`
		}
		if err := json.Unmarshal(campaign.UnifiedCampaign.BiddingStrategy["Search"], &search); err != nil {
			t.Fatal(err)
		}
		if search.BiddingStrategyType != "SERVING_OFF" ||
			!reflect.DeepEqual(search.PlacementTypes, map[string]string{
				"SearchResults":          "NO",
				"ProductGallery":         "NO",
				"DynamicPlaces":          "NO",
				"Maps":                   "NO",
				"SearchOrganizationList": "NO",
			}) {
			t.Errorf("search strategy = %#v", search)
		}
		var network struct {
			BiddingStrategyType string            `json:"BiddingStrategyType"`
			PlacementTypes      map[string]string `json:"PlacementTypes"`
			WbMaximumClicks     struct {
				WeeklySpendLimit int64 `json:"WeeklySpendLimit"`
			} `json:"WbMaximumClicks"`
		}
		if err := json.Unmarshal(campaign.UnifiedCampaign.BiddingStrategy["Network"], &network); err != nil {
			t.Fatal(err)
		}
		if network.BiddingStrategyType != "WB_MAXIMUM_CLICKS" ||
			network.WbMaximumClicks.WeeklySpendLimit != 123_450_000 ||
			network.PlacementTypes["Network"] != "YES" ||
			network.PlacementTypes["Maps"] != "NO" {
			t.Errorf("network strategy = %#v", network)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"result":{"AddResults":[{
"Id":7001,"Warnings":[{"Code":100,"Message":"normalized","Details":"provider detail"}]
}]}}`)
	}))
	defer server.Close()
	client, err := New(
		server.URL+"/json/v501", "client-id", "secret",
		CallbackRedirectURI, server.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}
	campaign, err := client.CreateCampaignDraft(
		context.Background(), "access-token", "client-login", CampaignDraft{
			Name: "Safe", WeeklyBudgetMinor: 12_345,
			StartsAt: time.Date(2044, 1, 2, 0, 0, 0, 0, time.UTC),
			EndsAt:   time.Date(2044, 2, 2, 0, 0, 0, 0, time.UTC),
			TimeZone: "Asia/Yekaterinburg", OperationMarker: "submission_42",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if campaign.TimeZone != "Asia/Yekaterinburg" ||
		campaign.TrackingParams != "mp_op=submission_42" ||
		len(campaign.Warnings) != 1 || campaign.Warnings[0].Code != 100 ||
		campaign.Warnings[0].Details != "provider detail" {
		t.Fatalf("campaign = %#v", campaign)
	}
}

func TestCreateUnifiedCampaignRejectsInvalidOperationMarkerBeforeWrite(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()
	client, err := New(
		server.URL+"/json/v501", "client-id", "secret",
		CallbackRedirectURI, server.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.CreateCampaignDraft(
		context.Background(), "access-token", "client-login", CampaignDraft{
			Name: "Unsafe", WeeklyBudgetMinor: 12_345,
			StartsAt:        time.Date(2044, 1, 2, 0, 0, 0, 0, time.UTC),
			EndsAt:          time.Date(2044, 2, 2, 0, 0, 0, 0, time.UTC),
			OperationMarker: "contains spaces",
		},
	)
	if err == nil || calls.Load() != 0 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestNewRejectsSOAPV501PathForJSONClient(t *testing.T) {
	t.Parallel()
	_, err := New(
		"https://api.direct.yandex.com/v501", "client-id", "secret",
		CallbackRedirectURI, nil,
	)
	if err == nil {
		t.Fatal("SOAP /v501 path was accepted by the JSON client")
	}
}

func TestGetCampaignPreservesIntegerBudgetAboveFloatPrecision(t *testing.T) {
	t.Parallel()
	const budgetMicros int64 = 9_007_199_254_740_000
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"result":{"Campaigns":[{
"Id":7002,"Name":"Exact","Status":"ACCEPTED","State":"OFF",
"StartDate":"2044-01-02","EndDate":"2044-02-02",
"UnifiedCampaign":{"BiddingStrategy":{
"Search":{"BiddingStrategyType":"SERVING_OFF"},
"Network":{"BiddingStrategyType":"WB_MAXIMUM_CLICKS","WbMaximumClicks":{"WeeklySpendLimit":%d}}
}}
}]}}`, budgetMicros)
	}))
	defer server.Close()
	client, err := New(
		server.URL+"/json/v501", "client-id", "secret",
		CallbackRedirectURI, server.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}
	campaign, err := client.GetCampaign(context.Background(), "token", "", 7002)
	if err != nil {
		t.Fatal(err)
	}
	if campaign.WeeklyBudgetMinor != budgetMicros/10_000 {
		t.Fatalf("minor budget = %d, want %d", campaign.WeeklyBudgetMinor, budgetMicros/10_000)
	}
}

func TestFindWeeklySpendLimitUsesExactJSONNumber(t *testing.T) {
	t.Parallel()
	const exact int64 = 9_007_199_254_740_993
	value, ok := findWeeklySpendLimit(json.RawMessage(
		`{"BiddingStrategy":{
"Search":{"BiddingStrategyType":"SERVING_OFF"},
"Network":{"BiddingStrategyType":"WB_MAXIMUM_CLICKS","WbMaximumClicks":{"WeeklySpendLimit":9007199254740993}}
}}`,
	))
	if !ok || value != exact {
		t.Fatalf("value=%d ok=%v, want exact %d", value, ok, exact)
	}
}

func TestGetAccountFailsClosedForAgencyUnknownOrNonChiefActors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		itemType string
		role     string
		grant    string
		readOnly bool
	}{
		{
			name: "direct chief with explicit edit grant", itemType: "CLIENT",
			role: "CHIEF", grant: "YES", readOnly: false,
		},
		{
			name: "agency is unsupported", itemType: "AGENCY",
			role: "CHIEF", grant: "YES", readOnly: true,
		},
		{
			name: "unknown account type", itemType: "",
			role: "CHIEF", grant: "YES", readOnly: true,
		},
		{
			name: "delegate actor", itemType: "CLIENT",
			role: "DELEGATE", grant: "YES", readOnly: true,
		},
		{
			name: "missing edit grant", itemType: "CLIENT",
			role: "CHIEF", grant: "", readOnly: true,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload struct {
					Params struct {
						FieldNames []string `json:"FieldNames"`
					} `json:"params"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
					return
				}
				foundType := false
				for _, field := range payload.Params.FieldNames {
					foundType = foundType || field == "Type"
				}
				if !foundType {
					t.Error("Clients.get did not request the account Type")
				}
				_, _ = fmt.Fprintf(w, `{"result":{"Clients":[{
"ClientId":7003,"Login":"client-login","ClientInfo":"Direct client","Currency":"RUB",
"Type":%q,"Representatives":[{"Login":"client-login","Role":%q}],
"Grants":[{"Privilege":"EDIT_CAMPAIGNS","Value":%q}]
}]}}`, test.itemType, test.role, test.grant)
			}))
			defer server.Close()
			client, err := New(
				server.URL+"/json/v501", "client-id", "secret",
				CallbackRedirectURI, server.Client(),
			)
			if err != nil {
				t.Fatal(err)
			}
			account, err := client.GetAccount(context.Background(), "token", "")
			if err != nil {
				t.Fatal(err)
			}
			if account.ReadOnly != test.readOnly {
				t.Fatalf("account read_only = %t, want %t: %#v",
					account.ReadOnly, test.readOnly, account)
			}
		})
	}
}

func TestAPIErrorPreservesInvalidTokenCode(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"error":{
"error_code":53,"error_string":"Authorization error","error_detail":"Invalid OAuth token"
}}`)
	}))
	defer server.Close()
	client, err := New(
		server.URL+"/json/v501", "client-id", "secret",
		CallbackRedirectURI, server.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.GetAccount(context.Background(), "expired-token", "")
	var providerErr *Error
	if !errors.As(err, &providerErr) {
		t.Fatalf("GetAccount error = %v, want *Error", err)
	}
	if providerErr.APIErrorCode != 53 || providerErr.Code != "Authorization error" {
		t.Fatalf("provider error = %#v, want API error 53 with provider text", providerErr)
	}
}

func TestFindWeeklySpendLimitRejectsDuplicateOrStrategyDrift(t *testing.T) {
	t.Parallel()
	tests := []json.RawMessage{
		json.RawMessage(`{"WeeklySpendLimit":300000000,"BiddingStrategy":{
"Search":{"BiddingStrategyType":"SERVING_OFF"},
"Network":{"BiddingStrategyType":"WB_MAXIMUM_CLICKS","WbMaximumClicks":{"WeeklySpendLimit":300000000}}
}}`),
		json.RawMessage(`{"BiddingStrategy":{
"Search":{"BiddingStrategyType":"HIGHEST_POSITION"},
"Network":{"BiddingStrategyType":"WB_MAXIMUM_CLICKS","WbMaximumClicks":{"WeeklySpendLimit":300000000}}
}}`),
		json.RawMessage(`{"BiddingStrategy":{
"Search":{"BiddingStrategyType":"SERVING_OFF"},
"Network":{"BiddingStrategyType":"SERVING_OFF","WbMaximumClicks":{"WeeklySpendLimit":300000000}}
}}`),
		json.RawMessage(`{"BiddingStrategy":{
"Search":{"BiddingStrategyType":"SERVING_OFF"},
"Network":{"BiddingStrategyType":"WB_MAXIMUM_CLICKS","WbMaximumClicks":{"WeeklySpendLimit":300000000}},
"CustomPeriodBudget":{"Amount":900000000}
}}`),
	}
	for _, raw := range tests {
		if value, ok := findWeeklySpendLimit(raw); ok {
			t.Fatalf("unsafe strategy was accepted with budget %d: %s", value, raw)
		}
	}
}

func TestMoneyConversionRejectsOverflowAndFractionalMinorUnits(t *testing.T) {
	t.Parallel()
	if _, err := MinorToMicros(math.MaxInt64/10_000 + 1); err == nil {
		t.Fatal("overflowing minor amount was accepted")
	}
	if _, err := MicrosToMinor(10_001); err == nil {
		t.Fatal("fractional minor amount was accepted")
	}
	if value, err := MinorToMicros(123); err != nil || value != 1_230_000 {
		t.Fatalf("MinorToMicros = %d, %v", value, err)
	}
}

func TestDirectClientNeverFollowsRedirectWithBearerToken(t *testing.T) {
	t.Parallel()
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Add(1)
		if strings.Contains(r.Header.Get("Authorization"), "access-token") {
			t.Error("bearer token reached redirected host")
		}
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	client, err := New(
		source.URL+"/json/v501", "client-id", "secret",
		CallbackRedirectURI, source.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = client.CreateCampaignDraft(context.Background(), "access-token", "", CampaignDraft{
		Name: "Redirect", WeeklyBudgetMinor: 100,
		StartsAt: time.Now().UTC(), EndsAt: time.Now().UTC().AddDate(0, 1, 0),
	})
	if redirected.Load() != 0 {
		t.Fatalf("redirect target calls = %d", redirected.Load())
	}
}

func TestSandboxRejectsUndocumentedUnifiedEndpoint(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"/v501", "/json/v501"} {
		if _, err := New(
			"https://api-sandbox.direct.yandex.com"+path,
			"client-id", "secret",
			"https://maxposty.ru/api/v1/advertising/direct/oauth/callback", nil,
		); err == nil {
			t.Fatalf("undocumented sandbox %s endpoint was accepted", path)
		}
	}
	client, err := New(
		DefaultSandboxAPIBaseURL, "client-id", "secret",
		"https://maxposty.ru/api/v1/advertising/direct/oauth/callback", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !client.Sandbox() || client.unified {
		t.Fatalf("sandbox client = %#v", client)
	}
}
