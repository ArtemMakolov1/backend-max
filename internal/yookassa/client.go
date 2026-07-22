// Package yookassa implements the small, security-sensitive subset of the
// YooKassa Payments API used by MaxPosty.
package yookassa

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// APIBaseURL is intentionally fixed to the official YooKassa API origin.
	// Production callers cannot replace it with a request-controlled URL.
	APIBaseURL = "https://api.yookassa.ru/v3"

	maxResponseBytes = 1 << 20
	maxRequestBytes  = 64 << 10
	maxReturnURLLen  = 255
)

var (
	amountPattern        = regexp.MustCompile(`^(0|[1-9][0-9]{0,9})\.[0-9]{2}$`)
	shopIDPattern        = regexp.MustCompile(`^[0-9]{1,64}$`)
	providerIDPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
	metadataKeyPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	validPaymentStatuses = map[string]struct{}{
		"pending":             {},
		"waiting_for_capture": {},
		"succeeded":           {},
		"canceled":            {},
	}

	// ErrResponseTooLarge is returned before attempting to decode an oversized
	// response. The body is never copied into an error or a loggable value.
	ErrResponseTooLarge = errors.New("YooKassa response is too large")
)

// Client is safe for concurrent use as long as the supplied http.Client is not
// mutated concurrently.
type Client struct {
	shopID     string
	secretKey  string
	httpClient *http.Client
	baseURL    *url.URL
}

// Amount is the decimal amount representation required by YooKassa. MaxPosty
// accepts RUB only and requires exactly two fractional digits, avoiding float
// rounding at the payment boundary.
type Amount struct {
	Value    string `json:"value"`
	Currency string `json:"currency"`
}

// ConfirmationRequest requests YooKassa's hosted redirect flow.
type ConfirmationRequest struct {
	Type      string `json:"type"`
	ReturnURL string `json:"return_url"`
}

// PaymentMethodData constrains the initial recurring checkout to a bank card,
// the saved-method flow supported by this integration.
type PaymentMethodData struct {
	Type string `json:"type"`
}

// CreatePaymentRequest supports both the initial redirect payment and a later
// recurring charge using a saved payment method. Exactly one of Confirmation
// and PaymentMethodID must be supplied.
type CreatePaymentRequest struct {
	Amount            Amount               `json:"amount"`
	Capture           bool                 `json:"capture"`
	Confirmation      *ConfirmationRequest `json:"confirmation,omitempty"`
	PaymentMethodData *PaymentMethodData   `json:"payment_method_data,omitempty"`
	Description       string               `json:"description,omitempty"`
	Metadata          map[string]string    `json:"metadata,omitempty"`
	SavePaymentMethod bool                 `json:"save_payment_method,omitempty"`
	PaymentMethodID   string               `json:"payment_method_id,omitempty"`
}

// Confirmation is returned for payments that require user interaction.
type Confirmation struct {
	Type            string `json:"type"`
	ReturnURL       string `json:"return_url,omitempty"`
	ConfirmationURL string `json:"confirmation_url,omitempty"`
}

// PaymentMethod contains only the non-card fields needed to run recurring
// payments. Card data returned by YooKassa is deliberately not modeled.
type PaymentMethod struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Saved bool   `json:"saved"`
	Title string `json:"title,omitempty"`
}

// CancellationDetails explains a terminal provider-side cancellation.
type CancellationDetails struct {
	Party  string `json:"party"`
	Reason string `json:"reason"`
}

// Payment is the verified subset of the YooKassa payment object used by the
// billing layer.
type Payment struct {
	ID                  string               `json:"id"`
	Status              string               `json:"status"`
	Paid                bool                 `json:"paid"`
	Amount              Amount               `json:"amount"`
	Confirmation        *Confirmation        `json:"confirmation,omitempty"`
	PaymentMethod       *PaymentMethod       `json:"payment_method,omitempty"`
	Metadata            map[string]string    `json:"metadata,omitempty"`
	Description         string               `json:"description,omitempty"`
	CreatedAt           string               `json:"created_at,omitempty"`
	CapturedAt          string               `json:"captured_at,omitempty"`
	ExpiresAt           string               `json:"expires_at,omitempty"`
	Test                bool                 `json:"test"`
	Refundable          bool                 `json:"refundable"`
	CancellationDetails *CancellationDetails `json:"cancellation_details,omitempty"`
}

// Error is a sanitized YooKassa API error. It never contains credentials or a
// raw response body.
type Error struct {
	StatusCode  int
	Type        string
	ID          string
	Code        string
	Description string
	Parameter   string
	RequestID   string
}

func (e *Error) Error() string {
	message := strings.TrimSpace(e.Description)
	if message == "" {
		message = http.StatusText(e.StatusCode)
	}
	if e.Code != "" {
		return fmt.Sprintf("YooKassa API error (%s, HTTP %d): %s", e.Code, e.StatusCode, message)
	}
	return fmt.Sprintf("YooKassa API error (HTTP %d): %s", e.StatusCode, message)
}

// New constructs a client pinned to YooKassa's official API endpoint.
func New(shopID, secretKey string, httpClient *http.Client) (*Client, error) {
	return newClient(APIBaseURL, shopID, secretKey, httpClient, true)
}

// newClient permits an alternate HTTP endpoint only inside this package's
// tests. Production callers can only use New, which pins APIBaseURL.
func newClient(baseURL, shopID, secretKey string, httpClient *http.Client, requireHTTPS bool) (*Client, error) {
	shopID = strings.TrimSpace(shopID)
	if !shopIDPattern.MatchString(shopID) {
		return nil, errors.New("YooKassa shop ID must contain 1 to 64 digits")
	}
	if secretKey == "" || secretKey != strings.TrimSpace(secretKey) || len(secretKey) > 512 || containsControl(secretKey) {
		return nil, errors.New("YooKassa secret key is invalid")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}

	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse YooKassa API base URL: %w", err)
	}
	if !parsed.IsAbs() || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("YooKassa API base URL must be an absolute URL without user info, query or fragment")
	}
	if (requireHTTPS && parsed.Scheme != "https") || (!requireHTTPS && parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, errors.New("YooKassa API base URL must use HTTPS")
	}
	copyURL := *parsed
	copyURL.Path = strings.TrimRight(copyURL.Path, "/")
	return &Client{shopID: shopID, secretKey: secretKey, httpClient: httpClient, baseURL: &copyURL}, nil
}

// CreatePayment creates an initial or recurring payment. Reusing the same
// idempotency key with the same request body is safe for YooKassa's documented
// 24-hour idempotency window.
func (c *Client) CreatePayment(ctx context.Context, idempotencyKey string, input CreatePaymentRequest) (Payment, error) {
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return Payment{}, err
	}
	if err := validateCreatePaymentRequest(input); err != nil {
		return Payment{}, err
	}

	var payment Payment
	if err := c.doJSON(ctx, http.MethodPost, "/payments", idempotencyKey, input, &payment); err != nil {
		return Payment{}, err
	}
	if err := validatePayment(payment); err != nil {
		return Payment{}, fmt.Errorf("validate YooKassa payment response: %w", err)
	}
	return payment, nil
}

// GetPayment fetches a payment by its provider-generated ID. The request is
// naturally idempotent and therefore does not send Idempotence-Key.
func (c *Client) GetPayment(ctx context.Context, paymentID string) (Payment, error) {
	if err := validateProviderID("payment ID", paymentID); err != nil {
		return Payment{}, err
	}

	var payment Payment
	if err := c.doJSON(ctx, http.MethodGet, "/payments/"+url.PathEscape(paymentID), "", nil, &payment); err != nil {
		return Payment{}, err
	}
	if err := validatePayment(payment); err != nil {
		return Payment{}, fmt.Errorf("validate YooKassa payment response: %w", err)
	}
	if payment.ID != paymentID {
		return Payment{}, errors.New("YooKassa payment response ID does not match the request")
	}
	return payment, nil
}

func (c *Client) doJSON(ctx context.Context, method, endpointPath, idempotencyKey string, body, output any) error {
	if ctx == nil {
		return errors.New("YooKassa API request: nil context")
	}

	requestURL := *c.baseURL
	requestURL.Path = strings.TrimRight(requestURL.Path, "/") + "/" + strings.TrimLeft(endpointPath, "/")

	var requestBody io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode YooKassa API request: %w", err)
		}
		if len(encoded) > maxRequestBytes {
			return errors.New("YooKassa request JSON is too large")
		}
		requestBody = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, requestURL.String(), requestBody)
	if err != nil {
		return fmt.Errorf("create YooKassa API request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(c.shopID, c.secretKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotence-Key", idempotencyKey)
	}

	// The Basic Auth credential must never be forwarded by a redirect. Returning
	// the 3xx response also lets the caller treat unexpected redirects as an API
	// failure instead of silently changing payment semantics.
	requestClient := *c.httpClient
	requestClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	// #nosec G704 -- production clients are pinned to the fixed official YooKassa HTTPS origin; endpoint paths are constants/payment IDs with strict validation, and redirects are disabled above.
	resp, err := requestClient.Do(req)
	if err != nil {
		return fmt.Errorf("call YooKassa API: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	responseBody, readErr := readBoundedJSON(resp.Body)
	if readErr != nil {
		return readErr
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return parseAPIError(resp, responseBody)
	}
	if len(bytes.TrimSpace(responseBody)) == 0 {
		return errors.New("YooKassa API returned an empty JSON response")
	}
	if err := json.Unmarshal(responseBody, output); err != nil {
		return fmt.Errorf("decode YooKassa API response: %w", err)
	}
	return nil
}

func validateCreatePaymentRequest(input CreatePaymentRequest) error {
	if err := validateAmount(input.Amount); err != nil {
		return fmt.Errorf("create YooKassa payment: %w", err)
	}
	if utf8.RuneCountInString(input.Description) > 128 || containsControl(input.Description) {
		return errors.New("create YooKassa payment: description must contain at most 128 characters and no control characters")
	}
	if err := validateMetadata(input.Metadata); err != nil {
		return fmt.Errorf("create YooKassa payment: %w", err)
	}

	hasSavedMethod := input.PaymentMethodID != ""
	hasConfirmation := input.Confirmation != nil
	if hasSavedMethod == hasConfirmation {
		return errors.New("create YooKassa payment: provide exactly one of confirmation or payment method ID")
	}
	if hasSavedMethod {
		if err := validateProviderID("payment method ID", input.PaymentMethodID); err != nil {
			return fmt.Errorf("create YooKassa payment: %w", err)
		}
		if input.SavePaymentMethod {
			return errors.New("create YooKassa payment: an already saved payment method cannot be saved again")
		}
		if input.PaymentMethodData != nil {
			return errors.New("create YooKassa payment: payment method data is only valid for an initial checkout")
		}
		return nil
	}
	if input.Confirmation.Type != "redirect" {
		return errors.New("create YooKassa payment: confirmation type must be redirect")
	}
	if err := validateReturnURL("return URL", input.Confirmation.ReturnURL, maxReturnURLLen); err != nil {
		return fmt.Errorf("create YooKassa payment: %w", err)
	}
	if input.PaymentMethodData == nil || input.PaymentMethodData.Type != "bank_card" {
		return errors.New("create YooKassa payment: initial recurring checkout must use bank_card")
	}
	if !input.SavePaymentMethod {
		return errors.New("create YooKassa payment: initial recurring checkout must save the payment method")
	}
	return nil
}

func validatePayment(payment Payment) error {
	if err := validateProviderID("payment ID", payment.ID); err != nil {
		return err
	}
	if _, ok := validPaymentStatuses[payment.Status]; !ok {
		return errors.New("payment status is invalid")
	}
	if err := validateAmount(payment.Amount); err != nil {
		return err
	}
	if err := validateMetadata(payment.Metadata); err != nil {
		return err
	}
	if payment.Confirmation != nil {
		if payment.Confirmation.Type != "redirect" {
			return errors.New("confirmation type is not redirect")
		}
		if payment.Confirmation.ReturnURL != "" {
			if err := validateReturnURL("confirmation return URL", payment.Confirmation.ReturnURL, maxReturnURLLen); err != nil {
				return err
			}
		}
		if payment.Confirmation.ConfirmationURL == "" {
			return errors.New("confirmation URL is required")
		}
		if err := validateHTTPSURL("confirmation URL", payment.Confirmation.ConfirmationURL, 2048); err != nil {
			return err
		}
	}
	if payment.PaymentMethod != nil {
		if err := validateProviderID("payment method ID", payment.PaymentMethod.ID); err != nil {
			return err
		}
		if payment.PaymentMethod.Type == "" || len(payment.PaymentMethod.Type) > 64 || containsControl(payment.PaymentMethod.Type) {
			return errors.New("payment method type is invalid")
		}
	}
	return nil
}

func validateAmount(amount Amount) error {
	if amount.Currency != "RUB" {
		return errors.New("currency must be RUB")
	}
	if !amountPattern.MatchString(amount.Value) || amount.Value == "0.00" {
		return errors.New("amount must be a positive decimal with exactly two fractional digits")
	}
	return nil
}

func validateMetadata(metadata map[string]string) error {
	if len(metadata) > 16 {
		return errors.New("metadata must contain at most 16 entries")
	}
	for key, value := range metadata {
		if !metadataKeyPattern.MatchString(key) {
			return errors.New("metadata contains an invalid key")
		}
		if value == "" || len(value) > 512 || containsControl(value) || !utf8.ValidString(value) {
			return fmt.Errorf("metadata value for %q is invalid", key)
		}
	}
	return nil
}

func validateIdempotencyKey(value string) error {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 64 || containsControl(value) {
		return errors.New("YooKassa idempotency key must contain 1 to 64 non-control characters without surrounding whitespace")
	}
	return nil
}

func validateProviderID(label, value string) error {
	if !providerIDPattern.MatchString(value) {
		return fmt.Errorf("YooKassa %s is invalid", label)
	}
	return nil
}

func validateHTTPSURL(label, raw string, maxLen int) error {
	if raw == "" || len(raw) > maxLen || containsControl(raw) {
		return fmt.Errorf("YooKassa %s is invalid", label)
	}
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return fmt.Errorf("YooKassa %s must be an absolute HTTPS URL without user info or fragment", label)
	}
	if parsed.Port() != "" && parsed.Port() != "443" {
		return fmt.Errorf("YooKassa %s must use the default HTTPS port", label)
	}
	return nil
}

func validateReturnURL(label, raw string, maxLen int) error {
	if raw == "" || len(raw) > maxLen || containsControl(raw) {
		return fmt.Errorf("YooKassa %s is invalid", label)
	}
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return fmt.Errorf("YooKassa %s must be an absolute HTTPS URL without user info", label)
	}
	if parsed.Port() != "" && parsed.Port() != "443" {
		return fmt.Errorf("YooKassa %s must use the default HTTPS port", label)
	}
	return nil
}

func containsControl(value string) bool {
	for _, char := range value {
		if char < ' ' || char == 0x7f {
			return true
		}
	}
	return false
}

func readBoundedJSON(reader io.Reader) ([]byte, error) {
	limited := io.LimitReader(reader, maxResponseBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read YooKassa API response: %w", err)
	}
	if len(body) > maxResponseBytes {
		return nil, ErrResponseTooLarge
	}
	return body, nil
}

func parseAPIError(resp *http.Response, body []byte) *Error {
	var payload struct {
		Type        string `json:"type"`
		ID          string `json:"id"`
		Code        string `json:"code"`
		Description string `json:"description"`
		Parameter   string `json:"parameter"`
	}
	_ = json.Unmarshal(body, &payload)
	return &Error{
		StatusCode:  resp.StatusCode,
		Type:        payload.Type,
		ID:          payload.ID,
		Code:        payload.Code,
		Description: payload.Description,
		Parameter:   payload.Parameter,
		RequestID:   firstHeader(resp.Header, "X-Request-Id", "X-Request-ID"),
	}
}

func firstHeader(headers http.Header, names ...string) string {
	for _, name := range names {
		if value := headers.Get(name); value != "" {
			return value
		}
	}
	return ""
}
