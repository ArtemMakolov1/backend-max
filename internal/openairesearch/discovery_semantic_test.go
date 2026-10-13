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
	"time"

	"maxpilot/backend/internal/contentsearch"
)

func semanticPublicationFixture(t *testing.T) (DiscoveryPublishedReference, []DiscoveryPublishedCandidate) {
	t.Helper()
	raw, err := os.ReadFile("../contentdedup/testdata/published_aviation_rewrite.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Reference DiscoveryPublishedReference `json:"reference"`
		Secondary DiscoveryPublishedCandidate `json:"secondary_article"`
	}
	if json.Unmarshal(raw, &fixture) != nil {
		t.Fatal("invalid semantic publication fixture")
	}
	return fixture.Reference, []DiscoveryPublishedCandidate{
		fixture.Secondary,
		{Title: "Новый керосиновый двигатель испытали на орбите", Text: "Керосин и кислород использует новый двигатель спутника. Вакуумные испытания подтвердили устойчивое зажигание и тягу на орбите. Топливо для него производится традиционным способом, без опилок и растительного сырья."},
		{Title: "В России заработал промышленный завод биокеросина", Text: fixture.Reference.Text + " Теперь произошел новый самостоятельный этап: впервые запустили промышленный завод и отгрузили первую коммерческую партию топлива. Предыдущая заметка описывала только лабораторный эксперимент, а эта новость сообщает запуск производства."},
	}
}

func fixedPublishedReferenceLookup(reference DiscoveryPublishedReference) DiscoveryPublishedReferenceLookup {
	return func(_ context.Context, candidates []DiscoveryPublishedCandidate) ([][]DiscoveryPublishedReference, error) {
		result := make([][]DiscoveryPublishedReference, len(candidates))
		for i := range result {
			result[i] = []DiscoveryPublishedReference{reference}
		}
		return result, nil
	}
}

func TestSemanticPublicationJudgmentRejectsParaphraseWithoutNumbersAndKeepsNewMilestones(t *testing.T) {
	reference, candidates := semanticPublicationFixture(t)
	reference.Text += " https://private.example.com/secret-reference?token=private-token max://user/private-user max&#58;//user/encoded-private-user"
	reference.SourceURLs = []string{"https://private.example.com/never-send"}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		payload := assertQwenRequest(t, r, true)
		if payload.MaxTokens != 600 {
			t.Error("semantic classification token budget changed")
		}
		var pairs []semanticPublicationPair
		input := payload.Messages[1].Content.(string)
		_, data, ok := strings.Cut(input, "\n")
		if !ok || json.Unmarshal([]byte(data), &pairs) != nil || len(pairs) != 3 {
			t.Fatal("semantic batch omitted ambiguous event pairs")
		}
		for _, pair := range pairs {
			if len([]rune(pair.Candidate.Text)) > 1000 || len([]rune(pair.Published.Text)) > 1000 {
				t.Error("unbounded history or article prose reached the model")
			}
		}
		if strings.Contains(input, "private.example") || strings.Contains(input, "private-token") || strings.Contains(input, "max://") || strings.Contains(input, "encoded-private-user") || strings.Contains(input, "SourceURL") {
			t.Error("private publication URLs or metadata crossed model boundary")
		}
		decisions := make([]map[string]any, 0, len(pairs))
		for _, pair := range pairs {
			decisions = append(decisions, map[string]any{"pair_id": pair.PairID, "duplicate": pair.Candidate.Title == semanticPublicationSnippet(candidates[0].Title, candidates[0].Text).Title})
		}
		encoded, _ := json.Marshal(map[string]any{"pairs": decisions})
		writeQwenText(t, w, string(encoded))
	}))
	defer server.Close()
	result, err := mustQwenClient(t, server).matchPublishedCandidatesDetailed(t.Context(), DiscoverContentRequest{PublishedReferenceLookup: fixedPublishedReferenceLookup(reference)}, candidates)
	if err != nil || calls != 1 || len(result.Matched) != 3 || !result.Matched[0] || result.Matched[1] || result.Matched[2] || result.Unjudged {
		t.Fatalf("paraphrase/new-event decisions=%v calls=%d err=%v", result.Matched, calls, err)
	}
}

func TestSemanticPublicationSkipsKnownIdentityAndUnrelatedGenericCategories(t *testing.T) {
	reference, candidates := semanticPublicationFixture(t)
	reference.SourceURLs = []string{candidates[0].SourceURL}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(http.StatusInternalServerError) }))
	defer server.Close()
	candidates = []DiscoveryPublishedCandidate{candidates[0], {Title: "Российские ученые разработали новый аккумулятор", Text: "Специалисты получили результат исследования и провели испытания нового устройства. Натриевая батарея сохраняет емкость на морозе."}}
	result, err := mustQwenClient(t, server).matchPublishedCandidatesDetailed(t.Context(), DiscoverContentRequest{PublishedReferenceLookup: fixedPublishedReferenceLookup(reference)}, candidates)
	if err != nil || calls != 0 || !result.Matched[0] || result.Matched[1] {
		t.Fatalf("fast identity/generic topic invoked paid judgment: %v calls=%d err=%v", result.Matched, calls, err)
	}
}

func TestSemanticPublicationBudgetDropsUnjudgedAmbiguityAndKeepsKnownNewSources(t *testing.T) {
	reference, sample := semanticPublicationFixture(t)
	candidates := []DiscoveryPublishedCandidate{sample[0], sample[0], sample[0], sample[0], {Title: "Натриевая батарея", Text: "Аккумулятор для холодного климата"}}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		payload := assertQwenRequest(t, r, true)
		_, data, _ := strings.Cut(payload.Messages[1].Content.(string), "\n")
		var pairs []semanticPublicationPair
		if json.Unmarshal([]byte(data), &pairs) != nil || len(pairs) != 9 {
			t.Fatal("semantic budget exceeded three candidate judgments or lost reference comparisons")
		}
		output := make([]map[string]any, 0, len(pairs))
		for _, pair := range pairs {
			output = append(output, map[string]any{"pair_id": pair.PairID, "duplicate": false})
		}
		encoded, _ := json.Marshal(map[string]any{"pairs": output})
		writeQwenText(t, w, string(encoded))
	}))
	defer server.Close()
	lookup := func(_ context.Context, candidates []DiscoveryPublishedCandidate) ([][]DiscoveryPublishedReference, error) {
		result := make([][]DiscoveryPublishedReference, len(candidates))
		for i := range result {
			result[i] = []DiscoveryPublishedReference{reference, reference, reference}
		}
		return result, nil
	}
	result, err := mustQwenClient(t, server).matchPublishedCandidatesDetailed(t.Context(), DiscoverContentRequest{PublishedReferenceLookup: lookup}, candidates)
	if err != nil || calls != 1 || !result.Unjudged || result.Matched[0] || result.Matched[1] || result.Matched[2] || !result.Matched[3] || result.Matched[4] {
		t.Fatalf("budget decisions=%v calls=%d err=%v", result.Matched, calls, err)
	}
}

func TestSemanticPublicationRejectsUntrustedOrIncompletePairDecisionsWithoutRetry(t *testing.T) {
	reference, sample := semanticPublicationFixture(t)
	for _, output := range []string{
		`{"pairs":[]}`,
		`{"pairs":[{"pair_id":"private-provider-id","duplicate":true}]}`,
		`{"pairs":[{"pair_id":"p1"}]}`,
		`{"pairs":[{"pair_id":"p1","duplicate":"false"}]}`,
		`{"pairs":[{"pair_id":"p1","duplicate":false,"reason":"private-provider-text"}]}`,
		`{"pairs":[{"pair_id":"p1","duplicate":false}]}{}`,
		`{"pairs":[{"pair_id":"p1","duplicate":true,"duplicate":false}]}`,
		`{"pairs":[{"pair_id":"private-provider-id","pair_id":"p1","duplicate":false}]}`,
		`{"pairs":[{"PAIR_ID":"p1","DUPLICATE":false}]}`,
		`{"pairs":[{"pair_id":"p1","duplicate":true}],"pairs":[{"pair_id":"p1","duplicate":false}]}`,
		`{"PAIRS":[{"pair_id":"p1","duplicate":false}]}`,
	} {
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; writeQwenText(t, w, output) }))
		_, err := mustQwenClient(t, server).matchPublishedCandidatesDetailed(t.Context(), DiscoverContentRequest{PublishedReferenceLookup: fixedPublishedReferenceLookup(reference)}, sample[:1])
		server.Close()
		var providerErr *Error
		if !errors.As(err, &providerErr) || providerErr.Code != "invalid_structured_output" || calls != 1 || strings.Contains(err.Error(), "private-provider") {
			t.Fatalf("untrusted decision escaped closed validation: calls=%d err=%v", calls, err)
		}
	}
	pairs := []semanticPublicationPair{{PairID: "p1"}, {PairID: "p2"}}
	if _, err := decodeSemanticPublicationDecisions(`{"pairs":[{"pair_id":"p1","duplicate":true},{"pair_id":"p1","duplicate":false}]}`, pairs); err == nil {
		t.Fatal("repeated pair ID replaced a missing comparison")
	}
}

func TestSemanticPublicationFailsClosedOnLookupErrorsAndProviderRejection(t *testing.T) {
	reference, candidates := semanticPublicationFixture(t)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(http.StatusUnauthorized) }))
	defer server.Close()
	client := mustQwenClient(t, server)
	for _, lookup := range []DiscoveryPublishedReferenceLookup{
		func(context.Context, []DiscoveryPublishedCandidate) ([][]DiscoveryPublishedReference, error) {
			return nil, errors.New("private DB connection")
		},
		func(context.Context, []DiscoveryPublishedCandidate) ([][]DiscoveryPublishedReference, error) {
			return [][]DiscoveryPublishedReference{}, nil
		},
		func(context.Context, []DiscoveryPublishedCandidate) ([][]DiscoveryPublishedReference, error) {
			return [][]DiscoveryPublishedReference{{reference, reference, reference, reference}}, nil
		},
	} {
		_, err := client.matchPublishedCandidatesDetailed(t.Context(), DiscoverContentRequest{PublishedReferenceLookup: lookup}, candidates[:1])
		if err == nil || calls != 0 || strings.Contains(err.Error(), "private DB") {
			t.Fatalf("invalid history lookup did not fail closed: %v", err)
		}
	}
	_, err := client.matchPublishedCandidatesDetailed(t.Context(), DiscoverContentRequest{PublishedReferenceLookup: fixedPublishedReferenceLookup(reference)}, candidates[:1])
	var providerErr *Error
	if !errors.As(err, &providerErr) || providerErr.StatusCode != 401 || calls != 1 {
		t.Fatalf("provider failure retried or allowed material: calls=%d err=%v", calls, err)
	}
}

func TestSemanticPublicationUsesBoundedSharedCallerDeadline(t *testing.T) {
	reference, candidates := semanticPublicationFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	started := time.Now()
	client, err := NewQwen("https://provider.example.com", "synthetic-qwen-key", "qwen3.6-plus", &http.Client{Transport: diagnosticRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok || deadline.Sub(started) > semanticPublicationTimeout+time.Second {
			t.Error("semantic model reset or lost the caller's bounded deadline")
		}
		cancel()
		return nil, context.Canceled
	})})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.matchPublishedCandidatesDetailed(ctx, DiscoverContentRequest{PublishedReferenceLookup: fixedPublishedReferenceLookup(reference)}, candidates[:1])
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("caller cancellation was lost: %v", err)
	}
}

func TestSemanticPublicationEmptyUnjudgedBudgetDoesNotClaimAllWerePublished(t *testing.T) {
	reference, candidates := semanticPublicationFixture(t)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		pairs := []map[string]any{}
		for i := 1; i <= 3; i++ {
			pairs = append(pairs, map[string]any{"pair_id": fmt.Sprintf("p%d", i), "duplicate": true})
		}
		encoded, _ := json.Marshal(map[string]any{"pairs": pairs})
		writeQwenText(t, w, string(encoded))
	}))
	defer server.Close()
	searcher := &fakeContentSearcher{}
	for i := 0; i < 4; i++ {
		searcher.result.Sources = append(searcher.result.Sources, contentsearch.Source{Title: candidates[0].Title, Content: candidates[0].Text, URL: fmt.Sprintf("https://example.com/other-source-%d", i)})
	}
	result, err := mustQwenClient(t, server).WithContentSearch(searcher).DiscoverContent(t.Context(), DiscoverContentRequest{Topic: "Изобретения", PublishedReferenceLookup: fixedPublishedReferenceLookup(reference)})
	if err != nil || calls != 1 || len(result.Cards) != 0 || result.EmptyReason != "no_matching_materials" {
		t.Fatalf("unjudged sources misrepresented as proven repeats: calls=%d result=%#v err=%v", calls, result, err)
	}
}

func TestSemanticPublicationSelectionKeepsTwoIndependentNewMilestonesWithSharedBackground(t *testing.T) {
	reference, candidates := semanticPublicationFixture(t)
	first := ContentCard{Title: candidates[2].Title, Summary: "Запуск промышленного производства.", Source: Source{URL: "https://example.com/factory-launch"}, Draft: Draft{Content: candidates[2].Text}}
	second := ContentCard{Title: "Начались пилотируемые полеты на биотопливе", Summary: "Новый самостоятельный этап летных испытаний.", Source: Source{URL: "https://example.com/first-piloted-flight"}, Draft: Draft{Content: reference.Text + " Теперь впервые провели пилотируемый полет. Это новое событие после прежнего лабораторного эксперимента."}}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		payload := assertQwenRequest(t, r, true)
		_, data, _ := strings.Cut(payload.Messages[1].Content.(string), "\n")
		var pairs []semanticPublicationPair
		if json.Unmarshal([]byte(data), &pairs) != nil || len(pairs) != 2 {
			t.Fatal("new milestones bypassed semantic publication judgment")
		}
		decisions := make([]map[string]any, 0, len(pairs))
		for _, pair := range pairs {
			decisions = append(decisions, map[string]any{"pair_id": pair.PairID, "duplicate": false})
		}
		encoded, _ := json.Marshal(map[string]any{"pairs": decisions})
		writeQwenText(t, w, string(encoded))
	}))
	defer server.Close()
	cards, rejected, err := mustQwenClient(t, server).filterPublishedCards(t.Context(), DiscoverContentRequest{PublishedReferenceLookup: fixedPublishedReferenceLookup(reference)}, []ContentCard{first, second})
	if err != nil || rejected || len(cards) != 2 || calls != 1 {
		t.Fatalf("independent new milestones were merged by shared old background: cards=%d calls=%d err=%v", len(cards), calls, err)
	}
}

func TestSemanticPublicationFreshLookupAfterGenerationRejectsConcurrentlyPublishedCard(t *testing.T) {
	calls, lookups := 0, 0
	const sourceURL = "https://example.com/new-material"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		payload := assertQwenRequest(t, r, true)
		if payload.MaxTokens == 600 {
			t.Fatal("definitive fresh source identity unexpectedly invoked a semantic model")
		}
		output, _ := json.Marshal(map[string]any{"cards": []any{discoveryFixtureCard(sourceURL, "idea")}})
		writeQwenText(t, w, string(output))
	}))
	defer server.Close()
	searcher := &fakeContentSearcher{result: contentsearch.Result{Sources: []contentsearch.Source{{Title: "Новый материал", URL: sourceURL, Content: "Проверенный материал для новой публикации."}}}}
	lookup := func(_ context.Context, candidates []DiscoveryPublishedCandidate) ([][]DiscoveryPublishedReference, error) {
		lookups++
		if len(candidates) != 1 {
			t.Fatal("fresh publication lookup lost the candidate bound")
		}
		if lookups == 1 {
			return [][]DiscoveryPublishedReference{{}}, nil
		}
		return [][]DiscoveryPublishedReference{{{Title: "Только что опубликовано", Text: "Подтвержденная новая публикация другого редактора.", SourceURLs: []string{sourceURL}}}}, nil
	}
	result, err := mustQwenClient(t, server).WithContentSearch(searcher).DiscoverContent(t.Context(), DiscoverContentRequest{Topic: "Тема канала", ContentKind: "idea", PublishedReferenceLookup: lookup})
	if err != nil || lookups != 2 || calls != 1 || len(result.Cards) != 0 || result.EmptyReason != "already_published" {
		t.Fatalf("concurrently published source survived the fresh final lookup: calls=%d lookups=%d result=%#v err=%v", calls, lookups, result, err)
	}
}
