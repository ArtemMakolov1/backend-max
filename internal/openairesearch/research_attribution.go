package openairesearch

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"sort"
	"strings"
	"unicode/utf8"
)

type draftSourceReference struct {
	SourceID string `json:"source_id"`
	Title    string `json:"title"`
}

func draftSourceEvidence(sources []Source) []draftSourceReference {
	evidence := make([]draftSourceReference, 0, len(sources))
	for i, source := range sources {
		evidence = append(evidence, draftSourceReference{SourceID: fmt.Sprintf("s%d", i+1), Title: source.Title})
	}
	return evidence
}

// Reserve the longest possible selected targets, rather than guessing which
// source the model will use. The final selected block is still validated.
func longestAttributionSources(sources []Source, limit int) []Source {
	byLength := append([]Source(nil), sources...)
	sort.SliceStable(byLength, func(i, j int) bool {
		return utf8.RuneCountInString(byLength[i].URL) > utf8.RuneCountInString(byLength[j].URL)
	})
	return limitSources(byLength, limit)
}

func addGroundedDraftSchema(schema map[string]any, sources []Source, limit int) {
	ids := make([]string, 0, len(sources))
	for i := range sources {
		ids = append(ids, fmt.Sprintf("s%d", i+1))
	}
	items := map[string]any{"type": "string"}
	if len(ids) != 0 {
		items["enum"] = ids
	}
	minItems := 0
	if limit > 0 {
		minItems = 1
	}
	properties := schema["properties"].(map[string]any)
	properties["content"].(map[string]any)["maxLength"] = maxPostContentRunes
	properties["source_ids"] = map[string]any{"type": "array", "minItems": minItems, "maxItems": limit, "items": items}
	schema["required"] = append(schema["required"].([]string), "source_ids")
}

func decodeGroundedDraft(raw, format string, sources []Source, limit int) (Draft, []Source, error) {
	var result struct {
		Draft
		SourceIDs json.RawMessage `json:"source_ids"`
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return Draft{}, nil, fmt.Errorf("decode grounded post draft: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Draft{}, nil, errors.New("grounded post draft must contain one JSON value")
	}
	var ids []string
	if len(result.SourceIDs) == 0 || json.Unmarshal(result.SourceIDs, &ids) != nil || ids == nil || len(ids) > limit || (limit > 0 && len(ids) == 0) {
		return Draft{}, nil, errors.New("grounded post draft must declare bounded source IDs")
	}
	byID := make(map[string]Source, len(sources))
	for i, source := range sources {
		byID[fmt.Sprintf("s%d", i+1)] = source
	}
	selected := make([]Source, 0, len(ids))
	for _, id := range ids {
		source, exists := byID[id]
		if !exists {
			return Draft{}, nil, errors.New("grounded post draft cited a source not used by research")
		}
		selected = append(selected, source)
	}
	// Reuse the same strict content/format/MAX policy as the public draft type,
	// keeping provider-only citation fields out of public API responses.
	draftJSON, _ := json.Marshal(result.Draft)
	draft, err := decodeDraft(string(draftJSON), format)
	return draft, deduplicateSources(selected), err
}

// Grounded reports contain citations added by decodeExternalResearch, so only
// those exact server-built suffixes are replaced when preparing draft context.
func reportWithSourceIDs(report string, sources []Source) string {
	for i, source := range sources {
		citation := " [" + escapeMarkdownLabel(source.Title) + "](<" + source.URL + ">)"
		report = strings.ReplaceAll(report, citation, fmt.Sprintf(" [s%d]", i+1))
	}
	return report
}

func appendResearchSources(draft Draft, sources []Source) (Draft, error) {
	for i, source := range sources {
		label := "Источник"
		if len(sources) > 1 {
			label = fmt.Sprintf("Источник %d", i+1)
		}
		if draft.Format == "markdown" {
			target := strings.NewReplacer("(", "%28", ")", "%29", "[", "%5B", "]", "%5D", "<", "%3C", ">", "%3E", "\\", "%5C", " ", "%20", "\"", "%22", "'", "%27").Replace(source.URL)
			draft.Content += "\n\n[" + label + "](" + target + ")"
		} else {
			draft.Content += "\n\n<a href=\"" + html.EscapeString(source.URL) + "\">" + label + "</a>"
		}
	}
	if utf8.RuneCountInString(draft.Content) > maxPostContentRunes {
		return Draft{}, errDraftContentTooLong
	}
	var err error
	if draft.Format == "markdown" {
		err = validateMAXMarkdown(draft.Content)
	} else {
		err = validateMAXHTML(draft.Content)
	}
	if err != nil {
		return Draft{}, errors.New("research source cannot be safely formatted for MAX")
	}
	return draft, nil
}
