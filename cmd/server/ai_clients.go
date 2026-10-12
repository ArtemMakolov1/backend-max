package main

import (
	"fmt"
	"net/http"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/config"
	"maxpilot/backend/internal/contentsearch"
	"maxpilot/backend/internal/openaiimg"
	"maxpilot/backend/internal/openairesearch"
	"maxpilot/backend/internal/qwenimg"
)

func newAIClients(cfg config.Config, imageHTTP, textHTTP *http.Client) (app.ImageClient, app.ResearchClient, error) {
	var images app.ImageClient
	var openAIText *openairesearch.Client
	if cfg.OpenAIAPIKey != "" {
		var err error
		openAIText, err = openairesearch.New(cfg.OpenAIAPIBaseURL, cfg.OpenAIAPIKey, cfg.OpenAIResearchModel, textHTTP)
		if err != nil {
			return nil, nil, fmt.Errorf("initialize OpenAI text/audio: %w", err)
		}
	}
	if cfg.ImageAIConfigured() {
		var err error
		switch cfg.ImageAIProvider {
		case "qwen":
			images, err = qwenimg.New(cfg.QwenAPIBaseURL, cfg.QwenAPIKey, cfg.QwenImageModel, imageHTTP)
		case "openai":
			images, err = openaiimg.New(cfg.OpenAIAPIBaseURL, cfg.OpenAIAPIKey, cfg.OpenAIImageModel, imageHTTP)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("initialize image generation: %w", err)
		}
	}
	if !cfg.TextAIConfigured() {
		return images, nil, nil
	}

	var research *openairesearch.Client
	switch cfg.TextAIProvider {
	case "deepseek":
		client, err := openairesearch.NewDeepSeek(cfg.DeepSeekAPIBaseURL, cfg.DeepSeekAPIKey, cfg.DeepSeekTextModel, textHTTP)
		if err != nil {
			return nil, nil, fmt.Errorf("initialize DeepSeek text: %w", err)
		}
		research = client
		if openAIText != nil {
			research = research.WithAudioAnalysis(openAIText)
		}
	case "openai":
		research = openAIText
	}
	searchClient, err := contentsearch.NewClient(contentsearch.Config{ExaAPIKey: cfg.ExaAPIKey, TavilyAPIKey: cfg.TavilyAPIKey})
	if err != nil {
		return nil, nil, fmt.Errorf("initialize content search: %w", err)
	}
	if searchClient.Configured() {
		research = research.WithContentSearch(searchClient)
	}
	return images, research, nil
}
