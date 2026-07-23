package yandexdirect

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const (
	// CampaignGraphFingerprintVersion is part of the consent contract. Any
	// change to the canonical fields or their normalization requires a new
	// version rather than silently changing an existing fingerprint.
	CampaignGraphFingerprintVersion = "yandex-direct-campaign-graph-v1"

	maxGraphRegions           = 100
	maxGraphPageSize          = 10_000
	maxGraphMutationItems     = 1_000
	maxGraphCampaignUpdates   = 10
	maxModerationItems        = 10_000
	maxAdGroupNameRunes       = 255
	maxTrackingMarkerRunes    = 128
	maxTrackingParamsRunes    = 1024
	maxNegativeKeywordRunes   = 4096
	maxCampaignNegativeRunes  = 20_000
	maxCampaignBlockedIPs     = 25
	maxCampaignExcludedSites  = 1_000
	maxExcludedSiteRunes      = 255
	maxCampaignCounters       = 1_000
	maxTimeTargetingItems     = 168
	maxGraphReconcileObjects  = 10_000
	maxKeywordRunes           = 4096
	maxKeywordWords           = 7
	maxKeywordWordRunes       = 35
	maxResponsiveTitles       = 7
	maxResponsiveTexts        = 3
	maxResponsiveTitleRunes   = 56
	maxResponsiveTitleWord    = 22
	maxResponsiveTextRunes    = 81
	maxResponsiveTextWord     = 23
	maxResponsiveHrefRunes    = 1024
	graphOperationMarkerParam = "mp_op"
	graphTrackingMarkerParam  = "mp_group"
)

var graphTrackingMarkerPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// ProviderIssue preserves per-item errors and warnings returned by Direct.
// Array mutation methods may partially succeed, so callers must not discard
// these details even when the method also returns an error.
type ProviderIssue struct {
	Code    int    `json:"code"`
	Message string `json:"message,omitempty"`
	Details string `json:"details,omitempty"`
}

// MutationResult corresponds to one input item in a Direct array mutation.
type MutationResult struct {
	ID       int64           `json:"id,omitempty"`
	Warnings []ProviderIssue `json:"warnings,omitempty"`
	Errors   []ProviderIssue `json:"errors,omitempty"`
}

// PartialMutationError indicates that Direct processed an array mutation but
// at least one item failed. Results contains successful IDs as well as every
// per-item error, in the same order as the request.
type PartialMutationError struct {
	Operation string
	Results   []MutationResult
}

func (e *PartialMutationError) Error() string {
	if e == nil {
		return "Yandex Direct mutation partially failed"
	}
	failed := 0
	for _, result := range e.Results {
		if result.ID <= 0 || len(result.Errors) != 0 {
			failed++
		}
	}
	return fmt.Sprintf("Yandex Direct %s partially failed for %d of %d items",
		e.Operation, failed, len(e.Results))
}

// GeoRegion is an exact region-name match returned by Dictionaries.
type GeoRegion struct {
	ID          int64    `json:"id"`
	Name        string   `json:"name"`
	ParentNames []string `json:"parent_names,omitempty"`
}

// UnifiedAdGroupDraft is the supported, intentionally narrow v501 group
// shape. OfferRetargeting is always sent as NO.
type UnifiedAdGroupDraft struct {
	CampaignID       int64
	Name             string
	RegionIDs        []int64
	NegativeKeywords []string
	TrackingMarker   string
}

// UnifiedAdGroupOperationName is the durable idempotency key for a group add.
// Unified groups do not support TrackingParams, so recovery relies on this
// exact provider-visible name and fails closed if multiple groups match.
func UnifiedAdGroupOperationName(marker string) string {
	return "MaxPosty · " + strings.TrimSpace(marker)
}

// UnifiedCampaignUpdate is the complete narrow campaign snapshot this client
// is allowed to edit. The strategy must remain the supported weekly-budget
// strategy and its budget must match WeeklyBudgetMinor.
type UnifiedCampaignUpdate struct {
	ID                int64
	Name              string
	WeeklyBudgetMinor int64
	StartsAt          time.Time
	EndsAt            time.Time
	BiddingStrategy   json.RawMessage
	Settings          []GraphCampaignSetting
	TrackingParams    string
}

// UnifiedAdGroupUpdate contains only group fields managed by this client.
type UnifiedAdGroupUpdate struct {
	ID               int64
	Name             string
	RegionIDs        []int64
	NegativeKeywords []string
	TrackingMarker   string
}

// UnifiedAdGroup is a normalized v501 unified performance group.
type UnifiedAdGroup struct {
	ID               int64    `json:"id"`
	CampaignID       int64    `json:"campaign_id"`
	Name             string   `json:"name"`
	RegionIDs        []int64  `json:"region_ids"`
	NegativeKeywords []string `json:"negative_keywords"`
	TrackingParams   string   `json:"tracking_params,omitempty"`
	TrackingMarker   string   `json:"tracking_marker,omitempty"`
	OfferRetargeting string   `json:"offer_retargeting"`
	Status           string   `json:"status,omitempty"`
	ServingStatus    string   `json:"serving_status,omitempty"`
}

// ResponsiveAdDraft is the minimal current combinatorial ad accepted for a
// unified performance group.
type ResponsiveAdDraft struct {
	AdGroupID int64
	Titles    []string
	Texts     []string
	Href      string
}

// ResponsiveAdUpdate contains only the supported combinatorial creative.
type ResponsiveAdUpdate struct {
	ID     int64
	Titles []string
	Texts  []string
	Href   string
}

// ModeratedText preserves the provider's status for an individual title or
// text while keeping the content available for canonical fingerprinting.
type ModeratedText struct {
	Value               string `json:"value"`
	Status              string `json:"status,omitempty"`
	StatusClarification string `json:"status_clarification,omitempty"`
}

// ResponsiveAd is the supported current combinatorial ad representation.
type ResponsiveAd struct {
	ID                  int64           `json:"id"`
	CampaignID          int64           `json:"campaign_id"`
	AdGroupID           int64           `json:"ad_group_id"`
	Titles              []ModeratedText `json:"titles"`
	Texts               []ModeratedText `json:"texts"`
	Href                string          `json:"href"`
	Status              string          `json:"status,omitempty"`
	State               string          `json:"state,omitempty"`
	StatusClarification string          `json:"status_clarification,omitempty"`
}

// KeywordDraft is one phrase to add. Bids are deliberately absent because the
// supported campaign uses an automatic bidding strategy.
type KeywordDraft struct {
	AdGroupID int64
	Keyword   string
}

// KeywordUpdate edits one existing keyword phrase. Autotargeting objects are
// intentionally unsupported.
type KeywordUpdate struct {
	ID      int64
	Keyword string
}

// Keyword is the normalized provider phrase representation used by the graph.
type Keyword struct {
	ID               int64  `json:"id"`
	CampaignID       int64  `json:"campaign_id"`
	AdGroupID        int64  `json:"ad_group_id"`
	Keyword          string `json:"keyword"`
	UserParam1       string `json:"user_param_1"`
	UserParam2       string `json:"user_param_2"`
	StrategyPriority string `json:"strategy_priority,omitempty"`
	Status           string `json:"status,omitempty"`
	State            string `json:"state,omitempty"`
	ServingStatus    string `json:"serving_status,omitempty"`
}

// GraphCampaignSetting is one explicitly returned UnifiedCampaign option.
// Unknown future options remain part of the graph instead of being discarded.
type GraphCampaignSetting struct {
	Option string `json:"option"`
	Value  string `json:"value"`
}

// GraphPriorityGoal is one campaign-level priority goal returned by Direct.
type GraphPriorityGoal struct {
	GoalID                 int64  `json:"goal_id"`
	Value                  int64  `json:"value"`
	IsMetrikaSourceOfValue string `json:"is_metrika_source_of_value,omitempty"`
}

// GraphHolidaysSchedule preserves the holiday branch of TimeTargeting.
type GraphHolidaysSchedule struct {
	SuspendOnHolidays string `json:"suspend_on_holidays"`
	BidPercent        int64  `json:"bid_percent"`
	StartHour         int64  `json:"start_hour"`
	EndHour           int64  `json:"end_hour"`
}

// GraphTimeTargeting preserves the full campaign time-targeting object.
// Present distinguishes an omitted/null object from an explicitly empty one.
type GraphTimeTargeting struct {
	Present                 bool                   `json:"present"`
	Schedule                []string               `json:"schedule"`
	ConsiderWorkingWeekends string                 `json:"consider_working_weekends,omitempty"`
	HolidaysSchedule        *GraphHolidaysSchedule `json:"holidays_schedule,omitempty"`
}

// GraphCampaign is the consent-sensitive v501 campaign representation.
// It deliberately contains every campaign setting that can change delivery.
type GraphCampaign struct {
	ID                          int64                  `json:"id"`
	Name                        string                 `json:"name"`
	Status                      string                 `json:"status,omitempty"`
	State                       string                 `json:"state,omitempty"`
	Type                        string                 `json:"type"`
	WeeklyBudgetMinor           int64                  `json:"weekly_budget_minor"`
	StartsAt                    time.Time              `json:"starts_at"`
	EndsAt                      time.Time              `json:"ends_at"`
	TimeZone                    string                 `json:"time_zone"`
	NegativeKeywords            []string               `json:"negative_keywords"`
	NegativeKeywordSharedSetIDs []int64                `json:"negative_keyword_shared_set_ids"`
	BlockedIPs                  []string               `json:"blocked_ips"`
	ExcludedSites               []string               `json:"excluded_sites"`
	TimeTargeting               GraphTimeTargeting     `json:"time_targeting"`
	BiddingStrategy             json.RawMessage        `json:"bidding_strategy"`
	Settings                    []GraphCampaignSetting `json:"settings"`
	CounterIDs                  []int64                `json:"counter_ids"`
	PriorityGoals               []GraphPriorityGoal    `json:"priority_goals"`
	TrackingParams              string                 `json:"tracking_params"`
	AttributionModel            string                 `json:"attribution_model"`
}

// CampaignGraph is the complete graph supported by this client. Operational
// statuses are retained for launch-readiness checks but excluded from the
// content fingerprint.
type CampaignGraph struct {
	Campaign GraphCampaign    `json:"campaign"`
	AdGroups []UnifiedAdGroup `json:"ad_groups"`
	Ads      []ResponsiveAd   `json:"ads"`
	Keywords []Keyword        `json:"keywords"`
}

type graphActionResult struct {
	ID       int64      `json:"Id"`
	Errors   []apiIssue `json:"Errors"`
	Warnings []apiIssue `json:"Warnings"`
}

type graphStringItems struct {
	Items []string `json:"Items"`
}

type graphInt64Items struct {
	Items []int64 `json:"Items"`
}

// SupportsUnifiedGraph reports whether the client targets the v501 endpoint
// required by the graph primitives.
func (c *Client) SupportsUnifiedGraph() bool {
	return c != nil && c.unified
}

// ResolveRegionNames resolves every requested name through the current
// getGeoRegions ExactNames API. It preserves request order and fails if a name
// is missing, returns multiple IDs, or is not an exact Unicode/case-folded
// match. No fuzzy result is ever selected.
func (c *Client) ResolveRegionNames(
	ctx context.Context, token, clientLogin string, names []string,
) ([]GeoRegion, error) {
	if err := c.requireUnifiedGraph(); err != nil {
		return nil, err
	}
	normalizedNames, err := validateRegionNames(names)
	if err != nil {
		return nil, err
	}
	var response struct {
		LimitedBy *int64 `json:"LimitedBy"`
		Regions   []struct {
			ID          int64             `json:"GeoRegionId"`
			Name        string            `json:"GeoRegionName"`
			ParentNames *graphStringItems `json:"ParentGeoRegionNames"`
		} `json:"GeoRegions"`
	}
	err = c.call(ctx, "dictionaries", token, clientLogin, map[string]any{
		"method": "getGeoRegions",
		"params": map[string]any{
			"SelectionCriteria": map[string]any{"ExactNames": normalizedNames},
			"FieldNames": []string{
				"GeoRegionId", "GeoRegionName", "ParentGeoRegionNames",
			},
			"Page": map[string]any{"Limit": 1000, "Offset": 0},
		},
	}, &response)
	if err != nil {
		return nil, err
	}
	if response.LimitedBy != nil {
		return nil, &Error{Code: "region_resolution_incomplete"}
	}
	requested := make(map[string]struct{}, len(normalizedNames))
	for _, name := range normalizedNames {
		requested[graphFold(name)] = struct{}{}
	}
	matches := make(map[string]map[int64]GeoRegion, len(normalizedNames))
	for _, item := range response.Regions {
		item.Name = graphText(item.Name)
		key := graphFold(item.Name)
		if _, ok := requested[key]; !ok || item.ID <= 0 || item.Name == "" {
			return nil, &Error{Code: "invalid_region_response"}
		}
		parentNames := []string{}
		if item.ParentNames != nil {
			for _, parent := range item.ParentNames.Items {
				if parent = graphText(parent); parent != "" {
					parentNames = append(parentNames, parent)
				}
			}
		}
		sort.Strings(parentNames)
		if matches[key] == nil {
			matches[key] = make(map[int64]GeoRegion)
		}
		matches[key][item.ID] = GeoRegion{
			ID: item.ID, Name: item.Name, ParentNames: parentNames,
		}
	}
	resolved := make([]GeoRegion, 0, len(normalizedNames))
	for _, name := range normalizedNames {
		byID := matches[graphFold(name)]
		switch len(byID) {
		case 0:
			return nil, &Error{Code: "region_name_not_found", Message: name}
		case 1:
			for _, region := range byID {
				resolved = append(resolved, region)
			}
		default:
			return nil, &Error{Code: "region_name_ambiguous", Message: name}
		}
	}
	return resolved, nil
}

// CreateUnifiedAdGroup creates one unified performance group and preserves
// provider warnings. Per-item provider errors are returned as
// *PartialMutationError with the result still available to the caller.
func (c *Client) CreateUnifiedAdGroup(
	ctx context.Context, token, clientLogin string, draft UnifiedAdGroupDraft,
) (MutationResult, error) {
	if err := c.requireUnifiedGraph(); err != nil {
		return MutationResult{}, err
	}
	draft, err := normalizeAdGroupDraft(draft)
	if err != nil {
		return MutationResult{}, err
	}
	group := map[string]any{
		"CampaignId":     draft.CampaignID,
		"Name":           draft.Name,
		"RegionIds":      draft.RegionIDs,
		"UnifiedAdGroup": map[string]any{"OfferRetargeting": "NO"},
	}
	if len(draft.NegativeKeywords) != 0 {
		group["NegativeKeywords"] = map[string]any{"Items": draft.NegativeKeywords}
	}
	var response struct {
		AddResults []graphActionResult `json:"AddResults"`
	}
	err = c.call(ctx, "adgroups", token, clientLogin, map[string]any{
		"method": "add",
		"params": map[string]any{"AdGroups": []any{group}},
	}, &response)
	if err != nil {
		return MutationResult{}, err
	}
	results, err := graphMutationResults("adgroup_add", 1, response.AddResults)
	if len(results) != 1 {
		return MutationResult{}, err
	}
	return results[0], err
}

// ListUnifiedAdGroups returns every group in the campaign and fails if Direct
// returns a different group type. This prevents an unsupported object from
// being silently omitted from a launch fingerprint.
func (c *Client) ListUnifiedAdGroups(
	ctx context.Context, token, clientLogin string, campaignID int64,
) ([]UnifiedAdGroup, error) {
	if err := c.requireUnifiedGraph(); err != nil {
		return nil, err
	}
	if campaignID <= 0 {
		return nil, graphValidationError("invalid_campaign_id")
	}
	offset := int64(0)
	groups := make([]UnifiedAdGroup, 0)
	for {
		var response struct {
			LimitedBy *int64 `json:"LimitedBy"`
			AdGroups  []struct {
				ID                          int64             `json:"Id"`
				CampaignID                  int64             `json:"CampaignId"`
				Name                        string            `json:"Name"`
				RegionIDs                   []int64           `json:"RegionIds"`
				RestrictedRegionIDs         *graphInt64Items  `json:"RestrictedRegionIds"`
				NegativeKeywords            *graphStringItems `json:"NegativeKeywords"`
				NegativeKeywordSharedSetIDs *graphInt64Items  `json:"NegativeKeywordSharedSetIds"`
				TrackingParams              string            `json:"TrackingParams"`
				Status                      string            `json:"Status"`
				ServingStatus               string            `json:"ServingStatus"`
				Type                        string            `json:"Type"`
				Subtype                     string            `json:"Subtype"`
				UnifiedAdGroup              *struct {
					OfferRetargeting string `json:"OfferRetargeting"`
				} `json:"UnifiedAdGroup"`
			} `json:"AdGroups"`
		}
		err := c.call(ctx, "adgroups", token, clientLogin, map[string]any{
			"method": "get",
			"params": map[string]any{
				"SelectionCriteria": map[string]any{"CampaignIds": []int64{campaignID}},
				"FieldNames": []string{
					"Id", "CampaignId", "Name", "RegionIds", "RestrictedRegionIds",
					"NegativeKeywords", "NegativeKeywordSharedSetIds",
					"TrackingParams", "Status", "ServingStatus", "Type", "Subtype",
				},
				"UnifiedAdGroupFieldNames": []string{"OfferRetargeting"},
				"Page":                     map[string]any{"Limit": maxGraphPageSize, "Offset": offset},
			},
		}, &response)
		if err != nil {
			return nil, err
		}
		for _, item := range response.AdGroups {
			if strings.ToUpper(strings.TrimSpace(item.Type)) != "UNIFIED_AD_GROUP" ||
				item.UnifiedAdGroup == nil {
				return nil, &Error{Code: "unsupported_adgroup_in_campaign"}
			}
			if strings.ToUpper(strings.TrimSpace(item.Subtype)) != "NONE" {
				return nil, &Error{Code: "unsupported_adgroup_subtype"}
			}
			if item.NegativeKeywordSharedSetIDs != nil &&
				len(item.NegativeKeywordSharedSetIDs.Items) != 0 {
				return nil, &Error{Code: "unsupported_adgroup_negative_shared_sets"}
			}
			// RestrictedRegionIds is provider-derived from legal restrictions
			// and cannot be edited by the advertiser, so it is intentionally
			// excluded from the user-authorized content fingerprint.
			_ = item.RestrictedRegionIDs
			negativeKeywords := []string{}
			if item.NegativeKeywords != nil {
				negativeKeywords = append(negativeKeywords, item.NegativeKeywords.Items...)
			}
			group := UnifiedAdGroup{
				ID: item.ID, CampaignID: item.CampaignID, Name: item.Name,
				RegionIDs: item.RegionIDs, NegativeKeywords: negativeKeywords,
				TrackingParams:   item.TrackingParams,
				TrackingMarker:   trackingMarkerFromParams(item.TrackingParams),
				OfferRetargeting: item.UnifiedAdGroup.OfferRetargeting,
				Status:           strings.ToUpper(strings.TrimSpace(item.Status)),
				ServingStatus:    strings.ToUpper(strings.TrimSpace(item.ServingStatus)),
			}
			group, err = normalizeUnifiedAdGroup(group, campaignID)
			if err != nil {
				return nil, err
			}
			groups = append(groups, group)
		}
		next, more, err := nextGraphPage(offset, response.LimitedBy)
		if err != nil {
			return nil, err
		}
		if !more {
			break
		}
		offset = next
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].ID < groups[j].ID })
	if err := validateUniqueGroupIDs(groups); err != nil {
		return nil, err
	}
	return groups, nil
}

// UpdateUnifiedAdGroups edits only name, regions, and negative keywords.
func (c *Client) UpdateUnifiedAdGroups(
	ctx context.Context, token, clientLogin string, updates []UnifiedAdGroupUpdate,
) ([]MutationResult, error) {
	if err := c.requireUnifiedGraph(); err != nil {
		return nil, err
	}
	normalized, err := normalizeUnifiedAdGroupUpdates(updates)
	if err != nil {
		return nil, err
	}
	items := make([]any, 0, len(normalized))
	ids := make([]int64, 0, len(normalized))
	for _, update := range normalized {
		items = append(items, map[string]any{
			"Id":               update.ID,
			"Name":             update.Name,
			"RegionIds":        update.RegionIDs,
			"NegativeKeywords": map[string]any{"Items": update.NegativeKeywords},
		})
		ids = append(ids, update.ID)
	}
	var response struct {
		UpdateResults []graphActionResult `json:"UpdateResults"`
	}
	err = c.call(ctx, "adgroups", token, clientLogin, map[string]any{
		"method": "update",
		"params": map[string]any{"AdGroups": items},
	}, &response)
	if err != nil {
		return nil, err
	}
	return graphMutationResultsForIDs(
		"adgroup_update", ids, response.UpdateResults, true,
	)
}

// CreateResponsiveAd creates one current v501 combinatorial ad.
func (c *Client) CreateResponsiveAd(
	ctx context.Context, token, clientLogin string, draft ResponsiveAdDraft,
) (MutationResult, error) {
	if err := c.requireUnifiedGraph(); err != nil {
		return MutationResult{}, err
	}
	draft, err := normalizeResponsiveAdDraft(draft)
	if err != nil {
		return MutationResult{}, err
	}
	var response struct {
		AddResults []graphActionResult `json:"AddResults"`
	}
	err = c.call(ctx, "ads", token, clientLogin, map[string]any{
		"method": "add",
		"params": map[string]any{
			"Ads": []any{map[string]any{
				"AdGroupId": draft.AdGroupID,
				"ResponsiveAd": map[string]any{
					"Titles": draft.Titles,
					"Texts":  draft.Texts,
					"Href":   draft.Href,
				},
			}},
		},
	}, &response)
	if err != nil {
		return MutationResult{}, err
	}
	results, err := graphMutationResults("responsive_ad_add", 1, response.AddResults)
	if len(results) != 1 {
		return MutationResult{}, err
	}
	return results[0], err
}

// ListResponsiveAds returns all ads in a campaign and fails on any unsupported
// ad type rather than filtering it out of the graph.
func (c *Client) ListResponsiveAds(
	ctx context.Context, token, clientLogin string, campaignID int64,
) ([]ResponsiveAd, error) {
	if err := c.requireUnifiedGraph(); err != nil {
		return nil, err
	}
	if campaignID <= 0 {
		return nil, graphValidationError("invalid_campaign_id")
	}
	offset := int64(0)
	ads := make([]ResponsiveAd, 0)
	for {
		var response struct {
			LimitedBy *int64 `json:"LimitedBy"`
			Ads       []struct {
				ID                  int64           `json:"Id"`
				CampaignID          int64           `json:"CampaignId"`
				AdGroupID           int64           `json:"AdGroupId"`
				Status              string          `json:"Status"`
				State               string          `json:"State"`
				StatusClarification string          `json:"StatusClarification"`
				Type                string          `json:"Type"`
				Subtype             string          `json:"Subtype"`
				AdCategories        json.RawMessage `json:"AdCategories"`
				AgeLabel            string          `json:"AgeLabel"`
				ResponsiveAd        *struct {
					Titles []struct {
						Value               string `json:"Title"`
						Status              string `json:"Status"`
						StatusClarification string `json:"StatusClarification"`
					} `json:"Titles"`
					Texts []struct {
						Value               string `json:"Text"`
						Status              string `json:"Status"`
						StatusClarification string `json:"StatusClarification"`
					} `json:"Texts"`
					Href                     string          `json:"Href"`
					DisplayDomain            string          `json:"DisplayDomain"`
					DisplayURLPath           string          `json:"DisplayUrlPath"`
					AdImages                 json.RawMessage `json:"AdImages"`
					SitelinkSetID            *int64          `json:"SitelinkSetId"`
					DisplayURLPathModeration json.RawMessage `json:"DisplayUrlPathModeration"`
					SitelinksModeration      json.RawMessage `json:"SitelinksModeration"`
					AdExtensions             json.RawMessage `json:"AdExtensions"`
					VideoExtensions          json.RawMessage `json:"VideoExtensions"`
					PriceExtension           json.RawMessage `json:"PriceExtension"`
					BusinessID               *int64          `json:"BusinessId"`
					ERIRAdDescription        string          `json:"ErirAdDescription"`
				} `json:"ResponsiveAd"`
			} `json:"Ads"`
		}
		err := c.call(ctx, "ads", token, clientLogin, map[string]any{
			"method": "get",
			"params": map[string]any{
				"SelectionCriteria": map[string]any{"CampaignIds": []int64{campaignID}},
				"FieldNames": []string{
					"Id", "CampaignId", "AdGroupId", "Status", "State",
					"StatusClarification", "Type", "Subtype", "AdCategories", "AgeLabel",
				},
				"ResponsiveAdFieldNames": []string{
					"Titles", "Texts", "Href", "DisplayDomain", "DisplayUrlPath",
					"AdImages", "SitelinkSetId", "DisplayUrlPathModeration",
					"SitelinksModeration", "AdExtensions", "VideoExtensions",
					"PriceExtension", "BusinessId", "ErirAdDescription",
				},
				"Page": map[string]any{"Limit": maxGraphPageSize, "Offset": offset},
			},
		}, &response)
		if err != nil {
			return nil, err
		}
		for _, item := range response.Ads {
			if strings.ToUpper(strings.TrimSpace(item.Type)) != "RESPONSIVE_AD" ||
				item.ResponsiveAd == nil {
				return nil, &Error{Code: "unsupported_ad_in_campaign"}
			}
			subtype := strings.ToUpper(strings.TrimSpace(item.Subtype))
			if subtype != "" && subtype != "NONE" {
				return nil, &Error{Code: "unsupported_ad_subtype"}
			}
			responsive := item.ResponsiveAd
			if graphOptionalFeaturePresent(item.AdCategories) ||
				graphText(item.AgeLabel) != "" ||
				graphText(responsive.DisplayURLPath) != "" ||
				graphOptionalFeaturePresent(responsive.AdImages) ||
				responsive.SitelinkSetID != nil ||
				graphOptionalFeaturePresent(responsive.DisplayURLPathModeration) ||
				graphOptionalFeaturePresent(responsive.SitelinksModeration) ||
				graphOptionalFeaturePresent(responsive.AdExtensions) ||
				graphOptionalFeaturePresent(responsive.VideoExtensions) ||
				graphOptionalFeaturePresent(responsive.PriceExtension) ||
				responsive.BusinessID != nil ||
				graphText(responsive.ERIRAdDescription) != "" {
				return nil, &Error{Code: "unsupported_responsive_ad_features"}
			}
			ad := ResponsiveAd{
				ID: item.ID, CampaignID: item.CampaignID, AdGroupID: item.AdGroupID,
				Href:                responsive.Href,
				Status:              strings.ToUpper(strings.TrimSpace(item.Status)),
				State:               strings.ToUpper(strings.TrimSpace(item.State)),
				StatusClarification: graphText(item.StatusClarification),
				Titles:              make([]ModeratedText, 0, len(responsive.Titles)),
				Texts:               make([]ModeratedText, 0, len(responsive.Texts)),
			}
			for _, title := range responsive.Titles {
				ad.Titles = append(ad.Titles, ModeratedText{
					Value:               graphText(title.Value),
					Status:              strings.ToUpper(strings.TrimSpace(title.Status)),
					StatusClarification: graphText(title.StatusClarification),
				})
			}
			for _, text := range responsive.Texts {
				ad.Texts = append(ad.Texts, ModeratedText{
					Value:               graphText(text.Value),
					Status:              strings.ToUpper(strings.TrimSpace(text.Status)),
					StatusClarification: graphText(text.StatusClarification),
				})
			}
			ad, err = normalizeResponsiveAd(ad, campaignID)
			if err != nil {
				return nil, err
			}
			ads = append(ads, ad)
		}
		next, more, err := nextGraphPage(offset, response.LimitedBy)
		if err != nil {
			return nil, err
		}
		if !more {
			break
		}
		offset = next
	}
	sort.Slice(ads, func(i, j int) bool { return ads[i].ID < ads[j].ID })
	if err := validateUniqueAdIDs(ads); err != nil {
		return nil, err
	}
	return ads, nil
}

// UpdateResponsiveAds edits only titles, texts, and HTTPS destination URLs.
func (c *Client) UpdateResponsiveAds(
	ctx context.Context, token, clientLogin string, updates []ResponsiveAdUpdate,
) ([]MutationResult, error) {
	if err := c.requireUnifiedGraph(); err != nil {
		return nil, err
	}
	normalized, err := normalizeResponsiveAdUpdates(updates)
	if err != nil {
		return nil, err
	}
	items := make([]any, 0, len(normalized))
	ids := make([]int64, 0, len(normalized))
	for _, update := range normalized {
		items = append(items, map[string]any{
			"Id": update.ID,
			"ResponsiveAd": map[string]any{
				"Titles": update.Titles,
				"Texts":  update.Texts,
				"Href":   update.Href,
			},
		})
		ids = append(ids, update.ID)
	}
	var response struct {
		UpdateResults []graphActionResult `json:"UpdateResults"`
	}
	err = c.call(ctx, "ads", token, clientLogin, map[string]any{
		"method": "update",
		"params": map[string]any{"Ads": items},
	}, &response)
	if err != nil {
		return nil, err
	}
	return graphMutationResultsForIDs(
		"responsive_ad_update", ids, response.UpdateResults, true,
	)
}

// AddKeywords adds phrases without manual bids and returns every per-item
// result, including successful IDs when another item fails.
func (c *Client) AddKeywords(
	ctx context.Context, token, clientLogin string, drafts []KeywordDraft,
) ([]MutationResult, error) {
	if err := c.requireUnifiedGraph(); err != nil {
		return nil, err
	}
	normalized, err := normalizeKeywordDrafts(drafts)
	if err != nil {
		return nil, err
	}
	items := make([]any, 0, len(normalized))
	for _, draft := range normalized {
		items = append(items, map[string]any{
			"AdGroupId":        draft.AdGroupID,
			"Keyword":          draft.Keyword,
			"StrategyPriority": "NORMAL",
		})
	}
	var response struct {
		AddResults []graphActionResult `json:"AddResults"`
	}
	err = c.call(ctx, "keywords", token, clientLogin, map[string]any{
		"method": "add",
		"params": map[string]any{"Keywords": items},
	}, &response)
	if err != nil {
		return nil, err
	}
	return graphMutationResults("keyword_add", len(normalized), response.AddResults)
}

// UpdateKeywords edits existing keyword phrases without touching bids,
// priorities, user parameters, or autotargeting settings. Direct may return a
// new ID when editing creates a replacement phrase, so returned IDs are
// intentionally preserved rather than forced to equal the input IDs.
func (c *Client) UpdateKeywords(
	ctx context.Context, token, clientLogin string, updates []KeywordUpdate,
) ([]MutationResult, error) {
	if err := c.requireUnifiedGraph(); err != nil {
		return nil, err
	}
	normalized, err := normalizeKeywordUpdates(updates)
	if err != nil {
		return nil, err
	}
	items := make([]any, 0, len(normalized))
	for _, update := range normalized {
		items = append(items, map[string]any{
			"Id": update.ID, "Keyword": update.Keyword,
		})
	}
	var response struct {
		UpdateResults []graphActionResult `json:"UpdateResults"`
	}
	err = c.call(ctx, "keywords", token, clientLogin, map[string]any{
		"method": "update",
		"params": map[string]any{"Keywords": items},
	}, &response)
	if err != nil {
		return nil, err
	}
	return graphMutationResults(
		"keyword_update", len(normalized), response.UpdateResults,
	)
}

// DeleteKeywords deletes exact keyword IDs for timeout reconciliation. It
// does not suspend, resume, or otherwise change campaign delivery state.
func (c *Client) DeleteKeywords(
	ctx context.Context, token, clientLogin string, keywordIDs []int64,
) ([]MutationResult, error) {
	if err := c.requireUnifiedGraph(); err != nil {
		return nil, err
	}
	ids, err := validatePositiveIDs(
		keywordIDs, maxModerationItems, "invalid_keyword_ids",
	)
	if err != nil {
		return nil, err
	}
	var response struct {
		DeleteResults []graphActionResult `json:"DeleteResults"`
	}
	err = c.call(ctx, "keywords", token, clientLogin, map[string]any{
		"method": "delete",
		"params": map[string]any{
			"SelectionCriteria": map[string]any{"Ids": ids},
		},
	}, &response)
	if err != nil {
		return nil, err
	}
	return graphMutationResultsForIDs(
		"keyword_delete", ids, response.DeleteResults, true,
	)
}

// ListKeywords returns all phrases and autotargetings in a campaign. The
// latter are represented by the provider keyword value ---autotargeting.
func (c *Client) ListKeywords(
	ctx context.Context, token, clientLogin string, campaignID int64,
) ([]Keyword, error) {
	if err := c.requireUnifiedGraph(); err != nil {
		return nil, err
	}
	if campaignID <= 0 {
		return nil, graphValidationError("invalid_campaign_id")
	}
	offset := int64(0)
	keywords := make([]Keyword, 0)
	for {
		var response struct {
			LimitedBy *int64 `json:"LimitedBy"`
			Keywords  []struct {
				ID               int64  `json:"Id"`
				CampaignID       int64  `json:"CampaignId"`
				AdGroupID        int64  `json:"AdGroupId"`
				Keyword          string `json:"Keyword"`
				UserParam1       string `json:"UserParam1"`
				UserParam2       string `json:"UserParam2"`
				StrategyPriority string `json:"StrategyPriority"`
				Status           string `json:"Status"`
				State            string `json:"State"`
				ServingStatus    string `json:"ServingStatus"`
			} `json:"Keywords"`
		}
		err := c.call(ctx, "keywords", token, clientLogin, map[string]any{
			"method": "get",
			"params": map[string]any{
				"SelectionCriteria": map[string]any{"CampaignIds": []int64{campaignID}},
				"FieldNames": []string{
					"Id", "CampaignId", "AdGroupId", "Keyword", "StrategyPriority",
					"UserParam1", "UserParam2", "Status", "State", "ServingStatus",
				},
				"Page": map[string]any{"Limit": maxGraphPageSize, "Offset": offset},
			},
		}, &response)
		if err != nil {
			return nil, err
		}
		for _, item := range response.Keywords {
			keyword := Keyword{
				ID: item.ID, CampaignID: item.CampaignID, AdGroupID: item.AdGroupID,
				Keyword:          graphText(item.Keyword),
				UserParam1:       graphText(item.UserParam1),
				UserParam2:       graphText(item.UserParam2),
				StrategyPriority: strings.ToUpper(strings.TrimSpace(item.StrategyPriority)),
				Status:           strings.ToUpper(strings.TrimSpace(item.Status)),
				State:            strings.ToUpper(strings.TrimSpace(item.State)),
				ServingStatus:    strings.ToUpper(strings.TrimSpace(item.ServingStatus)),
			}
			keyword, err = normalizeKeyword(keyword, campaignID)
			if err != nil {
				return nil, err
			}
			keywords = append(keywords, keyword)
		}
		next, more, err := nextGraphPage(offset, response.LimitedBy)
		if err != nil {
			return nil, err
		}
		if !more {
			break
		}
		offset = next
	}
	sort.Slice(keywords, func(i, j int) bool { return keywords[i].ID < keywords[j].ID })
	if err := validateUniqueKeywordIDs(keywords); err != nil {
		return nil, err
	}
	return keywords, nil
}

// ModerateAds sends draft ads to moderation. Direct requires every selected ad
// to belong to a group that already has at least one targeting condition.
func (c *Client) ModerateAds(
	ctx context.Context, token, clientLogin string, adIDs []int64,
) ([]MutationResult, error) {
	if err := c.requireUnifiedGraph(); err != nil {
		return nil, err
	}
	ids, err := validatePositiveIDs(adIDs, maxModerationItems, "invalid_ad_ids")
	if err != nil {
		return nil, err
	}
	var response struct {
		ModerateResults []graphActionResult `json:"ModerateResults"`
	}
	err = c.call(ctx, "ads", token, clientLogin, map[string]any{
		"method": "moderate",
		"params": map[string]any{
			"SelectionCriteria": map[string]any{"Ids": ids},
		},
	}, &response)
	if err != nil {
		return nil, err
	}
	results, resultErr := graphMutationResults(
		"ads_moderate", len(ids), response.ModerateResults,
	)
	if len(results) == len(ids) {
		for index, result := range results {
			if result.ID > 0 && result.ID != ids[index] {
				return results, &Error{Code: "invalid_ads_moderate_response"}
			}
		}
	}
	return results, resultErr
}

// FindUnifiedCampaignByOperationMarker reconciles an uncertain campaign add
// using the exact mp_op tracking parameter. Zero means not found; one exact
// match returns its ID; multiple matches fail as ambiguous. The scan is
// type-filtered, paginated, and capped at the provider's documented maximum.
func (c *Client) FindUnifiedCampaignByOperationMarker(
	ctx context.Context, token, clientLogin, marker string,
) (int64, error) {
	if err := c.requireUnifiedGraph(); err != nil {
		return 0, err
	}
	marker, err := normalizeGraphMarker(marker, "invalid_campaign_operation_marker")
	if err != nil {
		return 0, err
	}
	offset := int64(0)
	scanned := 0
	pages := 0
	seen := make(map[int64]struct{})
	var matchID int64
	for {
		pages++
		if pages > maxGraphReconcileObjects/1000+1 {
			return 0, &Error{Code: "campaign_reconcile_scan_limit"}
		}
		var response struct {
			LimitedBy *int64 `json:"LimitedBy"`
			Campaigns []struct {
				ID              int64  `json:"Id"`
				Type            string `json:"Type"`
				UnifiedCampaign *struct {
					TrackingParams string `json:"TrackingParams"`
				} `json:"UnifiedCampaign"`
			} `json:"Campaigns"`
		}
		err := c.call(ctx, "campaigns", token, clientLogin, map[string]any{
			"method": "get",
			"params": map[string]any{
				"SelectionCriteria": map[string]any{
					"Types": []string{"UNIFIED_CAMPAIGN"},
				},
				"FieldNames":                []string{"Id", "Type"},
				"UnifiedCampaignFieldNames": []string{"TrackingParams"},
				"Page":                      map[string]any{"Limit": 1000, "Offset": offset},
			},
		}, &response)
		if err != nil {
			return 0, err
		}
		scanned += len(response.Campaigns)
		if scanned > maxGraphReconcileObjects {
			return 0, &Error{Code: "campaign_reconcile_scan_limit"}
		}
		for _, item := range response.Campaigns {
			if item.ID <= 0 ||
				strings.ToUpper(strings.TrimSpace(item.Type)) != "UNIFIED_CAMPAIGN" ||
				item.UnifiedCampaign == nil {
				return 0, &Error{Code: "unsupported_campaign_in_reconcile"}
			}
			if _, exists := seen[item.ID]; exists {
				return 0, &Error{Code: "duplicate_campaign_reconcile_response"}
			}
			seen[item.ID] = struct{}{}
			candidate, present, valid := graphMarkerFromParams(
				item.UnifiedCampaign.TrackingParams, graphOperationMarkerParam,
			)
			if present && !valid {
				return 0, &Error{Code: "invalid_campaign_reconcile_tracking_params"}
			}
			if candidate == marker {
				if matchID != 0 {
					return 0, &Error{Code: "ambiguous_campaign_operation_marker"}
				}
				matchID = item.ID
			}
		}
		next, more, err := nextGraphPage(offset, response.LimitedBy)
		if err != nil {
			return 0, err
		}
		if !more {
			return matchID, nil
		}
		if scanned >= maxGraphReconcileObjects || next > 120_000 {
			return 0, &Error{Code: "campaign_reconcile_scan_limit"}
		}
		offset = next
	}
}

// FindUnifiedAdGroupByTrackingMarker reconciles an uncertain unified-group add
// using its exact deterministic operation name. The historical method name is
// retained for interface compatibility; no unsupported TrackingParams field is
// sent or relied upon.
func (c *Client) FindUnifiedAdGroupByTrackingMarker(
	ctx context.Context, token, clientLogin string, campaignID int64, marker string,
) (int64, error) {
	if err := c.requireUnifiedGraph(); err != nil {
		return 0, err
	}
	if campaignID <= 0 {
		return 0, graphValidationError("invalid_campaign_id")
	}
	marker, err := normalizeGraphMarker(marker, "invalid_adgroup_tracking_marker")
	if err != nil {
		return 0, err
	}
	expectedName := UnifiedAdGroupOperationName(marker)
	offset := int64(0)
	scanned := 0
	pages := 0
	seen := make(map[int64]struct{})
	var matchID int64
	for {
		pages++
		if pages > maxGraphReconcileObjects/1000+1 {
			return 0, &Error{Code: "adgroup_reconcile_scan_limit"}
		}
		var response struct {
			LimitedBy *int64 `json:"LimitedBy"`
			AdGroups  []struct {
				ID         int64  `json:"Id"`
				CampaignID int64  `json:"CampaignId"`
				Type       string `json:"Type"`
				Name       string `json:"Name"`
			} `json:"AdGroups"`
		}
		err := c.call(ctx, "adgroups", token, clientLogin, map[string]any{
			"method": "get",
			"params": map[string]any{
				"SelectionCriteria": map[string]any{
					"CampaignIds": []int64{campaignID},
				},
				"FieldNames": []string{"Id", "CampaignId", "Type", "Name"},
				"Page":       map[string]any{"Limit": 1000, "Offset": offset},
			},
		}, &response)
		if err != nil {
			return 0, err
		}
		scanned += len(response.AdGroups)
		if scanned > maxGraphReconcileObjects {
			return 0, &Error{Code: "adgroup_reconcile_scan_limit"}
		}
		for _, item := range response.AdGroups {
			if item.ID <= 0 || item.CampaignID != campaignID ||
				strings.ToUpper(strings.TrimSpace(item.Type)) != "UNIFIED_AD_GROUP" {
				return 0, &Error{Code: "unsupported_adgroup_in_reconcile"}
			}
			if _, exists := seen[item.ID]; exists {
				return 0, &Error{Code: "duplicate_adgroup_reconcile_response"}
			}
			seen[item.ID] = struct{}{}
			if strings.TrimSpace(item.Name) == expectedName {
				if matchID != 0 {
					return 0, &Error{Code: "ambiguous_adgroup_operation_name"}
				}
				matchID = item.ID
			}
		}
		next, more, err := nextGraphPage(offset, response.LimitedBy)
		if err != nil {
			return 0, err
		}
		if !more {
			return matchID, nil
		}
		if scanned >= maxGraphReconcileObjects || next > 120_000 {
			return 0, &Error{Code: "adgroup_reconcile_scan_limit"}
		}
		offset = next
	}
}

// EnsureNoBidModifiers checks campaign- and child-group-level modifiers using
// the documented v501 BidModifiers service. Any returned object, including a
// future unknown type, changes delivery and therefore fails closed.
func (c *Client) EnsureNoBidModifiers(
	ctx context.Context, token, clientLogin string, campaignID int64,
) error {
	if err := c.requireUnifiedGraph(); err != nil {
		return err
	}
	if campaignID <= 0 {
		return graphValidationError("invalid_campaign_id")
	}
	offset := int64(0)
	pages := 0
	for {
		pages++
		if pages > maxGraphReconcileObjects/1000+1 {
			return &Error{Code: "bid_modifiers_scan_limit"}
		}
		var response struct {
			LimitedBy    *int64             `json:"LimitedBy"`
			BidModifiers *[]json.RawMessage `json:"BidModifiers"`
		}
		err := c.call(ctx, "bidmodifiers", token, clientLogin, map[string]any{
			"method": "get",
			"params": map[string]any{
				"SelectionCriteria": map[string]any{
					"CampaignIds": []int64{campaignID},
					"Levels":      []string{"CAMPAIGN", "AD_GROUP"},
				},
				"FieldNames": []string{"Id", "CampaignId", "AdGroupId", "Level", "Type"},
				"Page":       map[string]any{"Limit": 1000, "Offset": offset},
			},
		}, &response)
		if err != nil {
			return err
		}
		if response.BidModifiers == nil {
			return &Error{Code: "invalid_bid_modifiers_response"}
		}
		if len(*response.BidModifiers) != 0 {
			return &Error{Code: "unsupported_bid_modifiers"}
		}
		next, more, err := nextGraphPage(offset, response.LimitedBy)
		if err != nil {
			return err
		}
		if !more {
			return nil
		}
		if next > 120_000 {
			return &Error{Code: "bid_modifiers_scan_limit"}
		}
		offset = next
	}
}

// EnsureNoAudienceTargets checks campaign- and child-group-level audience
// targeting. Audience targets change delivery but are outside the deliberately
// narrow MaxPosty graph, so any returned object, including an unknown future
// shape, fails closed.
func (c *Client) EnsureNoAudienceTargets(
	ctx context.Context, token, clientLogin string, campaignID int64,
) error {
	if err := c.requireUnifiedGraph(); err != nil {
		return err
	}
	if campaignID <= 0 {
		return graphValidationError("invalid_campaign_id")
	}
	offset := int64(0)
	pages := 0
	for {
		pages++
		if pages > maxGraphReconcileObjects/1000+1 {
			return &Error{Code: "audience_targets_scan_limit"}
		}
		var response struct {
			LimitedBy       *int64             `json:"LimitedBy"`
			AudienceTargets *[]json.RawMessage `json:"AudienceTargets"`
		}
		err := c.call(ctx, "audiencetargets", token, clientLogin, map[string]any{
			"method": "get",
			"params": map[string]any{
				"SelectionCriteria": map[string]any{
					"CampaignIds": []int64{campaignID},
				},
				"FieldNames": []string{
					"Id", "AdGroupId", "CampaignId", "RetargetingListId",
					"InterestId", "ContextBid", "StrategyPriority", "State",
				},
				"Page": map[string]any{"Limit": 1000, "Offset": offset},
			},
		}, &response)
		if err != nil {
			return err
		}
		if response.AudienceTargets == nil {
			return &Error{Code: "invalid_audience_targets_response"}
		}
		if len(*response.AudienceTargets) != 0 {
			return &Error{Code: "unsupported_audience_targets"}
		}
		next, more, err := nextGraphPage(offset, response.LimitedBy)
		if err != nil {
			return err
		}
		if !more {
			return nil
		}
		if next > 120_000 {
			return &Error{Code: "audience_targets_scan_limit"}
		}
		offset = next
	}
}

// UpdateUnifiedCampaigns edits only the supported campaign snapshot. It never
// resumes, moderates, launches, or changes package-strategy membership.
func (c *Client) UpdateUnifiedCampaigns(
	ctx context.Context, token, clientLogin string, updates []UnifiedCampaignUpdate,
) ([]MutationResult, error) {
	if err := c.requireUnifiedGraph(); err != nil {
		return nil, err
	}
	normalized, err := normalizeUnifiedCampaignUpdates(updates)
	if err != nil {
		return nil, err
	}
	items := make([]any, 0, len(normalized))
	ids := make([]int64, 0, len(normalized))
	for _, update := range normalized {
		settings := make([]any, 0, len(update.Settings))
		for _, setting := range update.Settings {
			settings = append(settings, map[string]any{
				"Option": setting.Option, "Value": setting.Value,
			})
		}
		items = append(items, map[string]any{
			"Id":        update.ID,
			"Name":      update.Name,
			"StartDate": update.StartsAt.Format(time.DateOnly),
			"EndDate":   update.EndsAt.Format(time.DateOnly),
			"UnifiedCampaign": map[string]any{
				"BiddingStrategy": update.BiddingStrategy,
				"Settings":        settings,
				"TrackingParams":  update.TrackingParams,
			},
		})
		ids = append(ids, update.ID)
	}
	var response struct {
		UpdateResults []graphActionResult `json:"UpdateResults"`
	}
	err = c.call(ctx, "campaigns", token, clientLogin, map[string]any{
		"method": "update",
		"params": map[string]any{"Campaigns": items},
	}, &response)
	if err != nil {
		return nil, err
	}
	return graphMutationResultsForIDs(
		"campaign_update", ids, response.UpdateResults, true,
	)
}

// GetGraphCampaign reads the complete campaign-level delivery configuration
// used by the graph fingerprint. Unsupported alternate budgets and package
// strategies fail closed instead of being silently omitted.
func (c *Client) GetGraphCampaign(
	ctx context.Context, token, clientLogin string, campaignID int64,
) (GraphCampaign, error) {
	if err := c.requireUnifiedGraph(); err != nil {
		return GraphCampaign{}, err
	}
	if campaignID <= 0 {
		return GraphCampaign{}, graphValidationError("invalid_campaign_id")
	}
	var response struct {
		Campaigns []json.RawMessage `json:"Campaigns"`
	}
	err := c.call(ctx, "campaigns", token, clientLogin, map[string]any{
		"method": "get",
		"params": map[string]any{
			"SelectionCriteria": map[string]any{"Ids": []int64{campaignID}},
			"FieldNames": []string{
				"Id", "Name", "Status", "State", "Type", "StartDate", "EndDate",
				"TimeZone", "NegativeKeywords", "BlockedIps", "ExcludedSites",
				"TimeTargeting", "DailyBudget",
			},
			"UnifiedCampaignFieldNames": []string{
				"BiddingStrategy", "Settings", "CounterIds", "PriorityGoals",
				"NegativeKeywordSharedSetIds", "TrackingParams",
				"AttributionModel", "PackageBiddingStrategy",
			},
			"UnifiedCampaignSearchStrategyPlacementTypesFieldNames": []string{
				"SearchResults", "ProductGallery", "DynamicPlaces", "Maps",
				"SearchOrganizationList",
			},
			"UnifiedCampaignPackageBiddingStrategyPlatformsFieldNames": []string{
				"SearchResult", "ProductGallery", "Maps", "SearchOrganizationList",
				"Network", "DynamicPlaces",
			},
		},
	}, &response)
	if err != nil {
		return GraphCampaign{}, err
	}
	if len(response.Campaigns) != 1 {
		return GraphCampaign{}, &Error{Code: "campaign_not_found"}
	}
	var envelope struct {
		ID               int64             `json:"Id"`
		Name             string            `json:"Name"`
		Status           string            `json:"Status"`
		State            string            `json:"State"`
		Type             string            `json:"Type"`
		StartDate        string            `json:"StartDate"`
		EndDate          string            `json:"EndDate"`
		TimeZone         string            `json:"TimeZone"`
		NegativeKeywords *graphStringItems `json:"NegativeKeywords"`
		BlockedIPs       *graphStringItems `json:"BlockedIps"`
		ExcludedSites    *graphStringItems `json:"ExcludedSites"`
		TimeTargeting    json.RawMessage   `json:"TimeTargeting"`
		DailyBudget      json.RawMessage   `json:"DailyBudget"`
		UnifiedCampaign  json.RawMessage   `json:"UnifiedCampaign"`
	}
	if err := json.Unmarshal(response.Campaigns[0], &envelope); err != nil {
		return GraphCampaign{}, &Error{Code: "invalid_campaign_response"}
	}
	var campaignFields map[string]json.RawMessage
	if err := json.Unmarshal(response.Campaigns[0], &campaignFields); err != nil {
		return GraphCampaign{}, &Error{Code: "invalid_campaign_response"}
	}
	if rawJSONPresent(envelope.DailyBudget) {
		return GraphCampaign{}, &Error{Code: "unsupported_campaign_daily_budget"}
	}
	var unified struct {
		BiddingStrategy json.RawMessage `json:"BiddingStrategy"`
		Settings        []struct {
			Option string `json:"Option"`
			Value  string `json:"Value"`
		} `json:"Settings"`
		CounterIDs                  *graphInt64Items `json:"CounterIds"`
		NegativeKeywordSharedSetIDs *graphInt64Items `json:"NegativeKeywordSharedSetIds"`
		PriorityGoals               *struct {
			Items []struct {
				GoalID                 int64  `json:"GoalId"`
				Value                  int64  `json:"Value"`
				IsMetrikaSourceOfValue string `json:"IsMetrikaSourceOfValue"`
			} `json:"Items"`
		} `json:"PriorityGoals"`
		TrackingParams         string          `json:"TrackingParams"`
		AttributionModel       string          `json:"AttributionModel"`
		PackageBiddingStrategy json.RawMessage `json:"PackageBiddingStrategy"`
	}
	if !rawJSONPresent(envelope.UnifiedCampaign) ||
		json.Unmarshal(envelope.UnifiedCampaign, &unified) != nil {
		return GraphCampaign{}, &Error{Code: "invalid_campaign_response"}
	}
	var unifiedFields map[string]json.RawMessage
	if err := json.Unmarshal(envelope.UnifiedCampaign, &unifiedFields); err != nil {
		return GraphCampaign{}, &Error{Code: "invalid_campaign_response"}
	}
	if rawJSONPresent(unified.PackageBiddingStrategy) {
		return GraphCampaign{}, &Error{Code: "unsupported_package_bidding_strategy"}
	}
	if !rawJSONHasKeys(campaignFields, []string{
		"Id", "Name", "Status", "State", "Type", "StartDate", "EndDate",
		"TimeZone", "NegativeKeywords", "BlockedIps", "ExcludedSites",
		"TimeTargeting", "DailyBudget", "UnifiedCampaign",
	}) || !rawJSONHasKeys(unifiedFields, []string{
		"BiddingStrategy", "Settings", "CounterIds", "PriorityGoals",
		"NegativeKeywordSharedSetIds", "TrackingParams", "AttributionModel",
		"PackageBiddingStrategy",
	}) {
		return GraphCampaign{}, &Error{Code: "incomplete_campaign_response"}
	}
	startsAt, err := time.Parse(time.DateOnly, envelope.StartDate)
	if err != nil {
		return GraphCampaign{}, &Error{Code: "invalid_campaign_response"}
	}
	endsAt, err := time.Parse(time.DateOnly, envelope.EndDate)
	if err != nil {
		return GraphCampaign{}, &Error{Code: "invalid_campaign_response"}
	}
	budgetMicros, ok := findWeeklySpendLimit(envelope.UnifiedCampaign)
	if !ok {
		return GraphCampaign{}, &Error{Code: "campaign_budget_unavailable"}
	}
	budgetMinor, err := MicrosToMinor(budgetMicros)
	if err != nil {
		return GraphCampaign{}, &Error{Code: "campaign_budget_invalid"}
	}
	timeTargeting, err := decodeGraphTimeTargeting(envelope.TimeTargeting)
	if err != nil {
		return GraphCampaign{}, err
	}
	campaign := GraphCampaign{
		ID: envelope.ID, Name: envelope.Name, Status: envelope.Status,
		State: envelope.State, Type: envelope.Type, WeeklyBudgetMinor: budgetMinor,
		StartsAt: startsAt, EndsAt: endsAt, TimeZone: envelope.TimeZone,
		TimeTargeting: timeTargeting, BiddingStrategy: unified.BiddingStrategy,
		TrackingParams: unified.TrackingParams, AttributionModel: unified.AttributionModel,
	}
	if envelope.NegativeKeywords != nil {
		campaign.NegativeKeywords = append(campaign.NegativeKeywords, envelope.NegativeKeywords.Items...)
	}
	if envelope.BlockedIPs != nil {
		campaign.BlockedIPs = append(campaign.BlockedIPs, envelope.BlockedIPs.Items...)
	}
	if envelope.ExcludedSites != nil {
		campaign.ExcludedSites = append(campaign.ExcludedSites, envelope.ExcludedSites.Items...)
	}
	for _, setting := range unified.Settings {
		campaign.Settings = append(campaign.Settings, GraphCampaignSetting{
			Option: setting.Option, Value: setting.Value,
		})
	}
	if unified.CounterIDs != nil {
		campaign.CounterIDs = append(campaign.CounterIDs, unified.CounterIDs.Items...)
	}
	if unified.NegativeKeywordSharedSetIDs != nil {
		campaign.NegativeKeywordSharedSetIDs = append(
			campaign.NegativeKeywordSharedSetIDs,
			unified.NegativeKeywordSharedSetIDs.Items...,
		)
	}
	if len(campaign.NegativeKeywordSharedSetIDs) != 0 {
		return GraphCampaign{}, &Error{Code: "unsupported_campaign_negative_shared_sets"}
	}
	if unified.PriorityGoals != nil {
		for _, goal := range unified.PriorityGoals.Items {
			campaign.PriorityGoals = append(campaign.PriorityGoals, GraphPriorityGoal{
				GoalID: goal.GoalID, Value: goal.Value,
				IsMetrikaSourceOfValue: goal.IsMetrikaSourceOfValue,
			})
		}
	}
	campaign, err = normalizeGraphCampaign(campaign)
	if err != nil {
		return GraphCampaign{}, err
	}
	if campaign.ID != campaignID {
		return GraphCampaign{}, &Error{Code: "invalid_campaign_response"}
	}
	return campaign, nil
}

// GetCampaignGraph reads and validates the complete provider graph supported
// by this client. Unsupported group/ad types and broken parent relationships
// fail closed instead of disappearing from the fingerprint.
func (c *Client) GetCampaignGraph(
	ctx context.Context, token, clientLogin string, campaignID int64,
) (CampaignGraph, error) {
	if err := c.requireUnifiedGraph(); err != nil {
		return CampaignGraph{}, err
	}
	if campaignID <= 0 {
		return CampaignGraph{}, graphValidationError("invalid_campaign_id")
	}
	campaign, err := c.GetGraphCampaign(ctx, token, clientLogin, campaignID)
	if err != nil {
		return CampaignGraph{}, err
	}
	if err := c.EnsureNoBidModifiers(ctx, token, clientLogin, campaignID); err != nil {
		return CampaignGraph{}, err
	}
	if err := c.EnsureNoAudienceTargets(ctx, token, clientLogin, campaignID); err != nil {
		return CampaignGraph{}, err
	}
	groups, err := c.ListUnifiedAdGroups(ctx, token, clientLogin, campaignID)
	if err != nil {
		return CampaignGraph{}, err
	}
	ads, err := c.ListResponsiveAds(ctx, token, clientLogin, campaignID)
	if err != nil {
		return CampaignGraph{}, err
	}
	keywords, err := c.ListKeywords(ctx, token, clientLogin, campaignID)
	if err != nil {
		return CampaignGraph{}, err
	}
	groupIDs := make(map[int64]struct{}, len(groups))
	for _, group := range groups {
		groupIDs[group.ID] = struct{}{}
	}
	for _, ad := range ads {
		if _, ok := groupIDs[ad.AdGroupID]; !ok {
			return CampaignGraph{}, &Error{Code: "orphan_responsive_ad"}
		}
	}
	for _, keyword := range keywords {
		if _, ok := groupIDs[keyword.AdGroupID]; !ok {
			return CampaignGraph{}, &Error{Code: "orphan_keyword"}
		}
	}
	graph := CampaignGraph{
		Campaign: campaign, AdGroups: groups, Ads: ads, Keywords: keywords,
	}
	if _, err := CampaignGraphFingerprint(graph); err != nil {
		return CampaignGraph{}, err
	}
	return graph, nil
}

// Fingerprint returns the deterministic content fingerprint for the graph.
func (g CampaignGraph) Fingerprint() (string, error) {
	return CampaignGraphFingerprint(g)
}

// CampaignGraphFingerprint returns a lowercase SHA-256 hex digest of a
// versioned canonical JSON representation. Moderation/status fields are
// intentionally excluded because readiness changes must not change consented
// content; callers validate readiness separately immediately before launch.
func CampaignGraphFingerprint(graph CampaignGraph) (string, error) {
	canonical, err := canonicalizeCampaignGraph(graph)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func (c *Client) requireUnifiedGraph() error {
	if c == nil || !c.unified {
		return &Error{Code: "unified_graph_requires_v501"}
	}
	return nil
}

func graphMutationResults(
	operation string, expected int, items []graphActionResult,
) ([]MutationResult, error) {
	results := make([]MutationResult, 0, len(items))
	for _, item := range items {
		results = append(results, MutationResult{
			ID: item.ID, Warnings: exportProviderIssues(item.Warnings),
			Errors: exportProviderIssues(item.Errors),
		})
	}
	if len(items) != expected {
		return results, &Error{Code: "invalid_" + operation + "_response"}
	}
	failed := false
	for _, result := range results {
		if result.ID <= 0 || len(result.Errors) != 0 {
			failed = true
		}
	}
	if failed {
		return results, &PartialMutationError{
			Operation: operation, Results: append([]MutationResult(nil), results...),
		}
	}
	return results, nil
}

func graphMutationResultsForIDs(
	operation string, expectedIDs []int64, items []graphActionResult, requireSameID bool,
) ([]MutationResult, error) {
	results, resultErr := graphMutationResults(operation, len(expectedIDs), items)
	if len(results) != len(expectedIDs) {
		return results, resultErr
	}
	if requireSameID {
		for index, result := range results {
			if result.ID > 0 && result.ID != expectedIDs[index] {
				return results, &Error{Code: "invalid_" + operation + "_response"}
			}
		}
	}
	return results, resultErr
}

func exportProviderIssues(items []apiIssue) []ProviderIssue {
	if len(items) == 0 {
		return nil
	}
	result := make([]ProviderIssue, 0, len(items))
	for _, item := range items {
		result = append(result, ProviderIssue{
			Code: item.Code, Message: graphText(item.Message), Details: graphText(item.Details),
		})
	}
	return result
}

func validateRegionNames(names []string) ([]string, error) {
	if len(names) == 0 || len(names) > maxGraphRegions {
		return nil, graphValidationError("invalid_region_names")
	}
	result := make([]string, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		name = graphText(name)
		if name == "" || utf8.RuneCountInString(name) > 255 {
			return nil, graphValidationError("invalid_region_name")
		}
		key := graphFold(name)
		if _, ok := seen[key]; ok {
			return nil, graphValidationError("duplicate_region_name")
		}
		seen[key] = struct{}{}
		result = append(result, name)
	}
	return result, nil
}

func rawJSONPresent(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) != 0 && !bytes.Equal(trimmed, []byte("null"))
}

func rawJSONHasKeys(values map[string]json.RawMessage, keys []string) bool {
	for _, key := range keys {
		if _, ok := values[key]; !ok {
			return false
		}
	}
	return true
}

func graphOptionalFeaturePresent(raw json.RawMessage) bool {
	if !rawJSONPresent(raw) {
		return false
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return true
	}
	var present func(any) bool
	present = func(current any) bool {
		switch typed := current.(type) {
		case nil:
			return false
		case string:
			return strings.TrimSpace(typed) != ""
		case bool:
			return typed
		case json.Number:
			return typed.String() != "" && typed.String() != "0" && typed.String() != "0.0"
		case []any:
			for _, child := range typed {
				if present(child) {
					return true
				}
			}
			return false
		case map[string]any:
			for _, child := range typed {
				if present(child) {
					return true
				}
			}
			return false
		default:
			return true
		}
	}
	return present(value)
}

func canonicalizeRawJSON(raw json.RawMessage) (json.RawMessage, error) {
	if !rawJSONPresent(raw) {
		return nil, graphValidationError("invalid_campaign_bidding_strategy")
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, graphValidationError("invalid_campaign_bidding_strategy")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, graphValidationError("invalid_campaign_bidding_strategy")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, graphValidationError("invalid_campaign_bidding_strategy")
	}
	return encoded, nil
}

func decodeGraphTimeTargeting(raw json.RawMessage) (GraphTimeTargeting, error) {
	if !rawJSONPresent(raw) {
		return GraphTimeTargeting{Schedule: []string{}}, nil
	}
	var value struct {
		Schedule                *graphStringItems `json:"Schedule"`
		ConsiderWorkingWeekends string            `json:"ConsiderWorkingWeekends"`
		HolidaysSchedule        *struct {
			SuspendOnHolidays string `json:"SuspendOnHolidays"`
			BidPercent        int64  `json:"BidPercent"`
			StartHour         int64  `json:"StartHour"`
			EndHour           int64  `json:"EndHour"`
		} `json:"HolidaysSchedule"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return GraphTimeTargeting{}, &Error{Code: "invalid_campaign_response"}
	}
	result := GraphTimeTargeting{
		Present: true, Schedule: []string{},
		ConsiderWorkingWeekends: value.ConsiderWorkingWeekends,
	}
	if value.Schedule != nil {
		result.Schedule = append(result.Schedule, value.Schedule.Items...)
	}
	if value.HolidaysSchedule != nil {
		result.HolidaysSchedule = &GraphHolidaysSchedule{
			SuspendOnHolidays: value.HolidaysSchedule.SuspendOnHolidays,
			BidPercent:        value.HolidaysSchedule.BidPercent,
			StartHour:         value.HolidaysSchedule.StartHour,
			EndHour:           value.HolidaysSchedule.EndHour,
		}
	}
	return result, nil
}

func normalizeGraphCampaign(campaign GraphCampaign) (GraphCampaign, error) {
	campaign.Name = graphText(campaign.Name)
	campaign.Status = strings.ToUpper(strings.TrimSpace(campaign.Status))
	campaign.State = strings.ToUpper(strings.TrimSpace(campaign.State))
	campaign.Type = strings.ToUpper(strings.TrimSpace(campaign.Type))
	campaign.TimeZone = graphText(campaign.TimeZone)
	campaign.TrackingParams = graphText(campaign.TrackingParams)
	campaign.AttributionModel = strings.ToUpper(strings.TrimSpace(campaign.AttributionModel))
	if campaign.ID <= 0 || campaign.Name == "" ||
		utf8.RuneCountInString(campaign.Name) > 255 ||
		campaign.Type != "UNIFIED_CAMPAIGN" ||
		campaign.WeeklyBudgetMinor <= 0 ||
		campaign.StartsAt.IsZero() || campaign.EndsAt.IsZero() ||
		campaign.EndsAt.Before(campaign.StartsAt) ||
		campaign.TimeZone == "" || utf8.RuneCountInString(campaign.TimeZone) > 128 ||
		utf8.RuneCountInString(campaign.TrackingParams) > maxTrackingParamsRunes ||
		campaign.AttributionModel == "" {
		return GraphCampaign{}, graphValidationError("invalid_campaign_graph")
	}
	campaign.StartsAt = dateOnly(campaign.StartsAt)
	campaign.EndsAt = dateOnly(campaign.EndsAt)

	var err error
	campaign.NegativeKeywords, err = normalizeNegativeKeywordsWithLimit(
		campaign.NegativeKeywords, maxCampaignNegativeRunes,
	)
	if err != nil {
		return GraphCampaign{}, graphValidationError("invalid_campaign_graph")
	}
	campaign.BlockedIPs, err = normalizeBlockedIPs(campaign.BlockedIPs)
	if err != nil {
		return GraphCampaign{}, err
	}
	campaign.ExcludedSites, err = normalizeExcludedSites(campaign.ExcludedSites)
	if err != nil {
		return GraphCampaign{}, err
	}
	campaign.TimeTargeting, err = normalizeGraphTimeTargeting(campaign.TimeTargeting)
	if err != nil {
		return GraphCampaign{}, err
	}
	campaign.BiddingStrategy, err = canonicalizeRawJSON(campaign.BiddingStrategy)
	if err != nil {
		return GraphCampaign{}, err
	}
	strategyEnvelope, err := json.Marshal(map[string]json.RawMessage{
		"BiddingStrategy": campaign.BiddingStrategy,
	})
	if err != nil {
		return GraphCampaign{}, err
	}
	budgetMicros, ok := findWeeklySpendLimit(strategyEnvelope)
	if !ok {
		return GraphCampaign{}, &Error{Code: "unsupported_campaign_bidding_strategy"}
	}
	budgetMinor, err := MicrosToMinor(budgetMicros)
	if err != nil || budgetMinor != campaign.WeeklyBudgetMinor {
		return GraphCampaign{}, &Error{Code: "campaign_budget_mismatch"}
	}
	campaign.Settings, err = normalizeGraphCampaignSettings(campaign.Settings)
	if err != nil {
		return GraphCampaign{}, err
	}
	campaign.CounterIDs, err = normalizeOptionalPositiveIDs(
		campaign.CounterIDs, maxCampaignCounters, "invalid_campaign_counters",
	)
	if err != nil {
		return GraphCampaign{}, err
	}
	campaign.PriorityGoals, err = normalizeGraphPriorityGoals(campaign.PriorityGoals)
	if err != nil {
		return GraphCampaign{}, err
	}
	return campaign, nil
}

func normalizeBlockedIPs(values []string) ([]string, error) {
	if len(values) > maxCampaignBlockedIPs {
		return nil, graphValidationError("invalid_campaign_blocked_ips")
	}
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, raw := range values {
		ip := net.ParseIP(graphText(raw))
		if ip == nil {
			return nil, graphValidationError("invalid_campaign_blocked_ips")
		}
		value := ip.String()
		if _, ok := seen[value]; ok {
			return nil, graphValidationError("invalid_campaign_blocked_ips")
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result, nil
}

func normalizeExcludedSites(values []string) ([]string, error) {
	if len(values) > maxCampaignExcludedSites {
		return nil, graphValidationError("invalid_campaign_excluded_sites")
	}
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, raw := range values {
		value := strings.ToLower(graphText(raw))
		if value == "" || utf8.RuneCountInString(value) > maxExcludedSiteRunes {
			return nil, graphValidationError("invalid_campaign_excluded_sites")
		}
		if _, ok := seen[value]; ok {
			return nil, graphValidationError("invalid_campaign_excluded_sites")
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result, nil
}

func normalizeGraphTimeTargeting(value GraphTimeTargeting) (GraphTimeTargeting, error) {
	if !value.Present {
		if len(value.Schedule) != 0 || value.ConsiderWorkingWeekends != "" ||
			value.HolidaysSchedule != nil {
			return GraphTimeTargeting{}, graphValidationError("invalid_campaign_time_targeting")
		}
		value.Schedule = []string{}
		return value, nil
	}
	if len(value.Schedule) > maxTimeTargetingItems {
		return GraphTimeTargeting{}, graphValidationError("invalid_campaign_time_targeting")
	}
	schedule := make([]string, 0, len(value.Schedule))
	seen := make(map[string]struct{}, len(value.Schedule))
	for _, raw := range value.Schedule {
		item := graphText(raw)
		if item == "" || utf8.RuneCountInString(item) > 128 {
			return GraphTimeTargeting{}, graphValidationError("invalid_campaign_time_targeting")
		}
		if _, ok := seen[item]; ok {
			return GraphTimeTargeting{}, graphValidationError("invalid_campaign_time_targeting")
		}
		seen[item] = struct{}{}
		schedule = append(schedule, item)
	}
	sort.Strings(schedule)
	value.Schedule = schedule
	value.ConsiderWorkingWeekends = strings.ToUpper(
		strings.TrimSpace(value.ConsiderWorkingWeekends),
	)
	if value.ConsiderWorkingWeekends != "" &&
		value.ConsiderWorkingWeekends != "YES" &&
		value.ConsiderWorkingWeekends != "NO" {
		return GraphTimeTargeting{}, graphValidationError("invalid_campaign_time_targeting")
	}
	if value.HolidaysSchedule != nil {
		holiday := *value.HolidaysSchedule
		holiday.SuspendOnHolidays = strings.ToUpper(strings.TrimSpace(holiday.SuspendOnHolidays))
		if holiday.SuspendOnHolidays != "" &&
			holiday.SuspendOnHolidays != "YES" &&
			holiday.SuspendOnHolidays != "NO" ||
			holiday.BidPercent < 0 || holiday.BidPercent > 200 ||
			holiday.StartHour < 0 || holiday.StartHour > 24 ||
			holiday.EndHour < 0 || holiday.EndHour > 24 {
			return GraphTimeTargeting{}, graphValidationError("invalid_campaign_time_targeting")
		}
		value.HolidaysSchedule = &holiday
	}
	return value, nil
}

func normalizeGraphCampaignSettings(
	values []GraphCampaignSetting,
) ([]GraphCampaignSetting, error) {
	result := make([]GraphCampaignSetting, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value.Option = strings.ToUpper(strings.TrimSpace(value.Option))
		value.Value = strings.ToUpper(strings.TrimSpace(value.Value))
		if value.Option == "" || value.Value == "" ||
			utf8.RuneCountInString(value.Option) > 128 ||
			utf8.RuneCountInString(value.Value) > 128 {
			return nil, graphValidationError("invalid_campaign_settings")
		}
		if _, ok := seen[value.Option]; ok {
			return nil, graphValidationError("invalid_campaign_settings")
		}
		seen[value.Option] = struct{}{}
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Option < result[j].Option
	})
	return result, nil
}

func normalizeOptionalPositiveIDs(
	values []int64, maxItems int, code string,
) ([]int64, error) {
	if len(values) > maxItems {
		return nil, graphValidationError(code)
	}
	result := append([]int64(nil), values...)
	seen := make(map[int64]struct{}, len(values))
	for _, value := range result {
		if value <= 0 {
			return nil, graphValidationError(code)
		}
		if _, ok := seen[value]; ok {
			return nil, graphValidationError(code)
		}
		seen[value] = struct{}{}
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result, nil
}

func normalizeGraphPriorityGoals(values []GraphPriorityGoal) ([]GraphPriorityGoal, error) {
	if len(values) > 30 {
		return nil, graphValidationError("invalid_campaign_priority_goals")
	}
	result := append([]GraphPriorityGoal(nil), values...)
	seen := make(map[int64]struct{}, len(result))
	for index := range result {
		item := &result[index]
		item.IsMetrikaSourceOfValue = strings.ToUpper(
			strings.TrimSpace(item.IsMetrikaSourceOfValue),
		)
		if item.GoalID <= 0 || item.Value < 0 ||
			item.IsMetrikaSourceOfValue != "" &&
				item.IsMetrikaSourceOfValue != "YES" &&
				item.IsMetrikaSourceOfValue != "NO" {
			return nil, graphValidationError("invalid_campaign_priority_goals")
		}
		if _, ok := seen[item.GoalID]; ok {
			return nil, graphValidationError("invalid_campaign_priority_goals")
		}
		seen[item.GoalID] = struct{}{}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].GoalID < result[j].GoalID })
	return result, nil
}

func normalizeUnifiedCampaignUpdates(
	updates []UnifiedCampaignUpdate,
) ([]UnifiedCampaignUpdate, error) {
	if len(updates) == 0 || len(updates) > maxGraphCampaignUpdates {
		return nil, graphValidationError("invalid_campaign_updates")
	}
	result := make([]UnifiedCampaignUpdate, 0, len(updates))
	seen := make(map[int64]struct{}, len(updates))
	for _, update := range updates {
		if update.ID <= 0 {
			return nil, graphValidationError("invalid_campaign_update")
		}
		if _, exists := seen[update.ID]; exists {
			return nil, graphValidationError("duplicate_campaign_update")
		}
		seen[update.ID] = struct{}{}
		update.Name = graphText(update.Name)
		update.TrackingParams = graphText(update.TrackingParams)
		if update.Name == "" || utf8.RuneCountInString(update.Name) > 255 ||
			update.WeeklyBudgetMinor <= 0 ||
			update.StartsAt.IsZero() || update.EndsAt.IsZero() ||
			update.EndsAt.Before(update.StartsAt) ||
			utf8.RuneCountInString(update.TrackingParams) > maxTrackingParamsRunes {
			return nil, graphValidationError("invalid_campaign_update")
		}
		update.StartsAt = dateOnly(update.StartsAt)
		update.EndsAt = dateOnly(update.EndsAt)
		var err error
		update.BiddingStrategy, err = canonicalizeRawJSON(update.BiddingStrategy)
		if err != nil {
			return nil, err
		}
		strategyEnvelope, err := json.Marshal(map[string]json.RawMessage{
			"BiddingStrategy": update.BiddingStrategy,
		})
		if err != nil {
			return nil, err
		}
		budgetMicros, ok := findWeeklySpendLimit(strategyEnvelope)
		if !ok {
			return nil, &Error{Code: "unsupported_campaign_bidding_strategy"}
		}
		budgetMinor, err := MicrosToMinor(budgetMicros)
		if err != nil || budgetMinor != update.WeeklyBudgetMinor {
			return nil, &Error{Code: "campaign_budget_mismatch"}
		}
		update.Settings, err = normalizeGraphCampaignSettings(update.Settings)
		if err != nil {
			return nil, err
		}
		result = append(result, update)
	}
	return result, nil
}

func normalizeUnifiedAdGroupUpdates(
	updates []UnifiedAdGroupUpdate,
) ([]UnifiedAdGroupUpdate, error) {
	if len(updates) == 0 || len(updates) > maxGraphMutationItems {
		return nil, graphValidationError("invalid_adgroup_updates")
	}
	result := make([]UnifiedAdGroupUpdate, 0, len(updates))
	seen := make(map[int64]struct{}, len(updates))
	for _, update := range updates {
		if update.ID <= 0 {
			return nil, graphValidationError("invalid_adgroup_update")
		}
		if _, exists := seen[update.ID]; exists {
			return nil, graphValidationError("duplicate_adgroup_update")
		}
		seen[update.ID] = struct{}{}
		update.Name = graphText(update.Name)
		if update.Name == "" || utf8.RuneCountInString(update.Name) > maxAdGroupNameRunes {
			return nil, graphValidationError("invalid_adgroup_update")
		}
		var err error
		update.RegionIDs, err = validateRegionIDs(update.RegionIDs)
		if err != nil {
			return nil, err
		}
		update.NegativeKeywords, err = normalizeNegativeKeywords(update.NegativeKeywords)
		if err != nil {
			return nil, err
		}
		update.TrackingMarker, err = normalizeGraphMarker(
			update.TrackingMarker, "invalid_tracking_marker",
		)
		if err != nil {
			return nil, err
		}
		update.Name = UnifiedAdGroupOperationName(update.TrackingMarker)
		result = append(result, update)
	}
	return result, nil
}

func normalizeResponsiveAdUpdates(
	updates []ResponsiveAdUpdate,
) ([]ResponsiveAdUpdate, error) {
	if len(updates) == 0 || len(updates) > maxGraphMutationItems {
		return nil, graphValidationError("invalid_responsive_ad_updates")
	}
	result := make([]ResponsiveAdUpdate, 0, len(updates))
	seen := make(map[int64]struct{}, len(updates))
	for _, update := range updates {
		if update.ID <= 0 {
			return nil, graphValidationError("invalid_responsive_ad_update")
		}
		if _, exists := seen[update.ID]; exists {
			return nil, graphValidationError("duplicate_responsive_ad_update")
		}
		seen[update.ID] = struct{}{}
		normalized, err := normalizeResponsiveAdDraft(ResponsiveAdDraft{
			AdGroupID: 1, Titles: update.Titles, Texts: update.Texts, Href: update.Href,
		})
		if err != nil {
			return nil, err
		}
		update.Titles = normalized.Titles
		update.Texts = normalized.Texts
		update.Href = normalized.Href
		result = append(result, update)
	}
	return result, nil
}

func normalizeKeywordUpdates(updates []KeywordUpdate) ([]KeywordUpdate, error) {
	if len(updates) == 0 || len(updates) > maxGraphMutationItems {
		return nil, graphValidationError("invalid_keyword_updates")
	}
	result := make([]KeywordUpdate, 0, len(updates))
	seen := make(map[int64]struct{}, len(updates))
	for _, update := range updates {
		if update.ID <= 0 {
			return nil, graphValidationError("invalid_keyword_update")
		}
		if _, exists := seen[update.ID]; exists {
			return nil, graphValidationError("duplicate_keyword_update")
		}
		seen[update.ID] = struct{}{}
		keyword, err := normalizeKeywordText(update.Keyword, false)
		if err != nil {
			return nil, err
		}
		update.Keyword = keyword
		result = append(result, update)
	}
	return result, nil
}

func normalizeAdGroupDraft(draft UnifiedAdGroupDraft) (UnifiedAdGroupDraft, error) {
	if draft.CampaignID <= 0 {
		return UnifiedAdGroupDraft{}, graphValidationError("invalid_campaign_id")
	}
	draft.Name = graphText(draft.Name)
	if draft.Name == "" || utf8.RuneCountInString(draft.Name) > maxAdGroupNameRunes {
		return UnifiedAdGroupDraft{}, graphValidationError("invalid_adgroup_name")
	}
	regionIDs, err := validateRegionIDs(draft.RegionIDs)
	if err != nil {
		return UnifiedAdGroupDraft{}, err
	}
	draft.RegionIDs = regionIDs
	draft.NegativeKeywords, err = normalizeNegativeKeywords(draft.NegativeKeywords)
	if err != nil {
		return UnifiedAdGroupDraft{}, err
	}
	draft.TrackingMarker = graphText(draft.TrackingMarker)
	if draft.TrackingMarker == "" ||
		utf8.RuneCountInString(draft.TrackingMarker) > maxTrackingMarkerRunes ||
		!graphTrackingMarkerPattern.MatchString(draft.TrackingMarker) {
		return UnifiedAdGroupDraft{}, graphValidationError("invalid_tracking_marker")
	}
	draft.Name = UnifiedAdGroupOperationName(draft.TrackingMarker)
	return draft, nil
}

func normalizeUnifiedAdGroup(
	group UnifiedAdGroup, expectedCampaignID int64,
) (UnifiedAdGroup, error) {
	if group.ID <= 0 || group.CampaignID <= 0 || group.CampaignID != expectedCampaignID {
		return UnifiedAdGroup{}, &Error{Code: "invalid_adgroup_response"}
	}
	group.Name = graphText(group.Name)
	if group.Name == "" || utf8.RuneCountInString(group.Name) > maxAdGroupNameRunes {
		return UnifiedAdGroup{}, &Error{Code: "invalid_adgroup_response"}
	}
	var err error
	group.RegionIDs, err = validateRegionIDs(group.RegionIDs)
	if err != nil {
		return UnifiedAdGroup{}, &Error{Code: "invalid_adgroup_response"}
	}
	group.NegativeKeywords, err = normalizeNegativeKeywords(group.NegativeKeywords)
	if err != nil {
		return UnifiedAdGroup{}, &Error{Code: "invalid_adgroup_response"}
	}
	group.TrackingParams = graphText(group.TrackingParams)
	if utf8.RuneCountInString(group.TrackingParams) > maxTrackingParamsRunes {
		return UnifiedAdGroup{}, &Error{Code: "invalid_adgroup_response"}
	}
	group.TrackingMarker = trackingMarkerFromParams(group.TrackingParams)
	group.OfferRetargeting = strings.ToUpper(strings.TrimSpace(group.OfferRetargeting))
	if group.OfferRetargeting != "NO" {
		return UnifiedAdGroup{}, &Error{Code: "unsupported_offer_retargeting"}
	}
	group.Status = strings.ToUpper(strings.TrimSpace(group.Status))
	group.ServingStatus = strings.ToUpper(strings.TrimSpace(group.ServingStatus))
	return group, nil
}

func validateRegionIDs(ids []int64) ([]int64, error) {
	if len(ids) == 0 || len(ids) > maxGraphRegions {
		return nil, graphValidationError("invalid_region_ids")
	}
	result := append([]int64(nil), ids...)
	seen := make(map[int64]struct{}, len(result))
	hasZero := false
	hasPositive := false
	for _, id := range result {
		if id == math.MinInt64 {
			return nil, graphValidationError("invalid_region_ids")
		}
		if _, ok := seen[id]; ok {
			return nil, graphValidationError("duplicate_region_id")
		}
		seen[id] = struct{}{}
		if id == 0 {
			hasZero = true
		} else if id > 0 {
			hasPositive = true
		}
	}
	if hasZero && len(result) != 1 || !hasZero && !hasPositive {
		return nil, graphValidationError("invalid_region_ids")
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result, nil
}

func normalizeNegativeKeywords(values []string) ([]string, error) {
	return normalizeNegativeKeywordsWithLimit(values, maxNegativeKeywordRunes)
}

func normalizeNegativeKeywordsWithLimit(values []string, maxRunes int) ([]string, error) {
	if len(values) == 0 {
		return []string{}, nil
	}
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	total := 0
	for _, value := range values {
		value = graphText(value)
		if value == "" || strings.HasPrefix(value, "-") {
			return nil, graphValidationError("invalid_negative_keyword")
		}
		words := strings.Fields(value)
		if len(words) == 0 || len(words) > maxKeywordWords {
			return nil, graphValidationError("invalid_negative_keyword")
		}
		for _, word := range words {
			if utf8.RuneCountInString(keywordWordCore(word)) > maxKeywordWordRunes {
				return nil, graphValidationError("invalid_negative_keyword")
			}
		}
		total += utf8.RuneCountInString(value)
		if total > maxRunes {
			return nil, graphValidationError("invalid_negative_keywords")
		}
		key := graphFold(value)
		if _, ok := seen[key]; ok {
			return nil, graphValidationError("duplicate_negative_keyword")
		}
		seen[key] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result, nil
}

func normalizeResponsiveAdDraft(draft ResponsiveAdDraft) (ResponsiveAdDraft, error) {
	if draft.AdGroupID <= 0 {
		return ResponsiveAdDraft{}, graphValidationError("invalid_adgroup_id")
	}
	titles, err := normalizeAdTextValues(
		draft.Titles, 1, maxResponsiveTitles,
		maxResponsiveTitleRunes, maxResponsiveTitleWord, "title",
	)
	if err != nil {
		return ResponsiveAdDraft{}, err
	}
	texts, err := normalizeAdTextValues(
		draft.Texts, 1, maxResponsiveTexts,
		maxResponsiveTextRunes, maxResponsiveTextWord, "text",
	)
	if err != nil {
		return ResponsiveAdDraft{}, err
	}
	href, err := normalizeHTTPSHref(draft.Href)
	if err != nil {
		return ResponsiveAdDraft{}, err
	}
	draft.Titles, draft.Texts, draft.Href = titles, texts, href
	return draft, nil
}

func normalizeResponsiveAd(
	ad ResponsiveAd, expectedCampaignID int64,
) (ResponsiveAd, error) {
	if ad.ID <= 0 || ad.CampaignID <= 0 || ad.CampaignID != expectedCampaignID ||
		ad.AdGroupID <= 0 {
		return ResponsiveAd{}, &Error{Code: "invalid_responsive_ad_response"}
	}
	titleValues := make([]string, 0, len(ad.Titles))
	for _, item := range ad.Titles {
		titleValues = append(titleValues, item.Value)
	}
	normalizedTitles, err := normalizeAdTextValues(
		titleValues, 1, maxResponsiveTitles,
		maxResponsiveTitleRunes, maxResponsiveTitleWord, "title",
	)
	if err != nil {
		return ResponsiveAd{}, &Error{Code: "invalid_responsive_ad_response"}
	}
	for index := range ad.Titles {
		ad.Titles[index].Value = normalizedTitles[index]
		ad.Titles[index].Status = strings.ToUpper(strings.TrimSpace(ad.Titles[index].Status))
		ad.Titles[index].StatusClarification = graphText(ad.Titles[index].StatusClarification)
	}
	textValues := make([]string, 0, len(ad.Texts))
	for _, item := range ad.Texts {
		textValues = append(textValues, item.Value)
	}
	normalizedTexts, err := normalizeAdTextValues(
		textValues, 1, maxResponsiveTexts,
		maxResponsiveTextRunes, maxResponsiveTextWord, "text",
	)
	if err != nil {
		return ResponsiveAd{}, &Error{Code: "invalid_responsive_ad_response"}
	}
	for index := range ad.Texts {
		ad.Texts[index].Value = normalizedTexts[index]
		ad.Texts[index].Status = strings.ToUpper(strings.TrimSpace(ad.Texts[index].Status))
		ad.Texts[index].StatusClarification = graphText(ad.Texts[index].StatusClarification)
	}
	ad.Href, err = normalizeHTTPSHref(ad.Href)
	if err != nil {
		return ResponsiveAd{}, &Error{Code: "invalid_responsive_ad_response"}
	}
	ad.Status = strings.ToUpper(strings.TrimSpace(ad.Status))
	ad.State = strings.ToUpper(strings.TrimSpace(ad.State))
	ad.StatusClarification = graphText(ad.StatusClarification)
	return ad, nil
}

func normalizeAdTextValues(
	values []string, minItems, maxItems, maxRunes, maxWordRunes int, kind string,
) ([]string, error) {
	if len(values) < minItems || len(values) > maxItems {
		return nil, graphValidationError("invalid_responsive_ad_" + kind + "s")
	}
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = graphText(value)
		if value == "" || utf8.RuneCountInString(value) > maxRunes {
			return nil, graphValidationError("invalid_responsive_ad_" + kind)
		}
		for _, word := range strings.Fields(value) {
			if utf8.RuneCountInString(word) > maxWordRunes {
				return nil, graphValidationError("invalid_responsive_ad_" + kind + "_word")
			}
		}
		key := graphFold(value)
		if _, ok := seen[key]; ok {
			return nil, graphValidationError("duplicate_responsive_ad_" + kind)
		}
		seen[key] = struct{}{}
		result = append(result, value)
	}
	return result, nil
}

func normalizeHTTPSHref(raw string) (string, error) {
	raw = graphText(raw)
	if raw == "" || utf8.RuneCountInString(raw) > maxResponsiveHrefRunes {
		return "", graphValidationError("invalid_responsive_ad_href")
	}
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || !strings.EqualFold(parsed.Scheme, "https") ||
		parsed.Hostname() == "" || parsed.User != nil {
		return "", graphValidationError("invalid_responsive_ad_href")
	}
	parsed.Scheme = "https"
	host := strings.ToLower(parsed.Host)
	host = strings.TrimSuffix(host, ":443")
	parsed.Host = host
	if parsed.RawQuery != "" {
		values, err := url.ParseQuery(parsed.RawQuery)
		if err != nil {
			return "", graphValidationError("invalid_responsive_ad_href")
		}
		parsed.RawQuery = values.Encode()
	}
	return parsed.String(), nil
}

func normalizeKeywordDrafts(drafts []KeywordDraft) ([]KeywordDraft, error) {
	if len(drafts) == 0 || len(drafts) > maxGraphMutationItems {
		return nil, graphValidationError("invalid_keywords")
	}
	result := make([]KeywordDraft, 0, len(drafts))
	seen := make(map[string]struct{}, len(drafts))
	for _, draft := range drafts {
		if draft.AdGroupID <= 0 {
			return nil, graphValidationError("invalid_adgroup_id")
		}
		keyword, err := normalizeKeywordText(draft.Keyword, false)
		if err != nil {
			return nil, err
		}
		key := fmt.Sprintf("%d\x00%s", draft.AdGroupID, graphFold(keyword))
		if _, ok := seen[key]; ok {
			return nil, graphValidationError("duplicate_keyword")
		}
		seen[key] = struct{}{}
		result = append(result, KeywordDraft{AdGroupID: draft.AdGroupID, Keyword: keyword})
	}
	return result, nil
}

func normalizeKeyword(keyword Keyword, expectedCampaignID int64) (Keyword, error) {
	if keyword.ID <= 0 || keyword.CampaignID <= 0 ||
		keyword.CampaignID != expectedCampaignID || keyword.AdGroupID <= 0 {
		return Keyword{}, &Error{Code: "invalid_keyword_response"}
	}
	var err error
	keyword.Keyword, err = normalizeKeywordText(keyword.Keyword, true)
	if err != nil {
		return Keyword{}, &Error{Code: "invalid_keyword_response"}
	}
	keyword.StrategyPriority = strings.ToUpper(strings.TrimSpace(keyword.StrategyPriority))
	keyword.UserParam1 = graphText(keyword.UserParam1)
	keyword.UserParam2 = graphText(keyword.UserParam2)
	if keyword.UserParam1 != "" || keyword.UserParam2 != "" {
		return Keyword{}, &Error{Code: "unsupported_keyword_user_params"}
	}
	keyword.Status = strings.ToUpper(strings.TrimSpace(keyword.Status))
	keyword.State = strings.ToUpper(strings.TrimSpace(keyword.State))
	keyword.ServingStatus = strings.ToUpper(strings.TrimSpace(keyword.ServingStatus))
	return keyword, nil
}

func normalizeKeywordText(raw string, allowAutotargeting bool) (string, error) {
	raw = graphText(raw)
	if raw == "---autotargeting" {
		if allowAutotargeting {
			return raw, nil
		}
		return "", graphValidationError("autotargeting_requires_explicit_configuration")
	}
	if raw == "" || utf8.RuneCountInString(raw) > maxKeywordRunes {
		return "", graphValidationError("invalid_keyword")
	}
	positiveWords := 0
	for _, word := range strings.Fields(raw) {
		core := keywordWordCore(word)
		if core == "" || utf8.RuneCountInString(core) > maxKeywordWordRunes {
			return "", graphValidationError("invalid_keyword_word")
		}
		if !strings.HasPrefix(word, "-") {
			positiveWords++
		}
	}
	if positiveWords == 0 || positiveWords > maxKeywordWords {
		return "", graphValidationError("invalid_keyword_words")
	}
	return raw, nil
}

func keywordWordCore(word string) string {
	return strings.Trim(word, `-+!()[]"`)
}

func validatePositiveIDs(values []int64, maxItems int, code string) ([]int64, error) {
	if len(values) == 0 || len(values) > maxItems {
		return nil, graphValidationError(code)
	}
	result := append([]int64(nil), values...)
	seen := make(map[int64]struct{}, len(values))
	for _, value := range result {
		if value <= 0 {
			return nil, graphValidationError(code)
		}
		if _, ok := seen[value]; ok {
			return nil, graphValidationError(code)
		}
		seen[value] = struct{}{}
	}
	return result, nil
}

func trackingMarkerFromParams(raw string) string {
	marker, present, valid := graphMarkerFromParams(raw, graphTrackingMarkerParam)
	if !present || !valid {
		return ""
	}
	return marker
}

func normalizeGraphMarker(raw, code string) (string, error) {
	marker := graphText(raw)
	if marker == "" || utf8.RuneCountInString(marker) > maxTrackingMarkerRunes ||
		!graphTrackingMarkerPattern.MatchString(marker) {
		return "", graphValidationError(code)
	}
	return marker, nil
}

func graphMarkerFromParams(raw, parameter string) (string, bool, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false, true
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return "", true, false
	}
	markers, ok := values[parameter]
	if !ok {
		return "", false, true
	}
	if len(markers) != 1 {
		return "", true, false
	}
	marker, err := normalizeGraphMarker(markers[0], "invalid_tracking_marker")
	if err != nil {
		return "", true, false
	}
	return marker, true, true
}

func nextGraphPage(current int64, limitedBy *int64) (int64, bool, error) {
	if limitedBy == nil {
		return 0, false, nil
	}
	if *limitedBy <= current {
		return 0, false, &Error{Code: "invalid_provider_pagination"}
	}
	return *limitedBy, true, nil
}

func validateUniqueGroupIDs(items []UnifiedAdGroup) error {
	seen := make(map[int64]struct{}, len(items))
	for _, item := range items {
		if _, ok := seen[item.ID]; ok {
			return &Error{Code: "duplicate_adgroup_response"}
		}
		seen[item.ID] = struct{}{}
	}
	return nil
}

func validateUniqueAdIDs(items []ResponsiveAd) error {
	seen := make(map[int64]struct{}, len(items))
	for _, item := range items {
		if _, ok := seen[item.ID]; ok {
			return &Error{Code: "duplicate_responsive_ad_response"}
		}
		seen[item.ID] = struct{}{}
	}
	return nil
}

func validateUniqueKeywordIDs(items []Keyword) error {
	seen := make(map[int64]struct{}, len(items))
	for _, item := range items {
		if _, ok := seen[item.ID]; ok {
			return &Error{Code: "duplicate_keyword_response"}
		}
		seen[item.ID] = struct{}{}
	}
	return nil
}

type canonicalCampaignGraph struct {
	Version  string                  `json:"version"`
	Campaign canonicalGraphCampaign  `json:"campaign"`
	AdGroups []canonicalGraphAdGroup `json:"ad_groups"`
	Ads      []canonicalGraphAd      `json:"ads"`
	Keywords []canonicalGraphKeyword `json:"keywords"`
}

type canonicalGraphCampaign struct {
	ID                          int64                  `json:"id"`
	Name                        string                 `json:"name"`
	Type                        string                 `json:"type"`
	WeeklyBudgetMinor           int64                  `json:"weekly_budget_minor"`
	StartsAt                    string                 `json:"starts_at"`
	EndsAt                      string                 `json:"ends_at"`
	TimeZone                    string                 `json:"time_zone"`
	NegativeKeywords            []string               `json:"negative_keywords"`
	NegativeKeywordSharedSetIDs []int64                `json:"negative_keyword_shared_set_ids"`
	BlockedIPs                  []string               `json:"blocked_ips"`
	ExcludedSites               []string               `json:"excluded_sites"`
	TimeTargeting               GraphTimeTargeting     `json:"time_targeting"`
	BiddingStrategy             json.RawMessage        `json:"bidding_strategy"`
	Settings                    []GraphCampaignSetting `json:"settings"`
	CounterIDs                  []int64                `json:"counter_ids"`
	PriorityGoals               []GraphPriorityGoal    `json:"priority_goals"`
	TrackingParams              string                 `json:"tracking_params"`
	AttributionModel            string                 `json:"attribution_model"`
}

type canonicalGraphAdGroup struct {
	ID               int64    `json:"id"`
	CampaignID       int64    `json:"campaign_id"`
	Name             string   `json:"name"`
	RegionIDs        []int64  `json:"region_ids"`
	NegativeKeywords []string `json:"negative_keywords"`
	TrackingParams   string   `json:"tracking_params"`
	OfferRetargeting string   `json:"offer_retargeting"`
}

type canonicalGraphAd struct {
	ID         int64    `json:"id"`
	CampaignID int64    `json:"campaign_id"`
	AdGroupID  int64    `json:"ad_group_id"`
	Titles     []string `json:"titles"`
	Texts      []string `json:"texts"`
	Href       string   `json:"href"`
}

type canonicalGraphKeyword struct {
	ID               int64  `json:"id"`
	CampaignID       int64  `json:"campaign_id"`
	AdGroupID        int64  `json:"ad_group_id"`
	Keyword          string `json:"keyword"`
	UserParam1       string `json:"user_param_1"`
	UserParam2       string `json:"user_param_2"`
	StrategyPriority string `json:"strategy_priority"`
}

func canonicalizeCampaignGraph(graph CampaignGraph) (canonicalCampaignGraph, error) {
	campaign, err := normalizeGraphCampaign(graph.Campaign)
	if err != nil {
		return canonicalCampaignGraph{}, err
	}
	result := canonicalCampaignGraph{
		Version: CampaignGraphFingerprintVersion,
		Campaign: canonicalGraphCampaign{
			ID: campaign.ID, Name: campaign.Name, Type: campaign.Type,
			WeeklyBudgetMinor: campaign.WeeklyBudgetMinor,
			StartsAt:          dateOnly(campaign.StartsAt).Format("2006-01-02"),
			EndsAt:            dateOnly(campaign.EndsAt).Format("2006-01-02"),
			TimeZone:          campaign.TimeZone,
			NegativeKeywords: append(
				[]string(nil), campaign.NegativeKeywords...,
			),
			NegativeKeywordSharedSetIDs: append(
				[]int64(nil), campaign.NegativeKeywordSharedSetIDs...,
			),
			BlockedIPs:    append([]string(nil), campaign.BlockedIPs...),
			ExcludedSites: append([]string(nil), campaign.ExcludedSites...),
			TimeTargeting: campaign.TimeTargeting,
			BiddingStrategy: append(
				json.RawMessage(nil), campaign.BiddingStrategy...,
			),
			Settings:   append([]GraphCampaignSetting(nil), campaign.Settings...),
			CounterIDs: append([]int64(nil), campaign.CounterIDs...),
			PriorityGoals: append(
				[]GraphPriorityGoal(nil), campaign.PriorityGoals...,
			),
			TrackingParams:   campaign.TrackingParams,
			AttributionModel: campaign.AttributionModel,
		},
		AdGroups: make([]canonicalGraphAdGroup, 0, len(graph.AdGroups)),
		Ads:      make([]canonicalGraphAd, 0, len(graph.Ads)),
		Keywords: make([]canonicalGraphKeyword, 0, len(graph.Keywords)),
	}
	groupIDs := make(map[int64]struct{}, len(graph.AdGroups))
	for _, group := range graph.AdGroups {
		normalized, err := normalizeUnifiedAdGroup(group, campaign.ID)
		if err != nil {
			return canonicalCampaignGraph{}, err
		}
		if _, exists := groupIDs[normalized.ID]; exists {
			return canonicalCampaignGraph{}, &Error{Code: "duplicate_adgroup_response"}
		}
		groupIDs[normalized.ID] = struct{}{}
		result.AdGroups = append(result.AdGroups, canonicalGraphAdGroup{
			ID: normalized.ID, CampaignID: normalized.CampaignID, Name: normalized.Name,
			RegionIDs:        append([]int64(nil), normalized.RegionIDs...),
			NegativeKeywords: append([]string(nil), normalized.NegativeKeywords...),
			TrackingParams:   normalized.TrackingParams,
			OfferRetargeting: normalized.OfferRetargeting,
		})
	}
	sort.Slice(result.AdGroups, func(i, j int) bool {
		return result.AdGroups[i].ID < result.AdGroups[j].ID
	})
	adIDs := make(map[int64]struct{}, len(graph.Ads))
	for _, ad := range graph.Ads {
		normalized, err := normalizeResponsiveAd(ad, campaign.ID)
		if err != nil {
			return canonicalCampaignGraph{}, err
		}
		if _, ok := groupIDs[normalized.AdGroupID]; !ok {
			return canonicalCampaignGraph{}, &Error{Code: "orphan_responsive_ad"}
		}
		if _, exists := adIDs[normalized.ID]; exists {
			return canonicalCampaignGraph{}, &Error{Code: "duplicate_responsive_ad_response"}
		}
		adIDs[normalized.ID] = struct{}{}
		titles := make([]string, 0, len(normalized.Titles))
		for _, title := range normalized.Titles {
			titles = append(titles, title.Value)
		}
		texts := make([]string, 0, len(normalized.Texts))
		for _, text := range normalized.Texts {
			texts = append(texts, text.Value)
		}
		sort.Strings(titles)
		sort.Strings(texts)
		result.Ads = append(result.Ads, canonicalGraphAd{
			ID: normalized.ID, CampaignID: normalized.CampaignID,
			AdGroupID: normalized.AdGroupID, Titles: titles, Texts: texts,
			Href: normalized.Href,
		})
	}
	sort.Slice(result.Ads, func(i, j int) bool { return result.Ads[i].ID < result.Ads[j].ID })
	keywordIDs := make(map[int64]struct{}, len(graph.Keywords))
	for _, keyword := range graph.Keywords {
		normalized, err := normalizeKeyword(keyword, campaign.ID)
		if err != nil {
			return canonicalCampaignGraph{}, err
		}
		if _, ok := groupIDs[normalized.AdGroupID]; !ok {
			return canonicalCampaignGraph{}, &Error{Code: "orphan_keyword"}
		}
		if _, exists := keywordIDs[normalized.ID]; exists {
			return canonicalCampaignGraph{}, &Error{Code: "duplicate_keyword_response"}
		}
		keywordIDs[normalized.ID] = struct{}{}
		result.Keywords = append(result.Keywords, canonicalGraphKeyword{
			ID: normalized.ID, CampaignID: normalized.CampaignID,
			AdGroupID: normalized.AdGroupID, Keyword: normalized.Keyword,
			UserParam1: normalized.UserParam1, UserParam2: normalized.UserParam2,
			StrategyPriority: normalized.StrategyPriority,
		})
	}
	sort.Slice(result.Keywords, func(i, j int) bool {
		return result.Keywords[i].ID < result.Keywords[j].ID
	})
	return result, nil
}

func graphText(value string) string {
	return norm.NFC.String(strings.TrimSpace(value))
}

func graphFold(value string) string {
	return strings.ToLower(graphText(value))
}

func graphValidationError(code string) error {
	return &Error{Code: code}
}
