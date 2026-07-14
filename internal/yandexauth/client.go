package yandexauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultAuthorizeURL = "https://oauth.yandex.ru/authorize"
	defaultTokenURL     = "https://oauth.yandex.ru/token"
	defaultUserInfoURL  = "https://login.yandex.ru/info"
)

type Client struct {
	clientID     string
	clientSecret string
	httpClient   *http.Client
	authorizeURL string
	tokenURL     string
	userInfoURL  string
}

type Profile struct {
	ID              string   `json:"id"`
	PSUID           string   `json:"psuid"`
	ClientID        string   `json:"client_id"`
	Login           string   `json:"login"`
	DefaultEmail    string   `json:"default_email"`
	Emails          []string `json:"emails"`
	DisplayName     string   `json:"display_name"`
	RealName        string   `json:"real_name"`
	FirstName       string   `json:"first_name"`
	LastName        string   `json:"last_name"`
	DefaultAvatarID string   `json:"default_avatar_id"`
}

type Error struct {
	Operation  string
	StatusCode int
	Code       string
}

func (e *Error) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("Yandex OAuth %s failed with HTTP %d", e.Operation, e.StatusCode)
	}
	if e.Code != "" {
		return fmt.Sprintf("Yandex OAuth %s failed: %s", e.Operation, e.Code)
	}
	return fmt.Sprintf("Yandex OAuth %s failed", e.Operation)
}

func New(clientID, clientSecret string, httpClient *http.Client) (*Client, error) {
	clientID = strings.TrimSpace(clientID)
	clientSecret = strings.TrimSpace(clientSecret)
	if clientID == "" || clientSecret == "" {
		return nil, errors.New("Yandex OAuth client ID and secret are required")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{
		clientID: clientID, clientSecret: clientSecret, httpClient: httpClient,
		authorizeURL: defaultAuthorizeURL, tokenURL: defaultTokenURL, userInfoURL: defaultUserInfoURL,
	}, nil
}

func (c *Client) AuthorizationURL(redirectURI, state, codeChallenge string) string {
	values := url.Values{
		"response_type":         {"code"},
		"client_id":             {c.clientID},
		"redirect_uri":          {redirectURI},
		"scope":                 {"login:info login:email"},
		"optional_scope":        {"login:avatar"},
		"state":                 {state},
		"code_challenge":        {codeChallenge},
		"code_challenge_method": {"S256"},
	}
	return c.authorizeURL + "?" + values.Encode()
}

func (c *Client) ExchangeCode(ctx context.Context, code, codeVerifier string) (string, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {codeVerifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build Yandex token request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(c.clientID, c.clientSecret)
	response, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request Yandex token: %w", err)
	}
	defer response.Body.Close()

	var payload struct {
		AccessToken      string `json:"access_token"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := decodeLimitedJSON(response.Body, &payload); err != nil {
		return "", &Error{Operation: "token exchange", StatusCode: response.StatusCode, Code: "invalid_response"}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || strings.TrimSpace(payload.AccessToken) == "" {
		return "", &Error{Operation: "token exchange", StatusCode: response.StatusCode, Code: payload.Error}
	}
	return payload.AccessToken, nil
}

func (c *Client) UserInfo(ctx context.Context, accessToken string) (Profile, error) {
	endpoint, err := url.Parse(c.userInfoURL)
	if err != nil {
		return Profile{}, fmt.Errorf("parse Yandex user info URL: %w", err)
	}
	query := endpoint.Query()
	query.Set("format", "json")
	endpoint.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return Profile{}, fmt.Errorf("build Yandex user info request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "OAuth "+accessToken)
	response, err := c.httpClient.Do(req)
	if err != nil {
		return Profile{}, fmt.Errorf("request Yandex user info: %w", err)
	}
	defer response.Body.Close()

	var profile Profile
	if err := decodeLimitedJSON(response.Body, &profile); err != nil {
		return Profile{}, &Error{Operation: "user info", StatusCode: response.StatusCode, Code: "invalid_response"}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || profile.ID == "" || profile.ClientID != c.clientID {
		return Profile{}, &Error{Operation: "user info", StatusCode: response.StatusCode, Code: "invalid_profile"}
	}
	return profile, nil
}

func decodeLimitedJSON(reader io.Reader, target any) error {
	decoder := json.NewDecoder(io.LimitReader(reader, 1<<20))
	return decoder.Decode(target)
}
