package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"maxpilot/backend/internal/contentsearch"
	"maxpilot/backend/internal/openairesearch"
	"maxpilot/backend/internal/store"
)

type discoveryTestClient struct {
	requests []openairesearch.DiscoverContentRequest
}

func TestContentDiscoveryPublishedMatcherIsServerOwnedAndSelectedChannelScoped(t *testing.T) {
	fake := &discoveryTestClient{}
	application, storage, workspace := newChannelDescriptionFixture(t, fake)
	activatePaidWorkspaceForTest(t, storage, "description-owner", workspace.ID)
	channel, err := storage.CreateChannel(t.Context(), store.Channel{UserID: "description-owner", WorkspaceID: workspace.ID,
		VerifiedMAXOwnerID: "100", MAXChatID: "-70001", Title: "Изобретено в России", IsChannel: true, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	other, err := storage.CreateChannel(t.Context(), store.Channel{UserID: "description-owner", WorkspaceID: workspace.ID,
		VerifiedMAXOwnerID: "100", MAXChatID: "-70002", Title: "Другой канал", IsChannel: true, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	createDeliveredDiscoveryTestPost(t, storage, store.Post{UserID: "description-owner", WorkspaceID: workspace.ID,
		ChannelID: &channel.ID, Title: "Керосиновый двигатель", Content: "Русский инженер создал керосиновый двигатель. https://example.com/engine?utm_source=channel", Format: store.FormatMarkdown})
	createDeliveredDiscoveryTestPost(t, storage, store.Post{UserID: "description-owner", WorkspaceID: workspace.ID,
		ChannelID: &other.ID, Title: "Подводная лодка", Content: "Создали новую подводную лодку. https://example.com/other-channel", Format: store.FormatMarkdown})
	if _, err := storage.CreatePost(t.Context(), store.Post{UserID: "description-owner", WorkspaceID: workspace.ID,
		ChannelID: &channel.ID, Title: "Черновик", Content: "Секретный неопубликованный черновик. https://example.com/draft", Format: store.FormatMarkdown}); err != nil {
		t.Fatal(err)
	}
	injectedCalls := 0
	_, err = application.DiscoverContentForWorkspaceWithBeforeGenerate(t.Context(), "description-owner", workspace.ID, &channel.ID,
		openairesearch.DiscoverContentRequest{
			PublishedMatcher: func(context.Context, []openairesearch.DiscoveryPublishedCandidate) ([]bool, error) {
				injectedCalls++
				return []bool{true, true, true}, nil
			},
			PublishedReferenceLookup: func(context.Context, []openairesearch.DiscoveryPublishedCandidate) ([][]openairesearch.DiscoveryPublishedReference, error) {
				injectedCalls++
				return [][]openairesearch.DiscoveryPublishedReference{{{Text: "Injected private publication"}}}, nil
			},
		}, nil)
	if err != nil || len(fake.requests) != 1 || fake.requests[0].PublishedMatcher != nil || fake.requests[0].PublishedReferenceLookup == nil {
		t.Fatalf("server publication reference lookup unavailable: %v", err)
	}
	request := fake.requests[0]
	candidates := []openairesearch.DiscoveryPublishedCandidate{
		{SourceURL: "https://example.com/engine?utm_campaign=another", Title: "Другая подача уже опубликованного двигателя"},
		{SourceURL: "https://example.com/other-channel", Title: "Подводная лодка"},
		{SourceURL: "https://example.com/draft", Title: "Черновик"},
	}
	references, err := request.PublishedReferenceLookup(t.Context(), candidates)
	matches := discoveryReferencesMatchCandidates(candidates, references)
	if err != nil || len(matches) != 3 || !matches[0] || matches[1] || matches[2] || injectedCalls != 0 {
		t.Fatalf("matcher trusted caller, draft or foreign channel: matches=%v injected=%d err=%v", matches, injectedCalls, err)
	}
	encoded, err := json.Marshal(request)
	if err != nil || strings.Contains(string(encoded), "PublishedMatcher") || strings.Contains(string(encoded), "PublishedReferenceLookup") || strings.Contains(string(encoded), "секретный") {
		t.Fatal("local publication index leaked into serialized model/public request")
	}
}

func TestContentDiscoveryPublishedMatcherRecognizesShortCaptionAndLongSourceRewrite(t *testing.T) {
	fake := &discoveryTestClient{}
	application, storage, workspace := newChannelDescriptionFixture(t, fake)
	activatePaidWorkspaceForTest(t, storage, "description-owner", workspace.ID)
	channel, err := storage.CreateChannel(t.Context(), store.Channel{UserID: "description-owner", WorkspaceID: workspace.ID,
		VerifiedMAXOwnerID: "100", MAXChatID: "-70003", Title: "Изобретено в России", IsChannel: true, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("../contentdedup/testdata/published_aviation_rewrite.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Reference          openairesearch.DiscoveryPublishedReference
		Candidate, Article openairesearch.DiscoveryPublishedCandidate
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	createDeliveredDiscoveryTestPost(t, storage, store.Post{UserID: "description-owner", WorkspaceID: workspace.ID,
		ChannelID: &channel.ID, Title: "Новая публикация", Content: fixture.Reference.Text, Format: store.FormatMarkdown})
	_, err = application.DiscoverContentForWorkspaceWithBeforeGenerate(t.Context(), "description-owner", workspace.ID, &channel.ID,
		openairesearch.DiscoverContentRequest{Topic: "Керосин из опилок и подсолнечной лузги"}, nil)
	if err != nil || len(fake.requests) != 1 || fake.requests[0].PublishedMatcher != nil || fake.requests[0].PublishedReferenceLookup == nil {
		t.Fatalf("server publication reference lookup unavailable: %v", err)
	}
	article := fixture.Article
	article.Text += strings.Repeat(" Научные исследования продолжаются, подробности описаны в публикации.", 80)
	candidates := []openairesearch.DiscoveryPublishedCandidate{
		article,
		fixture.Candidate,
		{Title: "Керосиновый двигатель выдержал испытания на орбите",
			Text:      "Новый двигатель спутника использует керосин и кислород. Вакуумные испытания подтвердили стабильное зажигание и тягу в условиях орбиты.",
			SourceURL: "https://example.com/new-space-engine"},
		{Title: "Опилки и лузга стали основой строительных панелей",
			Text:      "Древесные опилки и подсолнечную лузгу прессуют в прочные строительные панели. Полученный утеплитель сохраняет тепло в стенах жилых домов.",
			SourceURL: "https://example.com/new-construction-panels"},
	}
	references, err := fake.requests[0].PublishedReferenceLookup(t.Context(), candidates)
	matched := discoveryReferencesMatchCandidates(candidates, references)
	if err != nil || len(matched) != 4 || !matched[0] || !matched[1] || matched[2] || matched[3] {
		t.Fatalf("short published caption/rewrite event identity mismatch: matches=%v err=%v", matched, err)
	}
}

func discoveryReferencesMatchCandidates(candidates []openairesearch.DiscoveryPublishedCandidate, references [][]openairesearch.DiscoveryPublishedReference) []bool {
	if len(candidates) != len(references) {
		return nil
	}
	matches := make([]bool, len(candidates))
	for index, candidate := range candidates {
		for _, reference := range references[index] {
			if openairesearch.IsPublishedDiscoveryDuplicate(candidate, reference) {
				matches[index] = true
				break
			}
		}
	}
	return matches
}

func TestContentDiscoveryPublishedReferenceLookupBoundsComparisonsAndRechecksMembership(t *testing.T) {
	fake := &discoveryTestClient{}
	application, storage, _ := newChannelDescriptionFixture(t, fake)
	workspace, err := storage.CreateWorkspace(t.Context(), "description-owner", store.Workspace{Name: "Shared editorial workspace"})
	if err != nil {
		t.Fatal(err)
	}
	activatePaidWorkspaceForTest(t, storage, "description-owner", workspace.ID)
	channel, err := storage.CreateChannel(t.Context(), store.Channel{UserID: "description-owner", WorkspaceID: workspace.ID,
		VerifiedMAXOwnerID: "100", MAXChatID: "-70004", Title: "Инженерные новости", IsChannel: true, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 6; index++ {
		createDeliveredDiscoveryTestPost(t, storage, store.Post{UserID: "description-owner", WorkspaceID: workspace.ID,
			ChannelID: &channel.ID, Title: "Private internal title", Format: store.FormatMarkdown,
			Content: fmt.Sprintf("Ракетный двигатель. Отдельная публикация о конструктивном решении номер %d.", index)})
	}
	if err := storage.UpsertUser(t.Context(), store.User{ID: "projection-editor", DisplayName: "Editor"}); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.AddWorkspaceMember(t.Context(), "description-owner", store.WorkspaceMember{
		WorkspaceID: workspace.ID, UserID: "projection-editor", Role: store.WorkspaceRoleEditor,
	}); err != nil {
		t.Fatal(err)
	}
	_, err = application.DiscoverContentForWorkspaceWithBeforeGenerate(t.Context(), "projection-editor", workspace.ID, &channel.ID,
		openairesearch.DiscoverContentRequest{Topic: "Ракетный двигатель"}, nil)
	if err != nil || len(fake.requests) != 1 || fake.requests[0].PublishedReferenceLookup == nil || fake.requests[0].PublishedMatcher != nil {
		t.Fatalf("scoped reference projection unavailable: %v", err)
	}
	lookup := fake.requests[0].PublishedReferenceLookup
	candidates := []openairesearch.DiscoveryPublishedCandidate{{Title: "Ракетный двигатель", Text: "Устройство нового поколения прошло испытания."}}
	refs, err := lookup(t.Context(), candidates)
	if err != nil || len(refs) != 1 || len(refs[0]) != 3 {
		t.Fatalf("nearest reference projection is not bounded to three: groups=%d err=%v", len(refs), err)
	}
	for _, reference := range refs[0] {
		if reference.Title != "Ракетный двигатель" || utf8.RuneCountInString(reference.Text) > 4000 || strings.Contains(reference.Text, "Private") {
			t.Fatal("reference projection included an unproven internal title or unbounded body")
		}
	}
	if refs, err := lookup(t.Context(), make([]openairesearch.DiscoveryPublishedCandidate, 21)); err == nil || refs != nil {
		t.Fatal("an unbounded candidate list reached publication lookup")
	}
	if err := storage.RemoveWorkspaceMember(t.Context(), "description-owner", workspace.ID, "projection-editor"); err != nil {
		t.Fatal(err)
	}
	if refs, err := lookup(t.Context(), candidates); !errors.Is(err, store.ErrNotFound) || refs != nil {
		t.Fatalf("revoked member retained publication comparisons: groups=%d err=%v", len(refs), err)
	}
}

type discoverySemanticTestSearcher struct {
	source contentsearch.Source
}

func TestContentDiscoveryPublishedReferenceLookupRetainsNewerMilestoneAlongsideOldBackground(t *testing.T) {
	fake := &discoveryTestClient{}
	application, storage, workspace := newChannelDescriptionFixture(t, fake)
	activatePaidWorkspaceForTest(t, storage, "description-owner", workspace.ID)
	channel, err := storage.CreateChannel(t.Context(), store.Channel{UserID: "description-owner", WorkspaceID: workspace.ID,
		VerifiedMAXOwnerID: "100", MAXChatID: "-70006", Title: "Изобретено в России", IsChannel: true, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("../contentdedup/testdata/published_aviation_rewrite.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Reference openairesearch.DiscoveryPublishedReference
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	newer := "Открыт промышленный завод авиационного биотоплива. Предприятие впервые выпустило коммерческую партию; далее приведён старый лабораторный контекст. " + fixture.Reference.Text
	for _, content := range []string{fixture.Reference.Text, newer} {
		createDeliveredDiscoveryTestPost(t, storage, store.Post{UserID: "description-owner", WorkspaceID: workspace.ID,
			ChannelID: &channel.ID, Title: "Private internal label", Content: content, Format: store.FormatMarkdown})
	}
	_, err = application.DiscoverContentForWorkspaceWithBeforeGenerate(t.Context(), "description-owner", workspace.ID, &channel.ID,
		openairesearch.DiscoverContentRequest{Topic: "Авиационное биотопливо"}, nil)
	if err != nil || len(fake.requests) != 1 || fake.requests[0].PublishedReferenceLookup == nil {
		t.Fatalf("milestone reference lookup unavailable: %v", err)
	}
	candidate := openairesearch.DiscoveryPublishedCandidate{Title: "Завод запустил промышленное производство авиационного биотоплива",
		Text:      "Коммерческий выпуск начался на промышленном заводе авиационного биотоплива. В качестве справки сообщаются прежние показатели топлива. " + fixture.Reference.Text,
		SourceURL: "https://example.com/another-publisher-new-plant"}
	refs, err := fake.requests[0].PublishedReferenceLookup(t.Context(), []openairesearch.DiscoveryPublishedCandidate{candidate})
	if err != nil || len(refs) != 1 || len(refs[0]) != 2 {
		t.Fatalf("old background relevance crowded out a confirmed newer milestone: groups=%d err=%v", len(refs), err)
	}
	seen := make(map[string]bool)
	for _, reference := range refs[0] {
		seen[reference.Text] = true
		if openairesearch.IsDefinitivePublishedDiscoveryDuplicate(candidate, reference) {
			t.Fatal("paraphrased milestone fixture unexpectedly established exact identity")
		}
	}
	if !seen[fixture.Reference.Text] || !seen[newer] {
		t.Fatal("semantic comparison omitted old background or the actual newer publication")
	}
}

func (f discoverySemanticTestSearcher) Search(context.Context, contentsearch.Request) (contentsearch.Result, error) {
	return contentsearch.Result{Provider: "fixture", Sources: []contentsearch.Source{f.source}}, nil
}

func TestContentDiscoveryUsesScopedPublishedIndexForSemanticAlternatePublisherAndNewEvents(t *testing.T) {
	data, err := os.ReadFile("../contentdedup/testdata/published_aviation_rewrite.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Reference        openairesearch.DiscoveryPublishedReference
		SecondaryArticle openairesearch.DiscoveryPublishedCandidate `json:"secondary_article"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		candidate openairesearch.DiscoveryPublishedCandidate
		duplicate bool
	}{
		{"same event at alternate publisher without measurements", fixture.SecondaryArticle, true},
		{"new industrial milestone with copied old fuel background", openairesearch.DiscoveryPublishedCandidate{
			Title:     "Открыт первый промышленный завод авиационного биотоплива",
			Text:      "Новое событие: впервые запущен промышленный завод авиационного биотоплива, который выпускает коммерческую партию. Ниже приведены прежние лабораторные результаты для контекста. " + fixture.Reference.Text,
			SourceURL: "https://example.com/new-industrial-plant"}, false},
		{"new aviation milestone", openairesearch.DiscoveryPublishedCandidate{Title: "Самолет выполнил первый перелет на топливе из опилок",
			Text:      "Новый этап разработки авиационного топлива: самолет впервые совершил испытательный перелет на горючем из древесных опилок и подсолнечной лузги. В отличие от лабораторной оценки топлива, эта публикация сообщает о первом летном испытании.",
			SourceURL: "https://example.com/first-flight"}, false},
		{"same biomass new construction result", openairesearch.DiscoveryPublishedCandidate{Title: "Строительные панели из опилок и подсолнечной лузги",
			Text:      "Из древесных опилок и подсолнечной лузги изготовили новые строительные панели. Эксперимент подтвердил теплоизоляцию стен; это самостоятельный результат материаловедения, без разработки авиационного топлива.",
			SourceURL: "https://example.com/construction-panels"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var payload struct {
					MaxTokens int `json:"max_tokens"`
					Messages  []struct {
						Role, Content string
					} `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error("invalid classifier/synthesis transport payload")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				content := `{"cards":[]}`
				for _, message := range payload.Messages {
					const prefix = "Пары редакционных материалов JSON:\n"
					if message.Role != "user" || !strings.HasPrefix(message.Content, prefix) {
						continue
					}
					var pairs []struct {
						PairID    string `json:"pair_id"`
						Candidate struct{ Title, Text string }
						Published struct{ Title, Text string }
					}
					if err := json.Unmarshal([]byte(strings.TrimPrefix(message.Content, prefix)), &pairs); err != nil || len(pairs) != 1 || pairs[0].PairID != "p1" || payload.MaxTokens != 600 {
						t.Error("semantic classifier did not receive one bounded authorized pair")
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					if utf8.RuneCountInString(pairs[0].Candidate.Text) > 1000 || utf8.RuneCountInString(pairs[0].Published.Text) > 1000 ||
						utf8.RuneCountInString(pairs[0].Candidate.Title) > 200 || utf8.RuneCountInString(pairs[0].Published.Title) > 200 ||
						strings.Contains(message.Content, "PRIVATE_INTERNAL_LABEL") || strings.Contains(message.Content, "private-media.example") ||
						strings.Contains(message.Content, "https://") || strings.Contains(message.Content, "description-owner") {
						t.Error("semantic projection exposed private fields, URLs or unbounded publication text")
					}
					content = fmt.Sprintf(`{"pairs":[{"pair_id":"p1","duplicate":%t}]}`, test.duplicate)
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
					"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": content},
				}}})
			}))
			defer server.Close()
			client, err := openairesearch.NewQwen(server.URL, "local-fixture-key", "qwen3.6-plus", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			client = client.WithContentSearch(discoverySemanticTestSearcher{source: contentsearch.Source{
				Title: test.candidate.Title, Content: test.candidate.Text, URL: test.candidate.SourceURL,
			}})
			application, storage, workspace := newChannelDescriptionFixture(t, client)
			activatePaidWorkspaceForTest(t, storage, "description-owner", workspace.ID)
			channel, err := storage.CreateChannel(t.Context(), store.Channel{UserID: "description-owner", WorkspaceID: workspace.ID,
				VerifiedMAXOwnerID: "100", MAXChatID: "-70005", Title: "Изобретено в России", IsChannel: true, Active: true})
			if err != nil {
				t.Fatal(err)
			}
			createDeliveredDiscoveryTestPost(t, storage, store.Post{UserID: "description-owner", WorkspaceID: workspace.ID,
				ChannelID: &channel.ID, Title: "PRIVATE_INTERNAL_LABEL", Format: store.FormatMarkdown,
				Content: fixture.Reference.Text + "\n\nhttps://private-media.example/attachment?token=fixture-only"})
			result, err := application.DiscoverContentForWorkspaceWithBeforeGenerate(t.Context(), "description-owner", workspace.ID, &channel.ID,
				openairesearch.DiscoverContentRequest{Topic: "Авиационное топливо"}, nil)
			if err != nil {
				t.Fatalf("authorized semantic discovery failed: %v", err)
			}
			if test.duplicate {
				if result.EmptyReason != "already_published" || len(result.Cards) != 0 || calls.Load() != 1 {
					t.Fatalf("alternate publisher survived prior publication: calls=%d empty=%s", calls.Load(), result.EmptyReason)
				}
			} else if calls.Load() != 2 || result.EmptyReason == "already_published" {
				t.Fatalf("a separately classified new event did not reach synthesis: calls=%d empty=%s", calls.Load(), result.EmptyReason)
			}
		})
	}
}

func (*discoveryTestClient) Generate(context.Context, openairesearch.Request) (openairesearch.Result, error) {
	return openairesearch.Result{}, errors.New("advanced research was not expected")
}

func (f *discoveryTestClient) DiscoverContent(_ context.Context, request openairesearch.DiscoverContentRequest) (openairesearch.DiscoverContentResult, error) {
	f.requests = append(f.requests, request)
	return openairesearch.DiscoverContentResult{Topic: request.Topic, ContentKind: request.ContentKind, Cards: []openairesearch.ContentCard{}}, nil
}

func TestContentDiscoveryUsesOwnedBoundedChannelAndPublishedContextBeforeQuota(t *testing.T) {
	fake := &discoveryTestClient{}
	application, storage, workspace := newChannelDescriptionFixture(t, fake)
	activatePaidWorkspaceForTest(t, storage, "description-owner", workspace.ID)
	channel, err := storage.CreateChannel(t.Context(), store.Channel{UserID: "description-owner", WorkspaceID: workspace.ID,
		VerifiedMAXOwnerID: "100", MAXChatID: "-100", Title: "Весёлые коты", Description: strings.Repeat("я", 16000), IsChannel: true, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	other, err := storage.CreateChannel(t.Context(), store.Channel{UserID: "description-owner", WorkspaceID: workspace.ID,
		VerifiedMAXOwnerID: "100", MAXChatID: "-101", Title: "Другой канал", IsChannel: true, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		createDeliveredDiscoveryTestPost(t, storage, store.Post{UserID: "description-owner", WorkspaceID: workspace.ID, ChannelID: &channel.ID,
			Title: fmt.Sprint(i), Content: fmt.Sprint(i) + strings.Repeat("я", 900), Format: store.FormatMarkdown})
	}
	for _, post := range []store.Post{
		{ChannelID: &other.ID, Content: "Не передавать чужой канал", Status: store.PostStatusPublished},
		{ChannelID: &channel.ID, Content: "Не передавать секретный черновик", Status: store.PostStatusDraft},
	} {
		post.UserID, post.WorkspaceID, post.Title, post.Format = "description-owner", workspace.ID, "Тест", store.FormatMarkdown
		if post.Status == store.PostStatusPublished {
			createDeliveredDiscoveryTestPost(t, storage, post)
			continue
		}
		if _, err := storage.CreatePost(t.Context(), post); err != nil {
			t.Fatal(err)
		}
	}
	dirty, err := storage.CreatePost(t.Context(), store.Post{UserID: "description-owner", WorkspaceID: workspace.ID, ChannelID: &channel.ID,
		Title: "Опубликованный", Content: "Реально отправленный текст", Format: store.FormatMarkdown})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ClaimForPublishing(t.Context(), dirty.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.MarkPublished(t.Context(), dirty.ID, "dirty-discovery-style", ""); err != nil {
		t.Fatal(err)
	}
	unpublished := "Не передавать неотправленные локальные изменения"
	if _, err := storage.UpdatePost(t.Context(), dirty.ID, store.PostChanges{Content: &unpublished}); err != nil {
		t.Fatal(err)
	}
	kit, err := storage.GetWorkspaceBrandKit(t.Context(), "description-owner", workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.UpdateWorkspaceBrandKit(t.Context(), "description-owner", workspace.ID, store.WorkspaceBrandKitUpdate{
		ExpectedVersion: kit.Version, BrandProfile: store.BrandProfile{
			Tone: "Простой", Audience: "Любители котов", ForbiddenWords: []string{"хайп"},
			CTA: "Подписывайтесь", VisualStyle: "Синяя графика", ExamplePosts: []string{"Сохранённый пример бренда"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	quotaCalls := 0
	_, err = application.DiscoverContentForWorkspaceWithBeforeGenerate(t.Context(), "description-owner", workspace.ID, &channel.ID,
		openairesearch.DiscoverContentRequest{
			ContentKind: "meme", ChannelTitle: "Injected", RecentPosts: []openairesearch.DiscoveryPublishedPost{{Text: "Injected private data"}},
			ChannelPosts: []openairesearch.ChannelPostStyle{{Text: "Injected writing style", Format: store.FormatHTML, CharacterCount: 3000}},
			ExamplePosts: []string{"Injected style example"}, ExamplePostsOverride: true, CTA: "Injected CTA", VisualStyle: "Injected visuals",
			Tone: "Injected tone", Audience: "Injected audience", ForbiddenWords: []string{"Injected forbidden word"},
		}, func() error {
			quotaCalls++
			if len(fake.requests) != 0 {
				t.Fatal("provider started before the quota reservation")
			}
			return nil
		})
	if err != nil || quotaCalls != 1 || len(fake.requests) != 1 {
		t.Fatalf("discovery/quota calls: quota=%d requests=%#v err=%v", quotaCalls, fake.requests, err)
	}
	request := fake.requests[0]
	if request.Topic != channel.Title || request.ChannelTitle != channel.Title || request.Tone != "Простой" || request.Audience != "Любители котов" || len(request.ForbiddenWords) != 1 || request.ForbiddenWords[0] != "хайп" {
		t.Fatalf("owned channel/brand context not applied: %#v", request)
	}
	if request.CTA != "Подписывайтесь" || request.VisualStyle != "Синяя графика" || request.ExamplePostsOverride ||
		len(request.ExamplePosts) != 1 || request.ExamplePosts[0] != "Сохранённый пример бренда" {
		t.Fatalf("workspace policy/example defaults were overwritten or got style priority: %#v", request)
	}
	if utf8.RuneCountInString(request.ChannelDescription) != openairesearch.MaxDiscoveryDescriptionRunes || !utf8.ValidString(request.ChannelDescription) || len(request.RecentPosts) != openairesearch.MaxDiscoverySamples {
		t.Fatalf("unbounded channel context: %#v", request)
	}
	for index, sample := range request.RecentPosts {
		if utf8.RuneCountInString(sample.Text) != openairesearch.MaxDiscoverySampleRunes || !strings.HasPrefix(sample.Text, fmt.Sprint(4-index)) || strings.Contains(sample.Text, "Не передавать") || strings.Contains(sample.Text, "Injected") || len(sample.Media) != 0 || sample.Format != store.FormatMarkdown || sample.CharacterCount != 901 {
			t.Fatalf("unscoped/unbounded sample %d: %#v", index, sample)
		}
	}
	if len(request.ChannelPosts) != openairesearch.MaxChannelStylePosts {
		t.Fatalf("writing style sample count = %d", len(request.ChannelPosts))
	}
	for index, sample := range request.ChannelPosts {
		if sample.Text != fmt.Sprint(4-index)+strings.Repeat("я", 900) || sample.CharacterCount != 901 || sample.Format != store.FormatMarkdown {
			t.Fatalf("writing style used truncated discovery text or unowned/unpublished context: %#v", sample)
		}
	}
	saved, err := storage.GetChannelForWorkspace(t.Context(), "description-owner", workspace.ID, channel.ID)
	if err != nil || saved.Description != channel.Description {
		t.Fatal("discovery changed stored channel metadata")
	}
	_, err = application.DiscoverContentForWorkspaceWithBeforeGenerate(t.Context(), "description-owner", workspace.ID, &other.ID,
		openairesearch.DiscoverContentRequest{}, nil)
	if err != nil || len(fake.requests) != 2 {
		t.Fatalf("second channel discovery failed: %#v err=%v", fake.requests, err)
	}
	otherRequest := fake.requests[1]
	if otherRequest.ChannelTitle != other.Title || len(otherRequest.ChannelPosts) != 1 || len(otherRequest.RecentPosts) != 1 ||
		otherRequest.ChannelPosts[0].Text != "Не передавать чужой канал" || otherRequest.RecentPosts[0].Text != "Не передавать чужой канал" {
		t.Fatalf("channel A writing style crossed into channel B: %#v", otherRequest)
	}
}

func TestContentDiscoveryChannelTemplatePreservesEditorialPolicyAndStylePriority(t *testing.T) {
	fake := &discoveryTestClient{}
	application, storage, workspace := newChannelDescriptionFixture(t, fake)
	activatePaidWorkspaceForTest(t, storage, "description-owner", workspace.ID)
	channel, err := storage.CreateChannel(t.Context(), store.Channel{UserID: "description-owner", WorkspaceID: workspace.ID,
		VerifiedMAXOwnerID: "100", MAXChatID: "-140", Title: "Авторский канал", IsChannel: true, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	longText := "<b>Авторский пример</b>\n\n" + strings.Repeat("Длинный авторский текст. ", 90)
	createDeliveredDiscoveryTestPost(t, storage, store.Post{UserID: "description-owner", WorkspaceID: workspace.ID, ChannelID: &channel.ID,
		Title: "Текст канала", Content: longText, Format: store.FormatHTML})
	kit, err := storage.GetWorkspaceBrandKit(t.Context(), "description-owner", workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.UpdateWorkspaceBrandKit(t.Context(), "description-owner", workspace.ID, store.WorkspaceBrandKitUpdate{
		ExpectedVersion: kit.Version, BrandProfile: store.BrandProfile{Tone: "Базовый", Audience: "Базовая аудитория", CTA: "Базовый CTA",
			VisualStyle: "Базовая графика", ForbiddenWords: []string{"Базовое слово"}, ExamplePosts: []string{"Базовый пример"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.CreateChannelTemplate(t.Context(), "description-owner", workspace.ID, store.ChannelTemplateCreate{
		ChannelID: &channel.ID, Name: "Авторский", BrandProfile: store.BrandProfile{Tone: "Авторский спокойный", Audience: "Постоянные читатели",
			CTA: "Обсудите в комментариях", VisualStyle: "Карандашные рисунки", ForbiddenWords: []string{"кликбейт"}, ExamplePosts: []string{"Образец автора"}},
	}); err != nil {
		t.Fatal(err)
	}
	_, err = application.DiscoverContentForWorkspaceWithBeforeGenerate(t.Context(), "description-owner", workspace.ID, &channel.ID,
		openairesearch.DiscoverContentRequest{}, nil)
	if err != nil || len(fake.requests) != 1 {
		t.Fatalf("template discovery failed: %#v err=%v", fake.requests, err)
	}
	request := fake.requests[0]
	if request.Tone != "Авторский спокойный" || request.Audience != "Постоянные читатели" || request.CTA != "Обсудите в комментариях" ||
		request.VisualStyle != "Карандашные рисунки" || strings.Join(request.ForbiddenWords, ",") != "кликбейт" ||
		strings.Join(request.ExamplePosts, ",") != "Образец автора" || !request.ExamplePostsOverride {
		t.Fatalf("channel template lost policy fields or explicit style priority: %#v", request)
	}
	if len(request.ChannelPosts) != 1 || request.ChannelPosts[0].Format != store.FormatHTML ||
		utf8.RuneCountInString(request.ChannelPosts[0].Text) > openairesearch.MaxChannelStyleSampleRunes ||
		!strings.HasPrefix(longText, request.ChannelPosts[0].Text) || request.ChannelPosts[0].CharacterCount != utf8.RuneCountInString(strings.TrimSpace(longText)) ||
		request.ChannelPosts[0].CharacterCount <= utf8.RuneCountInString(request.ChannelPosts[0].Text) {
		t.Fatalf("template style lost full original length, bounded text or HTML format: %#v", request.ChannelPosts)
	}
}

func createDeliveredDiscoveryTestPost(t *testing.T, storage *store.Store, post store.Post) store.Post {
	t.Helper()
	post.Status = store.PostStatusDraft
	created, err := storage.CreatePost(t.Context(), post)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ClaimForPublishing(t.Context(), created.ID); err != nil {
		t.Fatal(err)
	}
	delivered, err := storage.MarkPublished(t.Context(), created.ID, fmt.Sprintf("delivered-discovery-%d", created.ID), "")
	if err != nil {
		t.Fatal(err)
	}
	return delivered
}

func TestContentDiscoveryRejectsInvalidAndForeignTargetsBeforeQuota(t *testing.T) {
	fake := &discoveryTestClient{}
	application, _, workspace := newChannelDescriptionFixture(t, fake)
	missingID := int64(999999)
	for _, test := range []struct {
		request   openairesearch.DiscoverContentRequest
		channelID *int64
	}{
		{request: openairesearch.DiscoverContentRequest{}},
		{request: openairesearch.DiscoverContentRequest{Topic: "Коты", ContentKind: "unsupported"}},
		{request: openairesearch.DiscoverContentRequest{Topic: "Коты"}, channelID: &missingID},
	} {
		charged := false
		_, err := application.DiscoverContentForWorkspaceWithBeforeGenerate(t.Context(), "description-owner", workspace.ID, test.channelID,
			test.request, func() error { charged = true; return nil })
		if err == nil || charged || len(fake.requests) != 0 {
			t.Fatalf("invalid/foreign target consumed upstream/quota: charged=%v requests=%d err=%v", charged, len(fake.requests), err)
		}
	}
	quotaErr := errors.New("quota rejected")
	_, err := application.DiscoverContentForWorkspaceWithBeforeGenerate(t.Context(), "description-owner", workspace.ID, nil,
		openairesearch.DiscoverContentRequest{Topic: "Коты"}, func() error { return quotaErr })
	if !errors.Is(err, quotaErr) || len(fake.requests) != 0 {
		t.Fatalf("quota denial reached provider: requests=%d err=%v", len(fake.requests), err)
	}
}

func TestContentDiscoverySupportsChannelOnlyWithOneCharacterTitle(t *testing.T) {
	fake := &discoveryTestClient{}
	application, storage, workspace := newChannelDescriptionFixture(t, fake)
	activatePaidWorkspaceForTest(t, storage, "description-owner", workspace.ID)
	channel, err := storage.CreateChannel(t.Context(), store.Channel{UserID: "description-owner", WorkspaceID: workspace.ID,
		VerifiedMAXOwnerID: "100", MAXChatID: "-100", Title: "Я", IsChannel: true, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = application.DiscoverContentForWorkspaceWithBeforeGenerate(t.Context(), "description-owner", workspace.ID, &channel.ID,
		openairesearch.DiscoverContentRequest{}, nil)
	if err != nil || len(fake.requests) != 1 || fake.requests[0].Topic != "Канал Я" || fake.requests[0].ChannelTitle != "Я" {
		t.Fatalf("legal channel-only request rejected: requests=%#v err=%v", fake.requests, err)
	}
}

func TestContentDiscoveryAutoPassesOnlyOwnedMediaTypesForPostsWithoutCaptions(t *testing.T) {
	fake := &discoveryTestClient{}
	application, storage, workspace := newChannelDescriptionFixture(t, fake)
	activatePaidWorkspaceForTest(t, storage, "description-owner", workspace.ID)
	channel, err := storage.CreateChannel(t.Context(), store.Channel{UserID: "description-owner", WorkspaceID: workspace.ID,
		VerifiedMAXOwnerID: "100", MAXChatID: "-100", Title: "Канал", Description: "Юмор и короткие ролики", IsChannel: true, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	post, err := storage.CreatePost(t.Context(), store.Post{UserID: "description-owner", WorkspaceID: workspace.ID, ChannelID: &channel.ID, Title: "Без подписи"})
	if err != nil {
		t.Fatal(err)
	}
	for _, media := range []struct{ kind, key, mime string }{{store.PostAttachmentVideo, "private-video-key.mp4", "video/mp4"}, {store.PostAttachmentImage, "private-image-key.jpg", "image/jpeg"}} {
		reservation, err := storage.ReserveMedia(t.Context(), "description-owner", media.key, 100, store.MediaLimits{MaxFiles: 10, MaxBytes: 10000}, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		if err = storage.CompleteMediaReservation(t.Context(), reservation, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		post, err = storage.AddPostAttachmentForUser(t.Context(), "description-owner", post.ID, store.PostAttachment{Type: media.kind, StorageKey: media.key, MIMEType: media.mime, SizeBytes: 100, ProviderToken: "private-upload-token", Position: -1})
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err = storage.ClaimForPublishing(t.Context(), post.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = storage.MarkPublished(t.Context(), post.ID, "media-only-message", ""); err != nil {
		t.Fatal(err)
	}
	_, err = application.DiscoverContentForWorkspaceWithBeforeGenerate(t.Context(), "description-owner", workspace.ID, &channel.ID, openairesearch.DiscoverContentRequest{}, nil)
	if err != nil || len(fake.requests) != 1 {
		t.Fatalf("media-only channel discovery failed: requests=%#v err=%v", fake.requests, err)
	}
	request := fake.requests[0]
	if request.ContentKind != "auto" || request.ChannelDescription != channel.Description || len(request.RecentPosts) != 1 || request.RecentPosts[0].Text != "" || len(request.RecentPosts[0].Media) != 2 {
		t.Fatalf("auto/media-only context lost: %#v", request)
	}
	encoded, err := json.Marshal(request.RecentPosts)
	if err != nil || strings.Contains(string(encoded), "private-") || strings.Contains(string(encoded), "/media/") || strings.Contains(string(encoded), "http") || !strings.Contains(string(encoded), `"type":"video","count":1`) || !strings.Contains(string(encoded), `"type":"image","count":1`) {
		t.Fatalf("media credentials leaked or real type/count lost: %s err=%v", encoded, err)
	}
}
