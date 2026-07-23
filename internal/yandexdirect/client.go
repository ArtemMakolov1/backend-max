package yandexdirect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	DefaultAPIBaseURL           = "https://api.direct.yandex.com/json/v501"
	DefaultSandboxAPIBaseURL    = "https://api-sandbox.direct.yandex.com/json/v5"
	CallbackRedirectURI         = "https://maxposty.ru/api/v1/advertising/direct/oauth/callback"
	VerificationCodeRedirectURI = "https://oauth.yandex.ru/verification_code"
	oauthAuthorizeURL           = "https://oauth.yandex.ru/authorize"
	oauthExchangeEndpoint       = "https://oauth.yandex.ru/token"
)

type OAuthFlow string

const (
	OAuthFlowCallback         OAuthFlow = "callback"
	OAuthFlowVerificationCode OAuthFlow = "verification_code"
)

type Client struct {
	baseURL       *url.URL
	clientID      string
	clientSecret  string
	redirectURI   string
	oauthFlow     OAuthFlow
	http          *http.Client
	sandbox       bool
	unified       bool
	oauthTokenURL string
}

type Error struct {
	StatusCode   int
	APIErrorCode int
	Code         string
	Message      string
	RequestID    string
}

func (e *Error) Error() string {
	if e == nil {
		return "Yandex Direct request failed"
	}
	if e.Code != "" {
		return "Yandex Direct request failed: " + e.Code
	}
	return "Yandex Direct request failed"
}

type Account struct {
	ID           string
	Login        string
	DisplayName  string
	CurrencyCode string
	Timezone     string
	ReadOnly     bool
}

type OAuthToken struct {
	AccessToken      string
	RefreshToken     string
	ExpiresInSeconds int64
}

type CampaignDraft struct {
	Name              string
	WeeklyBudgetMinor int64
	StartsAt          time.Time
	EndsAt            time.Time
	TimeZone          string
	OperationMarker   string
}

type Campaign struct {
	ID                int64
	Name              string
	Status            string
	State             string
	WeeklyBudgetMinor int64
	StartsAt          time.Time
	EndsAt            time.Time
	TimeZone          string
	TrackingParams    string
	Warnings          []ProviderIssue
}

// SafeUnifiedCampaignSettings is the complete set of mutable YES/NO settings
// accepted for a v501 UnifiedCampaign. Returning every option explicitly
// avoids provider defaults enabling content or targeting features outside the
// graph authorized by the user.
func SafeUnifiedCampaignSettings() []GraphCampaignSetting {
	return []GraphCampaignSetting{
		{Option: "ADD_METRICA_TAG", Value: "NO"},
		{Option: "ADD_TO_FAVORITES", Value: "NO"},
		{Option: "ALTERNATIVE_TEXTS_ENABLED", Value: "NO"},
		{Option: "CAMPAIGN_EXACT_PHRASE_MATCHING_ENABLED", Value: "NO"},
		{Option: "ENABLE_AREA_OF_INTEREST_TARGETING", Value: "NO"},
		{Option: "ENABLE_COMPANY_INFO", Value: "NO"},
		{Option: "ENABLE_SITE_MONITORING", Value: "NO"},
		{Option: "REQUIRE_SERVICING", Value: "NO"},
	}
}

// SafeUnifiedCampaignBiddingStrategy returns the only delivery strategy the
// graph integration creates. Search is explicitly disabled and the network
// strategy is bounded by the exact weekly budget, with Maps disabled.
func SafeUnifiedCampaignBiddingStrategy(weeklyBudgetMinor int64) (json.RawMessage, error) {
	weeklyBudgetMicros, err := MinorToMicros(weeklyBudgetMinor)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"Search": map[string]any{
			"BiddingStrategyType": "SERVING_OFF",
			"PlacementTypes": map[string]any{
				"SearchResults":          "NO",
				"ProductGallery":         "NO",
				"DynamicPlaces":          "NO",
				"Maps":                   "NO",
				"SearchOrganizationList": "NO",
			},
		},
		"Network": map[string]any{
			"BiddingStrategyType": "WB_MAXIMUM_CLICKS",
			"PlacementTypes": map[string]any{
				"Maps":    "NO",
				"Network": "YES",
			},
			"WbMaximumClicks": map[string]any{
				"WeeklySpendLimit": weeklyBudgetMicros,
			},
		},
	})
}

// SafeUnifiedCampaignTimeTargeting fixes the product policy at 24/7 delivery.
// Keeping it explicit prevents a provider-side default from silently changing
// the graph authorized by the user.
func SafeUnifiedCampaignTimeTargeting() GraphTimeTargeting {
	schedule := make([]string, 0, 7)
	for day := 1; day <= 7; day++ {
		hours := make([]string, 25)
		hours[0] = strconv.Itoa(day)
		for hour := 1; hour < len(hours); hour++ {
			hours[hour] = "100"
		}
		schedule = append(schedule, strings.Join(hours, ","))
	}
	return GraphTimeTargeting{
		Present: true, Schedule: schedule, ConsiderWorkingWeekends: "NO",
	}
}

func New(
	apiBaseURL, clientID, clientSecret, redirectURI string, httpClient *http.Client,
) (*Client, error) {
	baseURL, err := url.Parse(strings.TrimRight(strings.TrimSpace(apiBaseURL), "/"))
	if err != nil || !baseURL.IsAbs() || baseURL.User != nil || baseURL.RawQuery != "" || baseURL.Fragment != "" {
		return nil, errors.New("invalid Yandex Direct API base URL")
	}
	host := strings.ToLower(baseURL.Hostname())
	isLoopback := net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback() || host == "localhost"
	if baseURL.Scheme != "https" && (baseURL.Scheme != "http" || !isLoopback) {
		return nil, errors.New("API base URL for Yandex Direct must use HTTPS outside localhost")
	}
	if !isLoopback && host != "api.direct.yandex.com" && host != "api-sandbox.direct.yandex.com" {
		return nil, errors.New("API base URL host for Yandex Direct is not allowed")
	}
	basePath := strings.TrimRight(baseURL.Path, "/")
	unified := strings.HasSuffix(basePath, "/json/v501")
	textV5 := strings.HasSuffix(basePath, "/json/v5")
	if !unified && !textV5 {
		return nil, errors.New("API base URL for Yandex Direct must target /json/v501 or /json/v5")
	}
	if host == "api-sandbox.direct.yandex.com" && !textV5 {
		return nil, errors.New("sandbox for Yandex Direct supports the documented /json/v5 endpoint only")
	}
	normalizedRedirectURI := strings.TrimSpace(redirectURI)
	redirect, err := url.Parse(normalizedRedirectURI)
	if err != nil {
		return nil, errors.New("invalid Yandex Direct OAuth redirect URI")
	}
	if !redirect.IsAbs() || redirect.User != nil || redirect.RawQuery != "" ||
		redirect.Fragment != "" || redirect.Scheme != "https" {
		return nil, errors.New("invalid Yandex Direct OAuth redirect URI")
	}
	oauthFlow := OAuthFlowCallback
	if redirect.String() == VerificationCodeRedirectURI {
		oauthFlow = OAuthFlowVerificationCode
	} else if redirect.String() != CallbackRedirectURI {
		return nil, errors.New("redirect URI for Yandex Direct OAuth is not in the fixed allowlist")
	}
	if strings.TrimSpace(clientID) == "" || strings.TrimSpace(clientSecret) == "" {
		return nil, errors.New("OAuth client credentials for Yandex Direct are required")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 20 * time.Second}
	}
	safeHTTPClient := *httpClient
	// Never forward a bearer token or OAuth secret through an upstream
	// redirect, even when a caller supplied a permissive default client.
	safeHTTPClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &Client{
		baseURL: baseURL, clientID: strings.TrimSpace(clientID), clientSecret: clientSecret,
		redirectURI: redirect.String(), oauthFlow: oauthFlow, http: &safeHTTPClient,
		sandbox: host == "api-sandbox.direct.yandex.com", unified: unified,
		oauthTokenURL: oauthExchangeEndpoint,
	}, nil
}

func (c *Client) Sandbox() bool { return c != nil && c.sandbox }

func (c *Client) OAuthFlow() OAuthFlow {
	if c == nil {
		return ""
	}
	return c.oauthFlow
}

func (c *Client) AuthorizationURL(state, codeChallenge string) string {
	query := url.Values{
		"response_type":         {"code"},
		"client_id":             {c.clientID},
		"redirect_uri":          {c.redirectURI},
		"scope":                 {"direct:api"},
		"state":                 {state},
		"code_challenge":        {codeChallenge},
		"code_challenge_method": {"S256"},
	}
	return oauthAuthorizeURL + "?" + query.Encode()
}

func (c *Client) ExchangeCode(
	ctx context.Context, code, verifier string,
) (OAuthToken, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {strings.TrimSpace(code)},
		"client_id":     {c.clientID},
		"client_secret": {c.clientSecret},
		"redirect_uri":  {c.redirectURI},
		"code_verifier": {verifier},
	}
	return c.exchangeOAuthToken(ctx, form)
}

func (c *Client) RefreshToken(
	ctx context.Context, refreshToken string,
) (OAuthToken, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {strings.TrimSpace(refreshToken)},
		"client_id":     {c.clientID},
		"client_secret": {c.clientSecret},
	}
	return c.exchangeOAuthToken(ctx, form)
}

func (c *Client) exchangeOAuthToken(
	ctx context.Context, form url.Values,
) (OAuthToken, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.oauthTokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return OAuthToken{}, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return OAuthToken{}, fmt.Errorf("exchange Yandex Direct OAuth token: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return OAuthToken{}, err
	}
	var payload struct {
		AccessToken  string      `json:"access_token"`
		RefreshToken string      `json:"refresh_token"`
		ExpiresIn    json.Number `json:"expires_in"`
		TokenType    string      `json:"token_type"`
		Error        string      `json:"error"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return OAuthToken{}, &Error{StatusCode: response.StatusCode, Code: "invalid_oauth_response"}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		code := strings.TrimSpace(payload.Error)
		if code == "" {
			code = "oauth_exchange_failed"
		}
		return OAuthToken{}, &Error{StatusCode: response.StatusCode, Code: code}
	}
	expiresIn, err := strconv.ParseInt(payload.ExpiresIn.String(), 10, 64)
	if err != nil || expiresIn <= 0 ||
		!strings.EqualFold(strings.TrimSpace(payload.TokenType), "bearer") ||
		strings.TrimSpace(payload.AccessToken) == "" ||
		strings.TrimSpace(payload.RefreshToken) == "" {
		return OAuthToken{}, &Error{
			StatusCode: response.StatusCode,
			Code:       "invalid_oauth_response",
		}
	}
	return OAuthToken{
		AccessToken:      strings.TrimSpace(payload.AccessToken),
		RefreshToken:     strings.TrimSpace(payload.RefreshToken),
		ExpiresInSeconds: expiresIn,
	}, nil
}

func (c *Client) GetAccount(
	ctx context.Context, token, clientLogin string,
) (Account, error) {
	var response struct {
		Clients []struct {
			ClientID        int64  `json:"ClientId"`
			Login           string `json:"Login"`
			ClientInfo      string `json:"ClientInfo"`
			Currency        string `json:"Currency"`
			Type            string `json:"Type"`
			Representatives []struct {
				Login string `json:"Login"`
				Role  string `json:"Role"`
			} `json:"Representatives"`
			Grants []struct {
				Privilege string `json:"Privilege"`
				Value     string `json:"Value"`
			} `json:"Grants"`
		} `json:"Clients"`
	}
	err := c.call(ctx, "clients", token, clientLogin, map[string]any{
		"method": "get",
		"params": map[string]any{
			"FieldNames": []string{
				"ClientId", "Login", "ClientInfo", "Currency", "Type",
				"Representatives", "Grants",
			},
		},
	}, &response)
	if err != nil {
		return Account{}, err
	}
	if len(response.Clients) != 1 || response.Clients[0].ClientID <= 0 {
		return Account{}, &Error{Code: "direct_account_not_found"}
	}
	item := response.Clients[0]
	chiefVerified := false
	for _, representative := range item.Representatives {
		if strings.EqualFold(representative.Login, item.Login) &&
			strings.EqualFold(representative.Role, "CHIEF") {
			chiefVerified = true
		}
	}
	editCampaigns := false
	for _, grant := range item.Grants {
		if strings.EqualFold(grant.Privilege, "EDIT_CAMPAIGNS") &&
			strings.EqualFold(grant.Value, "YES") {
			editCampaigns = true
		}
	}
	// The MVP does not implement AgencyClients selection and cannot safely
	// infer permissions for agencies, delegates, or unknown account types.
	// Default to read-only unless every provider assertion is explicit.
	readOnly := !strings.EqualFold(strings.TrimSpace(item.Type), "CLIENT") ||
		!chiefVerified || !editCampaigns
	return Account{
		ID: strconv.FormatInt(item.ClientID, 10), Login: item.Login,
		DisplayName: item.ClientInfo, CurrencyCode: item.Currency,
		Timezone: "Europe/Moscow", ReadOnly: readOnly,
	}, nil
}

func (c *Client) CreateCampaignDraft(
	ctx context.Context, token, clientLogin string, campaign CampaignDraft,
) (Campaign, error) {
	weeklyBudgetMicros, err := MinorToMicros(campaign.WeeklyBudgetMinor)
	if err != nil {
		return Campaign{}, err
	}
	timeZone := strings.TrimSpace(campaign.TimeZone)
	if timeZone == "" {
		timeZone = "Europe/Moscow"
	}
	if utf8.RuneCountInString(timeZone) > 128 {
		return Campaign{}, errors.New("campaign time zone is too long")
	}
	operationMarker := strings.TrimSpace(campaign.OperationMarker)
	if operationMarker != "" {
		operationMarker, err = normalizeGraphMarker(
			operationMarker, "invalid_campaign_operation_marker",
		)
		if err != nil {
			return Campaign{}, err
		}
	}
	var response struct {
		AddResults []struct {
			ID       int64      `json:"Id"`
			Errors   []apiIssue `json:"Errors"`
			Warnings []apiIssue `json:"Warnings"`
		} `json:"AddResults"`
	}
	campaignType := "UnifiedCampaign"
	if !c.unified {
		campaignType = "TextCampaign"
	}
	campaignDetails := map[string]any{
		"BiddingStrategy": map[string]any{
			"Search": map[string]any{"BiddingStrategyType": "SERVING_OFF"},
			"Network": map[string]any{
				"BiddingStrategyType": "WB_MAXIMUM_CLICKS",
				"WbMaximumClicks": map[string]any{
					"WeeklySpendLimit": weeklyBudgetMicros,
				},
			},
		},
	}
	trackingParams := ""
	if c.unified {
		settings := SafeUnifiedCampaignSettings()
		settingsPayload := make([]map[string]string, 0, len(settings))
		for _, setting := range settings {
			settingsPayload = append(settingsPayload, map[string]string{
				"Option": setting.Option, "Value": setting.Value,
			})
		}
		campaignDetails["Settings"] = settingsPayload
		campaignDetails["AttributionModel"] = "AUTO"
		campaignDetails["BiddingStrategy"] = map[string]any{
			"Search": map[string]any{
				"BiddingStrategyType": "SERVING_OFF",
				"PlacementTypes": map[string]any{
					"SearchResults":          "NO",
					"ProductGallery":         "NO",
					"DynamicPlaces":          "NO",
					"Maps":                   "NO",
					"SearchOrganizationList": "NO",
				},
			},
			"Network": map[string]any{
				"BiddingStrategyType": "WB_MAXIMUM_CLICKS",
				"PlacementTypes": map[string]any{
					"Maps":    "NO",
					"Network": "YES",
				},
				"WbMaximumClicks": map[string]any{
					"WeeklySpendLimit": weeklyBudgetMicros,
				},
			},
		}
		if operationMarker != "" {
			trackingParams = url.Values{
				graphOperationMarkerParam: []string{operationMarker},
			}.Encode()
			campaignDetails["TrackingParams"] = trackingParams
		}
	}
	request := map[string]any{
		"method": "add",
		"params": map[string]any{
			"Campaigns": []any{map[string]any{
				"Name":       strings.TrimSpace(campaign.Name),
				"StartDate":  campaign.StartsAt.UTC().Format(time.DateOnly),
				"EndDate":    campaign.EndsAt.UTC().Format(time.DateOnly),
				"TimeZone":   timeZone,
				campaignType: campaignDetails,
			}},
		},
	}
	if c.unified {
		timeTargeting := SafeUnifiedCampaignTimeTargeting()
		request["params"].(map[string]any)["Campaigns"].([]any)[0].(map[string]any)["TimeTargeting"] =
			map[string]any{
				"Schedule":                map[string]any{"Items": timeTargeting.Schedule},
				"ConsiderWorkingWeekends": timeTargeting.ConsiderWorkingWeekends,
			}
	}
	if err := c.call(ctx, "campaigns", token, clientLogin, request, &response); err != nil {
		return Campaign{}, err
	}
	if len(response.AddResults) != 1 {
		return Campaign{}, &Error{Code: "campaign_add_failed"}
	}
	if len(response.AddResults[0].Errors) != 0 {
		return Campaign{}, issueError(response.AddResults[0].Errors[0], "campaign_add_failed")
	}
	if response.AddResults[0].ID <= 0 {
		return Campaign{}, &Error{Code: "campaign_add_failed"}
	}
	return Campaign{
		ID: response.AddResults[0].ID, Name: strings.TrimSpace(campaign.Name),
		Status: "DRAFT", State: "OFF", WeeklyBudgetMinor: campaign.WeeklyBudgetMinor,
		StartsAt: dateOnly(campaign.StartsAt), EndsAt: dateOnly(campaign.EndsAt),
		TimeZone: timeZone, TrackingParams: trackingParams,
		Warnings: exportProviderIssues(response.AddResults[0].Warnings),
	}, nil
}

func (c *Client) GetCampaign(
	ctx context.Context, token, clientLogin string, campaignID int64,
) (Campaign, error) {
	var response struct {
		Campaigns []json.RawMessage `json:"Campaigns"`
	}
	campaignFieldNames := "UnifiedCampaignFieldNames"
	if !c.unified {
		campaignFieldNames = "TextCampaignFieldNames"
	}
	err := c.call(ctx, "campaigns", token, clientLogin, map[string]any{
		"method": "get",
		"params": map[string]any{
			"SelectionCriteria": map[string]any{"Ids": []int64{campaignID}},
			"FieldNames":        []string{"Id", "Name", "Status", "State", "StartDate", "EndDate"},
			campaignFieldNames:  []string{"BiddingStrategy"},
		},
	}, &response)
	if err != nil {
		return Campaign{}, err
	}
	if len(response.Campaigns) != 1 {
		return Campaign{}, &Error{Code: "campaign_not_found"}
	}
	var envelope struct {
		ID              int64           `json:"Id"`
		Name            string          `json:"Name"`
		Status          string          `json:"Status"`
		State           string          `json:"State"`
		StartDate       string          `json:"StartDate"`
		EndDate         string          `json:"EndDate"`
		UnifiedCampaign json.RawMessage `json:"UnifiedCampaign"`
		TextCampaign    json.RawMessage `json:"TextCampaign"`
	}
	if err := json.Unmarshal(response.Campaigns[0], &envelope); err != nil {
		return Campaign{}, &Error{Code: "invalid_campaign_response"}
	}
	startsAt, err := time.Parse(time.DateOnly, envelope.StartDate)
	if err != nil {
		return Campaign{}, &Error{Code: "invalid_campaign_response"}
	}
	endsAt, err := time.Parse(time.DateOnly, envelope.EndDate)
	if err != nil {
		return Campaign{}, &Error{Code: "invalid_campaign_response"}
	}
	campaignDetails := envelope.UnifiedCampaign
	if !c.unified {
		campaignDetails = envelope.TextCampaign
	}
	budgetMicros, ok := findWeeklySpendLimit(campaignDetails)
	if !ok {
		return Campaign{}, &Error{Code: "campaign_budget_unavailable"}
	}
	budgetMinor, err := MicrosToMinor(budgetMicros)
	if err != nil {
		return Campaign{}, &Error{Code: "campaign_budget_invalid"}
	}
	return Campaign{
		ID: envelope.ID, Name: envelope.Name, Status: strings.ToUpper(envelope.Status),
		State: strings.ToUpper(envelope.State), WeeklyBudgetMinor: budgetMinor,
		StartsAt: dateOnly(startsAt), EndsAt: dateOnly(endsAt),
	}, nil
}

func (c *Client) ResumeCampaign(
	ctx context.Context, token, clientLogin string, campaignID int64,
) error {
	var response struct {
		ResumeResults []struct {
			ID       int64      `json:"Id"`
			Errors   []apiIssue `json:"Errors"`
			Warnings []apiIssue `json:"Warnings"`
		} `json:"ResumeResults"`
	}
	err := c.call(ctx, "campaigns", token, clientLogin, map[string]any{
		"method": "resume",
		"params": map[string]any{
			"SelectionCriteria": map[string]any{"Ids": []int64{campaignID}},
		},
	}, &response)
	if err != nil {
		return err
	}
	if len(response.ResumeResults) != 1 {
		return &Error{Code: "campaign_resume_failed"}
	}
	result := response.ResumeResults[0]
	if len(result.Errors) != 0 {
		return issueError(result.Errors[0], "campaign_resume_failed")
	}
	if result.ID != campaignID {
		return &Error{Code: "invalid_campaign_resume_response"}
	}
	// Provider warnings are intentionally non-fatal. The DirectProvider
	// contract returns only an error for resume, so there is no lossy mapping
	// from warnings to failures.
	return nil
}

func MinorToMicros(minor int64) (int64, error) {
	if minor <= 0 || minor > math.MaxInt64/10_000 {
		return 0, errors.New("weekly budget is outside the provider monetary range")
	}
	return minor * 10_000, nil
}

func MicrosToMinor(micros int64) (int64, error) {
	if micros <= 0 || micros%10_000 != 0 {
		return 0, errors.New("provider budget cannot be represented in minor units")
	}
	return micros / 10_000, nil
}

type apiIssue struct {
	Code    int    `json:"Code"`
	Message string `json:"Message"`
	Details string `json:"Details"`
}

func (c *Client) call(
	ctx context.Context, service, token, clientLogin string, payload any, result any,
) error {
	if strings.TrimSpace(token) == "" {
		return &Error{Code: "missing_access_token"}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	endpoint := *c.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/" + service
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	request.Header.Set("Accept-Language", "ru")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if clientLogin = strings.TrimSpace(clientLogin); clientLogin != "" {
		request.Header.Set("Client-Login", clientLogin)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("call Yandex Direct: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return err
	}
	requestID := response.Header.Get("RequestId")
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code       int    `json:"error_code"`
			StringCode string `json:"error_string"`
			Detail     string `json:"error_detail"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return &Error{StatusCode: response.StatusCode, Code: "invalid_api_response", RequestID: requestID}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || envelope.Error != nil {
		code := "api_request_failed"
		message := ""
		if envelope.Error != nil {
			if envelope.Error.StringCode != "" {
				code = envelope.Error.StringCode
			} else if envelope.Error.Code != 0 {
				code = strconv.Itoa(envelope.Error.Code)
			}
			message = envelope.Error.Detail
		}
		apiErrorCode := 0
		if envelope.Error != nil {
			apiErrorCode = envelope.Error.Code
		}
		return &Error{
			StatusCode: response.StatusCode, APIErrorCode: apiErrorCode,
			Code: code, Message: message, RequestID: requestID,
		}
	}
	if len(envelope.Result) == 0 || string(envelope.Result) == "null" {
		return &Error{StatusCode: response.StatusCode, Code: "missing_api_result", RequestID: requestID}
	}
	if err := json.Unmarshal(envelope.Result, result); err != nil {
		return &Error{StatusCode: response.StatusCode, Code: "invalid_api_result", RequestID: requestID}
	}
	return nil
}

func findWeeklySpendLimit(raw json.RawMessage) (int64, bool) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if len(raw) == 0 || decoder.Decode(&value) != nil {
		return 0, false
	}
	root, ok := value.(map[string]any)
	if !ok {
		return 0, false
	}
	if hasAlternateSpendControl(root) {
		return 0, false
	}
	strategy, ok := root["BiddingStrategy"].(map[string]any)
	if !ok {
		return 0, false
	}
	search, ok := strategy["Search"].(map[string]any)
	if !ok || search["BiddingStrategyType"] != "SERVING_OFF" {
		return 0, false
	}
	network, ok := strategy["Network"].(map[string]any)
	if !ok || network["BiddingStrategyType"] != "WB_MAXIMUM_CLICKS" {
		return 0, false
	}
	maximumClicks, ok := network["WbMaximumClicks"].(map[string]any)
	if !ok {
		return 0, false
	}
	number, ok := maximumClicks["WeeklySpendLimit"].(json.Number)
	if !ok {
		return 0, false
	}
	budget, err := strconv.ParseInt(number.String(), 10, 64)
	if err != nil || budget <= 0 {
		return 0, false
	}
	occurrences := 0
	var count func(any)
	count = func(current any) {
		switch typed := current.(type) {
		case map[string]any:
			for key, child := range typed {
				if key == "WeeklySpendLimit" {
					occurrences++
				}
				count(child)
			}
		case []any:
			for _, child := range typed {
				count(child)
			}
		}
	}
	count(value)
	return budget, occurrences == 1
}

func hasAlternateSpendControl(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			normalized := strings.ToLower(strings.TrimSpace(key))
			if key != "WeeklySpendLimit" && child != nil &&
				(strings.Contains(normalized, "budget") ||
					strings.Contains(normalized, "spendlimit")) {
				return true
			}
			if hasAlternateSpendControl(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if hasAlternateSpendControl(child) {
				return true
			}
		}
	}
	return false
}

func issueError(issue apiIssue, fallback string) error {
	code := fallback
	if issue.Code != 0 {
		code = strconv.Itoa(issue.Code)
	}
	return &Error{Code: code, Message: issue.Details}
}

func dateOnly(value time.Time) time.Time {
	value = value.UTC()
	return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC)
}
