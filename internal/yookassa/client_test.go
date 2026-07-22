package yookassa

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const (
	testShopID    = "123456"
	testSecretKey = "live_secret-value"
	testPaymentID = "2a217a2d-000f-5000-9000-1bd6f124af9c"
	testMethodID  = "2a217a2d-000f-5000-9000-1bd6f124af9d"
)

func TestNewPinsOfficialEndpoint(t *testing.T) {
	client, err := New(testShopID, testSecretKey, &http.Client{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if got := client.baseURL.String(); got != APIBaseURL {
		t.Fatalf("base URL = %q, want %q", got, APIBaseURL)
	}
}

func TestNewValidatesCredentials(t *testing.T) {
	tests := []struct {
		name   string
		shopID string
		secret string
	}{
		{name: "missing shop", shopID: "", secret: testSecretKey},
		{name: "non numeric shop", shopID: "shop-1", secret: testSecretKey},
		{name: "missing secret", shopID: testShopID, secret: ""},
		{name: "surrounding secret whitespace", shopID: testShopID, secret: " secret"},
		{name: "secret control character", shopID: testShopID, secret: "secret\nvalue"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := New(test.shopID, test.secret, &http.Client{}); err == nil {
				t.Fatal("New() error = nil")
			}
		})
	}
}

func TestCreatePaymentSendsAuthHeadersAndBody(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/v3/payments" {
			t.Errorf("request = %s %s, want POST /v3/payments", r.Method, r.URL.Path)
		}
		username, password, ok := r.BasicAuth()
		if !ok || username != testShopID || password != testSecretKey {
			t.Errorf("BasicAuth() = %q, %q, %v", username, password, ok)
		}
		if got := r.Header.Get("Idempotence-Key"); got != "checkout-workspace-123" {
			t.Errorf("Idempotence-Key = %q", got)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}

		var input CreatePaymentRequest
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if input.Amount != (Amount{Value: "499.00", Currency: "RUB"}) || !input.Capture || !input.SavePaymentMethod {
			t.Errorf("unexpected payment request: %+v", input)
		}
		if input.Confirmation == nil || input.Confirmation.Type != "redirect" || input.Confirmation.ReturnURL != "https://maxposty.ru/app/billing/return?payment=1" {
			t.Errorf("unexpected confirmation: %+v", input.Confirmation)
		}
		if got := input.Metadata["workspace_id"]; got != "workspace-123" {
			t.Errorf("workspace_id metadata = %q", got)
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "request-1")
		_, _ = io.WriteString(w, paymentJSON(testPaymentID, true))
	}))
	defer server.Close()

	client := testClient(t, server.URL+"/v3")
	input := CreatePaymentRequest{
		Amount:            Amount{Value: "499.00", Currency: "RUB"},
		Capture:           true,
		Confirmation:      &ConfirmationRequest{Type: "redirect", ReturnURL: "https://maxposty.ru/app/billing/return?payment=1"},
		PaymentMethodData: &PaymentMethodData{Type: "bank_card"},
		Description:       "Подписка MaxPosty",
		Metadata:          map[string]string{"workspace_id": "workspace-123"},
		SavePaymentMethod: true,
	}
	payment, err := client.CreatePayment(context.Background(), "checkout-workspace-123", input)
	if err != nil {
		t.Fatalf("CreatePayment() error = %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
	if payment.ID != testPaymentID || payment.Confirmation == nil || payment.Confirmation.ConfirmationURL != "https://yoomoney.ru/payments/external/confirmation?order=1" {
		t.Fatalf("unexpected payment: %+v", payment)
	}
	if payment.PaymentMethod == nil || !payment.PaymentMethod.Saved || payment.PaymentMethod.ID != testMethodID {
		t.Fatalf("unexpected payment method: %+v", payment.PaymentMethod)
	}
	if payment.Metadata["workspace_id"] != "workspace-123" {
		t.Fatalf("unexpected metadata: %+v", payment.Metadata)
	}
}

func TestCreatePaymentSupportsRecurringCharge(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if got := body["payment_method_id"]; got != testMethodID {
			t.Errorf("payment_method_id = %v", got)
		}
		if _, ok := body["confirmation"]; ok {
			t.Error("recurring request unexpectedly contains confirmation")
		}
		if _, ok := body["save_payment_method"]; ok {
			t.Error("recurring request unexpectedly contains save_payment_method")
		}
		_, _ = io.WriteString(w, recurringPaymentJSON(testPaymentID))
	}))
	defer server.Close()

	client := testClient(t, server.URL)
	input := CreatePaymentRequest{
		Amount:          Amount{Value: "999.00", Currency: "RUB"},
		Capture:         true,
		PaymentMethodID: testMethodID,
		Metadata:        map[string]string{"period_id": "period-1"},
	}
	payment, err := client.CreatePayment(context.Background(), "renew-period-1", input)
	if err != nil {
		t.Fatalf("CreatePayment() error = %v", err)
	}
	if payment.Status != "succeeded" || !payment.Paid {
		t.Fatalf("unexpected payment: %+v", payment)
	}
}

func TestCreatePaymentUsesIdempotencyKeyOnEveryRetry(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Idempotence-Key"); got != "same-operation-key" {
			t.Errorf("Idempotence-Key = %q", got)
		}
		calls.Add(1)
		_, _ = io.WriteString(w, paymentJSON(testPaymentID, false))
	}))
	defer server.Close()

	client := testClient(t, server.URL)
	input := validRedirectRequest()
	for range 2 {
		if _, err := client.CreatePayment(context.Background(), "same-operation-key", input); err != nil {
			t.Fatalf("CreatePayment() error = %v", err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
}

func TestGetPaymentUsesExactPathAndNoIdempotencyHeader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v3/payments/"+testPaymentID {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Idempotence-Key"); got != "" {
			t.Errorf("GET Idempotence-Key = %q", got)
		}
		username, password, ok := r.BasicAuth()
		if !ok || username != testShopID || password != testSecretKey {
			t.Errorf("BasicAuth() = %q, %q, %v", username, password, ok)
		}
		_, _ = io.WriteString(w, paymentJSON(testPaymentID, false))
	}))
	defer server.Close()

	client := testClient(t, server.URL+"/v3")
	payment, err := client.GetPayment(context.Background(), testPaymentID)
	if err != nil {
		t.Fatalf("GetPayment() error = %v", err)
	}
	if payment.ID != testPaymentID {
		t.Fatalf("payment ID = %q", payment.ID)
	}
}

func TestClientDoesNotFollowRedirectsOrForwardBasicAuth(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetCalls.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Error("redirect target received Authorization")
		}
		_, _ = io.WriteString(w, paymentJSON(testPaymentID, false))
	}))
	defer target.Close()

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/stolen", http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	client := testClient(t, source.URL)
	_, err := client.CreatePayment(context.Background(), "redirect-test", validRedirectRequest())
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("CreatePayment() error = %#v, want HTTP 307 API error", err)
	}
	if targetCalls.Load() != 0 {
		t.Fatalf("redirect target calls = %d, want 0", targetCalls.Load())
	}
}

func TestClientReturnsStructuredAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", "req-error")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"type":"error","id":"error-id","code":"invalid_request","description":"Invalid amount","parameter":"amount.value"}`)
	}))
	defer server.Close()

	client := testClient(t, server.URL)
	_, err := client.CreatePayment(context.Background(), "api-error-test", validRedirectRequest())
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("CreatePayment() error = %T %v", err, err)
	}
	if apiErr.StatusCode != http.StatusBadRequest || apiErr.Code != "invalid_request" || apiErr.Parameter != "amount.value" || apiErr.RequestID != "req-error" {
		t.Fatalf("unexpected API error: %+v", apiErr)
	}
	if strings.Contains(apiErr.Error(), testSecretKey) {
		t.Fatal("API error leaks secret key")
	}
}

func TestClientRejectsOversizedResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", maxResponseBytes+1))
	}))
	defer server.Close()

	client := testClient(t, server.URL)
	_, err := client.CreatePayment(context.Background(), "oversized-response", validRedirectRequest())
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("CreatePayment() error = %v, want ErrResponseTooLarge", err)
	}
}

func TestClientRejectsMalformedAndInvalidPaymentResponses(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "malformed JSON", body: `{"id":`},
		{name: "wrong response currency", body: strings.Replace(paymentJSON(testPaymentID, false), `"RUB"`, `"USD"`, 1)},
		{name: "unsafe confirmation URL", body: strings.Replace(paymentJSON(testPaymentID, false), "https://yoomoney.ru/payments/external/confirmation?order=1", "http://evil.example/pay", 1)},
		{name: "unknown status", body: strings.Replace(paymentJSON(testPaymentID, false), `"pending"`, `"unknown"`, 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			client := testClient(t, server.URL)
			if _, err := client.CreatePayment(context.Background(), "invalid-response", validRedirectRequest()); err == nil {
				t.Fatal("CreatePayment() error = nil")
			}
		})
	}
}

func TestCreatePaymentValidatesInputBeforeNetwork(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()
	client := testClient(t, server.URL)

	tests := []struct {
		name string
		key  string
		edit func(*CreatePaymentRequest)
	}{
		{name: "missing idempotency key", key: "", edit: func(*CreatePaymentRequest) {}},
		{name: "long idempotency key", key: strings.Repeat("k", 65), edit: func(*CreatePaymentRequest) {}},
		{name: "header injection", key: "key\nvalue", edit: func(*CreatePaymentRequest) {}},
		{name: "zero amount", key: "key", edit: func(input *CreatePaymentRequest) { input.Amount.Value = "0.00" }},
		{name: "float-like amount", key: "key", edit: func(input *CreatePaymentRequest) { input.Amount.Value = "1.5" }},
		{name: "leading zero amount", key: "key", edit: func(input *CreatePaymentRequest) { input.Amount.Value = "01.00" }},
		{name: "wrong currency", key: "key", edit: func(input *CreatePaymentRequest) { input.Amount.Currency = "USD" }},
		{name: "HTTP return URL", key: "key", edit: func(input *CreatePaymentRequest) { input.Confirmation.ReturnURL = "http://maxposty.ru/return" }},
		{name: "return URL userinfo", key: "key", edit: func(input *CreatePaymentRequest) {
			input.Confirmation.ReturnURL = "https://user:maxposty.ru@evil.example/return"
		}},
		{name: "return URL missing host", key: "key", edit: func(input *CreatePaymentRequest) {
			input.Confirmation.ReturnURL = "https:///return"
		}},
		{name: "return URL alternate port", key: "key", edit: func(input *CreatePaymentRequest) { input.Confirmation.ReturnURL = "https://maxposty.ru:444/return" }},
		{name: "missing payment route", key: "key", edit: func(input *CreatePaymentRequest) { input.Confirmation = nil }},
		{name: "both payment routes", key: "key", edit: func(input *CreatePaymentRequest) { input.PaymentMethodID = testMethodID }},
		{name: "bad saved method ID", key: "key", edit: func(input *CreatePaymentRequest) { input.Confirmation = nil; input.PaymentMethodID = "../method" }},
		{name: "resave saved method", key: "key", edit: func(input *CreatePaymentRequest) {
			input.Confirmation = nil
			input.PaymentMethodID = testMethodID
			input.SavePaymentMethod = true
		}},
		{name: "missing initial method type", key: "key", edit: func(input *CreatePaymentRequest) { input.PaymentMethodData = nil }},
		{name: "unsupported initial method type", key: "key", edit: func(input *CreatePaymentRequest) {
			input.PaymentMethodData.Type = "sbp"
		}},
		{name: "bad metadata key", key: "key", edit: func(input *CreatePaymentRequest) { input.Metadata = map[string]string{"bad key": "value"} }},
		{name: "empty metadata value", key: "key", edit: func(input *CreatePaymentRequest) { input.Metadata = map[string]string{"key": ""} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := validRedirectRequest()
			test.edit(&input)
			if _, err := client.CreatePayment(context.Background(), test.key, input); err == nil {
				t.Fatal("CreatePayment() error = nil")
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("network calls = %d, want 0", calls.Load())
	}
}

func TestGetPaymentValidatesIDAndResponseMatch(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, paymentJSON("different-payment-id", false))
	}))
	defer server.Close()
	client := testClient(t, server.URL)

	if _, err := client.GetPayment(context.Background(), "../payment"); err == nil {
		t.Fatal("GetPayment(invalid ID) error = nil")
	}
	if calls.Load() != 0 {
		t.Fatalf("network calls after invalid ID = %d", calls.Load())
	}
	if _, err := client.GetPayment(context.Background(), testPaymentID); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("GetPayment(mismatched response) error = %v", err)
	}
}

func TestClientRejectsNilContext(t *testing.T) {
	client := testClient(t, "http://127.0.0.1:1")
	if _, err := client.CreatePayment(nil, "nil-context", validRedirectRequest()); err == nil || !strings.Contains(err.Error(), "nil context") {
		t.Fatalf("CreatePayment(nil) error = %v", err)
	}
}

func testClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	client, err := newClient(baseURL, testShopID, testSecretKey, &http.Client{}, false)
	if err != nil {
		t.Fatalf("newClient() error = %v", err)
	}
	return client
}

func validRedirectRequest() CreatePaymentRequest {
	return CreatePaymentRequest{
		Amount:            Amount{Value: "499.00", Currency: "RUB"},
		Capture:           true,
		Confirmation:      &ConfirmationRequest{Type: "redirect", ReturnURL: "https://maxposty.ru/app/billing/return"},
		PaymentMethodData: &PaymentMethodData{Type: "bank_card"},
		Metadata:          map[string]string{"workspace_id": "workspace-123"},
		SavePaymentMethod: true,
	}
}

func paymentJSON(paymentID string, saved bool) string {
	return `{
		"id":` + quote(paymentID) + `,
		"status":"pending",
		"paid":false,
		"amount":{"value":"499.00","currency":"RUB"},
		"confirmation":{"type":"redirect","return_url":"https://maxposty.ru/app/billing/return","confirmation_url":"https://yoomoney.ru/payments/external/confirmation?order=1"},
		"payment_method":{"id":"` + testMethodID + `","type":"bank_card","saved":` + boolJSON(saved) + `,"title":"Bank card *4444"},
		"metadata":{"workspace_id":"workspace-123"},
		"created_at":"2026-07-22T12:00:00Z",
		"test":true,
		"refundable":false
	}`
}

func recurringPaymentJSON(paymentID string) string {
	return `{
		"id":` + quote(paymentID) + `,
		"status":"succeeded",
		"paid":true,
		"amount":{"value":"999.00","currency":"RUB"},
		"payment_method":{"id":"` + testMethodID + `","type":"bank_card","saved":true},
		"metadata":{"period_id":"period-1"},
		"test":true,
		"refundable":true
	}`
}

func quote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func boolJSON(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
