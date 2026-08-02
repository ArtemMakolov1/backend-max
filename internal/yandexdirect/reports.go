package yandexdirect

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	defaultReportRetryInterval = 5 * time.Second
	maxReportResponseBytes     = 4 << 20
)

// ReportStatus describes the three successful states returned by the Direct
// Reports endpoint. Queued and processing reports must be requested again with
// byte-for-byte equivalent parameters after RetryAfter.
type ReportStatus string

const (
	ReportStatusReady      ReportStatus = "ready"
	ReportStatusQueued     ReportStatus = "queued"
	ReportStatusProcessing ReportStatus = "processing"
)

// CampaignStatisticsRequest is the deliberately narrow report supported by
// MaxPosty. DateFrom and DateTo are inclusive calendar dates.
type CampaignStatisticsRequest struct {
	CampaignID int64
	DateFrom   time.Time
	DateTo     time.Time
}

// CampaignStatisticsRow contains provider-native values. Direct reports money
// in micros by default; conversion to application minor units belongs to the
// app boundary so totals can be rounded only after exact integer aggregation.
type CampaignStatisticsRow struct {
	Date        string
	CampaignID  int64
	Impressions int64
	Clicks      int64
	CostMicros  int64
	Conversions float64
}

type CampaignStatisticsReport struct {
	Status         ReportStatus
	Rows           []CampaignStatisticsRow
	RetryAfter     time.Duration
	ReportsInQueue int
	RequestID      string
	Units          *UnitsUsage
	UnitsUsedLogin string
}

// RequestCampaignStatistics performs one Reports request. A 200 response is a
// ready online/offline report; 201 and 202 are successful pending states, not
// provider errors.
func (c *Client) RequestCampaignStatistics(
	ctx context.Context, token, clientLogin string, input CampaignStatisticsRequest,
) (CampaignStatisticsReport, error) {
	if c == nil || c.baseURL == nil || c.http == nil {
		return CampaignStatisticsReport{}, errors.New("yandex Direct client is unavailable")
	}
	if strings.TrimSpace(token) == "" {
		return CampaignStatisticsReport{}, &Error{Code: "missing_access_token"}
	}
	if err := validateCampaignStatisticsRequest(input); err != nil {
		return CampaignStatisticsReport{}, err
	}
	payload := campaignStatisticsPayload(input, clientLogin)
	encoded, err := json.Marshal(payload)
	if err != nil {
		return CampaignStatisticsReport{}, err
	}
	endpoint := *c.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/reports"
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, endpoint.String(), bytes.NewReader(encoded),
	)
	if err != nil {
		return CampaignStatisticsReport{}, err
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	request.Header.Set("Accept-Language", "ru")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/tab-separated")
	request.Header.Set("processingMode", "auto")
	request.Header.Set("skipReportHeader", "true")
	request.Header.Set("skipColumnHeader", "true")
	request.Header.Set("skipReportSummary", "true")
	if clientLogin = strings.TrimSpace(clientLogin); clientLogin != "" {
		request.Header.Set("Client-Login", clientLogin)
	}

	response, err := c.http.Do(request)
	if err != nil {
		return CampaignStatisticsReport{}, fmt.Errorf("request Yandex Direct report: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := readBoundedReportBody(response.Body)
	if err != nil {
		return CampaignStatisticsReport{}, err
	}
	metadata, err := parseReportResponseMetadata(response)
	if err != nil {
		return CampaignStatisticsReport{}, err
	}

	switch response.StatusCode {
	case http.StatusOK:
		rows, parseErr := parseCampaignStatisticsTSV(body)
		if parseErr != nil {
			return CampaignStatisticsReport{}, &Error{
				StatusCode: response.StatusCode, Code: "invalid_report_response",
				RequestID: metadata.RequestID,
			}
		}
		metadata.Status = ReportStatusReady
		metadata.Rows = rows
		return metadata, nil
	case http.StatusCreated:
		metadata.Status = ReportStatusQueued
		return metadata, nil
	case http.StatusAccepted:
		metadata.Status = ReportStatusProcessing
		return metadata, nil
	default:
		providerErr := reportHTTPError(response.StatusCode, metadata, body)
		var directErr *Error
		if c.transport != nil && errors.As(providerErr, &directErr) {
			c.transport.noteAPIError(clientLogin, directErr.APIErrorCode)
		}
		return CampaignStatisticsReport{}, providerErr
	}
}

// GetCampaignStatistics supports both online and offline processing. It
// repeats the exact same report request after the provider-recommended delay
// until the report is ready or the caller's context is canceled.
func (c *Client) GetCampaignStatistics(
	ctx context.Context, token, clientLogin string, input CampaignStatisticsRequest,
) (CampaignStatisticsReport, error) {
	for {
		report, err := c.RequestCampaignStatistics(ctx, token, clientLogin, input)
		if err != nil {
			return CampaignStatisticsReport{}, err
		}
		if report.Status == ReportStatusReady {
			return report, nil
		}
		delay := report.RetryAfter
		if delay < 0 {
			delay = defaultReportRetryInterval
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return CampaignStatisticsReport{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func validateCampaignStatisticsRequest(input CampaignStatisticsRequest) error {
	if input.CampaignID <= 0 {
		return errors.New("yandex Direct report campaign id must be positive")
	}
	from := input.DateFrom.UTC()
	to := input.DateTo.UTC()
	if from.IsZero() || to.IsZero() || from.After(to) {
		return errors.New("invalid Yandex Direct report date range")
	}
	if to.Sub(from) >= 366*24*time.Hour {
		return errors.New("yandex Direct report date range is too large")
	}
	return nil
}

func campaignStatisticsPayload(input CampaignStatisticsRequest, clientLogin string) map[string]any {
	from := input.DateFrom.UTC().Format(time.DateOnly)
	to := input.DateTo.UTC().Format(time.DateOnly)
	digest := sha256.Sum256([]byte(strings.Join([]string{
		"campaign-daily-v1", strings.TrimSpace(clientLogin),
		strconv.FormatInt(input.CampaignID, 10), from, to,
	}, "\x00")))
	reportName := "maxposty_campaign_daily_v1_" + hex.EncodeToString(digest[:16])
	return map[string]any{"params": map[string]any{
		"SelectionCriteria": map[string]any{
			"DateFrom": from,
			"DateTo":   to,
			"Filter": []any{map[string]any{
				"Field": "CampaignId", "Operator": "EQUALS",
				"Values": []string{strconv.FormatInt(input.CampaignID, 10)},
			}},
		},
		"FieldNames": []string{
			"Date", "CampaignId", "Impressions", "Clicks", "Cost", "Conversions",
		},
		"Page":          map[string]any{"Limit": 400},
		"OrderBy":       []any{map[string]any{"Field": "Date", "SortOrder": "ASCENDING"}},
		"ReportName":    reportName,
		"ReportType":    "CAMPAIGN_PERFORMANCE_REPORT",
		"DateRangeType": "CUSTOM_DATE",
		"Format":        "TSV",
		"IncludeVAT":    "YES",
	}}
}

func readBoundedReportBody(reader io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, maxReportResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxReportResponseBytes {
		return nil, &Error{Code: "report_response_too_large"}
	}
	return body, nil
}

func parseReportResponseMetadata(response *http.Response) (CampaignStatisticsReport, error) {
	requestID := strings.TrimSpace(response.Header.Get("RequestId"))
	unitsValue := response.Header.Get("Units")
	units, hasUnits := parseUnitsUsage(unitsValue)
	if strings.TrimSpace(unitsValue) != "" && !hasUnits {
		return CampaignStatisticsReport{}, &Error{
			StatusCode: response.StatusCode, Code: "invalid_units_header", RequestID: requestID,
		}
	}
	if hasUnits {
		units.UsedLogin = strings.TrimSpace(response.Header.Get("Units-Used-Login"))
		units.RequestID = requestID
		units.ObservedAt = time.Now().UTC()
	}
	retryAfter, present, err := parseNonnegativeReportHeader(
		response.Header.Get("retryIn"), "invalid_retry_in_header",
	)
	if err != nil {
		return CampaignStatisticsReport{}, &Error{
			StatusCode: response.StatusCode, Code: err.Error(), RequestID: requestID,
		}
	}
	if !present {
		retryAfter = -1
	}
	queue, _, err := parseNonnegativeReportHeader(
		response.Header.Get("reportsInQueue"), "invalid_reports_in_queue_header",
	)
	if err != nil {
		return CampaignStatisticsReport{}, &Error{
			StatusCode: response.StatusCode, Code: err.Error(), RequestID: requestID,
		}
	}
	var unitsSnapshot *UnitsUsage
	if hasUnits {
		unitsSnapshot = &units
	}
	return CampaignStatisticsReport{
		RetryAfter: time.Duration(retryAfter) * time.Second, ReportsInQueue: int(queue),
		RequestID:      requestID,
		UnitsUsedLogin: strings.TrimSpace(response.Header.Get("Units-Used-Login")),
		Units:          unitsSnapshot,
	}, nil
}

func parseNonnegativeReportHeader(value, code string) (int64, bool, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 32)
	if err != nil || parsed < 0 {
		return 0, true, errors.New(code)
	}
	return parsed, true, nil
}

func parseCampaignStatisticsTSV(body []byte) ([]CampaignStatisticsRow, error) {
	body = bytes.TrimPrefix(body, []byte{0xef, 0xbb, 0xbf})
	if len(bytes.TrimSpace(body)) == 0 {
		return []CampaignStatisticsRow{}, nil
	}
	reader := csv.NewReader(bytes.NewReader(body))
	reader.Comma = '\t'
	reader.FieldsPerRecord = 6
	reader.ReuseRecord = true
	result := make([]CampaignStatisticsRow, 0)
	for {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		row, err := parseCampaignStatisticsRecord(record)
		if err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, nil
}

func parseCampaignStatisticsRecord(record []string) (CampaignStatisticsRow, error) {
	if len(record) != 6 {
		return CampaignStatisticsRow{}, errors.New("unexpected report column count")
	}
	date := strings.TrimSpace(record[0])
	if parsed, err := time.Parse(time.DateOnly, date); err != nil || parsed.Format(time.DateOnly) != date {
		return CampaignStatisticsRow{}, errors.New("invalid report date")
	}
	campaignID, err := parseNonnegativeReportInteger(record[1], false)
	if err != nil || campaignID == 0 {
		return CampaignStatisticsRow{}, errors.New("invalid report campaign id")
	}
	impressions, err := parseNonnegativeReportInteger(record[2], true)
	if err != nil {
		return CampaignStatisticsRow{}, errors.New("invalid report impressions")
	}
	clicks, err := parseNonnegativeReportInteger(record[3], true)
	if err != nil {
		return CampaignStatisticsRow{}, errors.New("invalid report clicks")
	}
	cost, err := parseNonnegativeReportInteger(record[4], true)
	if err != nil {
		return CampaignStatisticsRow{}, errors.New("invalid report cost")
	}
	conversions, err := parseReportDecimal(record[5])
	if err != nil || conversions < 0 {
		return CampaignStatisticsRow{}, errors.New("invalid report conversions")
	}
	return CampaignStatisticsRow{
		Date: date, CampaignID: campaignID, Impressions: impressions,
		Clicks: clicks, CostMicros: cost, Conversions: conversions,
	}, nil
}

func parseNonnegativeReportInteger(value string, unavailableAsZero bool) (int64, error) {
	value = strings.TrimSpace(value)
	if unavailableAsZero && value == "--" {
		return 0, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		return 0, errors.New("invalid nonnegative report integer")
	}
	return parsed, nil
}

func parseReportDecimal(value string) (float64, error) {
	value = strings.TrimSpace(value)
	if value == "--" {
		return 0, nil
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, errors.New("invalid report decimal")
	}
	return parsed, nil
}

func reportHTTPError(
	statusCode int, metadata CampaignStatisticsReport, body []byte,
) error {
	var envelope struct {
		Error *struct {
			Code       int    `json:"error_code"`
			StringCode string `json:"error_string"`
			Detail     string `json:"error_detail"`
			RequestID  string `json:"request_id"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &envelope)
	code, message := "reports_request_failed", ""
	apiCode := 0
	if envelope.Error != nil {
		apiCode = envelope.Error.Code
		if strings.TrimSpace(envelope.Error.StringCode) != "" {
			code = strings.TrimSpace(envelope.Error.StringCode)
		} else if envelope.Error.Code != 0 {
			code = strconv.Itoa(envelope.Error.Code)
		}
		message = strings.TrimSpace(envelope.Error.Detail)
		if metadata.RequestID == "" {
			metadata.RequestID = strings.TrimSpace(envelope.Error.RequestID)
		}
	}
	result := &Error{
		StatusCode: statusCode, APIErrorCode: apiCode, Code: code,
		Message: message, RequestID: metadata.RequestID,
	}
	if metadata.RetryAfter > 0 {
		result.RetryAfter = metadata.RetryAfter
	}
	if metadata.Units != nil {
		usage := *metadata.Units
		result.Units = &usage
	}
	return result
}
