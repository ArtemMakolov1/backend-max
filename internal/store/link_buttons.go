package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"
)

const (
	MaxPostLinkButtons = 3
	MaxLinkButtonText  = 128
	MaxLinkButtonURL   = 2048
)

// LinkButton is a URL button shown below a MAX post.
type LinkButton struct {
	Text string `json:"text"`
	URL  string `json:"url"`
}

func (button *LinkButton) UnmarshalJSON(data []byte) error {
	var wire struct {
		Text *string `json:"text"`
		URL  *string `json:"url"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return fmt.Errorf("link button must be an object with text and url strings: %w", err)
	}
	if wire.Text == nil || wire.URL == nil {
		return errors.New("link button must contain text and url strings")
	}
	button.Text = *wire.Text
	button.URL = *wire.URL
	return nil
}

// ValidateLinkButtonsDraft keeps autosave friendly: an unfinished button may
// be persisted while the author is still typing. Shape, count and hard size
// limits are enforced immediately; publication uses the strict validator.
func ValidateLinkButtonsDraft(buttons []LinkButton) error {
	if len(buttons) > MaxPostLinkButtons {
		return fmt.Errorf("link_buttons must contain no more than %d buttons", MaxPostLinkButtons)
	}
	for i, button := range buttons {
		if utf8.RuneCountInString(button.Text) > MaxLinkButtonText {
			return fmt.Errorf("link_buttons[%d].text must not exceed %d characters", i, MaxLinkButtonText)
		}
		if utf8.RuneCountInString(button.URL) > MaxLinkButtonURL {
			return fmt.Errorf("link_buttons[%d].url must not exceed %d characters", i, MaxLinkButtonURL)
		}
	}
	return nil
}

// ValidateLinkButtonsForPublish rejects incomplete or unsafe buttons before a
// post enters the publication pipeline.
func ValidateLinkButtonsForPublish(buttons []LinkButton) error {
	if err := ValidateLinkButtonsDraft(buttons); err != nil {
		return err
	}
	for i, button := range buttons {
		text := strings.TrimSpace(button.Text)
		if text == "" {
			return fmt.Errorf("link_buttons[%d].text is required", i)
		}
		rawURL := strings.TrimSpace(button.URL)
		if rawURL == "" {
			return fmt.Errorf("link_buttons[%d].url is required", i)
		}
		parsed, err := url.Parse(rawURL)
		if err != nil || !parsed.IsAbs() || !strings.EqualFold(parsed.Scheme, "https") ||
			parsed.Host == "" || parsed.Hostname() == "" || parsed.User != nil || parsed.Opaque != "" {
			return fmt.Errorf("link_buttons[%d].url must be an absolute HTTPS URL without userinfo", i)
		}
	}
	return nil
}

func normalizeLinkButtons(buttons []LinkButton) []LinkButton {
	if buttons == nil {
		return []LinkButton{}
	}
	normalized := make([]LinkButton, len(buttons))
	for i, button := range buttons {
		normalized[i] = LinkButton{Text: strings.TrimSpace(button.Text), URL: strings.TrimSpace(button.URL)}
	}
	return normalized
}

func validateStoredLinkButtons(buttons []LinkButton) error {
	if err := ValidateLinkButtonsDraft(buttons); err != nil {
		return fmt.Errorf("invalid stored link buttons: %w", err)
	}
	return nil
}

func marshalLinkButtons(buttons []LinkButton) (string, error) {
	buttons = normalizeLinkButtons(buttons)
	if err := validateStoredLinkButtons(buttons); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(buttons)
	if err != nil {
		return "", fmt.Errorf("encode link_buttons: %w", err)
	}
	return string(encoded), nil
}

func unmarshalLinkButtons(encoded []byte) ([]LinkButton, error) {
	if value := strings.TrimSpace(string(encoded)); value == "" || !strings.HasPrefix(value, "[") {
		return nil, errors.New("link_buttons must be a JSON array")
	}
	var buttons []LinkButton
	if err := json.Unmarshal(encoded, &buttons); err != nil {
		return nil, fmt.Errorf("decode link_buttons: %w", err)
	}
	buttons = normalizeLinkButtons(buttons)
	if err := validateStoredLinkButtons(buttons); err != nil {
		return nil, err
	}
	return buttons, nil
}
