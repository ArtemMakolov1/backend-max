package app

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"maxpilot/backend/internal/store"
	"maxpilot/backend/internal/yandexdirect"
)

const directReportTimeout = 2 * time.Minute

var (
	ErrDirectReportsUnsupported    = errors.New("provider for Yandex Direct does not support reports")
	ErrDirectStatisticsUnavailable = errors.New("statistics are unavailable before the campaign exists in Yandex Direct")
)

// DirectReportsProvider is kept separate from the mutation provider contract:
// read-only statistics remain usable when provider writes are disabled.
type DirectReportsProvider interface {
	GetCampaignStatistics(
		context.Context, string, string, yandexdirect.CampaignStatisticsRequest,
	) (yandexdirect.CampaignStatisticsReport, error)
}

type DirectCampaignStatisticsValues struct {
	Impressions int64   `json:"impressions"`
	Clicks      int64   `json:"clicks"`
	CostMinor   int64   `json:"cost_minor"`
	Conversions float64 `json:"conversions"`
}

type DirectCampaignDailyStatistics struct {
	Date string `json:"date"`
	DirectCampaignStatisticsValues
}

type DirectCampaignStatistics struct {
	CampaignID   string                          `json:"campaign_id"`
	From         string                          `json:"from"`
	To           string                          `json:"to"`
	CurrencyCode string                          `json:"currency_code"`
	Totals       DirectCampaignStatisticsValues  `json:"totals"`
	Daily        []DirectCampaignDailyStatistics `json:"daily"`
	UpdatedAt    time.Time                       `json:"updated_at"`
	// Provider metadata is retained for safe server-side diagnostics and unit
	// accounting without widening the stable browser contract.
	ProviderRequestID string                   `json:"-"`
	ProviderUnits     *yandexdirect.UnitsUsage `json:"-"`
}

// GetDirectCampaignStatistics reads a bounded daily report directly from
// Yandex Direct. Statistics are deliberately not persisted at this stage.
func (a *App) GetDirectCampaignStatistics(
	ctx context.Context, actorUserID, workspaceID, campaignID string,
	from, to time.Time,
) (DirectCampaignStatistics, error) {
	if !a.DirectConfigured() {
		return DirectCampaignStatistics{}, ErrDirectNotConfigured
	}
	reports, ok := a.direct.(DirectReportsProvider)
	if !ok {
		return DirectCampaignStatistics{}, ErrDirectReportsUnsupported
	}
	from, to = directReportDate(from), directReportDate(to)
	if from.IsZero() || to.IsZero() || from.After(to) ||
		to.Sub(from) >= 366*24*time.Hour {
		return DirectCampaignStatistics{}, fmt.Errorf(
			"%w: invalid statistics date range", store.ErrDirectValidation,
		)
	}
	campaign, err := a.store.GetDirectCampaign(ctx, actorUserID, workspaceID, campaignID)
	if err != nil {
		return DirectCampaignStatistics{}, err
	}
	if campaign.ProviderCampaignID == nil || *campaign.ProviderCampaignID <= 0 {
		return DirectCampaignStatistics{}, ErrDirectStatisticsUnavailable
	}
	connection, err := a.store.GetDirectConnection(ctx, actorUserID, workspaceID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return DirectCampaignStatistics{}, store.ErrDirectConnectionRequired
		}
		return DirectCampaignStatistics{}, err
	}
	if connection.ID != campaign.ConnectionID ||
		(!strings.EqualFold(strings.TrimSpace(connection.Status), "active") &&
			!strings.EqualFold(strings.TrimSpace(connection.Status), "error")) {
		return DirectCampaignStatistics{}, store.ErrDirectConnectionRequired
	}
	token, err := a.directAccessToken(ctx, connection)
	if err != nil {
		return DirectCampaignStatistics{}, err
	}
	reportCtx, cancel := context.WithTimeout(ctx, directReportTimeout)
	defer cancel()
	report, err := reports.GetCampaignStatistics(
		reportCtx, token, connection.ClientLogin,
		yandexdirect.CampaignStatisticsRequest{
			CampaignID: *campaign.ProviderCampaignID, DateFrom: from, DateTo: to,
		},
	)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return DirectCampaignStatistics{}, err
		}
		if directProviderAuthorizationError(err) {
			markErr := a.markDirectConnectionAuthorizationRequired(
				ctx, connection, err, a.now().UTC(),
			)
			return DirectCampaignStatistics{}, errors.Join(
				store.ErrDirectConnectionRequired, markErr,
			)
		}
		return DirectCampaignStatistics{}, fmt.Errorf("%w: %w", ErrDirectProvider, err)
	}
	if report.Status != yandexdirect.ReportStatusReady {
		return DirectCampaignStatistics{}, fmt.Errorf(
			"%w: report did not reach ready state", ErrDirectProvider,
		)
	}
	statistics, err := aggregateDirectCampaignStatistics(
		campaign.ID, campaign.CurrencyCode, *campaign.ProviderCampaignID,
		from, to, report.Rows, a.now().UTC(),
	)
	if err != nil {
		return DirectCampaignStatistics{}, fmt.Errorf("%w: %w", ErrDirectProvider, err)
	}
	statistics.ProviderRequestID = strings.TrimSpace(report.RequestID)
	statistics.ProviderUnits = report.Units
	return statistics, nil
}

type directDailyReportAccumulator struct {
	Impressions int64
	Clicks      int64
	CostMicros  int64
	Conversions float64
}

func aggregateDirectCampaignStatistics(
	campaignID, currencyCode string, providerCampaignID int64,
	from, to time.Time, rows []yandexdirect.CampaignStatisticsRow, updatedAt time.Time,
) (DirectCampaignStatistics, error) {
	daily := make(map[string]directDailyReportAccumulator, len(rows))
	var totals directDailyReportAccumulator
	for _, row := range rows {
		if row.CampaignID != providerCampaignID {
			return DirectCampaignStatistics{}, errors.New("report contains another campaign")
		}
		rowDate, err := time.Parse(time.DateOnly, row.Date)
		if err != nil || rowDate.Before(from) || rowDate.After(to) {
			return DirectCampaignStatistics{}, errors.New("report row is outside requested date range")
		}
		if row.Impressions < 0 || row.Clicks < 0 || row.CostMicros < 0 ||
			row.Conversions < 0 || math.IsNaN(row.Conversions) || math.IsInf(row.Conversions, 0) {
			return DirectCampaignStatistics{}, errors.New("report contains invalid metrics")
		}
		current := daily[row.Date]
		if current.Impressions, err = checkedDirectReportAdd(current.Impressions, row.Impressions); err != nil {
			return DirectCampaignStatistics{}, err
		}
		if current.Clicks, err = checkedDirectReportAdd(current.Clicks, row.Clicks); err != nil {
			return DirectCampaignStatistics{}, err
		}
		if current.CostMicros, err = checkedDirectReportAdd(current.CostMicros, row.CostMicros); err != nil {
			return DirectCampaignStatistics{}, err
		}
		current.Conversions += row.Conversions
		if math.IsInf(current.Conversions, 0) {
			return DirectCampaignStatistics{}, errors.New("report conversions overflow")
		}
		daily[row.Date] = current
		if totals.Impressions, err = checkedDirectReportAdd(totals.Impressions, row.Impressions); err != nil {
			return DirectCampaignStatistics{}, err
		}
		if totals.Clicks, err = checkedDirectReportAdd(totals.Clicks, row.Clicks); err != nil {
			return DirectCampaignStatistics{}, err
		}
		if totals.CostMicros, err = checkedDirectReportAdd(totals.CostMicros, row.CostMicros); err != nil {
			return DirectCampaignStatistics{}, err
		}
		totals.Conversions += row.Conversions
		if math.IsInf(totals.Conversions, 0) {
			return DirectCampaignStatistics{}, errors.New("report conversions overflow")
		}
	}
	dates := make([]string, 0, len(daily))
	for date := range daily {
		dates = append(dates, date)
	}
	sort.Strings(dates)
	result := DirectCampaignStatistics{
		CampaignID: campaignID, From: from.Format(time.DateOnly), To: to.Format(time.DateOnly),
		CurrencyCode: strings.ToUpper(strings.TrimSpace(currencyCode)),
		Daily:        make([]DirectCampaignDailyStatistics, 0, len(dates)), UpdatedAt: updatedAt.UTC(),
	}
	for _, date := range dates {
		values := daily[date]
		result.Daily = append(result.Daily, DirectCampaignDailyStatistics{
			Date: date,
			DirectCampaignStatisticsValues: DirectCampaignStatisticsValues{
				Impressions: values.Impressions, Clicks: values.Clicks,
				CostMinor:   microsToRoundedDirectMinor(values.CostMicros),
				Conversions: values.Conversions,
			},
		})
	}
	result.Totals = DirectCampaignStatisticsValues{
		Impressions: totals.Impressions, Clicks: totals.Clicks,
		CostMinor:   microsToRoundedDirectMinor(totals.CostMicros),
		Conversions: totals.Conversions,
	}
	return result, nil
}

func directReportDate(value time.Time) time.Time {
	if value.IsZero() {
		return time.Time{}
	}
	year, month, day := value.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

// microsToRoundedDirectMinor converts 1/1,000,000 currency units to 1/100
// currency units using integer half-away-from-zero rounding.
func microsToRoundedDirectMinor(micros int64) int64 {
	const microsPerMinor int64 = 10_000
	minor := micros / microsPerMinor
	remainder := micros % microsPerMinor
	if remainder >= microsPerMinor/2 {
		return minor + 1
	}
	if remainder <= -microsPerMinor/2 {
		return minor - 1
	}
	return minor
}

func checkedDirectReportAdd(left, right int64) (int64, error) {
	if (right > 0 && left > math.MaxInt64-right) ||
		(right < 0 && left < math.MinInt64-right) {
		return 0, errors.New("report integer metrics overflow")
	}
	return left + right, nil
}
