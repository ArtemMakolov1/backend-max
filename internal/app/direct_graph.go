package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"sort"
	"strings"

	"maxpilot/backend/internal/store"
	"maxpilot/backend/internal/yandexdirect"
)

var errDirectProviderGraphMismatch = fmt.Errorf(
	"%w: provider campaign graph does not match the local desired graph",
	ErrDirectSnapshotMismatch,
)

type directVerifiedProviderGraph struct {
	ObservedGraph      json.RawMessage
	GraphHash          string
	CampaignID         int64
	AdGroupID          int64
	AdID               int64
	KeywordMappings    []store.DirectKeywordMapping
	CampaignModeration store.DirectModerationSnapshot
	AdGroupModeration  store.DirectModerationSnapshot
	AdModeration       store.DirectModerationSnapshot
	ModerationStatus   string
	Clarification      string
}

func verifyDirectProviderGraph(
	graph yandexdirect.CampaignGraph,
	campaign store.DirectCampaign,
	connection store.DirectConnection,
	operationMarker string,
	regionIDs []int64,
	expectedCampaignID, expectedAdGroupID, expectedAdID *int64,
) (directVerifiedProviderGraph, error) {
	return verifyDirectProviderGraphState(
		graph, campaign, connection, operationMarker, regionIDs,
		expectedCampaignID, expectedAdGroupID, expectedAdID, true,
	)
}

func verifyDirectProviderGraphState(
	graph yandexdirect.CampaignGraph,
	campaign store.DirectCampaign,
	connection store.DirectConnection,
	operationMarker string,
	regionIDs []int64,
	expectedCampaignID, expectedAdGroupID, expectedAdID *int64,
	requireOff bool,
) (directVerifiedProviderGraph, error) {
	mismatch := func(field string) (directVerifiedProviderGraph, error) {
		return directVerifiedProviderGraph{}, fmt.Errorf(
			"%w: %s", errDirectProviderGraphMismatch, field,
		)
	}
	operationMarker = strings.TrimSpace(operationMarker)
	if operationMarker == "" {
		return mismatch("operation marker")
	}
	providerCampaign := graph.Campaign
	if expectedCampaignID != nil && providerCampaign.ID != *expectedCampaignID {
		return mismatch("campaign id")
	}
	if providerCampaign.ID <= 0 ||
		strings.TrimSpace(providerCampaign.Name) != strings.TrimSpace(campaign.Name) ||
		providerCampaign.Type != "UNIFIED_CAMPAIGN" ||
		providerCampaign.WeeklyBudgetMinor != campaign.WeeklyBudgetMinor ||
		!sameDirectDate(providerCampaign.StartsAt, campaign.StartsAt) ||
		!sameDirectDate(providerCampaign.EndsAt, campaign.EndsAt) ||
		strings.TrimSpace(providerCampaign.TimeZone) != strings.TrimSpace(connection.Timezone) {
		return mismatch("campaign")
	}
	state := strings.ToUpper(strings.TrimSpace(providerCampaign.State))
	if (requireOff && state != "OFF") ||
		(!requireOff && state != "OFF" && state != "ON") {
		return mismatch("campaign state")
	}
	if len(providerCampaign.NegativeKeywords) != 0 ||
		len(providerCampaign.NegativeKeywordSharedSetIDs) != 0 ||
		len(providerCampaign.BlockedIPs) != 0 ||
		len(providerCampaign.ExcludedSites) != 0 ||
		len(providerCampaign.CounterIDs) != 0 ||
		len(providerCampaign.PriorityGoals) != 0 {
		return mismatch("unsupported campaign targeting")
	}
	if !reflect.DeepEqual(
		providerCampaign.TimeTargeting,
		yandexdirect.SafeUnifiedCampaignTimeTargeting(),
	) {
		return mismatch("time targeting")
	}
	if providerCampaign.TrackingParams != (url.Values{
		"mp_op": []string{operationMarker},
	}).Encode() {
		return mismatch("campaign tracking marker")
	}
	if providerCampaign.AttributionModel != "AUTO" {
		return mismatch("attribution model")
	}
	if err := validateDirectSafeCampaignSettings(providerCampaign.Settings); err != nil {
		return directVerifiedProviderGraph{}, err
	}
	if err := validateDirectSafeBiddingStrategy(
		providerCampaign.BiddingStrategy, campaign.WeeklyBudgetMinor,
	); err != nil {
		return directVerifiedProviderGraph{}, err
	}

	if len(graph.AdGroups) != 1 {
		return mismatch("ad group count")
	}
	group := graph.AdGroups[0]
	sortedRegionIDs := append([]int64(nil), regionIDs...)
	sort.Slice(sortedRegionIDs, func(i, j int) bool { return sortedRegionIDs[i] < sortedRegionIDs[j] })
	if expectedAdGroupID != nil && group.ID != *expectedAdGroupID {
		return mismatch("ad group id")
	}
	if group.ID <= 0 || group.CampaignID != providerCampaign.ID ||
		strings.TrimSpace(group.Name) != yandexdirect.UnifiedAdGroupOperationName(operationMarker) ||
		!reflect.DeepEqual(group.RegionIDs, sortedRegionIDs) ||
		!reflect.DeepEqual(group.NegativeKeywords, sortedCopy(campaign.NegativeKeywords)) ||
		group.TrackingMarker != "" || group.TrackingParams != "" ||
		group.OfferRetargeting != "NO" {
		return mismatch("ad group")
	}

	if len(graph.Ads) != 1 {
		return mismatch("ad count")
	}
	ad := graph.Ads[0]
	if expectedAdID != nil && ad.ID != *expectedAdID {
		return mismatch("ad id")
	}
	if ad.ID <= 0 || ad.CampaignID != providerCampaign.ID || ad.AdGroupID != group.ID ||
		!reflect.DeepEqual(moderatedTextValues(ad.Titles), campaign.Titles) ||
		!reflect.DeepEqual(moderatedTextValues(ad.Texts), campaign.Texts) {
		return mismatch("responsive ad")
	}
	href, err := normalizeDirectGraphHref(campaign.LandingURL)
	if err != nil || ad.Href != href {
		return mismatch("responsive ad href")
	}

	byKeyword := make(map[string]yandexdirect.Keyword, len(graph.Keywords))
	for _, keyword := range graph.Keywords {
		if keyword.CampaignID != providerCampaign.ID || keyword.AdGroupID != group.ID ||
			keyword.ID <= 0 || keyword.StrategyPriority != "NORMAL" ||
			keyword.UserParam1 != "" || keyword.UserParam2 != "" {
			return mismatch("keyword")
		}
		key := strings.ToLower(strings.TrimSpace(keyword.Keyword))
		if _, duplicate := byKeyword[key]; duplicate {
			return mismatch("duplicate keyword")
		}
		byKeyword[key] = keyword
	}
	if len(byKeyword) != len(campaign.Keywords) {
		return mismatch("keyword count")
	}
	keywordMappings := make([]store.DirectKeywordMapping, 0, len(campaign.Keywords))
	for _, desiredKeyword := range campaign.Keywords {
		keyword, ok := byKeyword[strings.ToLower(strings.TrimSpace(desiredKeyword))]
		if !ok || keyword.Keyword != desiredKeyword {
			return mismatch("keyword content")
		}
		keywordMappings = append(keywordMappings, store.DirectKeywordMapping{
			Keyword: desiredKeyword, ProviderKeywordID: keyword.ID,
			Moderation: directModerationSnapshot(
				keyword.Status, keyword.State, keyword.ServingStatus, "",
			),
		})
	}

	hash, err := graph.Fingerprint()
	if err != nil {
		return directVerifiedProviderGraph{}, err
	}
	observed, err := json.Marshal(graph)
	if err != nil {
		return directVerifiedProviderGraph{}, err
	}
	campaignModeration := directModerationSnapshot(
		providerCampaign.Status, providerCampaign.State, "", "",
	)
	adGroupModeration := directModerationSnapshot(
		group.Status, "", group.ServingStatus, "",
	)
	adModeration := directModerationSnapshot(
		ad.Status, ad.State, "", ad.StatusClarification,
	)
	status, clarification := directGraphModerationStatus(
		graph, campaignModeration, adGroupModeration, adModeration,
	)
	return directVerifiedProviderGraph{
		ObservedGraph: observed, GraphHash: hash,
		CampaignID: providerCampaign.ID, AdGroupID: group.ID, AdID: ad.ID,
		KeywordMappings:    keywordMappings,
		CampaignModeration: campaignModeration,
		AdGroupModeration:  adGroupModeration, AdModeration: adModeration,
		ModerationStatus: status, Clarification: clarification,
	}, nil
}

func validateDirectSafeCampaignSettings(settings []yandexdirect.GraphCampaignSetting) error {
	required := make(map[string]string)
	for _, setting := range yandexdirect.SafeUnifiedCampaignSettings() {
		required[setting.Option] = setting.Value
	}
	seen := make(map[string]struct{}, len(settings))
	for _, setting := range settings {
		if _, duplicate := seen[setting.Option]; duplicate {
			return fmt.Errorf("%w: duplicate campaign setting", errDirectProviderGraphMismatch)
		}
		seen[setting.Option] = struct{}{}
		if expected, ok := required[setting.Option]; ok {
			if setting.Value != expected {
				return fmt.Errorf("%w: unsafe campaign setting %s",
					errDirectProviderGraphMismatch, setting.Option)
			}
			continue
		}
		if setting.Option != "SHARED_ACCOUNT_ENABLED" ||
			(setting.Value != "YES" && setting.Value != "NO") {
			return fmt.Errorf("%w: unknown campaign setting %s",
				errDirectProviderGraphMismatch, setting.Option)
		}
	}
	for option := range required {
		if _, ok := seen[option]; !ok {
			return fmt.Errorf("%w: missing campaign setting %s",
				errDirectProviderGraphMismatch, option)
		}
	}
	return nil
}

func validateDirectSafeBiddingStrategy(raw json.RawMessage, budgetMinor int64) error {
	var strategy map[string]json.RawMessage
	if json.Unmarshal(raw, &strategy) != nil || len(strategy) != 2 {
		return fmt.Errorf("%w: bidding strategy", errDirectProviderGraphMismatch)
	}
	var search map[string]json.RawMessage
	if json.Unmarshal(strategy["Search"], &search) != nil {
		return fmt.Errorf("%w: search strategy", errDirectProviderGraphMismatch)
	}
	var searchType string
	if json.Unmarshal(search["BiddingStrategyType"], &searchType) != nil ||
		searchType != "SERVING_OFF" {
		return fmt.Errorf("%w: search is not disabled", errDirectProviderGraphMismatch)
	}
	var searchPlacementTypes map[string]string
	if json.Unmarshal(search["PlacementTypes"], &searchPlacementTypes) != nil ||
		len(search) != 2 || len(searchPlacementTypes) != 5 {
		return fmt.Errorf("%w: search placement strategy", errDirectProviderGraphMismatch)
	}
	for _, placement := range []string{
		"SearchResults", "ProductGallery", "DynamicPlaces", "Maps",
		"SearchOrganizationList",
	} {
		if searchPlacementTypes[placement] != "NO" {
			return fmt.Errorf(
				"%w: search placement %s is not disabled",
				errDirectProviderGraphMismatch, placement,
			)
		}
	}
	var network struct {
		BiddingStrategyType string            `json:"BiddingStrategyType"`
		PlacementTypes      map[string]string `json:"PlacementTypes"`
		WbMaximumClicks     struct {
			WeeklySpendLimit int64 `json:"WeeklySpendLimit"`
		} `json:"WbMaximumClicks"`
	}
	if json.Unmarshal(strategy["Network"], &network) != nil ||
		network.BiddingStrategyType != "WB_MAXIMUM_CLICKS" ||
		network.PlacementTypes["Network"] != "YES" ||
		network.PlacementTypes["Maps"] != "NO" ||
		len(network.PlacementTypes) != 2 {
		return fmt.Errorf("%w: network strategy", errDirectProviderGraphMismatch)
	}
	wantMicros, err := yandexdirect.MinorToMicros(budgetMinor)
	if err != nil || network.WbMaximumClicks.WeeklySpendLimit != wantMicros {
		return fmt.Errorf("%w: network budget", errDirectProviderGraphMismatch)
	}
	var networkFields map[string]json.RawMessage
	if json.Unmarshal(strategy["Network"], &networkFields) != nil ||
		len(networkFields) != 3 ||
		networkFields["BiddingStrategyType"] == nil ||
		networkFields["PlacementTypes"] == nil ||
		networkFields["WbMaximumClicks"] == nil {
		return fmt.Errorf("%w: unknown network strategy field", errDirectProviderGraphMismatch)
	}
	var clickFields map[string]json.RawMessage
	if json.Unmarshal(networkFields["WbMaximumClicks"], &clickFields) != nil ||
		len(clickFields) != 1 || clickFields["WeeklySpendLimit"] == nil {
		return fmt.Errorf("%w: click strategy", errDirectProviderGraphMismatch)
	}
	return nil
}

func normalizeDirectGraphHref(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !parsed.IsAbs() || !strings.EqualFold(parsed.Scheme, "https") ||
		parsed.Hostname() == "" || parsed.User != nil {
		return "", errors.New("invalid HTTPS landing URL")
	}
	parsed.Scheme = "https"
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Host = strings.TrimSuffix(parsed.Host, ":443")
	if parsed.RawQuery != "" {
		values, parseErr := url.ParseQuery(parsed.RawQuery)
		if parseErr != nil {
			return "", parseErr
		}
		parsed.RawQuery = values.Encode()
	}
	return parsed.String(), nil
}

func directGraphModerationStatus(
	graph yandexdirect.CampaignGraph,
	campaign, group, ad store.DirectModerationSnapshot,
) (string, string) {
	statuses := []string{campaign.Status, group.Status, ad.Status}
	for _, item := range graph.Ads {
		for _, title := range item.Titles {
			statuses = append(statuses, title.Status)
		}
		for _, text := range item.Texts {
			statuses = append(statuses, text.Status)
		}
	}
	for _, keyword := range graph.Keywords {
		statuses = append(statuses, keyword.Status)
	}
	pending := false
	for _, raw := range statuses {
		switch strings.ToUpper(strings.TrimSpace(raw)) {
		case "ACCEPTED":
		case "DRAFT", "MODERATION", "PREACCEPTED":
			pending = true
		case "REJECTED":
			return "REJECTED", firstDirectModerationClarification(campaign, group, ad)
		default:
			return "UNKNOWN", "provider_moderation_status_unknown"
		}
	}
	if pending {
		return "MODERATION", firstDirectModerationClarification(campaign, group, ad)
	}
	return "ACCEPTED", ""
}

func directModerationSnapshot(
	status, state, servingStatus, clarification string,
) store.DirectModerationSnapshot {
	return store.DirectModerationSnapshot{
		Status:              strings.ToUpper(strings.TrimSpace(status)),
		State:               strings.ToUpper(strings.TrimSpace(state)),
		ServingStatus:       strings.ToUpper(strings.TrimSpace(servingStatus)),
		StatusClarification: strings.TrimSpace(clarification),
	}
}

func firstDirectModerationClarification(values ...store.DirectModerationSnapshot) string {
	for _, value := range values {
		if clarification := strings.TrimSpace(value.StatusClarification); clarification != "" {
			return clarification
		}
	}
	return ""
}

func moderatedTextValues(values []yandexdirect.ModeratedText) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, value.Value)
	}
	return result
}

func sortedCopy(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}

func directProviderIssues(values []yandexdirect.ProviderIssue) []store.DirectProviderIssue {
	result := make([]store.DirectProviderIssue, 0, len(values))
	for _, value := range values {
		result = append(result, store.DirectProviderIssue{
			Code: value.Code, Message: value.Message, Details: value.Details,
		})
	}
	return result
}
