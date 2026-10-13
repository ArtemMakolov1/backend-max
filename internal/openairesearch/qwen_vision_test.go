package openairesearch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestQwenVisionRetriesOnlyOversizedSummaryUsingOriginalFramesAndSharedDeadline(t *testing.T) {
	for _, secondTooLong := range []bool{false, true} {
		t.Run(stringBool(secondTooLong), func(t *testing.T) {
			var requests []qwenChatPayload
			var deadlines []time.Time
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests = append(requests, assertQwenRequest(t, r, false))
				if len(requests) == 1 || secondTooLong {
					writeQwenText(t, w, strings.Repeat("я", 1201)+"invalid-generated-summary")
					return
				}
				writeQwenText(t, w, "Один краткий общий абзац о наблюдаемом визуальном стиле.")
			}))
			defer server.Close()
			transport := server.Client().Transport
			client, err := NewQwen(server.URL, "synthetic-qwen-key", "qwen3.6-plus", &http.Client{Transport: diagnosticRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				deadline, ok := r.Context().Deadline()
				if !ok {
					t.Error("vision request lost the original bounded context")
				}
				deadlines = append(deadlines, deadline)
				return transport.RoundTrip(r)
			})})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
			defer cancel()
			summary, err := client.AnalyzeContentMedia(ctx, ContentAnalysisRequest{Images: []ContentAnalysisImage{{JPEG: []byte{0xff, 0xd8, 0xff, 0xe0}, Label: "Первый кадр"}}, AudioSummary: "Короткий звуковой контекст."})
			if len(requests) != 2 || len(deadlines) != 2 || !deadlines[0].Equal(deadlines[1]) || (err != nil) != secondTooLong {
				t.Fatalf("calls=%d deadline reset or unexpected err=%v", len(requests), err)
			}
			if secondTooLong && !errors.Is(err, errContentAnalysisSummaryTooLong) {
				t.Fatalf("strict length validator was bypassed: %v", err)
			}
			if !secondTooLong && summary == "" {
				t.Error("valid aggregate summary was not returned")
			}
			if !reflect.DeepEqual(requests[0].Messages[1:], requests[1].Messages[1:]) || requests[0].MaxTokens != requests[1].MaxTokens || !strings.Contains(requests[1].Messages[0].Content.(string), "не более 800") {
				t.Error("repair changed prepared evidence or token limit")
			}
			encoded, _ := json.Marshal(requests[1])
			if strings.Contains(string(encoded), "invalid-generated-summary") {
				t.Error("rejected summary became retry context")
			}
		})
	}
}

func TestQwenVisionDoesNotRetryProviderRejectionRefusalTokenTruncationOrCancellation(t *testing.T) {
	for _, name := range []string{"http", "refusal", "truncation", "cancelled"} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				switch name {
				case "http":
					w.WriteHeader(http.StatusUnauthorized)
				case "refusal":
					_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"","refusal":"provider refusal"}}]}`))
				case "truncation":
					_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"length","message":{"role":"assistant","content":"truncated output"}}]}`))
				case "cancelled":
					cancel()
					writeQwenText(t, w, strings.Repeat("я", 1201))
				}
			}))
			defer server.Close()
			_, err := mustQwenClient(t, server).AnalyzeContentMedia(ctx, ContentAnalysisRequest{Images: []ContentAnalysisImage{{JPEG: []byte{0xff, 0xd8, 0xff, 0xe0}}}})
			if err == nil || calls != 1 {
				t.Fatalf("non-length failure retried: err=%v calls=%d", err, calls)
			}
			if name == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation was lost: %v", err)
			}
		})
	}
}
