package openairesearch

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDeepSeekDraftAttributionIsServerOwnedAndKeepsExactSignedURL(t *testing.T) {
	const sourceURL = "https://example.com/precise-slug?signature=abc%2Fdef&token=x%2By&part=2"
	sources := []Source{{Title: "Первичный [источник]", URL: sourceURL}}
	for _, format := range []string{"markdown", "html"} {
		for _, include := range []bool{true, false} {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var payload responsePayload
				if json.NewDecoder(r.Body).Decode(&payload) != nil {
					t.Fatal("invalid draft payload")
				}
				context := payload.Input[1].Content.(string)
				if strings.Contains(context, sourceURL) || !strings.Contains(context, "Проверенный факт. [s1]") || !strings.Contains(payload.Input[0].Content.(string), "Сервер сам добавит") {
					t.Error("model must write verified prose, while server owns source link targets")
				}
				var ids []string
				if include {
					ids = []string{"s1"}
				}
				writeDeepSeekText(t, w, groundedDraftJSON(t, Draft{Title: "Факт", Content: "Проверенный факт.", Format: format, ImagePrompt: "Editorial"}, ids))
			}))
			report := "Проверенный факт. [Первичный \\[источник\\]](<" + sourceURL + ">)"
			draft, err := mustDeepSeekClient(t, server).generatePostDraft(t.Context(), Request{Topic: "Тема поста", Tone: "Тон канала", Format: format, IncludeSources: include}, report, sources)
			server.Close()
			if err != nil || calls != 1 {
				t.Fatalf("server attribution failed: format=%s include=%t err=%v calls=%d", format, include, err, calls)
			}
			signature, err := canonicalContentSignature(draft.Content, format)
			if err != nil || (include && (len(signature.HiddenLinkTargets) != 1 || signature.HiddenLinkTargets[0] != sourceURL)) || (!include && len(signature.HiddenLinkTargets) != 0) {
				t.Fatalf("attribution changed the original URL or ignored include_sources: signature=%#v err=%v", signature, err)
			}
		}
	}
}

func TestDeepSeekDraftRetriesIfServerAttributionExceedsMAXAndPreservesChannelStyle(t *testing.T) {
	const sourceURL = "https://example.com/verified/report"
	source := Source{Title: "Официальный источник", URL: sourceURL}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var payload responsePayload
		if json.NewDecoder(r.Body).Decode(&payload) != nil {
			t.Fatal("invalid request")
		}
		if !strings.Contains(payload.Input[1].Content.(string), `"character_count":3900`) || !strings.Contains(payload.Input[1].Content.(string), "Призыв канала") {
			t.Error("attribution retry lost channel style and CTA")
		}
		content := strings.Repeat("я", 3980)
		if calls == 2 {
			content = strings.Repeat("я", 3500)
		}
		writeDeepSeekText(t, w, groundedDraftJSON(t, Draft{Title: "Пост", Content: content, Format: "markdown", ImagePrompt: "Editorial"}, []string{"s1"}))
	}))
	defer server.Close()
	draft, err := mustDeepSeekClient(t, server).generatePostDraft(t.Context(), Request{Topic: "Тема поста", Tone: "Тон канала", Format: "markdown", IncludeSources: true, CTA: "Призыв канала", ChannelPosts: []ChannelPostStyle{{Text: "Подробный пост", CharacterCount: 3900}}}, "Проверенный факт.", []Source{source})
	if err != nil || calls != 2 || !strings.Contains(draft.Content, sourceURL) {
		t.Fatalf("whole validated post did not retry after source exceeded MAX: err=%v calls=%d", err, calls)
	}
	_, err = appendResearchSources(Draft{Content: strings.Repeat("я", 3980), Format: "markdown"}, []Source{source})
	if !errors.Is(err, errDraftContentTooLong) {
		t.Fatal("source size overflow does not share the bounded retry condition")
	}
}

func TestLegacyOpenAIDoesNotReceiveDeepSeekTemperatureOrServerCitationChanges(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload responsePayload
		if json.NewDecoder(r.Body).Decode(&payload) != nil || payload.Temperature != nil {
			t.Error("legacy provider received a DeepSeek-specific sampling parameter")
		}
		if !strings.Contains(payload.Input[1].Content.(string), `"include_sources":true`) || !strings.Contains(payload.Input[1].Content.(string), "https://example.com/source") {
			t.Error("legacy source drafting behavior changed")
		}
		writeDeepSeekText(t, w, `{"title":"Пост","content":"Факт","format":"markdown","image_prompt":"Editorial"}`)
	}))
	defer server.Close()
	client, err := New(server.URL, "synthetic-openai-key", "gpt-6-luna", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	draft, err := client.generatePostDraft(t.Context(), Request{Topic: "Тема поста", Tone: "Тон", Format: "markdown", IncludeSources: true}, "Отчет", []Source{{Title: "Источник", URL: "https://example.com/source"}})
	if err != nil || draft.Content != "Факт" {
		t.Fatalf("legacy drafting result changed: %v", err)
	}
}

func TestDeepSeekDraftUsesDeclaredLateSourceInsteadOfUnrelatedFirstSources(t *testing.T) {
	sources := []Source{{Title: "Один", URL: "https://example.com/one"}, {Title: "Два", URL: "https://example.com/two"}, {Title: "Третий", URL: "https://example.com/exact-slug?token=x%2By&signature=a%2Fb"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload responsePayload
		if json.NewDecoder(r.Body).Decode(&payload) != nil {
			t.Fatal("invalid draft request")
		}
		properties := payload.Text.Format.Schema["properties"].(map[string]any)
		citation := properties["source_ids"].(map[string]any)
		ids := citation["items"].(map[string]any)["enum"].([]any)
		if citation["maxItems"] != float64(1) || len(ids) != 3 || ids[2] != "s3" || !strings.Contains(payload.Input[1].Content.(string), "Факт третьего источника. [s3]") || strings.Contains(payload.Input[1].Content.(string), sources[2].URL) {
			t.Error("short-channel draft must be able to select a later verified source without copying its URL")
		}
		writeDeepSeekText(t, w, groundedDraftJSON(t, Draft{Title: "Факт", Content: "Факт третьего источника.", Format: "markdown", ImagePrompt: "Editorial"}, []string{"s3"}))
	}))
	defer server.Close()
	report := "Факт третьего источника. [Третий](<" + sources[2].URL + ">)"
	draft, err := mustDeepSeekClient(t, server).generatePostDraft(t.Context(), Request{Topic: "Тема поста", Tone: "Тон канала", Format: "markdown", IncludeSources: true, ChannelPosts: []ChannelPostStyle{{Text: "Короткий пост", CharacterCount: 350}}}, report, sources)
	if err != nil || !strings.Contains(draft.Content, sources[2].URL) || strings.Contains(draft.Content, sources[0].URL) || strings.Contains(draft.Content, sources[1].URL) {
		t.Fatalf("server appended a source unrelated to declared draft evidence: draft=%#v err=%v", draft, err)
	}
}

func TestGroundedDraftSourceIDsAreStrictBoundedAndRemainInternal(t *testing.T) {
	sources := []Source{{Title: "Первый", URL: "https://example.com/one"}, {Title: "Второй", URL: "https://example.com/two"}}
	draft := Draft{Title: "Факт", Content: "Факт", Format: "markdown", ImagePrompt: "Editorial"}
	for _, test := range []struct {
		name  string
		raw   string
		limit int
	}{
		{name: "unknown ID", raw: groundedDraftJSON(t, draft, []string{"s99"}), limit: 1},
		{name: "URL instead of ID", raw: groundedDraftJSON(t, draft, []string{sources[0].URL}), limit: 1},
		{name: "missing evidence", raw: groundedDraftJSON(t, draft, nil), limit: 1},
		{name: "over reference cap", raw: groundedDraftJSON(t, draft, []string{"s1", "s2"}), limit: 1},
		{name: "user disabled sources", raw: groundedDraftJSON(t, draft, []string{"s1"}), limit: 0},
		{name: "missing field", raw: `{"title":"Факт","content":"Факт","format":"markdown","image_prompt":"Editorial"}`, limit: 1},
		{name: "null field", raw: `{"title":"Факт","content":"Факт","format":"markdown","image_prompt":"Editorial","source_ids":null}`, limit: 1},
		{name: "trailing JSON", raw: groundedDraftJSON(t, draft, []string{"s1"}) + "{}", limit: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := decodeGroundedDraft(test.raw, "markdown", sources, test.limit); err == nil {
				t.Fatal("unverified or malformed attribution was accepted")
			}
		})
	}
	decoded, selected, err := decodeGroundedDraft(groundedDraftJSON(t, draft, []string{"s2", "s2"}), "markdown", sources, 3)
	publicJSON, _ := json.Marshal(decoded)
	if err != nil || len(selected) != 1 || selected[0].URL != sources[1].URL || strings.Contains(string(publicJSON), "source_ids") {
		t.Fatal("source IDs leaked into public draft or duplicate references were not deduplicated")
	}
	longest := longestAttributionSources([]Source{{URL: "https://example.com/a"}, {URL: "https://example.com/" + strings.Repeat("a", 500)}, {URL: "https://example.com/bb"}}, 1)
	if len(longest) != 1 || len(longest[0].URL) < 500 {
		t.Fatal("draft length budget did not reserve the longest permitted source target")
	}
}
