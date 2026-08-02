package yandexdirect

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"time"
)

const (
	campaignDetailBatchSize       = 1_000
	maxCampaignListObjects  int64 = 10_000
)

var campaignSummaryFieldNames = []string{
	"Id", "Name", "Type", "Status", "State", "StatusPayment",
	"StartDate", "EndDate", "TimeZone",
}

// CampaignSummary is the common Campaigns.get projection used to enumerate
// an advertiser's campaigns without depending on campaign-type-specific
// fields. Dates retain Yandex Direct's YYYY-MM-DD representation.
type CampaignSummary struct {
	ID            int64   `json:"id"`
	Name          string  `json:"name"`
	Type          string  `json:"type"`
	Status        string  `json:"status"`
	State         string  `json:"state"`
	StatusPayment string  `json:"status_payment"`
	StartDate     string  `json:"start_date"`
	EndDate       *string `json:"end_date,omitempty"`
	TimeZone      string  `json:"time_zone"`
}

type campaignSummaryGetItem struct {
	ID            int64   `json:"Id"`
	Name          string  `json:"Name"`
	Type          string  `json:"Type"`
	Status        string  `json:"Status"`
	State         string  `json:"State"`
	StatusPayment string  `json:"StatusPayment"`
	StartDate     string  `json:"StartDate"`
	EndDate       *string `json:"EndDate"`
	TimeZone      string  `json:"TimeZone"`
}

type campaignIDGetItem struct {
	ID int64 `json:"Id"`
}

// ListCampaigns returns every campaign visible to the selected advertiser.
// Campaigns.get caps a result set at 10,000 objects. The first request freezes
// the provider ID set in one maximum-sized page; subsequent requests hydrate
// only those IDs in bounded batches. This avoids offset drift silently turning
// a concurrently changing provider collection into an incomplete snapshot.
func (c *Client) ListCampaigns(
	ctx context.Context, token, clientLogin string,
) ([]CampaignSummary, error) {
	ids, err := c.listCampaignIDs(ctx, token, clientLogin)
	if err != nil {
		return nil, err
	}
	campaigns := make([]CampaignSummary, len(ids))
	for start := 0; start < len(ids); start += campaignDetailBatchSize {
		end := min(start+campaignDetailBatchSize, len(ids))
		batch := ids[start:end]
		details, err := c.listCampaignDetails(ctx, token, clientLogin, batch)
		if err != nil {
			return nil, err
		}
		for index, id := range batch {
			campaigns[start+index] = details[id]
		}
	}
	return campaigns, nil
}

func (c *Client) listCampaignIDs(
	ctx context.Context, token, clientLogin string,
) ([]int64, error) {
	var response struct {
		Campaigns json.RawMessage `json:"Campaigns"`
		LimitedBy *int64          `json:"LimitedBy"`
	}
	err := c.call(ctx, "campaigns", token, clientLogin, map[string]any{
		"method": "get",
		"params": map[string]any{
			"SelectionCriteria": map[string]any{},
			"FieldNames":        []string{"Id"},
			"Page":              map[string]any{"Limit": maxCampaignListObjects},
		},
	}, &response)
	if err != nil {
		return nil, err
	}
	items, err := decodeCampaignIDPage(response.Campaigns)
	if err != nil {
		return nil, err
	}
	if int64(len(items)) > maxCampaignListObjects || response.LimitedBy != nil {
		return nil, &Error{Code: "campaign_list_limit_exceeded"}
	}
	ids := make([]int64, 0, len(items))
	seen := make(map[int64]struct{}, len(items))
	for _, item := range items {
		if item.ID <= 0 {
			return nil, &Error{Code: "invalid_campaign_list_response"}
		}
		if _, duplicate := seen[item.ID]; duplicate {
			return nil, &Error{Code: "duplicate_campaign_list_response"}
		}
		seen[item.ID] = struct{}{}
		ids = append(ids, item.ID)
	}
	return ids, nil
}

func (c *Client) listCampaignDetails(
	ctx context.Context, token, clientLogin string, ids []int64,
) (map[int64]CampaignSummary, error) {
	if len(ids) == 0 || len(ids) > campaignDetailBatchSize {
		return nil, &Error{Code: "invalid_campaign_list_response"}
	}
	var response struct {
		Campaigns json.RawMessage `json:"Campaigns"`
		LimitedBy *int64          `json:"LimitedBy"`
	}
	err := c.call(ctx, "campaigns", token, clientLogin, map[string]any{
		"method": "get",
		"params": map[string]any{
			"SelectionCriteria": map[string]any{
				"Ids": append([]int64(nil), ids...),
			},
			"FieldNames": append([]string(nil), campaignSummaryFieldNames...),
		},
	}, &response)
	if err != nil {
		return nil, err
	}
	if response.LimitedBy != nil {
		return nil, &Error{Code: "invalid_provider_pagination"}
	}
	items, err := decodeCampaignSummaryPage(response.Campaigns)
	if err != nil {
		return nil, err
	}
	if len(items) > len(ids) {
		return nil, &Error{Code: "invalid_campaign_list_response"}
	}
	expected := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return nil, &Error{Code: "invalid_campaign_list_response"}
		}
		expected[id] = struct{}{}
	}
	details := make(map[int64]CampaignSummary, len(items))
	for _, item := range items {
		campaign, err := exportCampaignSummary(item)
		if err != nil {
			return nil, err
		}
		if _, requested := expected[campaign.ID]; !requested {
			return nil, &Error{Code: "invalid_campaign_list_response"}
		}
		if _, duplicate := details[campaign.ID]; duplicate {
			return nil, &Error{Code: "duplicate_campaign_list_response"}
		}
		details[campaign.ID] = campaign
	}
	if len(details) != len(expected) {
		return nil, &Error{Code: "invalid_campaign_list_response"}
	}
	return details, nil
}

func decodeCampaignIDPage(raw json.RawMessage) ([]campaignIDGetItem, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, &Error{Code: "invalid_campaign_list_response"}
	}
	var items []campaignIDGetItem
	if err := json.Unmarshal(trimmed, &items); err != nil || items == nil {
		return nil, &Error{Code: "invalid_campaign_list_response"}
	}
	return items, nil
}

func decodeCampaignSummaryPage(raw json.RawMessage) ([]campaignSummaryGetItem, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, &Error{Code: "invalid_campaign_list_response"}
	}
	var items []campaignSummaryGetItem
	if err := json.Unmarshal(trimmed, &items); err != nil || items == nil {
		return nil, &Error{Code: "invalid_campaign_list_response"}
	}
	return items, nil
}

func exportCampaignSummary(item campaignSummaryGetItem) (CampaignSummary, error) {
	if item.ID <= 0 || strings.TrimSpace(item.Name) == "" ||
		strings.TrimSpace(item.Type) == "" || strings.TrimSpace(item.Status) == "" ||
		strings.TrimSpace(item.State) == "" || strings.TrimSpace(item.StatusPayment) == "" ||
		strings.TrimSpace(item.StartDate) == "" || strings.TrimSpace(item.TimeZone) == "" {
		return CampaignSummary{}, &Error{Code: "invalid_campaign_list_response"}
	}
	if _, err := time.Parse(time.DateOnly, item.StartDate); err != nil {
		return CampaignSummary{}, &Error{Code: "invalid_campaign_list_response"}
	}
	var endDate *string
	if item.EndDate != nil {
		if strings.TrimSpace(*item.EndDate) == "" {
			return CampaignSummary{}, &Error{Code: "invalid_campaign_list_response"}
		}
		if _, err := time.Parse(time.DateOnly, *item.EndDate); err != nil {
			return CampaignSummary{}, &Error{Code: "invalid_campaign_list_response"}
		}
		value := *item.EndDate
		endDate = &value
	}
	return CampaignSummary{
		ID: item.ID, Name: item.Name, Type: item.Type, Status: item.Status,
		State: item.State, StatusPayment: item.StatusPayment, StartDate: item.StartDate,
		EndDate: endDate, TimeZone: item.TimeZone,
	}, nil
}
