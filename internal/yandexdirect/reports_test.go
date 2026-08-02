package yandexdirect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestRequestCampaignStatisticsParsesOnlineReportAndMetadata(t *testing.T) {
	t.Parallel()
	const providerCampaignID int64 = 9_007_199_254_740_001
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/v501/reports" {
			t.Errorf("path = %q", r.URL.Path)
		}
		for name, want := range map[string]string{
			"Authorization":     "Bearer report-token",
			"Client-Login":      "direct-login",
			"processingMode":    "auto",
			"skipReportHeader":  "true",
			"skipColumnHeader":  "true",
			"skipReportSummary": "true",
		} {
			if got := r.Header.Get(name); got != want {
				t.Errorf("%s = %q, want %q", name, got, want)
			}
		}
		if got := r.Header.Get("returnMoneyInMicros"); got != "" {
			t.Errorf("returnMoneyInMicros = %q; default micros must be retained", got)
		}
		var payload struct {
			Params struct {
				SelectionCriteria struct {
					DateFrom string `json:"DateFrom"`
					DateTo   string `json:"DateTo"`
					Filter   []struct {
						Field    string   `json:"Field"`
						Operator string   `json:"Operator"`
						Values   []string `json:"Values"`
					} `json:"Filter"`
				} `json:"SelectionCriteria"`
				FieldNames    []string `json:"FieldNames"`
				ReportName    string   `json:"ReportName"`
				ReportType    string   `json:"ReportType"`
				DateRangeType string   `json:"DateRangeType"`
				Format        string   `json:"Format"`
				IncludeVAT    string   `json:"IncludeVAT"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		if payload.Params.SelectionCriteria.DateFrom != "2026-07-01" ||
			payload.Params.SelectionCriteria.DateTo != "2026-07-02" ||
			len(payload.Params.SelectionCriteria.Filter) != 1 ||
			!reflect.DeepEqual(payload.Params.SelectionCriteria.Filter[0].Values,
				[]string{"9007199254740001"}) ||
			payload.Params.SelectionCriteria.Filter[0].Operator != "EQUALS" ||
			payload.Params.ReportType != "CAMPAIGN_PERFORMANCE_REPORT" ||
			payload.Params.DateRangeType != "CUSTOM_DATE" ||
			payload.Params.Format != "TSV" || payload.Params.IncludeVAT != "YES" ||
			payload.Params.ReportName == "" {
			t.Errorf("report payload = %#v", payload.Params)
		}
		wantFields := []string{
			"Date", "CampaignId", "Impressions", "Clicks", "Cost", "Conversions",
		}
		if !reflect.DeepEqual(payload.Params.FieldNames, wantFields) {
			t.Errorf("fields = %#v", payload.Params.FieldNames)
		}
		w.Header().Set("RequestId", "report-request-123")
		w.Header().Set("Units", "7/93/100")
		w.Header().Set("Units-Used-Login", "direct-login")
		w.Header().Set("Content-Type", "text/tab-separated; charset=utf-8")
		_, _ = fmt.Fprintf(w,
			"2026-07-01\t%d\t100\t7\t123450000\t2\n"+
				"2026-07-02\t%d\t0\t0\t0\t--\n",
			providerCampaignID, providerCampaignID,
		)
	}))
	defer server.Close()
	client, err := New(
		server.URL+"/json/v501", "client-id", "secret", CallbackRedirectURI, server.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}
	report, err := client.RequestCampaignStatistics(
		context.Background(), "report-token", "direct-login", CampaignStatisticsRequest{
			CampaignID: providerCampaignID,
			DateFrom:   time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
			DateTo:     time.Date(2026, time.July, 2, 0, 0, 0, 0, time.UTC),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != ReportStatusReady || report.RequestID != "report-request-123" ||
		report.Units == nil || report.Units.Spent != 7 || report.Units.Remaining != 93 ||
		report.Units.DailyLimit != 100 || report.Units.RequestID != "report-request-123" ||
		report.UnitsUsedLogin != "direct-login" || len(report.Rows) != 2 {
		t.Fatalf("report = %#v", report)
	}
	if report.Rows[0].CostMicros != 123_450_000 || report.Rows[0].Conversions != 2 ||
		report.Rows[1].Conversions != 0 {
		t.Fatalf("rows = %#v", report.Rows)
	}
}

func TestGetCampaignStatisticsPolls201And202WithIdenticalRequest(t *testing.T) {
	t.Parallel()
	var (
		mu     sync.Mutex
		bodies [][]byte
		calls  int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body bytes.Buffer
		_, _ = body.ReadFrom(r.Body)
		mu.Lock()
		bodies = append(bodies, append([]byte(nil), body.Bytes()...))
		calls++
		call := calls
		mu.Unlock()
		w.Header().Set("RequestId", fmt.Sprintf("request-%d", call))
		w.Header().Set("retryIn", "0")
		switch call {
		case 1:
			w.Header().Set("reportsInQueue", "1")
			w.WriteHeader(http.StatusCreated)
		case 2:
			w.WriteHeader(http.StatusAccepted)
		default:
			_, _ = w.Write([]byte("2026-07-01\t7001\t12\t3\t15000\t1\n"))
		}
	}))
	defer server.Close()
	client, err := New(
		server.URL+"/json/v501", "client-id", "secret", CallbackRedirectURI, server.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}
	report, err := client.GetCampaignStatistics(
		context.Background(), "token", "", CampaignStatisticsRequest{
			CampaignID: 7001,
			DateFrom:   time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
			DateTo:     time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 3 || len(bodies) != 3 || !bytes.Equal(bodies[0], bodies[1]) ||
		!bytes.Equal(bodies[1], bodies[2]) {
		t.Fatalf("offline polling calls=%d identical=%t/%t", calls,
			bytes.Equal(bodies[0], bodies[1]), bytes.Equal(bodies[1], bodies[2]))
	}
	if report.Status != ReportStatusReady || report.RequestID != "request-3" ||
		len(report.Rows) != 1 {
		t.Fatalf("report = %#v", report)
	}
}

func TestRequestCampaignStatisticsPreservesProviderErrorMetadata(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("RequestId", "failed-report-request")
		w.Header().Set("Units", "5/95/100")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"error_code":152,"error_string":"UnitsExhausted","error_detail":"private provider detail"}}`))
	}))
	defer server.Close()
	client, err := New(
		server.URL+"/json/v501", "client-id", "secret", CallbackRedirectURI, server.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.RequestCampaignStatistics(
		context.Background(), "token", "", CampaignStatisticsRequest{
			CampaignID: 7001,
			DateFrom:   time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
			DateTo:     time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
		},
	)
	var providerErr *Error
	if !errors.As(err, &providerErr) || providerErr.StatusCode != http.StatusBadRequest ||
		providerErr.APIErrorCode != 152 || providerErr.Code != "UnitsExhausted" ||
		providerErr.RequestID != "failed-report-request" || providerErr.Units == nil ||
		providerErr.Units.Spent != 5 {
		t.Fatalf("provider error = %#v", err)
	}
	if got := err.Error(); bytes.Contains([]byte(got), []byte("private provider detail")) {
		t.Fatalf("provider detail leaked through public error: %s", got)
	}
}

func TestReportMetadataRejectsMalformedUnitsValues(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"1/2", "1/-2/3", "1/two/3", "1/2/3/4"} {
		response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Units": []string{value}}}
		if _, err := parseReportResponseMetadata(response); err == nil {
			t.Errorf("Units %q was accepted", value)
		}
	}
	response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header)}
	if metadata, err := parseReportResponseMetadata(response); err != nil || metadata.Units != nil {
		t.Fatalf("empty Units metadata = %#v, %v", metadata, err)
	}
}
