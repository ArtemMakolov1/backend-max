package openairesearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"maxpilot/backend/internal/contentsearch"
)

func publishedKeroseneReference() DiscoveryPublishedReference {
	return DiscoveryPublishedReference{Title: "Российские ученые создали авиакеросин из опилок и лузги", Text: "Российские ученые получили авиакеросин из древесных опилок и подсолнечной лузги. Биомассу перерабатывают в авиационное топливо с помощью каталитического процесса. Экспериментальная установка превращает отходы сельского хозяйства в горючее для самолетов.", SourceURLs: []string{"https://example.com/old-source?id=203"}}
}

func TestPublishedEventIdentityUsesSourceOrCorroboratingFactsNotCategory(t *testing.T) {
	reference := publishedKeroseneReference()
	for _, test := range []struct {
		name      string
		candidate DiscoveryPublishedCandidate
		duplicate bool
	}{
		{"canonical source", DiscoveryPublishedCandidate{Title: "Другой заголовок", SourceURL: "http://www.example.com/old-source/?utm_campaign=max&id=203#section"}, true},
		{"other publisher same event", DiscoveryPublishedCandidate{Title: "Керосин из лузги и опилок получили ученые России", Text: "Подсолнечная лузга и древесные опилки превращаются в авиакеросин. Каталитический процесс перерабатывает биомассу в авиационное топливо для самолетов. Экспериментальная установка использует отходы сельского хозяйства.", SourceURL: "https://other-publisher.ru/science/kerosene"}, true},
		{"generic channel category", DiscoveryPublishedCandidate{Title: "Российские ученые разработали новый аккумулятор", Text: "Сибирские исследователи испытали натриевый аккумулятор для холодного климата. Батарея сохраняет емкость при низких температурах.", SourceURL: "https://other-publisher.ru/science/battery"}, false},
		{"different kerosene advance", DiscoveryPublishedCandidate{Title: "Керосиновый двигатель выдержал испытания на орбите", Text: "Новый двигатель спутника использует керосин и кислород. Вакуумные испытания подтвердили стабильное зажигание и тягу в условиях орбиты.", SourceURL: "https://other-publisher.ru/space/engine"}, false},
		{"same biomass different invention", DiscoveryPublishedCandidate{Title: "Опилки и лузга стали основой строительных панелей", Text: "Древесные опилки и подсолнечную лузгу прессуют в прочные строительные панели. Полученный утеплитель сохраняет тепло в стенах жилых домов.", SourceURL: "https://other-publisher.ru/construction/panels"}, false},
		{"different identity query", DiscoveryPublishedCandidate{Title: "Космический эксперимент", SourceURL: "https://example.com/old-source?id=204"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := IsPublishedDiscoveryDuplicate(test.candidate, reference); got != test.duplicate {
				t.Fatalf("duplicate=%t want=%t", got, test.duplicate)
			}
		})
	}
	if IsPublishedDiscoveryDuplicate(DiscoveryPublishedCandidate{Title: "Керосиновый двигатель", Text: "Провели вакуумные испытания кислородной камеры с новым соплом."}, DiscoveryPublishedReference{Title: "Керосиновый двигатель", Text: "Разработали охлаждение двигателя для запусков наземных ракет."}) {
		t.Fatal("two shared topic words suppressed a different advance")
	}
}

func TestPublishedShortCaptionMatchesActualRewrittenCardAndLongArticleFacts(t *testing.T) {
	raw, err := os.ReadFile("../contentdedup/testdata/published_aviation_rewrite.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Reference DiscoveryPublishedReference `json:"reference"`
		Candidate DiscoveryPublishedCandidate `json:"candidate"`
		Article   DiscoveryPublishedCandidate `json:"article"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if !IsPublishedDiscoveryDuplicate(fixture.Candidate, fixture.Reference) {
		t.Fatal("actual rewritten title and short three-measure caption resurfaced a published event")
	}
	// The source paragraph is a modest paraphrase of the public article, not
	// a copyrighted full-text copy. Distinct synthetic background vocabulary
	// reproduces a 4,000-character source versus a short channel publication.
	for index := 0; len([]rune(fixture.Article.Text)) < 4000; index++ {
		fixture.Article.Text += fmt.Sprintf(" contextword%d", index)
	}
	if !IsPublishedDiscoveryDuplicate(fixture.Article, fixture.Reference) {
		t.Fatal("long article background diluted the same event's measured facts")
	}
	for _, candidate := range []DiscoveryPublishedCandidate{
		{Title: "Испытали новый керосиновый двигатель самолета", Text: "В испытаниях двигателя экономичность достигла 90%, масса сократилась на 10%, вибрация уменьшилась на 10 дБ. Керосин используется в авиационном топливе."},
		{Title: fixture.Candidate.Title, Text: "Древесные опилки пригодились для другого авиационного топлива. Выбросы диоксида серы снизились на 10%, тяга двигателя на 90%. Шум уменьшился на 10 дБ."},
		{Title: "Российские ученые испытали новый аккумулятор", Text: "Заряд сохранился на 90%, масса уменьшилась на 10%, шум измерителя составил 10 дБ."},
	} {
		if IsPublishedDiscoveryDuplicate(candidate, fixture.Reference) {
			t.Fatal("shared numbers, topic, or percentages assigned to different subjects established event identity")
		}
	}
}

func TestPublishedMeasuredFactExtractionBoundsRepetitionsAndRetainsSubjects(t *testing.T) {
	input := strings.Repeat("Шум уменьшился на 10 дБ. ", 300)
	facts := publicationMeasuredFacts(input)
	if len(facts) != 1 || publicationMeasuredFactMatches(input, input) != 1 {
		t.Fatal("one repeated measurement counted as three independent event facts")
	}
	if publicationMeasuredFactMatches("Шум уменьшился на 10 дБ, тяга снизилась на 90%, сера сократилась на 10%.", "Шум уменьшился на 10 дБ, тяга снизилась на 10%, сера сократилась на 90%.") != 1 {
		t.Fatal("neighboring fact windows confused measured subjects")
	}
}

func TestPublishedEventDoesNotMatchDifferentMeasuredSubjectsWithSamePredicateAndValues(t *testing.T) {
	reference := DiscoveryPublishedReference{Title: "Керосин из опилок", Text: "Керосин из опилок стал новым топливом. Уровень выбросов серы снизился на 90%. Уровень шума снизился на 10 дБ. Уровень тяги снизился на 10%."}
	candidate := DiscoveryPublishedCandidate{Title: "Керосин из опилок для нового двигателя", Text: "Топливо из опилок использует новый керосиновый двигатель. Уровень износа насосов снизился на 90%. Уровень вибрации крепежа снизился на 10 дБ. Уровень расхода топлива снизился на 10%."}
	if IsPublishedDiscoveryDuplicate(candidate, reference) {
		t.Fatal("the same measurement predicate identified different subjects as one event")
	}
	for _, predicate := range []string{"снизился", "снизилась", "снижался", "сократилась", "уменьшились", "увеличился", "повысилась"} {
		if !publicationMeasurementPredicate(publicationStem(predicate)) {
			t.Fatalf("Russian measurement predicate %q became a measured subject", predicate)
		}
	}
}

func TestPublicationIndexMatcherRejectsOldMaterialBeforeAnySynthesis(t *testing.T) {
	calls, matcherCalls := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(http.StatusInternalServerError) }))
	defer server.Close()
	ref := publishedKeroseneReference()
	searcher := &fakeContentSearcher{result: contentsearch.Result{Sources: []contentsearch.Source{{Title: ref.Title, URL: "https://example.com/old-source?id=203&utm_source=max", Content: ref.Text}}}}
	request := DiscoverContentRequest{Topic: "Изобретения", Format: "markdown", PublishedMatcher: func(ctx context.Context, candidates []DiscoveryPublishedCandidate) ([]bool, error) {
		matcherCalls++
		if len(candidates) != 1 {
			t.Fatal("index lookup was not one bounded candidate batch")
		}
		return []bool{IsPublishedDiscoveryDuplicate(candidates[0], ref)}, nil
	}}
	result, err := mustQwenClient(t, server).WithContentSearch(searcher).DiscoverContent(t.Context(), request)
	if err != nil || result.EmptyReason != "already_published" || len(result.Cards) != 0 || result.Cards == nil || calls != 0 || matcherCalls != 1 {
		t.Fatalf("result=%#v err=%v calls=%d matcher=%d", result, err, calls, matcherCalls)
	}
	encoded, _ := json.Marshal(request)
	if strings.Contains(string(encoded), "published") || strings.Contains(string(encoded), "old-source") {
		t.Fatal("server index callback or publication authority became HTTP input")
	}
}

func TestPublicationFilteringKeepsNewEvidenceAndChecksGeneratedCardsAgain(t *testing.T) {
	for _, rewrittenOldEvent := range []bool{false, true} {
		t.Run(stringBool(rewrittenOldEvent), func(t *testing.T) {
			ref := publishedKeroseneReference()
			searcher := &fakeContentSearcher{result: contentsearch.Result{Sources: []contentsearch.Source{
				{Title: ref.Title, URL: ref.SourceURLs[0], Content: ref.Text},
				{Title: "Натриевая батарея работает на морозе", URL: "https://example.com/new-battery", Content: "Испытали аккумулятор для низких температур."},
			}}}
			calls, matcherCalls := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				payload := assertQwenRequest(t, r, true)
				if strings.Contains(payload.Messages[2].Content.(string), "old-source") || strings.Contains(payload.Messages[2].Content.(string), "авиакеросин") {
					t.Error("published evidence reached synthesis authority")
				}
				card := discoveryFixtureCard("https://example.com/new-battery", "idea")
				if rewrittenOldEvent {
					card["title"], card["summary"] = ref.Title, "Повтор ранее опубликованного события."
					card["draft"].(map[string]any)["content"] = ref.Text
				} else {
					card["title"], card["summary"] = "Натриевая батарея работает на морозе", "Новый аккумулятор для холодного климата."
					card["draft"].(map[string]any)["content"] = "Испытали натриевую батарею при низких температурах."
				}
				output, _ := json.Marshal(map[string]any{"cards": []any{card}})
				writeQwenText(t, w, string(output))
			}))
			defer server.Close()
			request := DiscoverContentRequest{Topic: "Изобретения", ContentKind: "idea", Format: "markdown", PublishedMatcher: func(ctx context.Context, candidates []DiscoveryPublishedCandidate) ([]bool, error) {
				matcherCalls++
				matched := make([]bool, len(candidates))
				for i, candidate := range candidates {
					matched[i] = IsPublishedDiscoveryDuplicate(candidate, ref)
				}
				return matched, nil
			}}
			result, err := mustQwenClient(t, server).WithContentSearch(searcher).DiscoverContent(t.Context(), request)
			if err != nil || calls != 1 || matcherCalls != 2 || searcher.calls != 1 {
				t.Fatalf("err=%v calls=%d matcher=%d search=%d", err, calls, matcherCalls, searcher.calls)
			}
			if rewrittenOldEvent {
				if len(result.Cards) != 0 || result.EmptyReason != "already_published" {
					t.Fatal("model restated a published event under a new source")
				}
			} else if len(result.Cards) != 1 || result.EmptyReason != "" {
				t.Fatal("new source was suppressed by shared channel category")
			}
		})
	}
}

func TestPublicationMatcherErrorsFailClosedWithoutProviderOrDatabaseText(t *testing.T) {
	for _, failure := range []struct {
		name    string
		matched []bool
		err     error
	}{
		{"DB unavailable", nil, errors.New("private database connection string")},
		{"malformed length", []bool{}, nil},
		{"cancelled", nil, context.Canceled},
	} {
		t.Run(failure.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
			defer server.Close()
			searcher := &fakeContentSearcher{result: contentsearch.Result{Sources: []contentsearch.Source{{Title: "Новый материал", URL: "https://example.com/new", Content: "Текст источника"}}}}
			_, err := mustQwenClient(t, server).WithContentSearch(searcher).DiscoverContent(t.Context(), DiscoverContentRequest{Topic: "Тема канала", PublishedMatcher: func(context.Context, []DiscoveryPublishedCandidate) ([]bool, error) {
				return failure.matched, failure.err
			}})
			if err == nil || calls != 0 || strings.Contains(err.Error(), "private database") {
				t.Fatalf("history failure was not closed: err=%v calls=%d", err, calls)
			}
			if errors.Is(failure.err, context.Canceled) {
				if !errors.Is(err, context.Canceled) {
					t.Fatal("cancellation lost")
				}
				return
			}
			var providerErr *Error
			if !errors.As(err, &providerErr) || providerErr.Code != "publication_history_unavailable" {
				t.Fatalf("wrong matcher error: %v", err)
			}
		})
	}
}

func TestSelectionDeduplicatesSameEventAcrossPublishersWithoutAnotherLLMCall(t *testing.T) {
	ref := publishedKeroseneReference()
	client := &Client{provider: "qwen"}
	first := ContentCard{Title: ref.Title, Summary: ref.Text, Source: Source{URL: "https://one.example.com/story"}, Draft: Draft{Content: ref.Text}}
	second := first
	second.Source.URL = "https://two.example.com/another-headline"
	third := ContentCard{Title: "Натриевая батарея работает на морозе", Summary: "Другой независимый эксперимент.", Source: Source{URL: "https://three.example.com/battery"}, Draft: Draft{Content: "Испытали аккумулятор для низких температур и холодного климата."}}
	cards, rejected, err := client.filterPublishedCards(t.Context(), DiscoverContentRequest{}, []ContentCard{first, second, third})
	if err != nil || rejected || len(cards) != 2 || cards[0].Source.URL != first.Source.URL || cards[1].Source.URL != third.Source.URL {
		t.Fatalf("cross-publisher event selection not distinct: cards=%#v err=%v", cards, err)
	}
}

func TestChannelQueryDoesNotRecycleRecentPublicationEvents(t *testing.T) {
	query := contentSearchQuery(DiscoverContentRequest{Topic: "Изобретено в России", ChannelTitle: "Изобретено в России", ChannelDescription: "Наука и интересные изобретения", RecentPosts: []DiscoveryPublishedPost{{Text: "Керосин из опилок и лузги"}}, MediaContext: "Изображение керосина из опилок", ContentKind: "article"})
	if !strings.Contains(query, "Наука и интересные изобретения") || strings.Contains(query, "Керосин") || strings.Contains(query, "керосина") || strings.Contains(query, "опилок") {
		t.Fatalf("already-covered event biased retrieval: %s", query)
	}
}

func stringBool(value bool) string {
	if value {
		return "rewritten old event"
	}
	return "new material"
}
