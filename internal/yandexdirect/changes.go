package yandexdirect

import (
	"context"
	"strings"
	"time"
)

const directChangesOverlap = time.Minute

// CampaignChanges is the result of a Changes.check cursor advance for one
// campaign. Timestamp is provider time and must be persisted only after any
// indicated graph refresh has completed successfully.
type CampaignChanges struct {
	Modified           bool
	GraphModified      bool
	StatisticsModified bool
	Timestamp          time.Time
}

// CurrentChangesTimestamp obtains Yandex Direct server time for a new cursor.
// Calling checkDictionaries without Timestamp is the provider-documented
// bootstrap sequence and avoids gaps caused by clock skew.
func (c *Client) CurrentChangesTimestamp(
	ctx context.Context, token, clientLogin string,
) (time.Time, error) {
	var response struct {
		Timestamp string `json:"Timestamp"`
	}
	err := c.call(ctx, "changes", token, clientLogin, map[string]any{
		"method": "checkDictionaries",
		"params": map[string]any{},
	}, &response)
	if err != nil {
		return time.Time{}, err
	}
	timestamp, err := time.Parse(time.RFC3339, strings.TrimSpace(response.Timestamp))
	if err != nil || timestamp.IsZero() {
		return time.Time{}, &Error{Code: "invalid_changes_response"}
	}
	return timestamp.UTC(), nil
}

// CheckCampaignChanges checks campaign, group/keyword, ad and statistics
// changes without downloading the full provider graph.
func (c *Client) CheckCampaignChanges(
	ctx context.Context, token, clientLogin string, campaignID int64, since time.Time,
) (CampaignChanges, error) {
	if campaignID <= 0 {
		return CampaignChanges{}, &Error{Code: "invalid_campaign_id"}
	}
	if since.IsZero() {
		return CampaignChanges{}, &Error{Code: "invalid_changes_timestamp"}
	}
	// An overlap protects the boundary between the last full graph read and the
	// provider timestamp without increasing the cost of Changes.check.
	since = since.UTC().Add(-directChangesOverlap).Truncate(time.Second)
	var response struct {
		Modified struct {
			CampaignIDs   []int64 `json:"CampaignIds"`
			AdGroupIDs    []int64 `json:"AdGroupIds"`
			AdIDs         []int64 `json:"AdIds"`
			CampaignsStat []struct {
				CampaignID int64  `json:"CampaignId"`
				BorderDate string `json:"BorderDate"`
			} `json:"CampaignsStat"`
		} `json:"Modified"`
		NotFound struct {
			CampaignIDs []int64 `json:"CampaignIds"`
		} `json:"NotFound"`
		Unprocessed struct {
			CampaignIDs []int64 `json:"CampaignIds"`
		} `json:"Unprocessed"`
		Timestamp string `json:"Timestamp"`
	}
	err := c.call(ctx, "changes", token, clientLogin, map[string]any{
		"method": "check",
		"params": map[string]any{
			"CampaignIds": []int64{campaignID},
			"Timestamp":   since.Format(time.RFC3339),
			"FieldNames": []string{
				"CampaignIds", "AdGroupIds", "AdIds", "CampaignsStat",
			},
		},
	}, &response)
	if err != nil {
		return CampaignChanges{}, err
	}
	if containsDirectID(response.NotFound.CampaignIDs, campaignID) {
		return CampaignChanges{}, &Error{Code: "campaign_not_found"}
	}
	if len(response.Unprocessed.CampaignIDs) != 0 {
		return CampaignChanges{}, &Error{Code: "changes_unprocessed"}
	}
	timestamp, err := time.Parse(time.RFC3339, strings.TrimSpace(response.Timestamp))
	if err != nil || timestamp.Before(since) {
		return CampaignChanges{}, &Error{Code: "invalid_changes_response"}
	}
	for _, id := range response.Modified.CampaignIDs {
		if id != campaignID {
			return CampaignChanges{}, &Error{Code: "invalid_changes_response"}
		}
	}
	for _, statistic := range response.Modified.CampaignsStat {
		if statistic.CampaignID != campaignID {
			return CampaignChanges{}, &Error{Code: "invalid_changes_response"}
		}
		if statistic.BorderDate != "" {
			if _, parseErr := time.Parse(time.DateOnly, statistic.BorderDate); parseErr != nil {
				return CampaignChanges{}, &Error{Code: "invalid_changes_response"}
			}
		}
	}
	graphModified := len(response.Modified.CampaignIDs) != 0 ||
		len(response.Modified.AdGroupIDs) != 0 ||
		len(response.Modified.AdIDs) != 0
	statisticsModified := len(response.Modified.CampaignsStat) != 0
	return CampaignChanges{
		Modified:      graphModified || statisticsModified,
		GraphModified: graphModified, StatisticsModified: statisticsModified,
		Timestamp: timestamp.UTC(),
	}, nil
}

func containsDirectID(values []int64, expected int64) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
