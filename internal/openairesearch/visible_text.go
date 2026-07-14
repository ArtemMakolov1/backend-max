package openairesearch

import (
	"errors"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var markdownVisibleLinePrefix = regexp.MustCompile(`(?m)^[ \t]{0,3}(?:#{1,6}[ \t]+|>[ \t]?)`)

type contentSignature struct {
	Visible           string
	HiddenLinkTargets []string
}

func (signature contentSignature) equal(other contentSignature) bool {
	if signature.Visible != other.Visible || len(signature.HiddenLinkTargets) != len(other.HiddenLinkTargets) {
		return false
	}
	for index := range signature.HiddenLinkTargets {
		if signature.HiddenLinkTargets[index] != other.HiddenLinkTargets[index] {
			return false
		}
	}
	return true
}

// canonicalVisibleText makes whitespace and supported presentation markup
// irrelevant while keeping every visible word, number, URL and punctuation
// mark. It is intentionally conservative: unsupported Markdown is left as
// visible text, so the model cannot silently delete it as if it were markup.
func canonicalVisibleText(content, format string) (string, error) {
	signature, err := canonicalContentSignature(content, format)
	if err != nil {
		return "", err
	}
	return signature.Visible, nil
}

func canonicalContentSignature(content, format string) (contentSignature, error) {
	var (
		visible     string
		hiddenLinks []string
		err         error
	)
	switch format {
	case "markdown":
		visible, hiddenLinks, err = extractMAXMarkdownContent(content)
	case "html":
		visible, hiddenLinks, err = extractMAXHTMLContent(content)
	default:
		err = errors.New("unsupported content format")
	}
	if err != nil {
		return contentSignature{}, err
	}
	canonical := strings.Join(strings.Fields(visible), " ")
	if canonical == "" {
		return contentSignature{}, errors.New("content has no visible text")
	}
	return contentSignature{Visible: canonical, HiddenLinkTargets: hiddenLinks}, nil
}

func extractMAXMarkdownContent(content string) (string, []string, error) {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = markdownVisibleLinePrefix.ReplaceAllString(content, "")
	content, hiddenLinks, err := stripMarkdownLinks(content)
	if err != nil {
		return "", nil, err
	}
	content = stripMarkdownMarkers(content)
	return content, hiddenLinks, nil
}

func stripMarkdownMarkers(content string) string {
	for _, marker := range []string{"**", "__", "~~", "++", "^^", "`", "*", "_"} {
		content = stripPairedMarkdownMarker(content, marker)
	}
	return content
}

func stripMarkdownLinks(content string) (string, []string, error) {
	var output strings.Builder
	hiddenLinks := make([]string, 0)
	for offset := 0; offset < len(content); {
		openRelative := strings.IndexByte(content[offset:], '[')
		if openRelative < 0 {
			output.WriteString(content[offset:])
			break
		}
		open := offset + openRelative
		if isMarkdownEscaped(content, open) {
			output.WriteString(content[offset : open+1])
			offset = open + 1
			continue
		}
		close := findUnescapedByte(content, ']', open+1)
		if close < 0 || close+1 >= len(content) || content[close+1] != '(' {
			output.WriteString(content[offset : open+1])
			offset = open + 1
			continue
		}
		end := findUnescapedByte(content, ')', close+2)
		if end < 0 || strings.ContainsAny(content[close+2:end], "\r\n") {
			output.WriteString(content[offset : open+1])
			offset = open + 1
			continue
		}
		destination := content[close+2 : end]
		if err := validateMAXLinkURL(destination); err != nil {
			return "", nil, err
		}
		label := content[open+1 : close]
		canonicalLabel := strings.Join(strings.Fields(stripMarkdownMarkers(label)), " ")
		destination = strings.TrimSpace(destination)
		if canonicalLabel != destination {
			hiddenLinks = append(hiddenLinks, destination)
		}
		output.WriteString(content[offset:open])
		output.WriteString(label)
		offset = end + 1
	}
	return output.String(), hiddenLinks, nil
}

func findUnescapedByte(value string, target byte, start int) int {
	for index := start; index < len(value); index++ {
		if value[index] == target && !isMarkdownEscaped(value, index) {
			return index
		}
	}
	return -1
}

func stripPairedMarkdownMarker(content, marker string) string {
	var output strings.Builder
	for offset := 0; offset < len(content); {
		open := findMarkdownMarker(content, marker, offset, true)
		if open < 0 {
			output.WriteString(content[offset:])
			break
		}
		close := findMarkdownMarker(content, marker, open+len(marker), false)
		if close < 0 {
			output.WriteString(content[offset:])
			break
		}
		output.WriteString(content[offset:open])
		output.WriteString(content[open+len(marker) : close])
		offset = close + len(marker)
	}
	return output.String()
}

func findMarkdownMarker(content, marker string, start int, opening bool) int {
	for start < len(content) {
		relative := strings.Index(content[start:], marker)
		if relative < 0 {
			return -1
		}
		index := start + relative
		if !isMarkdownEscaped(content, index) && markerBoundaryIsValid(content, marker, index, opening) {
			return index
		}
		start = index + len(marker)
	}
	return -1
}

func markerBoundaryIsValid(content, marker string, index int, opening bool) bool {
	if marker == "`" {
		return true
	}
	if opening {
		after := index + len(marker)
		if after >= len(content) {
			return false
		}
		next, _ := utf8.DecodeRuneInString(content[after:])
		if unicode.IsSpace(next) {
			return false
		}
		if index == 0 {
			return true
		}
		previous, _ := utf8.DecodeLastRuneInString(content[:index])
		return unicode.IsSpace(previous) || unicode.IsPunct(previous) || unicode.IsSymbol(previous)
	}
	if index == 0 {
		return false
	}
	previous, _ := utf8.DecodeLastRuneInString(content[:index])
	if unicode.IsSpace(previous) {
		return false
	}
	after := index + len(marker)
	if after == len(content) {
		return true
	}
	next, _ := utf8.DecodeRuneInString(content[after:])
	return unicode.IsSpace(next) || unicode.IsPunct(next) || unicode.IsSymbol(next)
}

func isMarkdownEscaped(value string, index int) bool {
	backslashes := 0
	for index--; index >= 0 && value[index] == '\\'; index-- {
		backslashes++
	}
	return backslashes%2 != 0
}
