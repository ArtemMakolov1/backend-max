package email

import (
	"bytes"
	_ "embed"
	"fmt"
	htmltemplate "html/template"
	"strings"
	texttemplate "text/template"
)

//go:embed template_welcome.html
var welcomeHTML string

//go:embed template_welcome.txt
var welcomeText string

// welcomeSubject is the transactional subject line shared by both bodies.
const welcomeSubject = "Добро пожаловать в MaxPosty"

var (
	welcomeHTMLTemplate = htmltemplate.Must(htmltemplate.New("welcome.html").Parse(welcomeHTML))
	welcomeTextTemplate = texttemplate.Must(texttemplate.New("welcome.txt").Parse(welcomeText))
)

// WelcomeData is the template payload for the welcome email. AppURL is the CTA
// target (public base URL + /app/#/posts) and SiteURL is the public site shown
// in the footer.
type WelcomeData struct {
	DisplayName string
	AppURL      string
	SiteURL     string
}

// Render produces the HTML and plain-text bodies of the welcome email from the
// same data. An empty (or whitespace-only) DisplayName renders a nameless
// greeting ("Привет!") instead of a dangling "Привет, !".
func Render(data WelcomeData) (htmlBody string, textBody string, err error) {
	data.DisplayName = strings.TrimSpace(data.DisplayName)

	var htmlBuf bytes.Buffer
	if err := welcomeHTMLTemplate.Execute(&htmlBuf, data); err != nil {
		return "", "", fmt.Errorf("render welcome HTML body: %w", err)
	}
	var textBuf bytes.Buffer
	if err := welcomeTextTemplate.Execute(&textBuf, data); err != nil {
		return "", "", fmt.Errorf("render welcome text body: %w", err)
	}
	return htmlBuf.String(), textBuf.String(), nil
}
