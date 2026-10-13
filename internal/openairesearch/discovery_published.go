package openairesearch

import (
	"context"
	"errors"
	"html"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"maxpilot/backend/internal/contentdedup"
)

// The app binds a workspace/channel-scoped matcher backed by its persistent
// publication index. The model and HTTP caller cannot provide this authority.
type DiscoveryPublishedCandidate struct {
	Title     string
	Text      string
	SourceURL string
}

type DiscoveryPublishedReference struct {
	Title      string
	Text       string
	SourceURLs []string
}

type DiscoveryPublishedMatcher func(context.Context, []DiscoveryPublishedCandidate) ([]bool, error)

func CanonicalDiscoverySourceURL(raw string) string {
	return contentdedup.CanonicalSourceURL(raw)
}

func (c *Client) matchPublishedCandidates(ctx context.Context, request DiscoverContentRequest, candidates []DiscoveryPublishedCandidate) ([]bool, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if request.PublishedMatcher == nil || len(candidates) == 0 {
		return make([]bool, len(candidates)), nil
	}
	if len(candidates) > 20 {
		return nil, &Error{Provider: c.provider, Code: "publication_history_unavailable", Message: "Publication identity check exceeds its candidate bound"}
	}
	matched, err := request.PublishedMatcher(ctx, candidates)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, &Error{Provider: c.provider, Code: "publication_history_unavailable", Message: "Channel publication identity check is unavailable"}
	}
	if len(matched) != len(candidates) {
		return nil, &Error{Provider: c.provider, Code: "publication_history_unavailable", Message: "Channel publication identity check returned an invalid result"}
	}
	return matched, nil
}

func (c *Client) filterPublishedCards(ctx context.Context, request DiscoverContentRequest, cards []ContentCard) ([]ContentCard, bool, error) {
	candidates := make([]DiscoveryPublishedCandidate, len(cards))
	for index, card := range cards {
		candidates[index] = DiscoveryPublishedCandidate{Title: card.Title, Text: card.Summary + "\n" + card.Draft.Content, SourceURL: card.Source.URL}
	}
	matched, err := c.matchPublishedCandidates(ctx, request, candidates)
	if err != nil {
		return nil, false, err
	}
	filtered := make([]ContentCard, 0, len(cards))
	publishedRejected := false
	for index, card := range cards {
		if matched[index] {
			publishedRejected = true
			continue
		}
		duplicate := false
		for _, accepted := range filtered {
			if IsPublishedDiscoveryDuplicate(candidates[index], DiscoveryPublishedReference{Title: accepted.Title, Text: accepted.Summary + "\n" + accepted.Draft.Content, SourceURLs: []string{accepted.Source.URL}}) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			filtered = append(filtered, card)
		}
	}
	return filtered, publishedRejected, nil
}

// IsPublishedDiscoveryDuplicate compares indexed, proven publication snapshots
// with one candidate. Its caller supplies a bounded database shortlist; channel
// history is never loaded wholesale or sent to the model. Generic topic words
// cannot establish event identity, while original source URLs remain decisive.
func IsPublishedDiscoveryDuplicate(candidate DiscoveryPublishedCandidate, reference DiscoveryPublishedReference) bool {
	if source := contentdedup.CanonicalSourceURL(candidate.SourceURL); source != "" {
		for _, published := range reference.SourceURLs {
			if source == contentdedup.CanonicalSourceURL(published) {
				return true
			}
		}
	}
	candidateText := publicationVisibleText(truncateRunes(candidate.Text, 5000))
	publishedText := publicationVisibleText(truncateRunes(reference.Text, 4000))
	if utf8.RuneCountInString(candidateText) >= 40 && candidateText == publishedText {
		return true
	}
	candidateTitle := candidate.Title
	if strings.TrimSpace(candidateTitle) == "" {
		candidateTitle = publicationLead(candidateText)
	}
	publicationTitle := reference.Title
	if strings.TrimSpace(publicationTitle) == "" {
		publicationTitle = publicationLead(publishedText)
	}
	candidateTitleTokens, publishedTitleTokens := publicationTokens(candidateTitle), publicationTokens(publicationTitle)
	candidateBodyTokens, publishedBodyTokens := publicationTokens(candidateText), publicationTokens(publishedText)
	if publicationTokenOverlap(candidateTitleTokens, publishedTitleTokens, 3, 0.66, 0.45) &&
		publicationTokenOverlap(candidateBodyTokens, publishedBodyTokens, 4, 0.45, 0.3) {
		return true
	}
	// A shared category or two topic words cannot establish identity. Even
	// matching titles need corroborating body details; a heavily paraphrased
	// title instead needs a substantial body overlap with factual anchors.
	return publicationTokenOverlap(candidateBodyTokens, publishedBodyTokens, 10, 0.7, 0.5) ||
		(publicationTokenOverlap(candidateTitleTokens, publishedTitleTokens, 1, 0, 0) &&
			publicationTokenOverlap(candidateBodyTokens, publishedBodyTokens, 6, 0.6, 0.4))
}

var publicationURLPattern = regexp.MustCompile(`(?i)(?:https?://|www\.)[^\s<>]+`)
var publicationMarkupPattern = regexp.MustCompile(`<[^>]*>|[*_~+#^\x60]`)

func publicationVisibleText(value string) string {
	value = publicationURLPattern.ReplaceAllString(html.UnescapeString(value), " ")
	value = publicationMarkupPattern.ReplaceAllString(value, " ")
	return strings.Join(strings.Fields(strings.ToLower(strings.ReplaceAll(value, "ё", "е"))), " ")
}

func publicationLead(value string) string {
	if end := strings.IndexAny(value, ".!?\n"); end >= 0 {
		value = value[:end]
	}
	return truncateRunes(value, 200)
}

var publicationStopWords = func() map[string]bool {
	words := "российск росс россии учен науч исслед разработ созд изобрет откры нов новы канал публикац материал стать новост сообщ рассказ показ предлож помога позволя использ технолог устройств систем метод способ проект команд специалист эксперт инженер компан организац человек котор такой свой наш ваш этого этот году год сегодня теперь также более всего очень почему потому после перед между можно будет будут может новый новые нашли получили начал начал появил сделал делать сделать работ работа возможн первый впервые российские ученые россии исследователи разработали создали изобрели открыли новое новости исследование сообщили рассказали показали предложили помогает позволяет использовать технология устройство система метод способ проект команда специалисты эксперты инженеры компания организация человек который такой свой наши ваши это этом года году сегодня теперь также более всего очень почему потому после перед между можно будет будут может новый новые нашли получили начали появился сделал сделать работа возможность первый впервые препарат лекарств леч снижа повыш эффективн"
	result := make(map[string]bool)
	for _, word := range strings.Fields(words) {
		result[publicationStem(word)] = true
	}
	return result
}()

func publicationTokens(value string) []string {
	value = publicationVisibleText(value)
	words := strings.FieldsFunc(value, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	seen := make(map[string]bool, len(words))
	for _, word := range words {
		stem := publicationStem(word)
		if utf8.RuneCountInString(stem) >= 4 && !publicationStopWords[stem] {
			seen[stem] = true
		}
	}
	result := make([]string, 0, len(seen))
	for word := range seen {
		result = append(result, word)
	}
	sort.Strings(result)
	return result
}

func publicationStem(word string) string {
	word = strings.ToLower(strings.ReplaceAll(word, "ё", "е"))
	for _, suffix := range []string{"иями", "ями", "ами", "его", "ого", "ему", "ому", "ыми", "ими", "иях", "ах", "ях", "ая", "яя", "ое", "ее", "ые", "ие", "ый", "ий", "ой", "ую", "юю", "ов", "ев", "ам", "ям", "ом", "ем", "ы", "и", "а", "я", "у", "ю", "е"} {
		if strings.HasSuffix(word, suffix) {
			stem := strings.TrimSuffix(word, suffix)
			if utf8.RuneCountInString(stem) >= 4 {
				word = stem
				break
			}
		}
	}
	// Normalize the common fleeting vowel in -ок/-ка/-ки noun forms after
	// case endings, without a channel-specific list of subjects or events.
	if strings.HasSuffix(word, "ок") && utf8.RuneCountInString(word) >= 5 {
		word = strings.TrimSuffix(word, "ок") + "к"
	}
	for _, prefix := range []string{"авиа", "био", "электро", "радио", "нано"} {
		if strings.HasPrefix(word, prefix) && utf8.RuneCountInString(strings.TrimPrefix(word, prefix)) >= 5 {
			word = strings.TrimPrefix(word, prefix)
			break
		}
	}
	return word
}

func publicationTokenOverlap(left, right []string, minimum int, minCoverage, maxCoverage float64) bool {
	if len(left) < minimum || len(right) < minimum {
		return false
	}
	shared, distinctive := 0, 0
	for i, j := 0, 0; i < len(left) && j < len(right); {
		switch strings.Compare(left[i], right[j]) {
		case -1:
			i++
		case 1:
			j++
		default:
			shared++
			if strings.IndexFunc(left[i], unicode.IsLetter) >= 0 {
				distinctive++
			}
			i++
			j++
		}
	}
	return shared >= minimum && distinctive >= min(2, minimum) && float64(shared)/float64(min(len(left), len(right))) >= minCoverage && float64(shared)/float64(max(len(left), len(right))) >= maxCoverage
}
