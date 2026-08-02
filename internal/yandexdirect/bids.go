package yandexdirect

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
)

// CampaignBidConfiguration is the spend control that is actually applicable
// to MaxPosty's automatic WB_MAXIMUM_CLICKS campaign strategy. It deliberately
// does not expose Bids.set fields, which Direct ignores or rejects for this
// strategy.
type CampaignBidConfiguration struct {
	CampaignID        int64
	WeeklyBudgetMinor int64
	BidCeilingMinor   int64
}

// CurrencyBidLimits contains the authoritative monetary restrictions returned
// by Dictionaries.get. Values use minor currency units; zero is never returned
// for a successful lookup.
type CurrencyBidLimits struct {
	CurrencyCode      string
	MinimumBidMinor   int64
	MaximumBidMinor   int64
	BidIncrementMinor int64
}

// GetCampaignBidConfiguration reads the current provider strategy and extracts
// its optional BidCeiling. The method fails closed for any other strategy.
func (c *Client) GetCampaignBidConfiguration(
	ctx context.Context, token, clientLogin string, campaignID int64,
) (CampaignBidConfiguration, error) {
	if err := c.requireUnifiedGraph(); err != nil {
		return CampaignBidConfiguration{}, err
	}
	if campaignID <= 0 {
		return CampaignBidConfiguration{}, graphValidationError("invalid_campaign_id")
	}
	var response struct {
		Campaigns []struct {
			ID              int64  `json:"Id"`
			Type            string `json:"Type"`
			UnifiedCampaign *struct {
				BiddingStrategy json.RawMessage `json:"BiddingStrategy"`
			} `json:"UnifiedCampaign"`
		} `json:"Campaigns"`
	}
	err := c.call(ctx, "campaigns", token, clientLogin, map[string]any{
		"method": "get",
		"params": map[string]any{
			"SelectionCriteria":         map[string]any{"Ids": []int64{campaignID}},
			"FieldNames":                []string{"Id", "Type"},
			"UnifiedCampaignFieldNames": []string{"BiddingStrategy"},
			"UnifiedCampaignSearchStrategyPlacementTypesFieldNames": []string{
				"SearchResults", "ProductGallery", "DynamicPlaces", "Maps",
				"SearchOrganizationList",
			},
		},
	}, &response)
	if err != nil {
		return CampaignBidConfiguration{}, err
	}
	if len(response.Campaigns) != 1 || response.Campaigns[0].ID != campaignID ||
		strings.ToUpper(strings.TrimSpace(response.Campaigns[0].Type)) != "UNIFIED_CAMPAIGN" ||
		response.Campaigns[0].UnifiedCampaign == nil {
		return CampaignBidConfiguration{}, &Error{Code: "campaign_bid_configuration_unavailable"}
	}
	budgetMinor, ceilingMinor, err := supportedAutomaticBidStrategy(
		response.Campaigns[0].UnifiedCampaign.BiddingStrategy,
	)
	if err != nil {
		return CampaignBidConfiguration{}, err
	}
	return CampaignBidConfiguration{
		CampaignID: campaignID, WeeklyBudgetMinor: budgetMinor,
		BidCeilingMinor: ceilingMinor,
	}, nil
}

// GetCurrencyBidLimits reads provider-owned validation limits. Callers should
// not hard-code these values because they vary by currency and may change.
func (c *Client) GetCurrencyBidLimits(
	ctx context.Context, token, clientLogin, currencyCode string,
) (CurrencyBidLimits, error) {
	if c == nil {
		return CurrencyBidLimits{}, &Error{Code: "direct_client_unavailable"}
	}
	currencyCode = strings.ToUpper(strings.TrimSpace(currencyCode))
	if currencyCode == "" {
		return CurrencyBidLimits{}, graphValidationError("invalid_currency")
	}
	values, err := c.currencyCache.get(
		ctx, directCurrencyCacheKey(token, clientLogin),
		func(loadCtx context.Context) (map[string]CurrencyBidLimits, error) {
			return c.loadCurrencyBidLimits(loadCtx, token, clientLogin)
		},
	)
	if err != nil {
		return CurrencyBidLimits{}, err
	}
	match, ok := values[currencyCode]
	if !ok {
		return CurrencyBidLimits{}, &Error{Code: "currency_bid_limits_not_found"}
	}
	return match, nil
}

func (c *Client) loadCurrencyBidLimits(
	ctx context.Context, token, clientLogin string,
) (map[string]CurrencyBidLimits, error) {
	var response struct {
		Currencies []struct {
			Currency   string `json:"Currency"`
			Properties []struct {
				Name  string `json:"Name"`
				Value string `json:"Value"`
			} `json:"Properties"`
		} `json:"Currencies"`
	}
	err := c.call(ctx, "dictionaries", token, clientLogin, map[string]any{
		"method": "get",
		"params": map[string]any{"DictionaryNames": []string{"Currencies"}},
	}, &response)
	if err != nil {
		return nil, err
	}
	values := make(map[string]CurrencyBidLimits, len(response.Currencies))
	for _, currency := range response.Currencies {
		currencyCode := strings.ToUpper(strings.TrimSpace(currency.Currency))
		if currencyCode == "" {
			return nil, &Error{Code: "invalid_currency_bid_limits"}
		}
		if _, duplicate := values[currencyCode]; duplicate {
			return nil, &Error{Code: "duplicate_currency_bid_limits"}
		}
		properties := make(map[string]string, len(currency.Properties))
		for _, property := range currency.Properties {
			name := strings.TrimSpace(property.Name)
			if name == "" {
				return nil, &Error{Code: "invalid_currency_bid_limits"}
			}
			if _, duplicate := properties[name]; duplicate {
				return nil, &Error{Code: "invalid_currency_bid_limits"}
			}
			properties[name] = strings.TrimSpace(property.Value)
		}
		minimum, minimumErr := providerMoneyToMinor(properties["MinimumBid"])
		maximum, maximumErr := providerMoneyToMinor(properties["MaximumBid"])
		increment, incrementErr := providerMoneyToMinor(properties["BidIncrement"])
		if minimumErr != nil || maximumErr != nil || incrementErr != nil ||
			minimum > maximum {
			return nil, &Error{Code: "invalid_currency_bid_limits"}
		}
		values[currencyCode] = CurrencyBidLimits{
			CurrencyCode: currencyCode, MinimumBidMinor: minimum,
			MaximumBidMinor: maximum, BidIncrementMinor: increment,
		}
	}
	if len(values) == 0 {
		return nil, &Error{Code: "invalid_currency_bid_limits"}
	}
	return values, nil
}

func supportedAutomaticBidStrategy(raw json.RawMessage) (int64, int64, error) {
	var strategy struct {
		Search struct {
			BiddingStrategyType string `json:"BiddingStrategyType"`
		} `json:"Search"`
		Network struct {
			BiddingStrategyType string `json:"BiddingStrategyType"`
			WbMaximumClicks     struct {
				WeeklySpendLimit int64  `json:"WeeklySpendLimit"`
				BidCeiling       *int64 `json:"BidCeiling"`
			} `json:"WbMaximumClicks"`
		} `json:"Network"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &strategy) != nil ||
		strings.ToUpper(strings.TrimSpace(strategy.Search.BiddingStrategyType)) != "SERVING_OFF" ||
		strings.ToUpper(strings.TrimSpace(strategy.Network.BiddingStrategyType)) != "WB_MAXIMUM_CLICKS" {
		return 0, 0, &Error{Code: "unsupported_campaign_bidding_strategy"}
	}
	budgetMinor, err := MicrosToMinor(strategy.Network.WbMaximumClicks.WeeklySpendLimit)
	if err != nil {
		return 0, 0, &Error{Code: "invalid_campaign_bid_budget"}
	}
	ceilingMinor := int64(0)
	if strategy.Network.WbMaximumClicks.BidCeiling != nil {
		ceilingMinor, err = MicrosToMinor(*strategy.Network.WbMaximumClicks.BidCeiling)
		if err != nil {
			return 0, 0, &Error{Code: "invalid_campaign_bid_ceiling"}
		}
	}
	return budgetMinor, ceilingMinor, nil
}

func providerMoneyToMinor(value string) (int64, error) {
	micros, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return 0, err
	}
	return MicrosToMinor(micros)
}
