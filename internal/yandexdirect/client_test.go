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
	if _, err := New(
		"https://api-sandbox.direct.yandex.com/json/v501", "client-id", "secret",
		"https://maxposty.ru/api/v1/advertising/direct/oauth/callback", nil,
	); err == nil {
		t.Fatal("undocumented sandbox /json/v501 endpoint was accepted")
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
