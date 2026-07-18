package email

import (
	"strings"
	"testing"
)

const (
	testAppURL  = "https://app.maxposty.example/app/#/posts"
	testSiteURL = "https://maxposty.example"
)

func TestRenderWelcomePersonalised(t *testing.T) {
	t.Parallel()
	htmlBody, textBody, err := Render(WelcomeData{DisplayName: "  Иван  ", AppURL: testAppURL, SiteURL: testSiteURL})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if htmlBody == "" || textBody == "" {
		t.Fatalf("expected both bodies, got html=%d text=%d bytes", len(htmlBody), len(textBody))
	}
	for name, body := range map[string]string{"html": htmlBody, "text": textBody} {
		if !strings.Contains(body, "Добро пожаловать в MaxPosty!") {
			t.Errorf("%s body missing heading", name)
		}
		if !strings.Contains(body, "Привет, Иван!") {
			t.Errorf("%s body missing trimmed personalised greeting", name)
		}
		if strings.Contains(body, "{{") || strings.Contains(body, "}}") {
			t.Errorf("%s body has unresolved template markers", name)
		}
		if !strings.Contains(body, testAppURL) {
			t.Errorf("%s body missing AppURL", name)
		}
	}
	if !strings.Contains(htmlBody, `href="`+testAppURL+`"`) {
		t.Errorf("HTML CTA does not link to AppURL")
	}
}

func TestRenderWelcomeWithoutNameFallsBack(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"", "   "} {
		htmlBody, textBody, err := Render(WelcomeData{DisplayName: name, AppURL: testAppURL, SiteURL: testSiteURL})
		if err != nil {
			t.Fatalf("Render(%q): %v", name, err)
		}
		for label, body := range map[string]string{"html": htmlBody, "text": textBody} {
			if !strings.Contains(body, "Привет!") {
				t.Errorf("%s body missing nameless greeting for name %q", label, name)
			}
			if strings.Contains(body, "Привет, ") {
				t.Errorf("%s body rendered a dangling name greeting for name %q", label, name)
			}
		}
	}
}

func TestRenderWelcomeEscapesHTMLName(t *testing.T) {
	t.Parallel()
	htmlBody, _, err := Render(WelcomeData{DisplayName: "<script>", AppURL: testAppURL, SiteURL: testSiteURL})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(htmlBody, "<script>") {
		t.Errorf("HTML body did not escape the display name")
	}
}
