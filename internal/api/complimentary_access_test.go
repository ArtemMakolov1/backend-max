package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/media"
	"maxpilot/backend/internal/store"
	"maxpilot/backend/internal/yookassa"
)

type complimentaryBillingFake struct{ createCalls int }

func (f *complimentaryBillingFake) CreatePayment(context.Context, string, yookassa.CreatePaymentRequest) (yookassa.Payment, error) {
	f.createCalls++
	return yookassa.Payment{}, nil
}

func (*complimentaryBillingFake) GetPayment(context.Context, string) (yookassa.Payment, error) {
	return yookassa.Payment{}, nil
}

func TestComplimentaryAccessAPIKeepsPrivateCatalogRoleAndOperationalBoundaries(t *testing.T) {
	t.Parallel()
	baseURL := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if baseURL == "" {
		t.Fatal("TEST_DATABASE_URL is required")
	}
	config, err := pgx.ParseConfig(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	admin := stdlib.OpenDB(*config)
	t.Cleanup(func() { _ = admin.Close() })
	digest := sha256.Sum256([]byte(t.TempDir()))
	schema := "test_comp_api_" + hex.EncodeToString(digest[:12])
	if _, err = admin.ExecContext(t.Context(), `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, cleanupErr := admin.ExecContext(ctx, `DROP SCHEMA `+schema+` CASCADE`); cleanupErr != nil {
			t.Errorf("drop isolated API schema: %v", cleanupErr)
		}
	})
	parsed, err := url.Parse(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	operatorConfig, err := pgx.ParseConfig(parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	operator := stdlib.OpenDB(*operatorConfig)
	t.Cleanup(func() { _ = operator.Close() })
	storage, err := store.Open(t.Context(), parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	for _, id := range []string{"private-owner", "private-reviewer", "private-outsider"} {
		if err = storage.UpsertUser(t.Context(), store.User{ID: id, Email: id + "@example.invalid", DisplayName: id}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = operator.ExecContext(t.Context(), `INSERT INTO auth_identities(provider,subject,owner_id,created_at,updated_at)
VALUES('yandex','private-owner','private-owner',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	files, err := media.New(t.TempDir(), "http://localhost:8080")
	if err != nil {
		t.Fatal(err)
	}
	image := &quotaImageClient{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	application := app.New(storage, files, nil, image, nil, logger)
	payment := &complimentaryBillingFake{}
	if err = application.ConfigureBilling(payment, "https://example.invalid/billing", make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	if err = application.SetBillingLiveEnabled(true); err != nil {
		t.Fatal(err)
	}
	options := testAILimitOptions()
	options.MonthlyPlanEnforcement = true
	options.ImagePerMinute = 1
	server := New(application, logger, "http://localhost:4321", "webhook-secret", AuthOptions{
		YandexClient: &fakeYandexOAuth{}, AILimits: &options,
	})
	now := time.Now().UTC().Truncate(time.Minute)
	server.now = func() time.Time { return now }
	rawHandler := server.Handler()
	before := performJSONRequest(rawHandler, http.MethodGet, "/api/v1/plans", "")
	if before.Code != http.StatusOK {
		t.Fatal(before.Body.String())
	}
	var active bool
	if err = operator.QueryRowContext(t.Context(), `SELECT active FROM manage_account_complimentary_access($1,'grant','github:synthetic','github-run:synthetic')`, "private-owner@example.invalid").Scan(&active); err != nil || !active {
		t.Fatalf("activate private grant: %v %v", active, err)
	}
	after := performJSONRequest(rawHandler, http.MethodGet, "/api/v1/plans", "")
	if after.Code != http.StatusOK || after.Body.String() != before.Body.String() {
		t.Fatalf("private grant changed public plans: before=%s after=%s", before.Body.String(), after.Body.String())
	}
	team, err := storage.CreateWorkspace(t.Context(), "private-owner", store.Workspace{Name: "Private grant"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = storage.AddWorkspaceMember(t.Context(), "private-owner", store.WorkspaceMember{
		WorkspaceID: team.ID, UserID: "private-reviewer", Role: store.WorkspaceRoleViewer,
	}); err != nil {
		t.Fatal(err)
	}
	if err = storage.UpdateWorkspaceSubscriptionStatus(t.Context(), team.ID, "paused", now); err != nil {
		t.Fatal(err)
	}
	owner := withTestSession(t, storage, rawHandler, "private-owner")
	state := readWorkspaceBillingForTest(t, owner, team.ID)
	if !state.ComplimentaryAccess || state.Subscription.Plan.Code != "free" || state.Subscription.Status != "paused" ||
		!state.Features.AIImages || state.CheckoutEnabled || state.Actions.CanCheckout || state.Actions.CanResume || state.Contract != nil {
		t.Fatalf("private access DTO lost independent grant: %+v", state)
	}
	for _, entitlement := range state.Entitlements {
		if !entitlement.Unlimited || entitlement.HardLimit {
			t.Fatalf("private allowance was finite: %+v", entitlement)
		}
	}
	checkout := performJSONRequest(owner, http.MethodPost, "/api/v1/workspaces/"+team.ID+"/billing/checkout",
		`{"plan_code":"pro","recurring_consent":true}`)
	assertProblemCode(t, checkout, http.StatusConflict, "complimentary_access_active")
	if payment.createCalls != 0 {
		t.Fatalf("private access started an upstream payment: %d", payment.createCalls)
	}
	imagePath := "/api/v1/workspaces/" + team.ID + "/images/generate"
	reviewer := withTestSession(t, storage, rawHandler, "private-reviewer")
	response := performJSONRequest(reviewer, http.MethodPost, imagePath, `{"prompt":"role remains restricted"}`)
	assertProblemCode(t, response, http.StatusForbidden, "workspace_forbidden")
	if image.callCount() != 0 {
		t.Fatal("complimentary access bypassed reviewer permission")
	}
	response = performJSONRequest(owner, http.MethodPost, imagePath, `{"prompt":"granted Free AI","quality":"high"}`)
	if response.Code != http.StatusCreated || image.callCount() != 1 {
		t.Fatalf("grant failed paid AI: %d %s calls=%d", response.Code, response.Body.String(), image.callCount())
	}
	response = performJSONRequest(owner, http.MethodPost, imagePath, `{"prompt":"technical minute guard","quality":"high"}`)
	assertAI429(t, response, "minute", "60")
	if image.callCount() != 1 {
		t.Fatal("private grant bypassed technical AI limit")
	}
	state = readWorkspaceBillingForTest(t, owner, team.ID)
	if billingUsageQuantity(t, state.Usage, store.UsageMetricAIImageCredits) != 36 {
		t.Fatalf("grant erased charged usage: %+v", state.Usage)
	}
	outsider := withTestSession(t, storage, rawHandler, "private-outsider")
	response = performJSONRequest(outsider, http.MethodGet, "/api/v1/workspaces/"+team.ID+"/billing", "")
	assertProblemCode(t, response, http.StatusNotFound, "not_found")
}
