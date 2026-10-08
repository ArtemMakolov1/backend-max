package yandexdirect

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const externalControlsLimit = 1000

type ExternalBudget struct {
	Key             string  `json:"budget_key"`
	AmountMicros    string  `json:"amount_micros"`
	StrategyType    string  `json:"strategy_type"`
	MinimumMicros   string  `json:"minimum_micros"`
	MaximumMicros   *string `json:"maximum_micros"`
	IncrementMicros string  `json:"increment_micros"`
	Editable        bool    `json:"editable"`
	ReadOnlyReason  *string `json:"read_only_reason"`
	Notice          *string `json:"notice"`
	Branch          string  `json:"-"`
	Mode            string  `json:"-"`
}
type ExternalKeyword struct {
	ID               int64    `json:"provider_keyword_id,string"`
	AdGroupID        int64    `json:"provider_group_id,string"`
	Keyword          string   `json:"keyword"`
	State            string   `json:"provider_state"`
	SearchBidMicros  *string  `json:"search_bid_micros"`
	NetworkBidMicros *string  `json:"network_bid_micros"`
	EditableFields   []string `json:"editable_fields"`
	ReadOnlyReason   *string  `json:"read_only_reason"`
	SearchBidIsAuto  string   `json:"-"`
}
type ExternalGroup struct {
	ID               int64             `json:"provider_group_id,string"`
	Name             string            `json:"name"`
	Type             string            `json:"group_type"`
	RegionIDs        []int64           `json:"region_ids"`
	NegativeKeywords []string          `json:"negative_keywords"`
	EditableFields   []string          `json:"editable_fields"`
	ReadOnlyReason   *string           `json:"read_only_reason"`
	CanCreateKeyword bool              `json:"can_create_keyword"`
	Keywords         []ExternalKeyword `json:"keywords"`
}
type ExternalStateActions struct {
	CanPause       bool    `json:"can_pause"`
	CanResume      bool    `json:"can_resume"`
	ReadOnlyReason *string `json:"read_only_reason"`
}
type ExternalSpend struct {
	AmountMicros *string `json:"amount_micros"`
	ObservedAt   *string `json:"observed_at"`
	From         *string `json:"from"`
	To           *string `json:"to"`
	Reason       string  `json:"reason"`
}
type ExternalRegion struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	ParentID *int64 `json:"parent_id"`
}
type ExternalCampaignControls struct {
	Regions            []ExternalRegion     `json:"regions"`
	CurrencyCode       string               `json:"currency_code"`
	LifecycleState     string               `json:"provider_state"`
	SearchStrategy     string               `json:"search_strategy"`
	NetworkStrategy    string               `json:"network_strategy"`
	Budgets            []ExternalBudget     `json:"budgets"`
	Groups             []ExternalGroup      `json:"groups"`
	CanCreateGroup     bool                 `json:"can_create_group"`
	CreateGroupReason  *string              `json:"create_group_reason"`
	StateActions       ExternalStateActions `json:"state_actions"`
	Spend              ExternalSpend        `json:"spend"`
	MinimumBidMicros   string               `json:"minimum_bid_micros"`
	MaximumBidMicros   string               `json:"maximum_bid_micros"`
	BidIncrementMicros string               `json:"bid_increment_micros"`
	RawStrategy        json.RawMessage      `json:"-"`
	CampaignType       string               `json:"-"`
}

func controlReason(value string) *string { return &value }

// Read a bounded complete projection. A truncated provider page cannot be used
// as an ownership or revision fence for an edit.
func (c *Client) GetExternalCampaignControls(ctx context.Context, token, login string, campaignID int64) (ExternalCampaignControls, error) {
	var response struct {
		Campaigns []struct {
			ID          int64  `json:"Id"`
			Type        string `json:"Type"`
			State       string `json:"State"`
			Currency    string `json:"Currency"`
			DailyBudget *struct {
				Amount int64  `json:"Amount"`
				Mode   string `json:"Mode"`
			} `json:"DailyBudget"`
			TextCampaign    json.RawMessage `json:"TextCampaign"`
			UnifiedCampaign json.RawMessage `json:"UnifiedCampaign"`
		} `json:"Campaigns"`
		LimitedBy *int64 `json:"LimitedBy"`
	}
	params := map[string]any{"SelectionCriteria": map[string]any{"Ids": []int64{campaignID}}, "FieldNames": []string{"Id", "Type", "State", "Currency", "DailyBudget"}, "TextCampaignFieldNames": []string{"BiddingStrategy", "PackageBiddingStrategy"}}
	if c.unified {
		params["UnifiedCampaignFieldNames"] = []string{"BiddingStrategy", "PackageBiddingStrategy"}
	}
	if err := c.call(ctx, "campaigns", token, login, map[string]any{"method": "get", "params": params}, &response); err != nil {
		return ExternalCampaignControls{}, err
	}
	if len(response.Campaigns) != 1 || response.Campaigns[0].ID != campaignID || response.LimitedBy != nil {
		return ExternalCampaignControls{}, &Error{Code: "invalid_external_controls_response"}
	}
	item := response.Campaigns[0]
	out := ExternalCampaignControls{CurrencyCode: item.Currency, CampaignType: item.Type, LifecycleState: item.State, Budgets: []ExternalBudget{}, Groups: []ExternalGroup{}, Spend: ExternalSpend{Reason: "not_requested"}}
	if item.Type != "TEXT_CAMPAIGN" && item.Type != "UNIFIED_CAMPAIGN" {
		out.CreateGroupReason = controlReason("unsupported_campaign_type")
		out.StateActions.ReadOnlyReason = controlReason("unsupported_campaign_type")
		return out, nil
	}
	raw := item.TextCampaign
	if item.Type == "UNIFIED_CAMPAIGN" {
		raw = item.UnifiedCampaign
	}
	var settings struct {
		Strategy json.RawMessage `json:"BiddingStrategy"`
		Package  json.RawMessage `json:"PackageBiddingStrategy"`
	}
	if json.Unmarshal(raw, &settings) != nil || len(settings.Strategy) == 0 {
		return out, &Error{Code: "invalid_external_strategy_response"}
	}
	out.RawStrategy = append(json.RawMessage(nil), settings.Strategy...)
	var strategy map[string]json.RawMessage
	if json.Unmarshal(settings.Strategy, &strategy) != nil {
		return out, &Error{Code: "invalid_external_strategy_response"}
	}
	packaged := len(settings.Package) > 0 && string(settings.Package) != "null"
	properties, regions, err := c.externalCurrencyProperties(ctx, token, login, item.Currency)
	if err != nil {
		return out, err
	}
	out.Regions = regions
	out.MinimumBidMicros, out.MaximumBidMicros, out.BidIncrementMicros = properties["MinimumBid"], properties["MaximumBid"], properties["BidIncrement"]
	for _, platform := range []string{"Search", "Network"} {
		var branch map[string]json.RawMessage
		if json.Unmarshal(strategy[platform], &branch) != nil {
			continue
		}
		var kind string
		_ = json.Unmarshal(branch["BiddingStrategyType"], &kind)
		if platform == "Search" {
			out.SearchStrategy = kind
		} else {
			out.NetworkStrategy = kind
		}
		key := externalStrategyBudgetBranch(kind)
		if key == "" {
			continue
		}
		var values struct {
			Weekly     *int64          `json:"WeeklySpendLimit"`
			Custom     json.RawMessage `json:"CustomPeriodBudget"`
			Adaptation *int            `json:"BudgetIncreasePercent"`
		}
		if json.Unmarshal(branch[key], &values) != nil || values.Weekly == nil || *values.Weekly <= 0 {
			continue
		}
		budget := ExternalBudget{Key: strings.ToLower(platform) + "_weekly", AmountMicros: strconv.FormatInt(*values.Weekly, 10), StrategyType: kind, MinimumMicros: properties["MinimumWeeklySpendLimit"], IncrementMicros: "10000", Branch: key, Editable: true}
		if maximum := properties["MaxAutobudget"]; maximum != "" {
			budget.MaximumMicros = &maximum
		}
		if len(values.Custom) > 0 && string(values.Custom) != "null" {
			budget.ReadOnlyReason = controlReason("period_budget_strategy")
			budget.Editable = false
		}
		if packaged {
			budget.ReadOnlyReason = controlReason("shared_strategy")
			budget.Editable = false
		}
		if values.Adaptation != nil && *values.Adaptation > 0 {
			budget.Notice = controlReason("automatic_budget_increase_enabled")
		}
		if item.State == "ARCHIVED" {
			budget.Editable = false
			budget.ReadOnlyReason = controlReason("campaign_archived")
		}
		out.Budgets = append(out.Budgets, budget)
	}
	if item.DailyBudget != nil && item.DailyBudget.Amount > 0 {
		budget := ExternalBudget{Key: "daily", AmountMicros: strconv.FormatInt(item.DailyBudget.Amount, 10), StrategyType: out.SearchStrategy, MinimumMicros: properties["MinimumDailyBudget"], IncrementMicros: "10000", Mode: item.DailyBudget.Mode, Notice: controlReason("daily_budget_recalculated_weekly")}
		budget.Editable = out.SearchStrategy == "HIGHEST_POSITION" && (budget.Mode == "STANDARD" || budget.Mode == "DISTRIBUTED") && !packaged && item.State != "ARCHIVED"
		if !budget.Editable {
			budget.ReadOnlyReason = controlReason("unsupported_daily_budget_strategy")
		}
		out.Budgets = append(out.Budgets, budget)
	}
	if item.Currency != "RUB" {
		for i := range out.Budgets {
			out.Budgets[i].Editable = false
			out.Budgets[i].ReadOnlyReason = controlReason("unsupported_currency_precision")
		}
	}
	out.CanCreateGroup = item.State != "ARCHIVED"
	if !out.CanCreateGroup {
		out.CreateGroupReason = controlReason("campaign_archived")
	}
	out.StateActions.CanPause = item.State == "ON"
	out.StateActions.CanResume = item.State == "SUSPENDED"
	if !out.StateActions.CanPause && !out.StateActions.CanResume {
		out.StateActions.ReadOnlyReason = controlReason("campaign_state_not_actionable")
	}
	groups, err := c.externalGroups(ctx, token, login, campaignID)
	if err != nil {
		return out, err
	}
	keywords, err := c.externalKeywords(ctx, token, login, campaignID)
	if err != nil {
		return out, err
	}
	groupIndex := make(map[int64]int, len(groups))
	for i := range groups {
		groupIndex[groups[i].ID] = i
		if item.State == "ARCHIVED" {
			groups[i].EditableFields = []string{}
			groups[i].CanCreateKeyword = false
			groups[i].ReadOnlyReason = controlReason("campaign_archived")
		}
	}
	for _, keyword := range keywords {
		i, ok := groupIndex[keyword.AdGroupID]
		if !ok {
			return out, &Error{Code: "invalid_external_keyword_scope"}
		}
		keyword.EditableFields = []string{}
		if groups[i].ReadOnlyReason == nil && keyword.Keyword != "---autotargeting" {
			keyword.EditableFields = append(keyword.EditableFields, "keyword")
			if item.Currency == "RUB" && out.SearchStrategy == "HIGHEST_POSITION" && (keyword.SearchBidIsAuto == "" || keyword.SearchBidIsAuto == "NO") {
				keyword.EditableFields = append(keyword.EditableFields, "search_bid_micros")
			}
			if item.Currency == "RUB" && (out.NetworkStrategy == "MAXIMUM_COVERAGE" || out.NetworkStrategy == "MANUAL_CPM") {
				keyword.EditableFields = append(keyword.EditableFields, "network_bid_micros")
			}
		} else {
			keyword.ReadOnlyReason = controlReason("unsupported_keyword_type")
		}
		groups[i].Keywords = append(groups[i].Keywords, keyword)
	}
	out.Groups = groups
	return out, nil
}

func externalStrategyBudgetBranch(strategy string) string {
	return map[string]string{"WB_MAXIMUM_CLICKS": "WbMaximumClicks", "WB_MAXIMUM_CONVERSION_RATE": "WbMaximumConversionRate", "AVERAGE_CPC": "AverageCpc", "AVERAGE_CPA": "AverageCpa", "AVERAGE_CRR": "AverageCrr", "PAY_FOR_CONVERSION": "PayForConversion", "PAY_FOR_CONVERSION_CRR": "PayForConversionCrr"}[strategy]
}
func (c *Client) externalCurrencyProperties(ctx context.Context, token, login, currency string) (map[string]string, []ExternalRegion, error) {
	var response struct {
		GeoRegions []struct {
			ID       int64  `json:"GeoRegionId"`
			Name     string `json:"GeoRegionName"`
			ParentID *int64 `json:"ParentId"`
		} `json:"GeoRegions"`
		Currencies []struct {
			Currency   string `json:"Currency"`
			Properties []struct {
				Name  string `json:"Name"`
				Value string `json:"Value"`
			} `json:"Properties"`
		} `json:"Currencies"`
	}
	if err := c.call(ctx, "dictionaries", token, login, map[string]any{"method": "get", "params": map[string]any{"DictionaryNames": []string{"Currencies", "GeoRegions"}}}, &response); err != nil {
		return nil, nil, err
	}
	regions := make([]ExternalRegion, 0, len(response.GeoRegions))
	seenRegions := map[int64]bool{}
	if len(response.GeoRegions) > 20000 {
		return nil, nil, &Error{Code: "invalid_external_regions"}
	}
	for _, region := range response.GeoRegions {
		if region.ID < 0 || region.ID > 9007199254740991 || seenRegions[region.ID] || !validExternalText(region.Name, 500) {
			return nil, nil, &Error{Code: "invalid_external_regions"}
		}
		seenRegions[region.ID] = true
		regions = append(regions, ExternalRegion{ID: region.ID, Name: region.Name, ParentID: region.ParentID})
	}
	sort.Slice(regions, func(i, j int) bool { return regions[i].ID < regions[j].ID })
	for _, item := range response.Currencies {
		if item.Currency != currency {
			continue
		}
		out := map[string]string{}
		for _, p := range item.Properties {
			if _, exists := out[p.Name]; exists {
				return nil, nil, &Error{Code: "invalid_external_currency_limits"}
			}
			out[p.Name] = p.Value
		}
		for _, name := range []string{"MinimumBid", "MaximumBid", "BidIncrement", "MinimumDailyBudget", "MinimumWeeklySpendLimit"} {
			if _, err := ParseExternalMicros(out[name]); err != nil {
				return nil, nil, &Error{Code: "invalid_external_currency_limits"}
			}
		}
		if maximum := out["MaxAutobudget"]; maximum != "" {
			if _, err := ParseExternalMicros(maximum); err != nil {
				delete(out, "MaxAutobudget")
			}
		}
		return out, regions, nil
	}
	return nil, nil, &Error{Code: "external_currency_not_found"}
}

func (c *Client) externalGroups(ctx context.Context, token, login string, campaign int64) ([]ExternalGroup, error) {
	var response struct {
		Groups []struct {
			ID       int64   `json:"Id"`
			Campaign int64   `json:"CampaignId"`
			Name     string  `json:"Name"`
			Type     string  `json:"Type"`
			Regions  []int64 `json:"RegionIds"`
			Negative *struct {
				Items []string `json:"Items"`
			} `json:"NegativeKeywords"`
		} `json:"AdGroups"`
		Limited *int64 `json:"LimitedBy"`
	}
	if err := c.call(ctx, "adgroups", token, login, map[string]any{"method": "get", "params": map[string]any{"SelectionCriteria": map[string]any{"CampaignIds": []int64{campaign}}, "FieldNames": []string{"Id", "CampaignId", "Name", "Type", "RegionIds", "NegativeKeywords"}, "Page": map[string]any{"Limit": externalControlsLimit}}}, &response); err != nil {
		return nil, err
	}
	if response.Groups == nil || response.Limited != nil || len(response.Groups) > externalControlsLimit {
		return nil, &Error{Code: "external_controls_too_large"}
	}
	out := make([]ExternalGroup, 0, len(response.Groups))
	seen := map[int64]bool{}
	for _, g := range response.Groups {
		if g.ID <= 0 || g.Campaign != campaign || seen[g.ID] {
			return nil, &Error{Code: "invalid_external_group_scope"}
		}
		seen[g.ID] = true
		group := ExternalGroup{ID: g.ID, Name: g.Name, Type: g.Type, RegionIDs: append([]int64(nil), g.Regions...), NegativeKeywords: []string{}, Keywords: []ExternalKeyword{}, EditableFields: []string{}}
		sort.Slice(group.RegionIDs, func(i, j int) bool { return group.RegionIDs[i] < group.RegionIDs[j] })
		if g.Negative != nil {
			group.NegativeKeywords = append(group.NegativeKeywords, g.Negative.Items...)
		}
		sort.Strings(group.NegativeKeywords)
		if g.Type == "TEXT_AD_GROUP" || g.Type == "UNIFIED_AD_GROUP" {
			group.EditableFields = []string{"name", "region_ids", "negative_keywords"}
			group.CanCreateKeyword = true
		} else {
			group.ReadOnlyReason = controlReason("unsupported_group_type")
		}
		out = append(out, group)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
func (c *Client) externalKeywords(ctx context.Context, token, login string, campaign int64) ([]ExternalKeyword, error) {
	var response struct {
		Keywords []struct {
			ID         int64  `json:"Id"`
			Campaign   int64  `json:"CampaignId"`
			Group      int64  `json:"AdGroupId"`
			Keyword    string `json:"Keyword"`
			State      string `json:"State"`
			Bid        *int64 `json:"Bid"`
			ContextBid *int64 `json:"ContextBid"`
			Auto       string `json:"AutotargetingSearchBidIsAuto"`
		} `json:"Keywords"`
		Limited *int64 `json:"LimitedBy"`
	}
	if err := c.call(ctx, "keywords", token, login, map[string]any{"method": "get", "params": map[string]any{"SelectionCriteria": map[string]any{"CampaignIds": []int64{campaign}}, "FieldNames": []string{"Id", "CampaignId", "AdGroupId", "Keyword", "State", "Bid", "ContextBid", "AutotargetingSearchBidIsAuto"}, "Page": map[string]any{"Limit": externalControlsLimit}}}, &response); err != nil {
		return nil, err
	}
	if response.Keywords == nil || response.Limited != nil || len(response.Keywords) > externalControlsLimit {
		return nil, &Error{Code: "external_controls_too_large"}
	}
	out := make([]ExternalKeyword, 0, len(response.Keywords))
	seen := map[int64]bool{}
	for _, k := range response.Keywords {
		if k.ID <= 0 || k.Campaign != campaign || k.Group <= 0 || seen[k.ID] {
			return nil, &Error{Code: "invalid_external_keyword_scope"}
		}
		seen[k.ID] = true
		keyword := ExternalKeyword{ID: k.ID, AdGroupID: k.Group, Keyword: k.Keyword, State: k.State, SearchBidIsAuto: k.Auto}
		if k.Bid != nil && *k.Bid >= 0 {
			value := strconv.FormatInt(*k.Bid, 10)
			keyword.SearchBidMicros = &value
		}
		if k.ContextBid != nil && *k.ContextBid >= 0 {
			value := strconv.FormatInt(*k.ContextBid, 10)
			keyword.NetworkBidMicros = &value
		}
		out = append(out, keyword)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Micros are decimal strings at the HTTP boundary: JavaScript numbers would
// silently round provider money above 2^53. No floats are used in mutations.
func ParseExternalMicros(value string) (int64, error) {
	if value == "" || value[0] == '0' || len(value) > 19 {
		return 0, errors.New("a positive decimal micros amount is required")
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0, errors.New("invalid micros amount")
		}
	}
	number, err := strconv.ParseInt(value, 10, 64)
	if err != nil || number <= 0 {
		return 0, errors.New("micros amount is out of range")
	}
	return number, nil
}
func ValidateExternalAmount(value, minimum, maximum, increment string) error {
	n, err := ParseExternalMicros(value)
	if err != nil {
		return err
	}
	min, err := ParseExternalMicros(minimum)
	if err != nil {
		return err
	}
	step, err := ParseExternalMicros(increment)
	if err != nil {
		return err
	}
	if n < min || n%step != 0 {
		return errors.New("amount is outside currency limits or precision")
	}
	if maximum != "" {
		max, err := ParseExternalMicros(maximum)
		if err != nil || n > max {
			return errors.New("amount exceeds the currency limit")
		}
	}
	return nil
}

type ExternalGroupChanges struct {
	Name             *string   `json:"name,omitempty"`
	RegionIDs        *[]int64  `json:"region_ids,omitempty"`
	NegativeKeywords *[]string `json:"negative_keywords,omitempty"`
}

func (p ExternalGroupChanges) Validate() error {
	if p.Name == nil && p.RegionIDs == nil && p.NegativeKeywords == nil {
		return errors.New("at least one group field is required")
	}
	if p.Name != nil && !validExternalText(*p.Name, 255) {
		return errors.New("group name must contain 1 to 255 characters")
	}
	if p.RegionIDs != nil {
		if len(*p.RegionIDs) < 1 || len(*p.RegionIDs) > 1000 {
			return errors.New("invalid region count")
		}
		seen := map[int64]bool{}
		positive := false
		world := false
		negative := false
		for _, id := range *p.RegionIDs {
			if id < -9007199254740991 || id > 9007199254740991 || seen[id] {
				return errors.New("invalid or duplicate region")
			}
			seen[id] = true
			positive = positive || id >= 0
			world = world || id == 0
			negative = negative || id < 0
		}
		if !positive || world && negative {
			return errors.New("region selection must include a positive region and cannot exclude from worldwide")
		}
	}
	if p.NegativeKeywords != nil {
		if len(*p.NegativeKeywords) > 4096 {
			return errors.New("too many negative phrases")
		}
		total := 0
		for _, v := range *p.NegativeKeywords {
			// Direct excludes spaces, hyphens and keyword operators from the
			// group aggregate. Count Unicode characters rather than bytes.
			meaningful := 0
			for _, r := range v {
				if !unicode.IsSpace(r) && !strings.ContainsRune("-!+\"[]()|", r) {
					meaningful++
				}
			}
			total += meaningful
			if total > 4096 {
				return errors.New("negative phrases exceed aggregate limit")
			}
			if meaningful == 0 || !validExternalText(v, 4096) || strings.HasPrefix(strings.TrimSpace(v), "-") || len(strings.Fields(v)) > 7 {
				return errors.New("invalid negative phrase")
			}
			for _, word := range strings.Fields(v) {
				if utf8.RuneCountInString(strings.Trim(word, "-!+\"[]()|")) > 35 {
					return errors.New("negative phrase word is too long")
				}
			}
		}
	}
	return nil
}
func validExternalText(value string, limit int) bool {
	if strings.TrimSpace(value) == "" || utf8.RuneCountInString(value) > limit {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func ValidateExternalKeyword(value string) error {
	if !validExternalText(value, 4096) || strings.HasPrefix(strings.TrimSpace(value), "---") {
		return errors.New("a supported ordinary keyword is required")
	}
	for _, word := range strings.Fields(value) {
		if utf8.RuneCountInString(strings.TrimLeft(word, "-!+\"[]")) > 35 {
			return errors.New("keyword word is too long")
		}
	}
	return nil
}

func (c *Client) UpdateExternalBudget(ctx context.Context, token, login string, campaign int64, current ExternalCampaignControls, key, amount, currency string) error {
	if currency != current.CurrencyCode {
		return graphValidationError("currency_mismatch")
	}
	for _, budget := range current.Budgets {
		if budget.Key != key {
			continue
		}
		max := ""
		if budget.MaximumMicros != nil {
			max = *budget.MaximumMicros
		}
		if !budget.Editable || ValidateExternalAmount(amount, budget.MinimumMicros, max, budget.IncrementMicros) != nil {
			return graphValidationError("unsupported_external_budget")
		}
		micros, _ := ParseExternalMicros(amount)
		item := map[string]any{"Id": campaign}
		if key == "daily" {
			item["DailyBudget"] = map[string]any{"Amount": micros, "Mode": budget.Mode}
		} else {
			platform := "Search"
			if key == "network_weekly" {
				platform = "Network"
			}
			kind := "TextCampaign"
			if current.CampaignType == "UNIFIED_CAMPAIGN" {
				kind = "UnifiedCampaign"
			}
			item[kind] = map[string]any{"BiddingStrategy": map[string]any{platform: map[string]any{"BiddingStrategyType": budget.StrategyType, budget.Branch: map[string]any{"WeeklySpendLimit": micros}}}}
		}
		return c.externalUpdate(ctx, "campaigns", "Campaigns", "UpdateResults", token, login, campaign, item)
	}
	return graphValidationError("unsupported_external_budget")
}
func (c *Client) UpdateExternalGroup(ctx context.Context, token, login string, group ExternalGroup, patch ExternalGroupChanges) error {
	if err := patch.Validate(); err != nil {
		return err
	}
	if group.ReadOnlyReason != nil {
		return graphValidationError("unsupported_external_group")
	}
	item := map[string]any{"Id": group.ID}
	if patch.Name != nil {
		item["Name"] = *patch.Name
	}
	if patch.RegionIDs != nil {
		item["RegionIds"] = *patch.RegionIDs
	}
	if patch.NegativeKeywords != nil {
		if len(*patch.NegativeKeywords) == 0 {
			item["NegativeKeywords"] = nil
		} else {
			item["NegativeKeywords"] = map[string]any{"Items": *patch.NegativeKeywords}
		}
	}
	return c.externalUpdate(ctx, "adgroups", "AdGroups", "UpdateResults", token, login, group.ID, item)
}
func (c *Client) UpdateExternalKeyword(ctx context.Context, token, login string, keyword ExternalKeyword, text string) (int64, error) {
	if err := ValidateExternalKeyword(text); err != nil {
		return 0, err
	}
	// Editing may create a new keyword or merge a duplicate. Direct returns
	// that authoritative ID; it is acknowledged durably before readback.
	return c.externalAction(ctx, "keywords", "update", "Keywords", "UpdateResults", token, login, keyword.ID, map[string]any{"Id": keyword.ID, "Keyword": text}, true)
}
func (c *Client) SetExternalKeywordBid(ctx context.Context, token, login string, keyword ExternalKeyword, search, network *string) error {
	item := map[string]any{"KeywordId": keyword.ID}
	if search != nil {
		n, err := ParseExternalMicros(*search)
		if err != nil {
			return err
		}
		item["SearchBid"] = n
		if keyword.SearchBidIsAuto != "" {
			item["AutotargetingSearchBidIsAuto"] = keyword.SearchBidIsAuto
		}
	}
	if network != nil {
		n, err := ParseExternalMicros(*network)
		if err != nil {
			return err
		}
		item["NetworkBid"] = n
	}
	if search == nil && network == nil {
		return graphValidationError("missing_external_bid")
	}
	_, err := c.externalAction(ctx, "keywordbids", "set", "KeywordBids", "SetResults", token, login, keyword.ID, item, false)
	return err
}
func (c *Client) SetExternalCampaignState(ctx context.Context, token, login string, campaign int64, action string) error {
	method := "suspend"
	if action == "resume" {
		method = "resume"
	} else if action != "pause" {
		return graphValidationError("invalid_campaign_action")
	}
	_, err := c.externalAction(ctx, "campaigns", method, "", "", token, login, campaign, nil, false)
	return err
}
func (c *Client) AddExternalGroup(ctx context.Context, token, login string, campaign int64, kind string, patch ExternalGroupChanges) (int64, error) {
	if patch.Name == nil || patch.RegionIDs == nil {
		return 0, graphValidationError("missing_group_fields")
	}
	if err := patch.Validate(); err != nil {
		return 0, err
	}
	item := map[string]any{"CampaignId": campaign, "Name": *patch.Name, "RegionIds": *patch.RegionIDs}
	if kind == "UNIFIED_CAMPAIGN" {
		item["UnifiedAdGroup"] = map[string]any{"OfferRetargeting": "NO"}
	} else if kind != "TEXT_CAMPAIGN" {
		return 0, graphValidationError("unsupported_campaign_type")
	}
	if patch.NegativeKeywords != nil && len(*patch.NegativeKeywords) > 0 {
		item["NegativeKeywords"] = map[string]any{"Items": *patch.NegativeKeywords}
	}
	return c.externalAction(ctx, "adgroups", "add", "AdGroups", "AddResults", token, login, 0, item, true)
}
func (c *Client) AddExternalKeyword(ctx context.Context, token, login string, group int64, text string) (int64, error) {
	if err := ValidateExternalKeyword(text); err != nil {
		return 0, err
	}
	return c.externalAction(ctx, "keywords", "add", "Keywords", "AddResults", token, login, 0, map[string]any{"AdGroupId": group, "Keyword": text}, true)
}
func (c *Client) externalAction(ctx context.Context, service, method, collection, resultName, token, login string, id int64, item map[string]any, creating bool) (int64, error) {
	params := map[string]any{}
	if collection == "" {
		params["SelectionCriteria"] = map[string]any{"Ids": []int64{id}}
		resultName = "SuspendResults"
		if method == "resume" {
			resultName = "ResumeResults"
		}
	} else {
		params[collection] = []any{item}
	}
	var response map[string]json.RawMessage
	if err := c.call(ctx, service, token, login, map[string]any{"method": method, "params": params}, &response); err != nil {
		return 0, err
	}
	var results []struct {
		ID        int64           `json:"Id"`
		KeywordID int64           `json:"KeywordId"`
		Errors    []ProviderIssue `json:"Errors"`
	}
	if json.Unmarshal(response[resultName], &results) != nil || len(results) != 1 {
		return 0, &Error{Code: "invalid_external_mutation_response"}
	}
	r := results[0]
	if len(r.Errors) > 0 {
		return 0, &PartialMutationError{Operation: service + "." + method, Results: []MutationResult{{Errors: r.Errors}}}
	}
	if service == "keywordbids" {
		r.ID = r.KeywordID
	}
	if r.ID <= 0 || !creating && r.ID != id {
		return 0, &Error{Code: "invalid_external_mutation_response"}
	}
	return r.ID, nil
}
