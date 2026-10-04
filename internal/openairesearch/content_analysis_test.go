package openairesearch

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnalyzeContentMediaUsesLunaBytesAndNoSearchOrStorageURLs(t *testing.T) {
	var img bytes.Buffer
	if err := jpeg.Encode(&img, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil); err != nil {
		t.Fatal(err)
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer synthetic-analysis" {
			t.Error("wrong vision request")
		}
		var body struct {
			Model     string           `json:"model"`
			Reasoning reasoningOptions `json:"reasoning"`
			Input     []struct {
				Content json.RawMessage `json:"content"`
			} `json:"input"`
			Tools []json.RawMessage `json:"tools"`
			Store bool              `json:"store"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if body.Model != "gpt-6-luna" || body.Reasoning.Effort != "none" || body.Store || len(body.Tools) != 0 || len(body.Input) != 2 {
			t.Errorf("unbounded/incorrect analysis policy: %#v", body)
		}
		var parts []inputContentPart
		if json.Unmarshal(body.Input[1].Content, &parts) != nil {
			t.Error("missing multimodal parts")
			return
		}
		if len(parts) != 3 || parts[2].ImageURL != "data:image/jpeg;base64,"+base64.StdEncoding.EncodeToString(img.Bytes()) || strings.Contains(string(body.Input[1].Content), "https://") {
			t.Error("vision did not receive bounded local bytes")
		}
		_ = json.NewEncoder(w).Encode(responseEnvelope{Status: "completed", Output: []outputItem{{Type: "message", Content: []contentItem{{Type: "output_text", Text: "Короткие смешные видео о котах; лёгкий разговорный стиль."}}}}})
	}))
	defer server.Close()
	client, err := New(server.URL, "synthetic-analysis", "gpt-6-luna", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	summary, err := client.AnalyzeContentMedia(t.Context(), ContentAnalysisRequest{Images: []ContentAnalysisImage{{JPEG: img.Bytes(), Label: "Кадр видео"}}, AudioSummary: "Мяуканье и смех."})
	if err != nil || !strings.Contains(summary, "котах") || calls != 1 {
		t.Fatalf("analysis failed: %q %v calls=%d", summary, err, calls)
	}
	if _, err := client.AnalyzeContentMedia(t.Context(), ContentAnalysisRequest{Images: []ContentAnalysisImage{{JPEG: []byte("https://private.example/token")}}}); err == nil || calls != 1 {
		t.Fatal("invalid input reached provider")
	}
}

func TestAnalyzeContentAudioUsesDedicatedTextOutputAndRejectsPartialCompletion(t *testing.T) {
	wav := make([]byte, 44)
	copy(wav, "RIFF")
	copy(wav[8:], "WAVE")
	finish := "stop"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]json.RawMessage
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer synthetic-audio" || json.NewDecoder(r.Body).Decode(&payload) != nil {
			t.Error("bad audio request")
			return
		}
		if string(payload["model"]) != `"gpt-audio-1.5"` || string(payload["modalities"]) != `["text"]` || string(payload["store"]) != "false" || payload["audio"] != nil || payload["tools"] != nil {
			t.Errorf("paid audio generation/storage unexpectedly requested: %s", payload)
		}
		if !strings.Contains(string(payload["messages"]), base64.StdEncoding.EncodeToString(wav)) {
			t.Error("audio bytes missing")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": finish, "message": map[string]string{"role": "assistant", "content": "Музыка, смех и короткий диалог."}}}})
	}))
	defer server.Close()
	client, err := New(server.URL, "synthetic-audio", "gpt-6-luna", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.AnalyzeContentAudio(t.Context(), wav); err != nil {
		t.Fatal(err)
	}
	finish = "length"
	if _, err := client.AnalyzeContentAudio(t.Context(), wav); err == nil {
		t.Fatal("truncated audio analysis accepted")
	}
}
