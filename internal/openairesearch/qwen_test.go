package openairesearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"maxpilot/backend/internal/contentsearch"
)

func TestQwenEveryTextFeatureUsesNonThinkingChatJSONAndSharedValidators(t *testing.T) {
	description := SuggestChannelDescriptionResult{Suggestions: []ChannelDescriptionSuggestion{
		{Style: "concise", Label: "Кратко", Text: "Практические материалы о контенте."},
		{Style: "expert", Label: "Экспертно", Text: "Разборы подготовки публикаций для редакторов."},
		{Style: "promotional", Label: "С пользой", Text: "Помогаем небольшим командам работать с контентом."},
	}}
	for _, test := range []struct {
		name, schemaField string
		output            any
		maxTokens         int
		run               func(*Client) error
	}{
		{name: "format", schemaField: "content", output: FormatResult{Content: "**Первый**\n\n\u200B\n\nВторой"}, maxTokens: 6000,
			run: func(c *Client) error {
				result, err := c.FormatContent(t.Context(), FormatRequest{Content: "Первый\n\n\u200B\n\nВторой", Format: "markdown"})
				if err == nil && result.Content != "**Первый**\n\n\u200B\n\nВторой" {
					t.Error("formatting lost visible text or editor gaps")
				}
				return err
			}},
		{name: "image prompt", schemaField: "prompt", output: SuggestImagePromptResult{Prompt: "Лаконичная сцена с одним визуальным акцентом."}, maxTokens: 1000,
			run: func(c *Client) error {
				_, err := c.SuggestImagePrompt(t.Context(), SuggestImagePromptRequest{Content: "Готовый пост", Format: "markdown", BrandTone: "Тон канала", BrandVisualStyle: "Теплые оттенки"})
				return err
			}},
		{name: "brand kit", schemaField: "visual_style", output: SuggestBrandKitResult{Tone: "Практичный", Audience: "Редакторы", CTA: "Подписывайтесь", VisualStyle: "Лаконичные сцены", ExamplePosts: []string{"Разбор для редакторов."}}, maxTokens: 2000,
			run: func(c *Client) error {
				_, err := c.SuggestBrandKit(t.Context(), SuggestBrandKitRequest{Posts: validSuggestBrandKitPosts(), Images: []ImageInput{{MIME: "image/png", Data: []byte("synthetic-png")}}})
				return err
			}},
		{name: "description", schemaField: "suggestions", output: description, maxTokens: 1200,
			run: func(c *Client) error {
				_, err := c.SuggestChannelDescription(t.Context(), validChannelDescriptionRequest())
				return err
			}},
		{name: "advertising copy", schemaField: "risk_warnings", output: SuggestDirectCopyResult{Variants: []DirectCopyVariant{{Title: "Ясный заголовок", Text: "Короткий текст"}, {Title: "Другой заголовок", Text: "Еще один текст"}, {Title: "Третий вариант", Text: "Другая формулировка"}}, Rationale: []string{}, RiskWarnings: []string{}}, maxTokens: 2000,
			run: func(c *Client) error {
				_, err := c.SuggestDirectCopy(t.Context(), SuggestDirectCopyRequest{Brief: "Сделай объявление яснее", CampaignName: "Кампания", AdType: "TEXT_AD", Titles: []string{"Заголовок"}, Texts: []string{"Текущий текст"}})
				return err
			}},
		{name: "advertising campaign", schemaField: "suggested_regions", output: validDirectCampaignSuggestionResult(), maxTokens: 4000,
			run: func(c *Client) error {
				_, err := c.SuggestDirectCampaign(t.Context(), validDirectCampaignSuggestionRequest())
				return err
			}},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				payload := assertQwenRequest(t, r, true)
				if payload.MaxTokens != test.maxTokens {
					t.Errorf("max_tokens=%d, want %d", payload.MaxTokens, test.maxTokens)
				}
				if !strings.Contains(payload.Messages[0].Content.(string), test.schemaField) {
					t.Error("feature schema did not reach system JSON instructions")
				}
				encoded, _ := json.Marshal(test.output)
				writeQwenText(t, w, string(encoded))
			}))
			defer server.Close()
			client := mustQwenClient(t, server)
			if err := test.run(client); err != nil {
				t.Fatal(err)
			}
			if calls != 1 || client.TextAIProvider() != "qwen" {
				t.Fatalf("calls=%d provider=%s", calls, client.TextAIProvider())
			}
		})
	}
}

func TestQwenPreparedImagesMapToChatPartsWithoutStorageURLs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload := assertQwenRequest(t, r, false)
		if payload.MaxTokens != 600 {
			t.Error("media token bound changed")
		}
		parts := payload.Messages[1].Content.([]any)
		if len(parts) != 3 || parts[0].(map[string]any)["type"] != "text" || parts[1].(map[string]any)["text"] != "Первый кадр" {
			t.Fatal("image text parts were not preserved")
		}
		image := parts[2].(map[string]any)
		if image["type"] != "image_url" || !strings.HasPrefix(image["image_url"].(map[string]any)["url"].(string), "data:image/jpeg;base64,") {
			t.Error("prepared image did not map to inline chat image")
		}
		writeQwenText(t, w, "Наблюдаемый визуальный стиль канала.")
	}))
	defer server.Close()
	result, err := mustQwenClient(t, server).AnalyzeContentMedia(t.Context(), ContentAnalysisRequest{Images: []ContentAnalysisImage{{JPEG: []byte{0xff, 0xd8, 0xff, 0xe0}, Label: "Первый кадр"}}})
	if err != nil || result != "Наблюдаемый визуальный стиль канала." {
		t.Fatalf("result=%q err=%v", result, err)
	}
}

func TestQwenGroundedResearchRetainsChannelStyleAndServerOwnedSources(t *testing.T) {
	searcher := &fakeContentSearcher{result: contentsearch.Result{Sources: []contentsearch.Source{{Title: "Источник", URL: "https://example.com/report?token=signed%2Fvalue", Content: "Подтвержденный факт об изобретении."}}}}
	var requests []qwenChatPayload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload := assertQwenRequest(t, r, true)
		requests = append(requests, payload)
		if len(requests) == 1 {
			if len(payload.Messages) != 3 || strings.Contains(payload.Messages[2].Content.(string), "https:") || !strings.Contains(payload.Messages[2].Content.(string), `"source_id":"s1"`) {
				t.Error("research source authority was not isolated from model URL generation")
			}
			writeQwenText(t, w, `{"sections":[{"text":"Подтвержденный факт об изобретении.","source_ids":["s1"]}],"empty_reason":null}`)
			return
		}
		data := payload.Messages[1].Content.(string)
		for _, context := range []string{"Образец стиля выбранного канала.", "Простой и живой", "Расскажите мнение", "Подтвержденный факт"} {
			if !strings.Contains(data, context) {
				t.Errorf("draft lost channel context %s", context)
			}
		}
		if strings.Contains(data, "https:") {
			t.Error("draft model received attribution URLs")
		}
		writeQwenText(t, w, `{"title":"Изобретение","content":"**Подтвержденный факт об изобретении.**","format":"markdown","image_prompt":"Editorial illustration","source_ids":["s1"]}`)
	}))
	defer server.Close()
	result, err := mustQwenClient(t, server).WithContentSearch(searcher).Generate(t.Context(), Request{Topic: "Новые изобретения", Tone: "Простой и живой", CTA: "Расскажите мнение", ChannelPosts: []ChannelPostStyle{{Text: "Образец стиля выбранного канала.", CharacterCount: 500}}, Format: "markdown", IncludeSources: true})
	if err != nil || searcher.calls != 1 || len(requests) != 2 || len(result.Sources) != 1 {
		t.Fatalf("err=%v search=%d calls=%d", err, searcher.calls, len(requests))
	}
	if !strings.Contains(result.Draft.Content, "https://example.com/report?token=signed%2Fvalue") || !strings.Contains(result.Report, "https://example.com/report?token=signed%2Fvalue") {
		t.Error("server-owned exact source URL was not retained")
	}
}

func TestQwenDiscoveryRepairsLinksOnceWithoutRepeatingSearchOrLosingStyle(t *testing.T) {
	for _, stillInvalid := range []bool{false, true} {
		t.Run(fmt.Sprint(stillInvalid), func(t *testing.T) {
			searcher := &fakeContentSearcher{result: contentsearch.Result{Sources: []contentsearch.Source{{Title: "Источник", URL: "https://example.com/source", Content: "Проверенный факт."}}}}
			var requests []qwenChatPayload
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				payload := assertQwenRequest(t, r, true)
				requests = append(requests, payload)
				text := "Проверенный факт."
				if len(requests) == 1 || stillInvalid {
					text = "invalid-generated-marker https://example.com/source"
				}
				writeQwenText(t, w, deepSeekDiscoveryJSON(t, text, "markdown", "https://example.com/source"))
			}))
			defer server.Close()
			result, err := mustQwenClient(t, server).WithContentSearch(searcher).DiscoverContent(t.Context(), DiscoverContentRequest{Topic: "Тема канала", ContentKind: "idea", Format: "markdown", Tone: "Разговорный", CTA: "Расскажите мнение", ChannelPosts: []ChannelPostStyle{{Text: "Образец выбранного канала.", CharacterCount: 500}}})
			if searcher.calls != 1 || len(requests) != 2 {
				t.Fatalf("search=%d calls=%d err=%v", searcher.calls, len(requests), err)
			}
			if (err != nil) != stillInvalid {
				t.Fatalf("unexpected retry result: %v", err)
			}
			if !stillInvalid && (len(result.Cards) != 1 || !strings.Contains(result.Cards[0].Draft.Content, "https://example.com/source")) {
				t.Error("valid repaired card lost server attribution")
			}
			secondSystem := requests[1].Messages[0].Content.(string)
			requests[1].Messages[0].Content = strings.Replace(secondSystem, discoveryNoLinksRetryInstruction, "", 1)
			if !reflect.DeepEqual(requests[0], requests[1]) {
				t.Error("repair changed the original style, evidence or token policy")
			}
			for _, input := range requests[1].Messages {
				if strings.Contains(input.Content.(string), "invalid-generated-marker") {
					t.Error("rejected model output became context")
				}
			}
		})
	}
}

func TestQwenMissingExternalSearchAndBuiltInToolsFailBeforePaidCalls(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(http.StatusInternalServerError) }))
	defer server.Close()
	client := mustQwenClient(t, server)
	for _, run := range []func() error{
		func() error { return client.ValidateResearchSources() },
		func() error {
			_, err := client.Generate(t.Context(), Request{Topic: "Тема поста", Tone: "Простой", Format: "markdown"})
			return err
		},
		func() error {
			_, err := client.DiscoverContent(t.Context(), DiscoverContentRequest{Topic: "Тема канала"})
			return err
		},
		func() error {
			_, err := client.call(t.Context(), researchPayload(client.model, Request{Topic: "Тема поста"}))
			return err
		},
	} {
		var providerErr *Error
		if err := run(); !errors.As(err, &providerErr) || providerErr.Provider != "qwen" || providerErr.Code != "content_search_not_configured" {
			t.Fatalf("missing search not rejected: %v", err)
		}
	}
	if calls != 0 {
		t.Fatalf("made %d paid calls", calls)
	}
}

func TestQwenDraftLengthRepairPreservesOriginalChannelContextAndRemainsBounded(t *testing.T) {
	for _, stillOversized := range []bool{false, true} {
		t.Run(fmt.Sprint(stillOversized), func(t *testing.T) {
			var requests []qwenChatPayload
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				payload := assertQwenRequest(t, r, true)
				requests = append(requests, payload)
				content := "Короткий подтвержденный факт."
				if len(requests) == 1 || stillOversized {
					content = strings.Repeat("длинный ", 130)
				}
				writeQwenText(t, w, groundedDraftJSON(t, Draft{Title: "Факт", Content: content, Format: "markdown", ImagePrompt: "Editorial"}, []string{}))
			}))
			defer server.Close()
			request := Request{Topic: "Тема поста", Tone: "Тон канала", CTA: "Призыв канала", Format: "markdown", ChannelPosts: []ChannelPostStyle{{Text: "Образец стиля канала.", CharacterCount: 400}}}
			draft, err := mustQwenClient(t, server).generatePostDraft(t.Context(), request, "Подтвержденный факт.", nil)
			if len(requests) != 2 || (err != nil) != stillOversized {
				t.Fatalf("length repair did not stop after two attempts: calls=%d err=%v", len(requests), err)
			}
			if !stillOversized && draft.Content != "Короткий подтвержденный факт." {
				t.Error("valid shortened draft was not returned")
			}
			if !reflect.DeepEqual(requests[0].Messages[1:], requests[1].Messages[1:]) || !strings.Contains(requests[1].Messages[0].Content.(string), "Предыдущая попытка превысила предел длины") {
				t.Error("repair lost the original authoritative report or channel context")
			}
			for _, input := range requests[1].Messages {
				if strings.Contains(input.Content.(string), "длинный длинный") {
					t.Error("invalid model output became repair context")
				}
			}
		})
	}
}

func TestQwenRejectsIncompleteFilteredRefusedToolAndInvalidResponsesSafely(t *testing.T) {
	for _, test := range []struct{ name, raw, code string }{
		{"length", `{"choices":[{"finish_reason":"length","message":{"role":"assistant","content":"private-provider-content"}}]}`, "response_incomplete"},
		{"filter", `{"choices":[{"finish_reason":"content_filter","message":{"role":"assistant","content":"private-provider-content"}}]}`, "response_incomplete"},
		{"unknown finish", `{"choices":[{"finish_reason":"private-provider-content","message":{"role":"assistant","content":"{}"}}]}`, "response_not_completed"},
		{"refused", `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","refusal":"private-provider-content","content":"{}"}}]}`, "response_refused"},
		{"tools", `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","tool_calls":[{"private-provider-content":true}],"content":"{}"}}]}`, "invalid_provider_response"},
		{"legacy function", `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","function_call":{"name":"private-provider-content"},"content":"{}"}}]}`, "invalid_provider_response"},
		{"error", `{"error":{"message":"private-provider-content"}}`, "response_failed"},
		{"no choice", `{"choices":[]}`, "invalid_provider_response"},
		{"missing role", `{"choices":[{"finish_reason":"stop","message":{"content":"{}"}}]}`, "invalid_provider_response"},
		{"invalid JSON", `private-provider-content`, "invalid_provider_response"},
		{"nonstring content", `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":[{"text":"private-provider-content"}]}}]}`, "invalid_provider_response"},
		{"empty", `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":""}}]}`, "missing_output_text"},
		{"invalid unicode", "{\"choices\":\"\xff\"}", "invalid_provider_response"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("x-request-id", "synthetic-qwen-key")
				_, _ = w.Write([]byte(test.raw))
			}))
			defer server.Close()
			_, err := mustQwenClient(t, server).FormatContent(t.Context(), FormatRequest{Content: "Текст", Format: "markdown"})
			var providerErr *Error
			if !errors.As(err, &providerErr) || providerErr.Code != test.code || providerErr.RequestID != "" || strings.Contains(err.Error(), "private-provider-content") || strings.Contains(err.Error(), "synthetic-qwen-key") {
				t.Fatalf("unsafe or wrong failure: %v", err)
			}
		})
	}
}

func TestQwenDoesNotFollowCredentialRedirectsAndDoesNotLogProviderErrors(t *testing.T) {
	forwarded := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded = true }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	_, err := mustQwenClient(t, server).FormatContent(t.Context(), FormatRequest{Content: "Текст", Format: "markdown"})
	var providerErr *Error
	if forwarded || !errors.As(err, &providerErr) || providerErr.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("authorization crossed redirect: forwarded=%t err=%v", forwarded, err)
	}
	for _, status := range []int{http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusTooManyRequests} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			errorServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("x-request-id", "request-safe-123")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":{"code":"synthetic-qwen-key","message":"private-provider-content"}}`))
			}))
			defer errorServer.Close()
			_, err := mustQwenClient(t, errorServer).FormatContent(t.Context(), FormatRequest{Content: "Текст", Format: "markdown"})
			if !errors.As(err, &providerErr) || providerErr.StatusCode != status || providerErr.Provider != "qwen" || providerErr.RequestID != "request-safe-123" || strings.Contains(err.Error(), "synthetic-qwen-key") || strings.Contains(err.Error(), "private-provider-content") {
				t.Fatalf("unsafe provider HTTP failure: %v", err)
			}
		})
	}
}

func TestQwenResponseSizeBoundAndCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", maxResponseBytes+1)))
	}))
	defer server.Close()
	client := mustQwenClient(t, server)
	_, err := client.FormatContent(t.Context(), FormatRequest{Content: "Текст", Format: "markdown"})
	var providerErr *Error
	if !errors.As(err, &providerErr) || providerErr.Code != "invalid_provider_response" {
		t.Fatalf("oversized response was accepted: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = client.FormatContent(ctx, FormatRequest{Content: "Текст", Format: "markdown"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestQwenAudioNeverUsesTextProviderKeyAndCacheKeyIncludesProvider(t *testing.T) {
	qwenCalls := 0
	qwen := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		qwenCalls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer qwen.Close()
	client := mustQwenClient(t, qwen)
	wav := make([]byte, 44)
	copy(wav, "RIFF")
	copy(wav[8:], "WAVE")
	if _, err := client.AnalyzeContentAudio(t.Context(), wav); err == nil || qwenCalls != 0 {
		t.Error("unconfigured audio used Qwen text endpoint")
	}
	if got := client.ContentAnalysisModelKey(); got != "qwen:qwen3.6-plus|audio-unavailable|frames-v1" {
		t.Fatalf("wrong provider cache identity: %s", got)
	}
	openaiCalls := 0
	openai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		openaiCalls++
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer independent-openai-key" {
			t.Error("audio credential routing crossed providers")
		}
		writeQwenText(t, w, "Краткое описание звука.")
	}))
	defer openai.Close()
	analyzer, err := New(openai.URL, "independent-openai-key", "unused-text-model", openai.Client())
	if err != nil {
		t.Fatal(err)
	}
	client = client.WithAudioAnalysis(analyzer)
	if _, err := client.AnalyzeContentAudio(t.Context(), wav); err != nil || qwenCalls != 0 || openaiCalls != 1 {
		t.Fatalf("audio=%v qwen=%d openai=%d", err, qwenCalls, openaiCalls)
	}
	if !strings.Contains(client.ContentAnalysisModelKey(), "openai:gpt-audio-1.5") {
		t.Error("cache key omitted independent audio provider")
	}
}

func TestQwenStructuredFenceCompatibilityKeepsStrictValidation(t *testing.T) {
	for _, raw := range []string{`{"content":"Текст","unexpected":"private-provider-content"}`, `{"content":"Другой текст"}`, `{"content":"Текст"}{}`, "prefix {\"content\":\"Текст\"}"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeQwenText(t, w, "```json\n"+raw+"\n```") }))
		_, err := mustQwenClient(t, server).FormatContent(t.Context(), FormatRequest{Content: "Текст", Format: "markdown"})
		server.Close()
		var providerErr *Error
		if !errors.As(err, &providerErr) || (providerErr.Code != "invalid_structured_output" && providerErr.Code != "content_changed") || strings.Contains(err.Error(), "private-provider-content") {
			t.Fatalf("strict validation lost: %v", err)
		}
	}
}

func mustQwenClient(t *testing.T, server *httptest.Server) *Client {
	t.Helper()
	client, err := NewQwen(server.URL, "synthetic-qwen-key", "qwen3.6-plus", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func writeQwenText(t *testing.T, w http.ResponseWriter, text string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": text}}}}); err != nil {
		t.Error(err)
	}
}

func assertQwenRequest(t *testing.T, r *http.Request, structured bool) qwenChatPayload {
	t.Helper()
	if r.URL.Path != "/compatible-mode/v1/chat/completions" || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer synthetic-qwen-key" {
		t.Error("wrong Qwen endpoint or credential routing")
	}
	var raw map[string]json.RawMessage
	if json.NewDecoder(r.Body).Decode(&raw) != nil {
		t.Fatal("invalid Qwen request JSON")
	}
	for _, field := range []string{"tools", "tool_choice", "max_tool_calls", "include", "reasoning", "store", "input", "text", "max_output_tokens"} {
		if _, exists := raw[field]; exists {
			t.Errorf("unsupported field %s reached Qwen", field)
		}
	}
	var thinking, stream bool
	if json.Unmarshal(raw["enable_thinking"], &thinking) != nil || thinking || json.Unmarshal(raw["stream"], &stream) != nil || stream {
		t.Error("thinking/stream must be explicitly disabled")
	}
	encoded, _ := json.Marshal(raw)
	var payload qwenChatPayload
	if json.Unmarshal(encoded, &payload) != nil || payload.Model != "qwen3.6-plus" || payload.MaxTokens <= 0 || payload.Temperature != 0.2 || len(payload.Messages) < 2 {
		t.Fatal("invalid completion policy or missing prompts")
	}
	if structured {
		if payload.ResponseFormat == nil || payload.ResponseFormat.Type != "json_object" || !strings.Contains(payload.Messages[0].Content.(string), "JSON Schema:") {
			t.Error("strict shared schema was not instructed through JSON mode")
		}
	} else if payload.ResponseFormat != nil {
		t.Error("plain media summary unexpectedly requested JSON")
	}
	return payload
}
