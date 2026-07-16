package observability

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"maxpilot/backend/internal/store"
)

func TestMetricsExposeBoundedHTTPAndDatabaseLabels(t *testing.T) {
	t.Parallel()

	metrics := New()
	metrics.slowQueryThreshold = 0
	metrics.ObserveHTTPRequest(http.MethodGet, "/api/v1/posts/{id}", http.StatusBadGateway, 25*time.Millisecond)

	ctx := metrics.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{
		SQL:  "SELECT secret_email FROM users WHERE id=$1",
		Args: []any{"private-user-id"},
	})
	metrics.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{Err: errors.New("database unavailable")})
	emptyCtx := metrics.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: "SELECT 1"})
	metrics.TraceQueryEnd(emptyCtx, nil, pgx.TraceQueryEndData{Err: pgx.ErrNoRows})

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	metrics.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("metrics status = %d", response.Code)
	}
	body := response.Body.String()
	for _, expected := range []string{
		`maxposty_http_requests_total{method="GET",route="/api/v1/posts/{id}",status_class="5xx"} 1`,
		`maxposty_http_request_errors_total{method="GET",route="/api/v1/posts/{id}",status_class="5xx"} 1`,
		`maxposty_db_query_errors_total{operation="select"} 1`,
		`maxposty_db_slow_queries_total{operation="select"} 2`,
	} {
		if !strings.Contains(body, expected) {
			t.Errorf("metrics output does not contain %q", expected)
		}
	}
	for _, sensitive := range []string{"secret_email", "private-user-id", "SELECT"} {
		if strings.Contains(body, sensitive) {
			t.Errorf("metrics output leaked %q", sensitive)
		}
	}
}

func TestDBPoolCollectorExportsCurrentAndCumulativeStats(t *testing.T) {
	t.Parallel()

	metrics := New()
	if err := metrics.RegisterDBPoolStats(func() sql.DBStats {
		return sql.DBStats{
			MaxOpenConnections: 20, OpenConnections: 7, InUse: 4, Idle: 3,
			WaitCount: 11, WaitDuration: 1500 * time.Millisecond,
			MaxIdleClosed: 2, MaxIdleTimeClosed: 3, MaxLifetimeClosed: 5,
		}
	}); err != nil {
		t.Fatalf("register pool stats: %v", err)
	}

	response := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := response.Body.String()
	for _, expected := range []string{
		"maxposty_db_pool_max_open_connections 20",
		"maxposty_db_pool_in_use_connections 4",
		"maxposty_db_pool_wait_count_total 11",
		"maxposty_db_pool_wait_duration_seconds_total 1.5",
		`maxposty_db_pool_connections_closed_total{reason="lifetime"} 5`,
	} {
		if !strings.Contains(body, expected) {
			t.Errorf("metrics output does not contain %q", expected)
		}
	}
}

func TestSQLOperationHasFixedCardinality(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		" SELECT * FROM posts":               "select",
		"insert into posts values ($1)":      "insert",
		"UPDATE posts SET title=$1":          "update",
		"delete from posts":                  "delete",
		"WITH selected AS (SELECT 1) SELECT": "with",
		"VACUUM":                             "other",
		"":                                   "other",
	}
	for query, expected := range tests {
		if actual := sqlOperation(query); actual != expected {
			t.Errorf("sqlOperation(%q) = %q, want %q", query, actual, expected)
		}
	}
}

func TestHTTPMethodHasFixedCardinality(t *testing.T) {
	t.Parallel()
	if got := httpMethod("get"); got != http.MethodGet {
		t.Fatalf("httpMethod(get) = %q", got)
	}
	if got := httpMethod("ATTACKER-CONTROLLED-METHOD"); got != "OTHER" {
		t.Fatalf("unknown method label = %q", got)
	}
}

func TestSchedulerIntervalMetric(t *testing.T) {
	t.Parallel()
	metrics := New()
	metrics.SetSchedulerInterval(15 * time.Second)
	response := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(response.Body.String(), "maxposty_scheduler_interval_seconds 15") {
		t.Fatalf("scheduler interval metric is missing: %s", response.Body.String())
	}
}

func TestMediaMetricsHaveBoundedLabels(t *testing.T) {
	t.Parallel()
	metrics := New()
	metrics.ObserveMediaOperation("upload", "quota_exceeded")
	metrics.ObserveMediaOperation("attacker-operation", "attacker-outcome")
	response := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := response.Body.String()
	for _, expected := range []string{
		`maxposty_media_operations_total{operation="upload",outcome="quota_exceeded"} 1`,
		`maxposty_media_operations_total{operation="other",outcome="other"} 1`,
	} {
		if !strings.Contains(body, expected) {
			t.Errorf("metrics output does not contain %q", expected)
		}
	}
	if strings.Contains(body, "attacker-operation") || strings.Contains(body, "attacker-outcome") {
		t.Fatalf("unbounded media labels leaked into metrics: %s", body)
	}
}

func TestProductAnalyticsCollectorCachesAndPreservesLastSuccess(t *testing.T) {
	t.Parallel()

	now := time.Unix(2_000_000_000, 0).UTC()
	var calls atomic.Int64
	var fail atomic.Bool
	collector := newProductAnalyticsCollector(func(_ context.Context, _ time.Time) (store.ProductAnalyticsSnapshot, error) {
		calls.Add(1)
		if fail.Load() {
			return store.ProductAnalyticsSnapshot{}, errors.New("temporary database failure")
		}
		return store.ProductAnalyticsSnapshot{
			DailyActiveUsers: 3, WeeklyActiveUsers: 8, MonthlyActiveUsers: 13,
			RegisteredUsers: 21, MAXLinkedUsers: 17, ChannelConnectedUsers: 12,
			PostCreatedUsers: 9, PostScheduledOrPublishedUsers: 7, PostPublishedUsers: 5,
		}, nil
	}, time.Minute, time.Second)
	collector.now = func() time.Time { return now }

	metrics := New()
	if err := metrics.registry.Register(collector); err != nil {
		t.Fatalf("register product collector: %v", err)
	}
	scrape := func() string {
		response := httptest.NewRecorder()
		metrics.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		return response.Body.String()
	}

	first := scrape()
	if calls.Load() != 1 || !strings.Contains(first, `maxposty_product_active_users{period="day"} 3`) ||
		!strings.Contains(first, `maxposty_product_funnel_users{stage="published"} 5`) ||
		!strings.Contains(first, "maxposty_product_analytics_last_success_timestamp_seconds 2e+09") {
		t.Fatalf("unexpected first scrape (calls=%d): %s", calls.Load(), first)
	}
	_ = scrape()
	if calls.Load() != 1 {
		t.Fatalf("cached collector calls = %d, want 1", calls.Load())
	}

	fail.Store(true)
	now = now.Add(61 * time.Second)
	afterFailure := scrape()
	if calls.Load() != 2 || !strings.Contains(afterFailure, `maxposty_product_active_users{period="day"} 3`) ||
		!strings.Contains(afterFailure, `maxposty_product_funnel_users{stage="published"} 5`) ||
		!strings.Contains(afterFailure, "maxposty_product_analytics_collect_errors_total 1") ||
		!strings.Contains(afterFailure, "maxposty_product_analytics_last_success_timestamp_seconds 2e+09") {
		t.Fatalf("last success was not preserved (calls=%d): %s", calls.Load(), afterFailure)
	}
}
