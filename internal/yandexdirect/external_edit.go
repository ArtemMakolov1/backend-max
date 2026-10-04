package yandexdirect

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ExternalAd is a narrow read projection, not a MaxPosty-managed graph.
// Fields absent from this projection are never submitted by external edits.
type ExternalAd struct {
	ID         int64    `json:"id"`
	CampaignID int64    `json:"campaign_id"`
	AdGroupID  int64    `json:"ad_group_id"`
	Type       string   `json:"type"`
	Status     string   `json:"status"`
	State      string   `json:"state"`
	Title      string   `json:"title"`
	Title2     *string  `json:"title2"`
	Text       string   `json:"text"`
	Titles     []string `json:"titles"`
	Texts      []string `json:"texts"`
	Href       *string  `json:"href"`
	BusinessID *int64   `json:"business_id"`
}

type ExternalCampaignDetail struct {
	Campaign CampaignSummary           `json:"campaign"`
	Ads      []ExternalAd              `json:"ads"`
	Controls *ExternalCampaignControls `json:"controls,omitempty"`
}

func (d ExternalCampaignDetail) Fingerprint() (string, error) {
	// Lifecycle/moderation changes do not invalidate a creative draft. IDs,
	// ordered peers and the destination remain part of its revision fence.
	copy := d
	copy.Campaign.Status, copy.Campaign.State, copy.Campaign.StatusPayment = "", "", ""
	copy.Ads = append([]ExternalAd(nil), d.Ads...)
	sortExternalAds(copy.Ads)
	for i := range copy.Ads {
		copy.Ads[i].Status, copy.Ads[i].State = "", ""
	}
	value, err := json.Marshal(copy)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(value)
	return hex.EncodeToString(hash[:]), nil
}

func (c *Client) GetExternalCampaignDetail(ctx context.Context, token, login string, campaignID int64) (ExternalCampaignDetail, error) {
	items, err := c.listCampaignDetails(ctx, token, login, []int64{campaignID})
	if err != nil {
		return ExternalCampaignDetail{}, err
	}
	detail := ExternalCampaignDetail{Campaign: items[campaignID], Ads: []ExternalAd{}}
	var result struct {
		Ads []struct {
			ID         int64  `json:"Id"`
			CampaignID int64  `json:"CampaignId"`
			AdGroupID  int64  `json:"AdGroupId"`
			Type       string `json:"Type"`
			Status     string `json:"Status"`
			State      string `json:"State"`
			TextAd     *struct {
				Title      string  `json:"Title"`
				Title2     *string `json:"Title2"`
				Text       string  `json:"Text"`
				Href       *string `json:"Href"`
				BusinessID *int64  `json:"BusinessId"`
			} `json:"TextAd"`
			ResponsiveAd *struct {
				Titles []struct {
					Title string `json:"Title"`
				} `json:"Titles"`
				Texts []struct {
					Text string `json:"Text"`
				} `json:"Texts"`
				Href       *string `json:"Href"`
				BusinessID *int64  `json:"BusinessId"`
			} `json:"ResponsiveAd"`
		} `json:"Ads"`
		LimitedBy *int64 `json:"LimitedBy"`
	}
	params := map[string]any{"SelectionCriteria": map[string]any{"CampaignIds": []int64{campaignID}}, "FieldNames": []string{"Id", "CampaignId", "AdGroupId", "Type", "Status", "State"}, "TextAdFieldNames": []string{"Title", "Title2", "Text", "Href", "BusinessId"}, "Page": map[string]any{"Limit": 1000}}
	if c.unified {
		params["ResponsiveAdFieldNames"] = []string{"Titles", "Texts", "Href", "BusinessId"}
	}
	if err := c.call(ctx, "ads", token, login, map[string]any{"method": "get", "params": params}, &result); err != nil {
		return ExternalCampaignDetail{}, err
	}
	if result.Ads == nil {
		return ExternalCampaignDetail{}, &Error{Code: "invalid_external_ad_response"}
	}
	if result.LimitedBy != nil || len(result.Ads) > 1000 {
		return ExternalCampaignDetail{}, &Error{Code: "external_campaign_too_large"}
	}
	seen := map[int64]bool{}
	for _, item := range result.Ads {
		if item.ID <= 0 || item.CampaignID != campaignID || item.AdGroupID <= 0 || seen[item.ID] {
			return ExternalCampaignDetail{}, &Error{Code: "invalid_external_ad_response"}
		}
		if (item.Type == "TEXT_AD" && item.TextAd == nil) || (item.Type == "RESPONSIVE_AD" && item.ResponsiveAd == nil) {
			return ExternalCampaignDetail{}, &Error{Code: "invalid_external_ad_response"}
		}
		seen[item.ID] = true
		ad := ExternalAd{ID: item.ID, CampaignID: campaignID, AdGroupID: item.AdGroupID, Type: item.Type, Status: item.Status, State: item.State, Titles: []string{}, Texts: []string{}}
		if item.TextAd != nil && item.Type == "TEXT_AD" {
			ad.Title, ad.Title2, ad.Text, ad.Href, ad.BusinessID = item.TextAd.Title, item.TextAd.Title2, item.TextAd.Text, item.TextAd.Href, item.TextAd.BusinessID
		}
		if item.ResponsiveAd != nil && item.Type == "RESPONSIVE_AD" {
			ad.Href, ad.BusinessID = item.ResponsiveAd.Href, item.ResponsiveAd.BusinessID
			for _, title := range item.ResponsiveAd.Titles {
				ad.Titles = append(ad.Titles, title.Title)
			}
			for _, text := range item.ResponsiveAd.Texts {
				ad.Texts = append(ad.Texts, text.Text)
			}
		}
		detail.Ads = append(detail.Ads, ad)
	}
	// Stable provider ordering makes hashes independent of response ordering.
	sortExternalAds(detail.Ads)
	return detail, nil
}

func sortExternalAds(ads []ExternalAd) {
	sort.Slice(ads, func(i, j int) bool { return ads[i].ID < ads[j].ID })
}

// ExternalAdChanges uses pointers for omission and an explicit Title2Set for
// the provider's nullable second headline. Href clearing is out of scope.
type ExternalAdChanges struct {
	Title     *string   `json:"title,omitempty"`
	Title2    *string   `json:"title2,omitempty"`
	Title2Set bool      `json:"title2_set,omitempty"`
	Text      *string   `json:"text,omitempty"`
	Titles    *[]string `json:"titles,omitempty"`
	Texts     *[]string `json:"texts,omitempty"`
	Href      *string   `json:"href,omitempty"`
}

func ValidateExternalCopy(value string, field string) error {
	if strings.TrimSpace(value) == "" || utf8.RuneCountInString(value) > 200 {
		return errors.New("ad copy must contain 1 to 200 characters")
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return errors.New("ad copy must not contain control characters")
		}
	}
	max, word, narrowAllowed := 56, 22, 0
	if field == "text" {
		max, word, narrowAllowed = 81, 23, 15
	}
	if field == "title2" {
		max, narrowAllowed = 30, 15
	}
	length, narrow := 0, 0
	for _, r := range value {
		if r == '#' {
			continue
		}
		if narrowAllowed > 0 && strings.ContainsRune("!,.;:\"", r) {
			narrow++
			continue
		}
		length++
	}
	if length > max || narrow > narrowAllowed {
		return errors.New("ad copy exceeds the provider field limit")
	}
	for _, part := range strings.Fields(value) {
		if utf8.RuneCountInString(strings.ReplaceAll(part, "#", "")) > word {
			return errors.New("ad copy contains an oversized word")
		}
	}
	return nil
}

func (p ExternalAdChanges) Validate() error {
	if p.Title == nil && !p.Title2Set && p.Text == nil && p.Titles == nil && p.Texts == nil && p.Href == nil {
		return errors.New("at least one editable field is required")
	}
	for field, value := range map[string]*string{"title": p.Title, "title2": p.Title2, "text": p.Text} {
		if value != nil {
			if err := ValidateExternalCopy(*value, field); err != nil {
				return err
			}
		}
	}
	for field, values := range map[string]*[]string{"title": p.Titles, "text": p.Texts} {
		if values != nil {
			max := 7
			if field == "text" {
				max = 3
			}
			if len(*values) < 1 || len(*values) > max {
				return errors.New("ad copy array exceeds the provider limit")
			}
			for _, value := range *values {
				if err := ValidateExternalCopy(value, field); err != nil {
					return err
				}
			}
		}
	}
	if p.Href != nil {
		u, err := url.Parse(*p.Href)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || utf8.RuneCountInString(*p.Href) > 1024 {
			return errors.New("href must be an absolute HTTPS URL without credentials or fragment")
		}
	}
	return nil
}

func (p ExternalAdChanges) Merge(ad ExternalAd) (ExternalAd, error) {
	if err := p.Validate(); err != nil {
		return ad, err
	}
	if ad.State == "ARCHIVED" {
		return ad, errors.New("archived ads cannot be edited")
	}
	switch ad.Type {
	case "TEXT_AD":
		if p.Titles != nil || p.Texts != nil {
			return ad, errors.New("array fields require a responsive ad")
		}
		if p.Title != nil {
			ad.Title = *p.Title
		}
		if p.Title2Set {
			ad.Title2 = p.Title2
		}
		if p.Text != nil {
			ad.Text = *p.Text
		}
	case "RESPONSIVE_AD":
		if p.Title != nil || p.Title2Set || p.Text != nil {
			return ad, errors.New("scalar fields require a text ad")
		}
		if p.Titles != nil {
			ad.Titles = append([]string(nil), (*p.Titles)...)
		}
		if p.Texts != nil {
			ad.Texts = append([]string(nil), (*p.Texts)...)
		}
		if len(ad.Titles) < 1 || len(ad.Titles) > 7 || len(ad.Texts) < 1 || len(ad.Texts) > 3 {
			return ad, errors.New("responsive ad peers are unavailable")
		}
	default:
		return ad, errors.New("this ad type is read-only")
	}
	if p.Href != nil {
		value := *p.Href
		ad.Href = &value
	}
	if ad.Type == "RESPONSIVE_AD" && ad.Href == nil && (ad.BusinessID == nil || *ad.BusinessID <= 0) {
		return ad, errors.New("responsive ad destination is unavailable")
	}
	return ad, nil
}

func (c *Client) UpdateExternalCampaignName(ctx context.Context, token, login string, id int64, name string) error {
	if id <= 0 || strings.TrimSpace(name) == "" || utf8.RuneCountInString(name) > 255 {
		return &Error{Code: "invalid_external_campaign_name"}
	}
	return c.externalUpdate(ctx, "campaigns", "Campaigns", "UpdateResults", token, login, id, map[string]any{"Id": id, "Name": name})
}

func (c *Client) UpdateExternalAd(ctx context.Context, token, login string, current ExternalAd, patch ExternalAdChanges) error {
	merged, err := patch.Merge(current)
	if err != nil {
		return err
	}
	fields := map[string]any{}
	typeName := "TextAd"
	if current.Type == "TEXT_AD" {
		if patch.Title != nil {
			fields["Title"] = merged.Title
		}
		if patch.Title2Set {
			fields["Title2"] = merged.Title2
		}
		if patch.Text != nil {
			fields["Text"] = merged.Text
		}
		if patch.Href != nil {
			fields["Href"] = merged.Href
		}
	} else {
		typeName = "ResponsiveAd"
		fields["Titles"], fields["Texts"] = merged.Titles, merged.Texts
		if merged.Href != nil {
			fields["Href"] = *merged.Href
		}
		if merged.BusinessID != nil {
			fields["BusinessId"] = *merged.BusinessID
		}
	}
	return c.externalUpdate(ctx, "ads", "Ads", "UpdateResults", token, login, current.ID, map[string]any{"Id": current.ID, typeName: fields})
}

func (c *Client) externalUpdate(ctx context.Context, service, collection, resultName, token, login string, id int64, item map[string]any) error {
	var response map[string]json.RawMessage
	if err := c.call(ctx, service, token, login, map[string]any{"method": "update", "params": map[string]any{collection: []any{item}}}, &response); err != nil {
		return err
	}
	var results []struct {
		ID     int64           `json:"Id"`
		Errors []ProviderIssue `json:"Errors"`
	}
	if err := json.Unmarshal(response[resultName], &results); err != nil || len(results) != 1 {
		return &Error{Code: "invalid_external_update_response"}
	}
	if len(results[0].Errors) > 0 {
		return &PartialMutationError{Operation: service + ".update", Results: []MutationResult{{Errors: results[0].Errors}}}
	}
	if results[0].ID != id {
		return &Error{Code: "invalid_external_update_response"}
	}
	return nil
}
