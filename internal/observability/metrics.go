package observability

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"maxpilot/backend/internal/store"
)

const defaultSlowQueryThreshold = 500 * time.Millisecond

// Metrics owns the service-local Prometheus registry. Labels are deliberately
// limited to bounded operational dimensions; raw paths, SQL, IDs and user data
// must never be attached to these collectors.
type Metrics struct {
	registry *prometheus.Registry
	handler  http.Handler

	httpRequests       *prometheus.CounterVec
	httpErrors         *prometheus.CounterVec
	httpDuration       *prometheus.HistogramVec
	httpInFlight       prometheus.Gauge
	dbDuration         *prometheus.HistogramVec
	dbErrors           *prometheus.CounterVec
	dbSlow             *prometheus.CounterVec
	publicationTotal   *prometheus.CounterVec
	publicationTime    *prometheus.HistogramVec
	schedulerJobs      *prometheus.CounterVec
	schedulerDue       *prometheus.GaugeVec
	schedulerCycles    prometheus.Counter
	schedulerCycleTime prometheus.Histogram
	schedulerLastRun   prometheus.Gauge
	schedulerInterval  prometheus.Gauge
	recoveredPosts     prometheus.Counter
	mediaOperations    *prometheus.CounterVec
	attachmentUploads  *prometheus.CounterVec
	attachmentReady    *prometheus.HistogramVec
	attachmentBytes    *prometheus.HistogramVec

	slowQueryThreshold time.Duration
}

// New creates an isolated registry so tests and multiple server instances do
// not share mutable global metric state.
func New() *Metrics {
	return NewWithRegistry(prometheus.NewRegistry())
}

// NewWithRegistry is primarily useful for tests and embedded deployments.
func NewWithRegistry(registry *prometheus.Registry) *Metrics {
	if registry == nil {
		registry = prometheus.NewRegistry()
	}
	m := &Metrics{
		registry: registry,
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "maxposty", Name: "http_requests_total",
			Help: "Total HTTP requests handled by route template and status class.",
		}, []string{"method", "route", "status_class"}),
		httpErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "maxposty", Name: "http_request_errors_total",
			Help: "Total HTTP requests completed with a 4xx or 5xx response.",
		}, []string{"method", "route", "status_class"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "maxposty", Name: "http_request_duration_seconds",
			Help:    "HTTP request duration by route template.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 15, 30, 60, 180},
		}, []string{"method", "route"}),
		httpInFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "maxposty", Name: "http_requests_in_flight",
			Help: "Current number of HTTP requests being served.",
		}),
		dbDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "maxposty", Name: "db_query_duration_seconds",
			Help:    "PostgreSQL query duration by SQL operation and result.",
			Buckets: []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}, []string{"operation", "result"}),
		dbErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "maxposty", Name: "db_query_errors_total",
			Help: "Total PostgreSQL query failures by SQL operation.",
		}, []string{"operation"}),
		dbSlow: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "maxposty", Name: "db_slow_queries_total",
			Help: "Total PostgreSQL queries slower than the configured threshold by SQL operation.",
		}, []string{"operation"}),
		publicationTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "maxposty", Name: "publication_operations_total",
			Help: "Total MAX publication operations by operation and outcome.",
		}, []string{"operation", "outcome"}),
		publicationTime: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "maxposty", Name: "publication_operation_duration_seconds",
			Help:    "Duration of MAX publication operations by operation and outcome.",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 180},
		}, []string{"operation", "outcome"}),
		schedulerJobs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "maxposty", Name: "scheduler_jobs_total",
			Help: "Total scheduler jobs by job kind and outcome.",
		}, []string{"job", "outcome"}),
		schedulerDue: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "maxposty", Name: "scheduler_due_items",
			Help: "Items found due in the latest scheduler scan by job kind.",
		}, []string{"job"}),
		schedulerCycles: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "maxposty", Name: "scheduler_cycles_total",
			Help: "Total scheduler cycles completed.",
		}),
		schedulerCycleTime: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: "maxposty", Name: "scheduler_cycle_duration_seconds",
			Help:    "Duration of a complete scheduler cycle.",
			Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 15, 30, 60, 180},
		}),
		schedulerLastRun: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "maxposty", Name: "scheduler_last_cycle_timestamp_seconds",
			Help: "Unix timestamp of the latest completed scheduler cycle.",
		}),
		schedulerInterval: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "maxposty", Name: "scheduler_interval_seconds",
			Help: "Configured interval between scheduler cycles in seconds.",
		}),
		recoveredPosts: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "maxposty", Name: "scheduler_recovered_publications_total",
			Help: "Total interrupted publishing records recovered by the scheduler.",
		}),
		mediaOperations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "maxposty", Name: "media_operations_total",
			Help: "Total private media operations by bounded operation and outcome.",
		}, []string{"operation", "outcome"}),
		attachmentUploads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "maxposty", Name: "attachment_uploads_total",
			Help: "Total post attachment upload attempts by bounded media type and outcome.",
		}, []string{"type", "outcome"}),
		attachmentReady: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "maxposty", Name: "attachment_upload_ready_duration_seconds",
			Help:    "Server-observed time from attachment upload start until it is ready for preview.",
			Buckets: []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 30, 60, 120, 300},
		}, []string{"type", "outcome"}),
		attachmentBytes: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "maxposty", Name: "attachment_upload_size_bytes",
			Help:    "Uploaded post attachment size by bounded media type.",
			Buckets: []float64{10 << 10, 100 << 10, 1 << 20, 5 << 20, 20 << 20, 50 << 20, 100 << 20, 250 << 20},
		}, []string{"type"}),
		slowQueryThreshold: defaultSlowQueryThreshold,
	}
	registry.MustRegister(
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.httpRequests, m.httpErrors, m.httpDuration, m.httpInFlight,
		m.dbDuration, m.dbErrors, m.dbSlow,
		m.publicationTotal, m.publicationTime,
		m.schedulerJobs, m.schedulerDue, m.schedulerCycles, m.schedulerCycleTime,
		m.schedulerLastRun, m.schedulerInterval, m.recoveredPosts, m.mediaOperations,
		m.attachmentUploads, m.attachmentReady, m.attachmentBytes,
	)
	m.handler = promhttp.HandlerFor(registry, promhttp.HandlerOpts{EnableOpenMetrics: true})
	return m
}

// Handler returns the Prometheus exposition handler for this registry.
func (m *Metrics) Handler() http.Handler { return m.handler }

// Registry exposes the gatherer for focused tests and health diagnostics.
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }

// ObserveHTTPRequest records one completed HTTP request. route must be a Chi
// route template rather than the raw request path.
func (m *Metrics) ObserveHTTPRequest(method, route string, status int, elapsed time.Duration) {
	method = httpMethod(method)
	statusClass := httpStatusClass(status)
	m.httpRequests.WithLabelValues(method, route, statusClass).Inc()
	m.httpDuration.WithLabelValues(method, route).Observe(elapsed.Seconds())
	if status >= http.StatusBadRequest {
		m.httpErrors.WithLabelValues(method, route, statusClass).Inc()
	}
}

func httpMethod(method string) string {
	normalized := strings.ToUpper(strings.TrimSpace(method))
	switch normalized {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodOptions, http.MethodHead:
		return normalized
	default:
		return "OTHER"
	}
}

func (m *Metrics) IncHTTPInFlight() { m.httpInFlight.Inc() }

func (m *Metrics) DecHTTPInFlight() { m.httpInFlight.Dec() }

// ObservePublicationOperation records an external MAX mutation or metadata
// synchronization after its outcome is known.
func (m *Metrics) ObservePublicationOperation(operation, outcome string, elapsed time.Duration) {
	m.publicationTotal.WithLabelValues(operation, outcome).Inc()
	m.publicationTime.WithLabelValues(operation, outcome).Observe(elapsed.Seconds())
}

func (m *Metrics) ObserveSchedulerJob(job, outcome string) {
	m.schedulerJobs.WithLabelValues(job, outcome).Inc()
}

func (m *Metrics) SetSchedulerDue(job string, count int) {
	m.schedulerDue.WithLabelValues(job).Set(float64(count))
}

func (m *Metrics) ObserveSchedulerCycle(elapsed time.Duration, completedAt time.Time) {
	m.schedulerCycles.Inc()
	m.schedulerCycleTime.Observe(elapsed.Seconds())
	m.schedulerLastRun.Set(float64(completedAt.Unix()))
}

// SetSchedulerInterval publishes the expected cadence so stale-cycle alerts
// follow configuration instead of relying on a hard-coded deployment value.
func (m *Metrics) SetSchedulerInterval(interval time.Duration) {
	if interval > 0 {
		m.schedulerInterval.Set(interval.Seconds())
	}
}

func (m *Metrics) AddRecoveredPublications(count int64) {
	if count > 0 {
		m.recoveredPosts.Add(float64(count))
	}
}

func (m *Metrics) ObserveMediaOperation(operation, outcome string) {
	switch operation {
	case "upload", "cleanup":
	default:
		operation = "other"
	}
	switch outcome {
	case "success", "error", "quota_exceeded", "busy":
	default:
		outcome = "other"
	}
	m.mediaOperations.WithLabelValues(operation, outcome).Inc()
}

// ObserveAttachmentUpload records one browser-to-storage upload without using
// filenames, MIME strings, account IDs or other unbounded labels. Rejected
// formats use the "unknown" type because their content is intentionally not
// accepted as an image or video.
func (m *Metrics) ObserveAttachmentUpload(attachmentType, outcome string, sizeBytes int64, elapsed time.Duration) {
	switch attachmentType {
	case store.PostAttachmentImage, store.PostAttachmentVideo:
	default:
		attachmentType = "unknown"
	}
	switch outcome {
	case "success", "error", "unsupported", "too_large", "quota_exceeded", "busy", "canceled":
	default:
		outcome = "other"
	}
	m.attachmentUploads.WithLabelValues(attachmentType, outcome).Inc()
	if elapsed >= 0 {
		m.attachmentReady.WithLabelValues(attachmentType, outcome).Observe(elapsed.Seconds())
	}
	if sizeBytes > 0 {
		m.attachmentBytes.WithLabelValues(attachmentType).Observe(float64(sizeBytes))
	}
}

// RegisterDBPoolStats adds a collector backed by database/sql's concurrency
// and wait statistics. It must be called once for a Metrics instance.
func (m *Metrics) RegisterDBPoolStats(stats func() sql.DBStats) error {
	return m.registry.Register(newDBPoolCollector(stats))
}

// RegisterProductAnalytics adds a one-minute cached collector for global,
// PII-free active-user and funnel aggregates. A failed refresh preserves the
// latest successful values so transient database failures do not erase graphs.
func (m *Metrics) RegisterProductAnalytics(fetch func(context.Context, time.Time) (store.ProductAnalyticsSnapshot, error)) error {
	return m.registry.Register(newProductAnalyticsCollector(fetch, time.Minute, 5*time.Second))
}

type queryTrace struct {
	startedAt time.Time
	operation string
}

type queryTraceKey struct{}

// TraceQueryStart implements pgx.QueryTracer without retaining query text.
func (m *Metrics) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, queryTraceKey{}, queryTrace{
		startedAt: time.Now(), operation: sqlOperation(data.SQL),
	})
}

// TraceQueryEnd implements pgx.QueryTracer. Only the bounded SQL operation
// classification is exported; SQL text and arguments are intentionally ignored.
func (m *Metrics) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	trace, ok := ctx.Value(queryTraceKey{}).(queryTrace)
	if !ok {
		return
	}
	elapsed := time.Since(trace.startedAt)
	result := "success"
	if data.Err != nil && !errors.Is(data.Err, pgx.ErrNoRows) && !errors.Is(data.Err, sql.ErrNoRows) {
		result = "error"
		m.dbErrors.WithLabelValues(trace.operation).Inc()
	}
	m.dbDuration.WithLabelValues(trace.operation, result).Observe(elapsed.Seconds())
	if elapsed >= m.slowQueryThreshold {
		m.dbSlow.WithLabelValues(trace.operation).Inc()
	}
}

func httpStatusClass(status int) string {
	if status < 100 || status > 599 {
		return "unknown"
	}
	return string(rune('0'+status/100)) + "xx"
}

func sqlOperation(query string) string {
	fields := strings.Fields(strings.TrimSpace(query))
	if len(fields) == 0 {
		return "other"
	}
	operation := strings.ToLower(strings.Trim(fields[0], "();"))
	switch operation {
	case "select", "insert", "update", "delete", "copy", "begin", "commit", "rollback":
		return operation
	case "with":
		return "with"
	default:
		return "other"
	}
}

type dbPoolCollector struct {
	stats       func() sql.DBStats
	maxOpen     *prometheus.Desc
	open        *prometheus.Desc
	inUse       *prometheus.Desc
	idle        *prometheus.Desc
	waitCount   *prometheus.Desc
	waitSeconds *prometheus.Desc
	closed      *prometheus.Desc
}

func newDBPoolCollector(stats func() sql.DBStats) *dbPoolCollector {
	return &dbPoolCollector{
		stats:       stats,
		maxOpen:     prometheus.NewDesc("maxposty_db_pool_max_open_connections", "Configured maximum open database connections.", nil, nil),
		open:        prometheus.NewDesc("maxposty_db_pool_open_connections", "Current open database connections.", nil, nil),
		inUse:       prometheus.NewDesc("maxposty_db_pool_in_use_connections", "Current database connections in use.", nil, nil),
		idle:        prometheus.NewDesc("maxposty_db_pool_idle_connections", "Current idle database connections.", nil, nil),
		waitCount:   prometheus.NewDesc("maxposty_db_pool_wait_count_total", "Total waits for an available database connection.", nil, nil),
		waitSeconds: prometheus.NewDesc("maxposty_db_pool_wait_duration_seconds_total", "Total time spent waiting for database connections.", nil, nil),
		closed:      prometheus.NewDesc("maxposty_db_pool_connections_closed_total", "Total database connections closed by database/sql, by reason.", []string{"reason"}, nil),
	}
}

func (c *dbPoolCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.maxOpen
	ch <- c.open
	ch <- c.inUse
	ch <- c.idle
	ch <- c.waitCount
	ch <- c.waitSeconds
	ch <- c.closed
}

func (c *dbPoolCollector) Collect(ch chan<- prometheus.Metric) {
	stats := c.stats()
	ch <- prometheus.MustNewConstMetric(c.maxOpen, prometheus.GaugeValue, float64(stats.MaxOpenConnections))
	ch <- prometheus.MustNewConstMetric(c.open, prometheus.GaugeValue, float64(stats.OpenConnections))
	ch <- prometheus.MustNewConstMetric(c.inUse, prometheus.GaugeValue, float64(stats.InUse))
	ch <- prometheus.MustNewConstMetric(c.idle, prometheus.GaugeValue, float64(stats.Idle))
	ch <- prometheus.MustNewConstMetric(c.waitCount, prometheus.CounterValue, float64(stats.WaitCount))
	ch <- prometheus.MustNewConstMetric(c.waitSeconds, prometheus.CounterValue, stats.WaitDuration.Seconds())
	ch <- prometheus.MustNewConstMetric(c.closed, prometheus.CounterValue, float64(stats.MaxIdleClosed), "idle_limit")
	ch <- prometheus.MustNewConstMetric(c.closed, prometheus.CounterValue, float64(stats.MaxIdleTimeClosed), "idle_time")
	ch <- prometheus.MustNewConstMetric(c.closed, prometheus.CounterValue, float64(stats.MaxLifetimeClosed), "lifetime")
}

type productAnalyticsCollector struct {
	fetch           func(context.Context, time.Time) (store.ProductAnalyticsSnapshot, error)
	refreshInterval time.Duration
	queryTimeout    time.Duration
	now             func() time.Time

	mu             sync.Mutex
	refreshing     bool
	lastAttempt    time.Time
	lastSuccess    time.Time
	collectErrors  float64
	latestSnapshot store.ProductAnalyticsSnapshot

	activeUsers *prometheus.Desc
	funnelUsers *prometheus.Desc
	mediaPosts  *prometheus.Desc
	errors      *prometheus.Desc
	lastOK      *prometheus.Desc
}

func newProductAnalyticsCollector(
	fetch func(context.Context, time.Time) (store.ProductAnalyticsSnapshot, error),
	refreshInterval, queryTimeout time.Duration,
) *productAnalyticsCollector {
	return &productAnalyticsCollector{
		fetch: fetch, refreshInterval: refreshInterval, queryTimeout: queryTimeout, now: time.Now,
		activeUsers: prometheus.NewDesc(
			"maxposty_product_active_users", "Rolling unique active users by UTC period.", []string{"period"}, nil,
		),
		funnelUsers: prometheus.NewDesc(
			"maxposty_product_funnel_users", "Unique users at each progressive product funnel stage.", []string{"stage"}, nil,
		),
		mediaPosts: prometheus.NewDesc(
			"maxposty_product_published_posts", "Published posts by bounded attachment adoption segment.", []string{"kind"}, nil,
		),
		errors: prometheus.NewDesc(
			"maxposty_product_analytics_collect_errors_total", "Total failed product analytics refreshes.", nil, nil,
		),
		lastOK: prometheus.NewDesc(
			"maxposty_product_analytics_last_success_timestamp_seconds", "Unix timestamp of the latest successful product analytics refresh.", nil, nil,
		),
	}
}

func (c *productAnalyticsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.activeUsers
	ch <- c.funnelUsers
	ch <- c.mediaPosts
	ch <- c.errors
	ch <- c.lastOK
}

func (c *productAnalyticsCollector) Collect(ch chan<- prometheus.Metric) {
	c.refreshIfDue()

	c.mu.Lock()
	snapshot := c.latestSnapshot
	errorsTotal := c.collectErrors
	lastSuccess := c.lastSuccess
	c.mu.Unlock()

	for period, value := range map[string]int64{
		"day": snapshot.DailyActiveUsers, "week": snapshot.WeeklyActiveUsers, "month": snapshot.MonthlyActiveUsers,
	} {
		ch <- prometheus.MustNewConstMetric(c.activeUsers, prometheus.GaugeValue, float64(value), period)
	}
	for stage, value := range map[string]int64{
		"registered":             snapshot.RegisteredUsers,
		"max_linked":             snapshot.MAXLinkedUsers,
		"channel_connected":      snapshot.ChannelConnectedUsers,
		"post_created":           snapshot.PostCreatedUsers,
		"scheduled_or_published": snapshot.PostScheduledOrPublishedUsers,
		"published":              snapshot.PostPublishedUsers,
	} {
		ch <- prometheus.MustNewConstMetric(c.funnelUsers, prometheus.GaugeValue, float64(value), stage)
	}
	for kind, value := range map[string]int64{
		"total":    snapshot.PublishedPosts,
		"media":    snapshot.PublishedPostsWithMedia,
		"multiple": snapshot.PublishedPostsWithMultiple,
		"video":    snapshot.PublishedPostsWithVideo,
		"mixed":    snapshot.PublishedPostsWithMixedMedia,
	} {
		ch <- prometheus.MustNewConstMetric(c.mediaPosts, prometheus.GaugeValue, float64(value), kind)
	}
	ch <- prometheus.MustNewConstMetric(c.errors, prometheus.CounterValue, errorsTotal)
	lastSuccessUnix := float64(0)
	if !lastSuccess.IsZero() {
		lastSuccessUnix = float64(lastSuccess.Unix())
	}
	ch <- prometheus.MustNewConstMetric(c.lastOK, prometheus.GaugeValue, lastSuccessUnix)
}

func (c *productAnalyticsCollector) refreshIfDue() {
	now := c.now().UTC()
	c.mu.Lock()
	if c.refreshing || (!c.lastAttempt.IsZero() && now.Sub(c.lastAttempt) < c.refreshInterval) {
		c.mu.Unlock()
		return
	}
	c.refreshing = true
	c.lastAttempt = now
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), c.queryTimeout)
	snapshot, err := c.fetch(ctx, now)
	cancel()

	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshing = false
	if err != nil {
		c.collectErrors++
		return
	}
	c.latestSnapshot = snapshot
	c.lastSuccess = now
}
