package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/config"
	"maxpilot/backend/internal/openairesearch"
)

type aiRoundTripFunc func(*http.Request) (*http.Response, error)

func (f aiRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testAIConfig() config.Config {
	return config.Config{
		TextAIProvider: "deepseek", DeepSeekAPIBaseURL: "https://api.deepseek.com", DeepSeekTextModel: "deepseek-flash",
		ImageAIProvider: "openai", QwenAPIBaseURL: "https://dashscope-intl.aliyuncs.com", QwenImageModel: "qwen-image-3.0-pro",
		OpenAIAPIBaseURL: "https://api.openai.com", OpenAIImageModel: "gpt-image-2", OpenAIResearchModel: "gpt-6-luna",
	}
}

func TestAIClientsKeepImageAndTextProvidersIndependent(t *testing.T) {
	for _, test := range []struct {
		name, provider, deepseekKey, openaiKey string
		wantImages, wantText                   bool
	}{
		{name: "disabled", provider: "deepseek"},
		{name: "DeepSeek text only", provider: "deepseek", deepseekKey: "synthetic-deepseek-key", wantText: true},
		{name: "OpenAI images only", provider: "deepseek", openaiKey: "synthetic-openai-key", wantImages: true},
		{name: "DeepSeek text and OpenAI images", provider: "deepseek", deepseekKey: "synthetic-deepseek-key", openaiKey: "synthetic-openai-key", wantImages: true, wantText: true},
		{name: "explicit OpenAI text", provider: "openai", deepseekKey: "synthetic-deepseek-key", openaiKey: "synthetic-openai-key", wantImages: true, wantText: true},
		{name: "OpenAI selection without key", provider: "openai", deepseekKey: "synthetic-deepseek-key"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := testAIConfig()
			cfg.TextAIProvider, cfg.DeepSeekAPIKey, cfg.OpenAIAPIKey = test.provider, test.deepseekKey, test.openaiKey
			requests := 0
			client := &http.Client{Transport: aiRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				requests++
				wantHost, wantPath, wantKey, wantModel := "api.deepseek.com", "/responses", cfg.DeepSeekAPIKey, cfg.DeepSeekTextModel
				if test.provider == "openai" {
					wantHost, wantPath, wantKey, wantModel = "api.openai.com", "/v1/responses", cfg.OpenAIAPIKey, cfg.OpenAIResearchModel
				}
				if r.URL.Host != wantHost || r.URL.Path != wantPath || r.Header.Get("Authorization") != "Bearer "+wantKey {
					t.Fatal("text request used the wrong provider endpoint or credential")
				}
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload["model"] != wantModel {
					t.Fatalf("text request used the wrong model: %v", err)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{\"content\":\"**Hello world**\"}"}]}]}`))}, nil
			})}
			images, research, err := newAIClients(cfg, client, client)
			if err != nil {
				t.Fatal(err)
			}
			if (images != nil) != test.wantImages || (research != nil) != test.wantText {
				t.Fatal("wrong image/text client availability")
			}
			if research != nil {
				formatted, err := research.(app.ContentFormatter).FormatContent(context.Background(), openairesearch.FormatRequest{Content: "Hello world", Format: "markdown"})
				if err != nil || formatted.Content != "**Hello world**" {
					t.Fatalf("text workflow failed: %v", err)
				}
			}
			wantRequests := 0
			if test.wantText {
				wantRequests = 1
			}
			if requests != wantRequests {
				t.Fatalf("requests = %d, want %d", requests, wantRequests)
			}
		})
	}
}

func TestAIClientsKeepAudioOnOpenAIWhenTextUsesDeepSeek(t *testing.T) {
	cfg := testAIConfig()
	cfg.DeepSeekAPIKey, cfg.OpenAIAPIKey = "synthetic-deepseek-key", "synthetic-openai-key"
	client := &http.Client{Transport: aiRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.openai.com" || r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer "+cfg.OpenAIAPIKey {
			t.Fatal("audio analysis left the OpenAI provider")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"Тихая музыка."}}]}`))}, nil
	})}
	_, research, err := newAIClients(cfg, client, client)
	if err != nil {
		t.Fatal(err)
	}
	wav := make([]byte, 44)
	copy(wav[:4], "RIFF")
	copy(wav[8:12], "WAVE")
	analyzer := research.(interface {
		AnalyzeContentAudio(context.Context, []byte) (string, error)
	})
	summary, err := analyzer.AnalyzeContentAudio(context.Background(), wav)
	if err != nil || summary != "Тихая музыка." {
		t.Fatalf("OpenAI audio analysis failed: %v", err)
	}
}

func TestAIClientsAttachExternalSearchToSelectedProvider(t *testing.T) {
	for _, provider := range []string{"deepseek", "openai"} {
		t.Run(provider, func(t *testing.T) {
			cfg := testAIConfig()
			cfg.TextAIProvider = provider
			cfg.DeepSeekAPIKey, cfg.OpenAIAPIKey = "synthetic-deepseek-key", "synthetic-openai-key"
			cfg.ExaAPIKey = "synthetic-exa-key"
			client := &http.Client{Transport: aiRoundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("initializing search made an outbound request")
				return nil, nil
			})}
			_, research, err := newAIClients(cfg, client, client)
			if err != nil {
				t.Fatal(err)
			}
			selected := research.(*openairesearch.Client)
			if selected.TextAIProvider() != provider || !selected.ContentSearchConfigured() {
				t.Fatal("external search was not attached to the selected text provider")
			}
		})
	}
}

func TestAIClientsUseQwenImagesWithIndependentTextAndAudio(t *testing.T) {
	for _, withKey := range []bool{false, true} {
		cfg := testAIConfig()
		cfg.ImageAIProvider = "qwen"
		cfg.OpenAIAPIKey, cfg.DeepSeekAPIKey = "synthetic-openai-key", "synthetic-deepseek-key"
		if withKey {
			cfg.QwenAPIKey = "synthetic-qwen-key"
		}
		client := &http.Client{Transport: aiRoundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("client initialization made an outbound request")
			return nil, nil
		})}
		images, research, err := newAIClients(cfg, client, client)
		if err != nil || research == nil || (images != nil) != withKey {
			t.Fatalf("Qwen image selection failed: %v", err)
		}
		if images != nil {
			provider, ok := images.(interface{ ImageAIProvider() string })
			if !ok || provider.ImageAIProvider() != "qwen" {
				t.Fatal("Qwen image selection silently used OpenAI")
			}
		}
	}
}
