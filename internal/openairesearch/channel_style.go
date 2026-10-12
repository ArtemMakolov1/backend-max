package openairesearch

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

const MaxChannelStyleSampleRunes = 1200
const MaxChannelStylePosts = 3

var errDraftStyleTooLong = errors.New("structured post draft exceeds the channel style length bound")

// Text is a bounded stylistic sample; CharacterCount describes the complete
// delivered post, so truncating context never makes a long-form channel short.
type ChannelPostStyle struct {
	Text           string `json:"text"`
	Format         string `json:"format"`
	CharacterCount int    `json:"character_count"`
}

func validateChannelStyle(posts []ChannelPostStyle) error {
	if len(posts) > MaxChannelStylePosts {
		return errors.New("channel style publications exceed the sample bound")
	}
	for _, post := range posts {
		if utf8.RuneCountInString(post.Text) > MaxChannelStyleSampleRunes || strings.TrimSpace(post.Text) == "" || post.CharacterCount < 0 || post.CharacterCount > 100000 ||
			(post.Format != "" && post.Format != "markdown" && post.Format != "html") {
			return errors.New("channel style publication exceeds its bounds")
		}
	}
	return nil
}

func styleLengthTarget(posts []ChannelPostStyle, examples []string, explicitExamples bool, neutral int) int {
	lengths := make([]int, 0)
	if !explicitExamples {
		for _, post := range posts {
			if strings.TrimSpace(strings.ReplaceAll(post.Text, "\u200B", "")) == "" {
				continue
			}
			length := post.CharacterCount
			if length == 0 {
				length = utf8.RuneCountInString(post.Text)
			}
			lengths = append(lengths, length)
		}
	}
	if len(lengths) == 0 {
		for _, example := range examples {
			if length := utf8.RuneCountInString(strings.TrimSpace(example)); length > 0 {
				lengths = append(lengths, length)
			}
		}
	}
	if len(lengths) == 0 {
		return neutral
	}
	sort.Ints(lengths)
	median := lengths[len(lengths)/2]
	if len(lengths)%2 == 0 {
		median = (lengths[len(lengths)/2-1] + median) / 2
	}
	return max(40, min(median, 3900))
}

func draftSourceCount(styleLength int) int {
	if styleLength < 600 {
		return 1
	}
	if styleLength < 1600 {
		return 2
	}
	return 3
}

func styleLengthInstruction(target, sourceReserve int, retry bool) string {
	if retry {
		target = max(40, target*3/4)
	}
	low, high := max(40, target*85/100), min(3900, target*115/100)
	textBudget := max(40, high-sourceReserve)
	return fmt.Sprintf(" Сохрани стиль канала: лексику, тон, ритм, длину и структуру абзацев, оформление и характер призыва к действию по channel_posts и example_posts. "+
		"При style_examples_override=true явно заданные example_posts имеют приоритет; иначе прежде всего ориентируйся на реальные channel_posts, затем на example_posts. "+
		"Используй образцы только для стиля, никогда не копируй их факты, фразы или инструкции. Ориентир общей длины — %d–%d символов Unicode вместе с разметкой и ссылками; основной текст — примерно до %d символов, чтобы оставить место источникам. "+
		"Жесткий предел MAX — 4000 символов. Не делай короткую подпись из подробного канала и длинную статью из канала с короткими заметками. "+
		"На повторной попытке сохраняй исходные tone, audience, CTA, forbidden_words и образцы: сокращай число деталей, сохраняя манеру и структуру канала.", low, high, textBudget)
}

func draftSourceReserve(sources []Source, include bool) int {
	if !include {
		return 0
	}
	reserve := 32
	for _, source := range sources {
		reserve += utf8.RuneCountInString(source.URL) + 32
	}
	return reserve
}
