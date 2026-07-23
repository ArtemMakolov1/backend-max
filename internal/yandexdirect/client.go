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
)

const (
	DefaultAPIBaseURL        = "https://api.direct.yandex.com/json/v501"
	DefaultSandboxAPIBaseURL = "https://api-sandbox.direct.yandex.com/json/v5"
	oauthAuthorizeURL        = "https://oauth.yandex.ru/authorize"
	oauthTokenURL            = "https://oauth.yandex.ru/token"
)

type Client struct {
	baseURL      *url.URL
	clientID     string
	clientSecret string
	redirectURI  string
	http         *http.Client
	sandbox      bool
	unified      bool
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

type CampaignDraft struct {
	Name              string
	WeeklyBudgetMinor int64
	StartsAt          time.Time
	EndsAt            time.Time
}

type Campaign struct {
	ID                int64
	Name              string
	Status            string
	State             string
	WeeklyBudgetMinor int64
	StartsAt          time.Time
	EndsAt            time.Time
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
	if baseURL.Scheme != "https" && !(baseURL.Scheme == "http" && isLoopback) {
		return nil, errors.New("Yandex Direct API base URL must use HTTPS outside localhost")
	}
	if !isLoopback && host != "api.direct.yandex.com" && host != "api-sandbox.direct.yandex.com" {
		return nil, errors.New("Yandex Direct API base URL host is not allowed")
	}
	basePath := strings.TrimRight(baseURL.Path, "/")
	unified := strings.HasSuffix(basePath, "/json/v501")
	textV5 := strings.HasSuffix(basePath, "/json/v5")
	if !unified && !textV5 {
		return nil, errors.New("Yandex Direct API base URL must target /json/v501 or /json/v5")
	}
	if host == "api-sandbox.direct.yandex.com" && !textV5 {
		return nil, errors.New("Yandex Direct sandbox supports the documented /json/v5 endpoint only")
	}
	redirect, err := url.Parse(strings.TrimSpace(redirectURI))
	if err != nil || !redirect.IsAbs() || redirect.User != nil || redirect.RawQuery != "" ||
		redirect.Fragment != "" || (redirect.Scheme != "https" && !(redirect.Scheme == "http" &&
		(redirect.Hostname() == "localhost" || (net.ParseIP(redirect.Hostname()) != nil &&
			net.ParseIP(redirect.Hostname()).IsLoopback())))) {
		return nil, errors.New("invalid Yandex Direct OAuth redirect URI")
	}
	if strings.TrimSpace(clientID) == "" || strings.TrimSpace(clientSecret) == "" {
		return nil, errors.New("Yandex Direct OAuth client credentials are required")
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
		redirectURI: redirect.String(), http: &safeHTTPClient,
		sandbox: host == "api-sandbox.direct.yandex.com", unified: unified,
	}, nil
}

func (c *Client) Sandbox() bool { return c != nil && c.sandbox }

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

func (c *Client) ExchangeCode(ctx context.Context, code, verifier string) (string, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {strings.TrimSpace(code)},
		"client_id":     {c.clientID},
		"client_secret": {c.clientSecret},
		"redirect_uri":  {c.redirectURI},
		"code_verifier": {verifier},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, oauthTokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return "", fmt.Errorf("exchange Yandex Direct OAuth code: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var payload struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", &Error{StatusCode: response.StatusCode, Code: "invalid_oauth_response"}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || strings.TrimSpace(payload.AccessToken) == "" {
		code := strings.TrimSpace(payload.Error)
		if code == "" {
			code = "oauth_exchange_failed"
		}
		return "", &Error{StatusCode: response.StatusCode, Code: code}
	}
	return strings.TrimSpace(payload.AccessToken), nil
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
	request := map[string]any{
		"method": "add",
		"params": map[string]any{
			"Campaigns": []any{map[string]any{
				"Name":      strings.TrimSpace(campaign.Name),
				"StartDate": campaign.StartsAt.UTC().Format(time.DateOnly),
				"EndDate":   campaign.EndsAt.UTC().Format(time.DateOnly),
				campaignType: map[string]any{
					"BiddingStrategy": map[string]any{
						"Search": map[string]any{"BiddingStrategyType": "SERVING_OFF"},
						"Network": map[string]any{
							"BiddingStrategyType": "WB_MAXIMUM_CLICKS",
							"WbMaximumClicks": map[string]any{
								"WeeklySpendLimit": weeklyBudgetMicros,
							},
						},
					},
				},
			}},
		},
	}
	if err := c.call(ctx, "campaigns", token, clientLogin, request, &response); err != nil {
		return Campaign{}, err
	}
	if len(response.AddResults) != 1 || response.AddResults[0].ID <= 0 {
		return Campaign{}, &Error{Code: "campaign_add_failed"}
	}
	if len(response.AddResults[0].Errors) != 0 {
		return Campaign{}, issueError(response.AddResults[0].Errors[0], "campaign_add_failed")
	}
	return Campaign{
		ID: response.AddResults[0].ID, Name: strings.TrimSpace(campaign.Name),
		Status: "DRAFT", State: "OFF", WeeklyBudgetMinor: campaign.WeeklyBudgetMinor,
		StartsAt: dateOnly(campaign.StartsAt), EndsAt: dateOnly(campaign.EndsAt),
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
		ActionResults []struct {
			Errors []apiIssue `json:"Errors"`
		} `json:"ActionResults"`
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
	if len(response.ActionResults) != 1 {
		return &Error{Code: "campaign_resume_failed"}
	}
	if len(response.ActionResults[0].Errors) != 0 {
		return issueError(response.ActionResults[0].Errors[0], "campaign_resume_failed")
	}
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
