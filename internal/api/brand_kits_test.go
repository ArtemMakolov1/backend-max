package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

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

	// Explicit request values win over saved defaults.
	response = performJSONRequest(handler, http.MethodPost, base+"/research/generate",
		`{"topic":"Другая тема","audience":"Явная аудитория","tone":"Явный тон",`+
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
