package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/openairesearch"
	"maxpilot/backend/internal/store"
)

func TestWorkspaceBrandRoutesRBACCRUDAndOptimisticDelete(t *testing.T) {
	fixture := newWorkspaceAPIFixture(t)
	owner := brandFeatureHandler(t, fixture, fixture.app, "ws-owner")
	editor := brandFeatureHandler(t, fixture, fixture.app, "ws-editor")
	viewer := brandFeatureHandler(t, fixture, fixture.app, "ws-viewer")
	outsider := brandFeatureHandler(t, fixture, fixture.app, "ws-outsider")
	base := "/api/v1/workspaces/" + fixture.workspace.ID

	response := performJSONRequest(viewer, http.MethodGet, base+"/brand-kit", "")
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("viewer brand kit = %d %s", response.Code, response.Body.String())
	}
	var initial store.WorkspaceBrandKit
	if err := json.Unmarshal(response.Body.Bytes(), &initial); err != nil {
		t.Fatal(err)
	}
	response = performJSONRequest(viewer, http.MethodPut, base+"/brand-kit",
		`{"tone":"Viewer","expected_version":1}`)
	assertProblemCode(t, response, http.StatusForbidden, "workspace_forbidden")
	response = performJSONRequest(outsider, http.MethodGet, base+"/brand-kit", "")
	assertProblemCode(t, response, http.StatusNotFound, "not_found")

	response = performJSONRequest(editor, http.MethodPut, base+"/brand-kit",
		`{"audience":"Основатели","tone":"Деловой","cta":"Подписаться",`+
			`"forbidden_words":["хайп"],"example_posts":["Фирменный пример"],`+
			`"visual_style":"Синяя графика","expected_version":`+postID(initial.Version)+`}`)
	if response.Code != http.StatusOK {
		t.Fatalf("editor brand update = %d %s", response.Code, response.Body.String())
	}

	response = performJSONRequest(editor, http.MethodPost, base+"/channel-templates",
		`{"channel_id":`+postID(fixture.channel.ID)+`,"name":"Основной канал",`+
			`"audience":"Разработчики","tone":"Дружелюбный","cta":"Попробовать",`+
			`"forbidden_words":[],"example_posts":[],"visual_style":"Изометрия","is_default":false}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("editor create template = %d %s", response.Code, response.Body.String())
	}
	var template store.ChannelTemplate
	if err := json.Unmarshal(response.Body.Bytes(), &template); err != nil {
		t.Fatal(err)
	}
	response = performJSONRequest(owner, http.MethodDelete,
		base+"/channel-templates/"+postID(template.ID)+"?expected_version="+postID(template.Version+1), "")
	assertProblemCode(t, response, http.StatusConflict, "state_conflict")
	response = performJSONRequest(owner, http.MethodDelete,
		base+"/channel-templates/"+postID(template.ID)+"?expected_version="+postID(template.Version), "")
	if response.Code != http.StatusNoContent {
		t.Fatalf("delete template = %d %s", response.Code, response.Body.String())
	}
}

func TestWorkspaceResearchMergesChannelTemplateAndBrandContext(t *testing.T) {
	fixture := newWorkspaceAPIFixture(t)
	publishedText := strings.Repeat("Текст канала. ", 130)
	createPublishedResearchStylePost(t, fixture, fixture.workspace.ID, fixture.channel.ID, publishedText, store.FormatMarkdown)
	kit, err := fixture.storage.GetWorkspaceBrandKit(t.Context(), "ws-owner", fixture.workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fixture.storage.UpdateWorkspaceBrandKit(t.Context(), "ws-owner", fixture.workspace.ID,
		store.WorkspaceBrandKitUpdate{BrandProfile: store.BrandProfile{
			Audience: "Базовая аудитория", Tone: "Базовый тон", CTA: "Подписаться",
			ForbiddenWords: []string{"хайп"}, ExamplePosts: []string{"Фирменный пример"},
			VisualStyle: "Базовая графика",
		}, ExpectedVersion: kit.Version})
	if err != nil {
		t.Fatal(err)
	}
	template, err := fixture.storage.CreateChannelTemplate(t.Context(), "ws-owner", fixture.workspace.ID,
		store.ChannelTemplateCreate{
			ChannelID: &fixture.channel.ID, Name: "Канал", BrandProfile: store.BrandProfile{
				Audience: "Аудитория канала", Tone: "Тон канала", VisualStyle: "Стиль канала",
			},
		})
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeResearchClient{result: openairesearch.Result{Topic: "Тема исследования"}}
	application := app.New(fixture.storage, fixture.app.Media(), nil, nil, fake, fixture.logger)
	handler := brandFeatureHandler(t, fixture, application, "ws-editor")
	base := "/api/v1/workspaces/" + fixture.workspace.ID
	response := performJSONRequest(handler, http.MethodPost, base+"/research/generate",
		`{"topic":"Тема исследования","tone":"","format":"markdown","include_sources":false,`+
			`"channel_id":`+postID(fixture.channel.ID)+`}`)
	if response.Code != http.StatusOK {
		t.Fatalf("branded research = %d %s", response.Code, response.Body.String())
	}
	fake.mu.Lock()
	if len(fake.requests) != 1 {
		fake.mu.Unlock()
		t.Fatalf("research requests=%#v", fake.requests)
	}
	request := fake.requests[0]
	fake.mu.Unlock()
	if request.Audience != "Аудитория канала" || request.Tone != "Тон канала" || request.CTA != "Подписаться" ||
		request.VisualStyle != "Стиль канала" || len(request.ForbiddenWords) != 1 ||
		len(request.ExamplePosts) != 1 {
		t.Fatalf("merged research context=%#v", request)
	}
	if request.ExamplePostsOverride || len(request.ChannelPosts) != 1 ||
		utf8.RuneCountInString(request.ChannelPosts[0].Text) != openairesearch.MaxChannelStyleSampleRunes ||
		request.ChannelPosts[0].CharacterCount != utf8.RuneCountInString(strings.TrimSpace(publishedText)) ||
		request.ChannelPosts[0].Format != store.FormatMarkdown {
		t.Fatalf("direct channel selection lost authoritative writing style: %#v", request)
	}

	// Explicit request values win over saved defaults.
	response = performJSONRequest(handler, http.MethodPost, base+"/research/generate",
		`{"topic":"Другая тема","audience":"Явная аудитория","tone":"Явный тон",`+
			`"forbidden_words":[],"example_posts":["Явный пример"],`+
			`"format":"markdown","include_sources":false,"channel_template_id":`+postID(template.ID)+`}`)
	if response.Code != http.StatusOK {
		t.Fatalf("explicit branded research = %d %s", response.Code, response.Body.String())
	}
	fake.mu.Lock()
	request = fake.requests[len(fake.requests)-1]
	fake.mu.Unlock()
	if request.Audience != "Явная аудитория" || request.Tone != "Явный тон" {
		t.Fatalf("explicit context did not win: %#v", request)
	}
	if !request.ExamplePostsOverride || len(request.ExamplePosts) != 1 || request.ExamplePosts[0] != "Явный пример" ||
		request.ForbiddenWords == nil || len(request.ForbiddenWords) != 0 || len(request.ChannelPosts) != 1 ||
		request.ChannelPosts[0].CharacterCount != utf8.RuneCountInString(strings.TrimSpace(publishedText)) {
		t.Fatalf("template-selected channel or explicit example/array precedence lost: %#v", request)
	}

	foreignWorkspace, err := fixture.storage.CreateWorkspace(t.Context(), "ws-owner", store.Workspace{Name: "Foreign"})
	if err != nil {
		t.Fatal(err)
	}
	foreignTemplate, err := fixture.storage.CreateChannelTemplate(t.Context(), "ws-owner", foreignWorkspace.ID,
		store.ChannelTemplateCreate{Name: "Foreign", BrandProfile: store.BrandProfile{Tone: "Foreign"}})
	if err != nil {
		t.Fatal(err)
	}
	response = performJSONRequest(handler, http.MethodPost, base+"/research/generate",
		`{"topic":"Чужой шаблон","tone":"Тон","format":"markdown","include_sources":false,`+
			`"channel_template_id":`+postID(foreignTemplate.ID)+`}`)
	assertProblemCode(t, response, http.StatusNotFound, "not_found")
}

func createPublishedResearchStylePost(t *testing.T, fixture workspaceAPIFixture, workspaceID string, channelID int64, text, format string) store.Post {
	t.Helper()
	post, err := fixture.storage.CreatePostForWorkspace(t.Context(), "ws-owner", workspaceID,
		store.Post{Title: "Опубликованный пример", Content: text, Format: format, ChannelID: &channelID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.storage.ClaimForPublishing(t.Context(), post.ID); err != nil {
		t.Fatal(err)
	}
	post, err = fixture.storage.MarkPublished(t.Context(), post.ID, "research-style-"+postID(post.ID), "")
	if err != nil {
		t.Fatal(err)
	}
	return post
}

func TestWorkspaceResearchStyleUsesOnlyThreeRecentPublishedOwnChannelTexts(t *testing.T) {
	fixture := newWorkspaceAPIFixture(t)
	texts := []string{"Старый пример", "Третий пример", "<b>Второй пример</b>", strings.Repeat("🙂", 1800)}
	formats := []string{store.FormatMarkdown, store.FormatMarkdown, store.FormatHTML, store.FormatMarkdown}
	for i := range texts {
		createPublishedResearchStylePost(t, fixture, fixture.workspace.ID, fixture.channel.ID, texts[i], formats[i])
	}
	dirty := createPublishedResearchStylePost(t, fixture, fixture.workspace.ID, fixture.channel.ID, "Ранее опубликованный текст", store.FormatMarkdown)
	unpublished := "Неотправленный локальный черновик"
	if _, err := fixture.storage.UpdatePost(t.Context(), dirty.ID, store.PostChanges{Content: &unpublished}); err != nil {
		t.Fatal(err)
	}
	foreignWorkspace, err := fixture.storage.CreateWorkspace(t.Context(), "ws-owner", store.Workspace{Name: "Other style"})
	if err != nil {
		t.Fatal(err)
	}
	foreignChannel, err := fixture.storage.CreateChannelForWorkspace(t.Context(), "ws-owner", foreignWorkspace.ID, store.Channel{
		VerifiedMAXOwnerID: "other-owner", MAXChatID: "-991002", Title: "Чужой канал", IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	createPublishedResearchStylePost(t, fixture, foreignWorkspace.ID, foreignChannel.ID, "Не брать чужой стиль", store.FormatMarkdown)
	fake := &fakeResearchClient{result: openairesearch.Result{Topic: "Тема исследования"}}
	application := app.New(fixture.storage, fixture.app.Media(), nil, nil, fake, fixture.logger)
	handler := brandFeatureHandler(t, fixture, application, "ws-editor")
	response := performJSONRequest(handler, http.MethodPost, "/api/v1/workspaces/"+fixture.workspace.ID+"/research/generate",
		`{"topic":"Тема исследования","tone":"Тон","format":"markdown","example_posts":[],"channel_id":`+postID(fixture.channel.ID)+`}`)
	if response.Code != http.StatusOK {
		t.Fatalf("own channel style research = %d %s", response.Code, response.Body.String())
	}
	fake.mu.Lock()
	request := fake.requests[0]
	fake.mu.Unlock()
	if len(request.ChannelPosts) != 3 || request.ExamplePosts == nil || len(request.ExamplePosts) != 0 || request.ExamplePostsOverride {
		t.Fatalf("three-sample bound or explicit empty example semantics lost: %#v", request)
	}
	for i, style := range request.ChannelPosts {
		source := texts[len(texts)-1-i]
		wantText := []rune(source)
		if len(wantText) > openairesearch.MaxChannelStyleSampleRunes {
			wantText = wantText[:openairesearch.MaxChannelStyleSampleRunes]
		}
		if style.Text != string(wantText) || style.Format != formats[len(formats)-1-i] || style.CharacterCount != utf8.RuneCountInString(source) {
			t.Fatalf("published sample %d lost order, full Unicode length or format: %#v", i, style)
		}
	}
}

func TestWorkspaceResearchRejectsForeignStyleContextBeforeProviderAndQuota(t *testing.T) {
	fake := &fakeResearchClient{result: openairesearch.Result{Topic: "Тема исследования"}}
	options := testAILimitOptions()
	options.MonthlyPlanEnforcement, options.ResearchPerMinute = true, 1
	server, storage, raw := newAIQuotaTestServer(t, nil, fake, options, "style-owner", "style-foreign")
	server.now = func() time.Time { return time.Now().UTC() }
	handler := withTestSession(t, storage, raw, "style-owner")
	workspaceID := personalWorkspaceIDForTest(t, storage, "style-owner")
	foreignWorkspaceID := personalWorkspaceIDForTest(t, storage, "style-foreign")
	ownChannel, err := storage.CreateChannelForWorkspace(t.Context(), "style-owner", workspaceID, store.Channel{
		VerifiedMAXOwnerID: "style-owner", MAXChatID: "-991101", Title: "Свой стиль", IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	foreignChannel, err := storage.CreateChannelForWorkspace(t.Context(), "style-foreign", foreignWorkspaceID, store.Channel{
		VerifiedMAXOwnerID: "style-foreign", MAXChatID: "-991102", Title: "Чужой стиль", IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ownTemplate, err := storage.CreateChannelTemplate(t.Context(), "style-owner", workspaceID,
		store.ChannelTemplateCreate{ChannelID: &ownChannel.ID, Name: "Свой шаблон", BrandProfile: store.BrandProfile{Tone: "Тон"}})
	if err != nil {
		t.Fatal(err)
	}
	foreignTemplate, err := storage.CreateChannelTemplate(t.Context(), "style-foreign", foreignWorkspaceID,
		store.ChannelTemplateCreate{ChannelID: &foreignChannel.ID, Name: "Чужой шаблон", BrandProfile: store.BrandProfile{Tone: "Тон"}})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/workspaces/" + workspaceID + "/research/generate"
	for _, selection := range []string{
		fmt.Sprintf(`"channel_id":%d`, foreignChannel.ID),
		fmt.Sprintf(`"channel_template_id":%d`, foreignTemplate.ID),
		fmt.Sprintf(`"channel_id":%d,"channel_template_id":%d`, foreignChannel.ID, ownTemplate.ID),
	} {
		response := performJSONRequest(handler, http.MethodPost, path,
			`{"topic":"Тема исследования","tone":"Тон","format":"markdown",`+selection+`}`)
		assertProblemCode(t, response, http.StatusNotFound, "not_found")
	}
	for _, injection := range []string{
		`"channel_posts":[{"text":"Подменённый стиль","format":"markdown","character_count":99999}]`,
		`"example_posts_override":true`,
	} {
		response := performJSONRequest(handler, http.MethodPost, path,
			`{"topic":"Тема исследования","tone":"Тон","format":"markdown",`+injection+`}`)
		assertProblemCode(t, response, http.StatusBadRequest, "invalid_json")
	}
	billing := readWorkspaceBillingForTest(t, handler, workspaceID)
	fake.mu.Lock()
	calls := len(fake.requests)
	fake.mu.Unlock()
	if calls != 0 || billingUsageQuantity(t, billing.Usage, store.UsageMetricAIResearchRequests) != 0 {
		t.Fatal("foreign or injected style context reached provider or consumed quota")
	}
	response := performJSONRequest(handler, http.MethodPost, path,
		`{"topic":"Тема исследования","tone":"Тон","format":"markdown","channel_id":`+postID(ownChannel.ID)+`}`)
	if response.Code != http.StatusOK {
		t.Fatalf("rejected selections consumed minute quota: %d %s", response.Code, response.Body.String())
	}
}

func brandFeatureHandler(
	t *testing.T, fixture workspaceAPIFixture, application *app.App, userID string,
) http.Handler {
	t.Helper()
	server := New(application, fixture.logger, "http://localhost:4321", "webhook-secret",
		AuthOptions{YandexClient: &fakeYandexOAuth{}})
	router := chi.NewRouter()
	router.Use(server.cors)
	router.Route("/api/v1", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(server.requireSession)
			r.Route("/workspaces/{workspace_id}", func(r chi.Router) {
				server.RegisterWorkspaceBrandRoutes(r)
				r.Post("/research/generate", server.generateWorkspaceResearch)
			})
		})
	})
	return withTestSession(t, fixture.storage, router, userID)
}

func TestApplyBrandProfileDefaultsPreservesExplicitArrays(t *testing.T) {
	profile := store.BrandProfile{ForbiddenWords: []string{"saved"}, ExamplePosts: []string{"saved example"}}
	request := applyBrandProfileDefaults(openairesearch.Request{
		ForbiddenWords: []string{"explicit"}, ExamplePosts: []string{"explicit example"},
	}, profile)
	if strings.Join(request.ForbiddenWords, ",") != "explicit" || strings.Join(request.ExamplePosts, ",") != "explicit example" {
		t.Fatalf("explicit arrays overwritten: %#v", request)
	}
	request = applyBrandProfileDefaults(openairesearch.Request{
		ForbiddenWords: []string{}, ExamplePosts: []string{},
	}, profile)
	if request.ForbiddenWords == nil || len(request.ForbiddenWords) != 0 ||
		request.ExamplePosts == nil || len(request.ExamplePosts) != 0 {
		t.Fatalf("explicit empty arrays overwritten: %#v", request)
	}
}

func TestWorkspaceResearchAutoToneUsesSavedDefaultsThenNeutralFallback(t *testing.T) {
	for _, test := range []struct {
		name, workspaceTone, templateTone, manualTone, wantTone string
	}{
		{name: "neutral fallback", wantTone: "Естественный и понятный"},
		{name: "workspace tone", workspaceTone: "Спокойный авторский", wantTone: "Спокойный авторский"},
		{name: "channel template tone", workspaceTone: "Тон пространства", templateTone: "Тон канала", wantTone: "Тон канала"},
		{name: "manual tone wins", workspaceTone: "Тон пространства", templateTone: "Тон канала", manualTone: "Явный дружелюбный", wantTone: "Явный дружелюбный"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newWorkspaceAPIFixture(t)
			if test.workspaceTone != "" {
				kit, err := fixture.storage.GetWorkspaceBrandKit(t.Context(), "ws-owner", fixture.workspace.ID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := fixture.storage.UpdateWorkspaceBrandKit(t.Context(), "ws-owner", fixture.workspace.ID,
					store.WorkspaceBrandKitUpdate{ExpectedVersion: kit.Version, BrandProfile: store.BrandProfile{Tone: test.workspaceTone}}); err != nil {
					t.Fatal(err)
				}
			}
			if test.templateTone != "" {
				if _, err := fixture.storage.CreateChannelTemplate(t.Context(), "ws-owner", fixture.workspace.ID,
					store.ChannelTemplateCreate{ChannelID: &fixture.channel.ID, Name: "Тон канала", BrandProfile: store.BrandProfile{Tone: test.templateTone}}); err != nil {
					t.Fatal(err)
				}
			}
			fake := &fakeResearchClient{result: openairesearch.Result{Topic: "Тема исследования"}}
			application := app.New(fixture.storage, fixture.app.Media(), nil, nil, fake, fixture.logger)
			handler := brandFeatureHandler(t, fixture, application, "ws-editor")
			body, err := json.Marshal(map[string]any{"topic": "Тема исследования", "format": "markdown", "tone": test.manualTone, "channel_id": fixture.channel.ID})
			if err != nil {
				t.Fatal(err)
			}
			response := performJSONRequest(handler, http.MethodPost, "/api/v1/workspaces/"+fixture.workspace.ID+"/research/generate", string(body))
			if response.Code != http.StatusOK {
				t.Fatalf("workspace auto tone rejected: %d %s", response.Code, response.Body.String())
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if len(fake.requests) != 1 || fake.requests[0].Tone != test.wantTone {
				t.Fatalf("tone precedence changed: %#v", fake.requests)
			}
		})
	}
}

func TestBrandRequestsDecodeEmbeddedProfileFields(t *testing.T) {
	var request updateChannelTemplateRequest
	if err := json.Unmarshal([]byte(`{
"name":"Канал","audience":"Аудитория","tone":"Тон","cta":"CTA",
"forbidden_words":["хайп"],"example_posts":["пример"],"visual_style":"стиль",
"is_default":true,"expected_version":7}`), &request); err != nil {
		t.Fatal(err)
	}
	if request.Name != "Канал" || request.Audience != "Аудитория" || request.Tone != "Тон" ||
		request.CTA != "CTA" || len(request.ForbiddenWords) != 1 || len(request.ExamplePosts) != 1 ||
		request.VisualStyle != "стиль" || !request.IsDefault || request.ExpectedVersion != 7 {
		t.Fatalf("decoded request=%#v", request)
	}
}
