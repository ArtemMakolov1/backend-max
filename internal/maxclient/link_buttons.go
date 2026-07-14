package maxclient

import (
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"
)

const (
	maxLinkButtons    = 3
	maxLinkButtonText = 128
	maxLinkButtonURL  = 2048
)

// LinkButton is rendered by MAX as a URL button below a message.
type LinkButton struct {
	Text string
	URL  string
}

type inlineKeyboardPayload struct {
	Buttons [][]inlineKeyboardButton `json:"buttons"`
}

type inlineKeyboardButton struct {
	Type string `json:"type"`
	Text string `json:"text"`
	URL  string `json:"url"`
}

func messageAttachments(imageTokens []string, buttons []LinkButton) (*[]attachment, error) {
	attachments, err := imageAttachments(imageTokens)
	if err != nil {
		return nil, err
	}
	if buttons == nil {
		return attachments, nil
	}
	if err := validateLinkButtons(buttons); err != nil {
		return nil, err
	}

	result := make([]attachment, 0, len(imageTokens)+1)
	if attachments != nil {
		result = append(result, (*attachments)...)
	}
	if len(buttons) > 0 {
		rows := make([][]inlineKeyboardButton, len(buttons))
		for i, button := range buttons {
			rows[i] = []inlineKeyboardButton{{
				Type: "link",
				Text: strings.TrimSpace(button.Text),
				URL:  strings.TrimSpace(button.URL),
			}}
		}
		result = append(result, attachment{
			Type:    "inline_keyboard",
			Payload: inlineKeyboardPayload{Buttons: rows},
		})
	}
	return &result, nil
}

func validateLinkButtons(buttons []LinkButton) error {
	if len(buttons) > maxLinkButtons {
		return fmt.Errorf("link buttons must contain no more than %d buttons", maxLinkButtons)
	}
	for i, button := range buttons {
		text := strings.TrimSpace(button.Text)
		if text == "" || utf8.RuneCountInString(button.Text) > maxLinkButtonText {
			return fmt.Errorf("link button %d text must contain 1-%d characters", i, maxLinkButtonText)
		}
		rawURL := strings.TrimSpace(button.URL)
		if rawURL == "" || utf8.RuneCountInString(button.URL) > maxLinkButtonURL {
			return fmt.Errorf("link button %d URL must contain 1-%d characters", i, maxLinkButtonURL)
		}
		parsed, err := url.Parse(rawURL)
		if err != nil || !parsed.IsAbs() || !strings.EqualFold(parsed.Scheme, "https") ||
			parsed.Host == "" || parsed.Hostname() == "" || parsed.User != nil || parsed.Opaque != "" {
			return fmt.Errorf("link button %d URL must be an absolute HTTPS URL without userinfo", i)
		}
	}
	return nil
}
