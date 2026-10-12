package openairesearch

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChannelStyleUsesDeliveredLengthAndRespectsExplicitExamples(t *testing.T) {
	for _, test := range []struct {
		name     string
		posts    []ChannelPostStyle
		examples []string
		override bool
		want     int
	}{
		{name: "short channel", posts: []ChannelPostStyle{{Text: "Короткая заметка", CharacterCount: 320}}, want: 320},
		{name: "long channel with bounded snippet", posts: []ChannelPostStyle{{Text: strings.Repeat("я", 1200), CharacterCount: 3200}}, want: 3200},
		{name: "median excludes long outlier", posts: []ChannelPostStyle{{Text: "Один", CharacterCount: 280}, {Text: "Два", CharacterCount: 320}, {Text: "Три", CharacterCount: 3900}}, want: 320},
		{name: "channel ahead of workspace examples", posts: []ChannelPostStyle{{Text: "Длинный канал", CharacterCount: 3200}}, examples: []string{strings.Repeat("а", 120)}, want: 3200},
		{name: "explicit examples ahead of channel", posts: []ChannelPostStyle{{Text: "Длинный канал", CharacterCount: 3200}}, examples: []string{strings.Repeat("а", 120)}, override: true, want: 120},
		{name: "brand examples without channel history", examples: []string{strings.Repeat("а", 900)}, want: 900},
		{name: "neutral only without any examples", want: 2400},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := styleLengthTarget(test.posts, test.examples, test.override, 2400); got != test.want {
				t.Fatalf("style length=%d want%d", got, test.want)
			}
		})
	}
}

func TestDeepSeekDraftCarriesChannelStyleAndKeepsItOnOversizedRetry(t *testing.T) {
	for _, length := range []int{320, 3200} {
		calls := 0
		request := Request{Topic: "Тема поста", Tone: "Тон канала", Audience: "Аудитория канала", CTA: "Подписаться", ForbiddenWords: []string{"запрещено"}, ExamplePosts: []string{"Фирменный образец, его факты не копировать"}, ChannelPosts: []ChannelPostStyle{{Text: strings.Repeat("я", min(length, 1200)), Format: "markdown", CharacterCount: length}}, Format: "markdown"}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			var payload responsePayload
			if json.NewDecoder(r.Body).Decode(&payload) != nil {
				t.Fatal("invalid style request")
			}
			contextJSON := payload.Input[1].Content.(string)
			var context struct {
				ChannelPosts   []ChannelPostStyle `json:"channel_posts"`
				Tone           string             `json:"tone"`
				Audience       string             `json:"audience"`
				CTA            string             `json:"cta"`
				ExamplePosts   []string           `json:"example_posts"`
				ForbiddenWords []string           `json:"forbidden_words"`
			}
			if json.Unmarshal([]byte(contextJSON[strings.IndexByte(contextJSON, '{'):]), &context) != nil || len(context.ChannelPosts) != 1 || context.ChannelPosts[0].CharacterCount != length || context.Tone != request.Tone || context.Audience != request.Audience || context.CTA != request.CTA || len(context.ExamplePosts) != 1 || len(context.ForbiddenWords) != 1 {
				t.Fatal("retry dropped channel/style/brand context")
			}
			instruction := payload.Input[0].Content.(string)
			if strings.Contains(instruction, "2200–2600") || strings.Contains(instruction, "около 3600") || !strings.Contains(instruction, "никогда не копируй их факты") {
				t.Error("generic length overrode channel style or sample facts became authority")
			}
			if calls == 2 && !strings.Contains(instruction, "сохрани стиль и все бренд-правила") {
				t.Error("retry did not preserve channel style")
			}
			content := "Короткий корректный пост"
			if calls == 1 {
				content = strings.Repeat("я", 4001)
			}
			raw := groundedDraftJSON(t, Draft{Title: "Пост", Content: content, Format: "markdown", ImagePrompt: "Editorial"}, nil)
			writeDeepSeekText(t, w, raw)
		}))
		_, err := mustDeepSeekClient(t, server).generatePostDraft(t.Context(), request, "Проверенный факт.", nil)
		server.Close()
		if err != nil || calls != 2 {
			t.Fatalf("style-preserving retry failed: %v calls=%d", err, calls)
		}
	}
}

func TestQuickDiscoveryCarriesBrandStyleAndAllowsLongChannelDrafts(t *testing.T) {
	for _, length := range []int{320, 3200} {
		request := DiscoverContentRequest{Topic: "Тема канала", ContentKind: "article", Format: "markdown", Tone: "Тон канала", Audience: "Аудитория", CTA: "Подписаться", VisualStyle: "Графика канала", ExamplePosts: []string{"Образец манеры, не факты для нового поста"}, ChannelPosts: []ChannelPostStyle{{Text: strings.Repeat("я", min(length, 1200)), Format: "markdown", CharacterCount: length}}}
		payload := externalDiscoveryPayload("deepseek-flash", request, []discoveryEvidence{{Title: "Источник", URL: "https://example.com/source", Content: "Проверенный факт"}})
		contextJSON := payload.Input[1].Content.(string)
		if !strings.Contains(contextJSON, "channel_posts") || !strings.Contains(contextJSON, "character_count") || !strings.Contains(contextJSON, "Графика канала") || !strings.Contains(contextJSON, "Подписаться") || !strings.Contains(contextJSON, "Образец манеры") {
			t.Fatal("quick discovery dropped channel or brand style")
		}
		instruction := payload.Input[0].Content.(string)
		if strings.Contains(instruction, "до 1200 Unicode") || !strings.Contains(instruction, "никогда не копируй их факты") {
			t.Fatal("quick discovery kept a universal short-post override")
		}
		if length == 3200 && payload.MaxOutputTokens <= 4000 {
			t.Fatal("long channel's three drafts cannot fit the old output token budget")
		}
	}
	card := discoveryFixtureCard("https://example.com/one", "article")
	card["draft"].(map[string]any)["content"] = strings.Repeat("я", 3000)
	envelope := discoveryFixtureEnvelope([]any{card})
	text, _ := extractOutputText(envelope)
	if cards, err := decodeDiscoveryCards(text, envelope, DiscoverContentRequest{Format: "markdown", ContentKind: "article"}); err != nil || len(cards) != 1 {
		t.Fatalf("valid long channel draft rejected by old 1200 limit: %v", err)
	}
}

func TestDeepSeekDraftRetriesExcessiveChannelLengthDriftOnce(t *testing.T) {
	for _, test := range []struct {
		name      string
		posts     []ChannelPostStyle
		examples  []string
		override  bool
		recover   bool
		wantCalls int
	}{
		{name: "short channel recovers", posts: []ChannelPostStyle{{Text: "Краткая заметка", CharacterCount: 350}}, recover: true, wantCalls: 2},
		{name: "short channel persistent drift", posts: []ChannelPostStyle{{Text: "Краткая заметка", CharacterCount: 350}}, wantCalls: 2},
		{name: "manual examples take precedence", posts: []ChannelPostStyle{{Text: "Подробный канал", CharacterCount: 3200}}, examples: []string{strings.Repeat("я", 350)}, override: true, recover: true, wantCalls: 2},
		{name: "long channel accepts long content", posts: []ChannelPostStyle{{Text: "Подробный канал", CharacterCount: 3200}}, wantCalls: 1},
		{name: "no evidence has no short channel ceiling", wantCalls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var payload responsePayload
				if json.NewDecoder(r.Body).Decode(&payload) != nil {
					t.Fatal("invalid draft request")
				}
				if calls == 2 && !strings.Contains(payload.Input[0].Content.(string), "основной текст длиннее 525") {
					t.Error("retry failed to explain the channel-specific upper length limit")
				}
				context := payload.Input[1].Content.(string)
				if !strings.Contains(context, "Призыв канала") || !strings.Contains(context, "Тон канала") || !strings.Contains(context, "нельзя") || strings.Contains(context, "invalid-length-marker") {
					t.Error("fresh retry must retain brand policy without replaying failed generated content")
				}
				content := "invalid-length-marker " + strings.Repeat("я", 1800)
				if calls == 2 && test.recover {
					content = strings.Repeat("я", 350)
				}
				raw := groundedDraftJSON(t, Draft{Title: "Пост", Content: content, Format: "markdown", ImagePrompt: "Editorial"}, []string{"s1"})
				writeDeepSeekText(t, w, raw)
			}))
			defer server.Close()
			request := Request{Topic: "Тема поста", Tone: "Тон канала", CTA: "Призыв канала", Format: "markdown", ForbiddenWords: []string{"нельзя"}, IncludeSources: true, ChannelPosts: test.posts, ExamplePosts: test.examples, ExamplePostsOverride: test.override}
			draft, err := mustDeepSeekClient(t, server).generatePostDraft(t.Context(), request, "Проверенный факт.", []Source{{Title: "Источник", URL: "https://example.com/report"}})
			wantError := test.wantCalls == 2 && !test.recover
			if calls != test.wantCalls || (err != nil) != wantError || (!wantError && !strings.Contains(draft.Content, "https://example.com/report")) || (wantError && draft.Content != "") {
				t.Fatalf("channel drift validation/retry failed: err=%v calls=%d draft=%#v", err, calls, draft)
			}
		})
	}
}
