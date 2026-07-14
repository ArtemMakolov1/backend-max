package store

import (
	"strings"
	"testing"
)

func TestValidateLinkButtonsDraftAllowsIncompleteAutosave(t *testing.T) {
	t.Parallel()
	buttons := []LinkButton{{Text: "Подробнее", URL: "https://"}, {}}
	if err := ValidateLinkButtonsDraft(buttons); err != nil {
		t.Fatalf("ValidateLinkButtonsDraft() error = %v", err)
	}
	if err := ValidateLinkButtonsForPublish(buttons); err == nil {
		t.Fatal("ValidateLinkButtonsForPublish() accepted incomplete buttons")
	}
}

func TestValidateLinkButtonsDraftEnforcesCountAndSize(t *testing.T) {
	t.Parallel()
	if err := ValidateLinkButtonsDraft(make([]LinkButton, MaxPostLinkButtons+1)); err == nil {
		t.Fatal("ValidateLinkButtonsDraft() accepted too many buttons")
	}
	if err := ValidateLinkButtonsDraft([]LinkButton{{Text: strings.Repeat("я", MaxLinkButtonText+1)}}); err == nil {
		t.Fatal("ValidateLinkButtonsDraft() accepted oversized text")
	}
	if err := ValidateLinkButtonsDraft([]LinkButton{{URL: "https://example.com/" + strings.Repeat("a", MaxLinkButtonURL)}}); err == nil {
		t.Fatal("ValidateLinkButtonsDraft() accepted oversized URL")
	}
}

func TestValidateLinkButtonsForPublishRequiresSafeHTTPSURLs(t *testing.T) {
	t.Parallel()
	valid := []LinkButton{
		{Text: "Сайт", URL: "https://example.com/path?q=1"},
		{Text: "  Каталог  ", URL: "  HTTPS://shop.example.com/catalog  "},
	}
	if err := ValidateLinkButtonsForPublish(valid); err != nil {
		t.Fatalf("ValidateLinkButtonsForPublish() error = %v", err)
	}

	invalid := []LinkButton{
		{Text: "", URL: "https://example.com"},
		{Text: "Сайт", URL: ""},
		{Text: "Сайт", URL: "/relative"},
		{Text: "Сайт", URL: "http://example.com"},
		{Text: "Сайт", URL: "https://user@example.com"},
		{Text: "Сайт", URL: "https:example.com"},
	}
	for _, button := range invalid {
		button := button
		t.Run(button.Text+button.URL, func(t *testing.T) {
			t.Parallel()
			if err := ValidateLinkButtonsForPublish([]LinkButton{button}); err == nil {
				t.Fatalf("ValidateLinkButtonsForPublish(%#v) succeeded", button)
			}
		})
	}
}
