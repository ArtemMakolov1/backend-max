package qwenimg

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"maxpilot/backend/internal/mediafetch"
	"maxpilot/backend/internal/openaiimg"
)

const officialImageURL = "https://dashscope-463f.oss-accelerate.aliyuncs.com/generated.png?Expires=1788339600&Signature=synthetic-private-signature"

type fetchFunc func(context.Context, string, mediafetch.Limits) (*mediafetch.Download, error)

func (f fetchFunc) Fetch(ctx context.Context, rawURL string, limits mediafetch.Limits) (*mediafetch.Download, error) {
	return f(ctx, rawURL, limits)
}

func fixturePNG(t *testing.T, width, height int) []byte {
	t.Helper()
	var result bytes.Buffer
	if err := png.Encode(&result, image.NewNRGBA(image.Rect(0, 0, width, height))); err != nil {
		t.Fatal(err)
	}
	return result.Bytes()
}

func apiFixture(t *testing.T, response http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(response)
	t.Cleanup(server.Close)
	client, err := New(server.URL, "synthetic-qwen-key", "qwen-image-3.0-pro", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func nativeImageResponse(t *testing.T, urls ...string) string {
	t.Helper()
	content := make([]map[string]string, 0, len(urls))
	for _, raw := range urls {
		content = append(content, map[string]string{"image": raw})
	}
	encoded, err := json.Marshal(map[string]any{
		"request_id": "req_123",
		"output": map[string]any{"choices": []map[string]any{{
			"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": content},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestGenerateUsesQwenContractAndIndependentBoundedImageFetcher(t *testing.T) {
	want := fixturePNG(t, 512, 512)
	apiCalls, imageCalls := 0, 0
	client := apiFixture(t, func(w http.ResponseWriter, r *http.Request) {
		apiCalls++
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/services/aigc/multimodal-generation/generation" || r.Header.Get("Authorization") != "Bearer synthetic-qwen-key" {
			t.Error("Qwen image request used incorrect route or credentials")
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if len(payload) != 3 || payload["model"] != "qwen-image-3.0-pro" {
			t.Errorf("unexpected Qwen payload: %#v", payload)
		}
		for _, unsupported := range []string{"quality", "output_format", "response_format", "prompt", "size", "n"} {
			if _, exists := payload[unsupported]; exists {
				t.Errorf("Qwen received unsupported %s", unsupported)
			}
		}
		parameters, ok := payload["parameters"].(map[string]any)
		if !ok || len(parameters) != 3 || parameters["size"] != "1024*1024" || parameters["n"] != float64(1) || parameters["watermark"] != false {
			t.Errorf("unexpected Qwen native parameters: %#v", parameters)
		}
		encoded, _ := json.Marshal(payload)
		var native generationPayload
		if err := json.Unmarshal(encoded, &native); err != nil || len(native.Input.Messages) != 1 || native.Input.Messages[0].Role != "user" || len(native.Input.Messages[0].Content) != 1 || native.Input.Messages[0].Content[0].Text != "An illustration" {
			t.Error("Qwen native message did not contain exactly one user text")
		}
		_, _ = w.Write([]byte(nativeImageResponse(t, officialImageURL)))
	})
	// Production download transport is constructed independently: it cannot
	// inherit the API client's Authorization, CookieJar or environment proxy.
	if _, ok := client.images.(*mediafetch.Fetcher); !ok || client.ImageAIProvider() != "qwen" {
		t.Fatal("generated images do not use the credential-free public media fetcher")
	}
	client.images = fetchFunc(func(ctx context.Context, rawURL string, limits mediafetch.Limits) (*mediafetch.Download, error) {
		imageCalls++
		if rawURL != officialImageURL || limits.MaxRedirects != 0 || limits.MaxBytes != maxImageBytes || limits.Timeout != imageTimeout {
			t.Error("image URL/signature or bounded no-redirect policy was changed")
		}
		return &mediafetch.Download{Body: io.NopCloser(bytes.NewReader(want)), MIMEType: "image/png", ContentLength: int64(len(want))}, nil
	})
	result, err := client.Generate(t.Context(), openaiimg.GenerateRequest{Prompt: " An illustration ", Quality: "medium"})
	if err != nil || !bytes.Equal(result.Bytes, want) || result.MIMEType != "image/png" || result.Model != "qwen-image-3.0-pro" || result.RequestID != "req_123" || apiCalls != 1 || imageCalls != 1 {
		t.Fatalf("Qwen generation failed: result=%#v err=%v api=%d images=%d", result, err, apiCalls, imageCalls)
	}
}

func TestGenerateNativeAutoSizeOmitsResolutionAndConvertsLandscape(t *testing.T) {
	for _, test := range []struct{ size, want string }{{"auto", ""}, {"1536x1024", "1536*1024"}, {"1024x1536", "1024*1536"}} {
		t.Run(test.size, func(t *testing.T) {
			client := apiFixture(t, func(w http.ResponseWriter, r *http.Request) {
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				parameters, ok := payload["parameters"].(map[string]any)
				if !ok {
					t.Error("native parameters missing")
				} else if got, exists := parameters["size"]; (test.want == "" && exists) || (test.want != "" && got != test.want) {
					t.Errorf("native size = %v, exists=%v; want=%q", got, exists, test.want)
				}
				_, _ = w.Write([]byte(`{"code":"SyntheticFailure"}`))
			})
			_, _ = client.Generate(t.Context(), openaiimg.GenerateRequest{Prompt: "Illustration", Size: test.size})
		})
	}
}

func TestValidateRejectsUnsupportedOptionsBeforeCallingProvider(t *testing.T) {
	calls := 0
	client := apiFixture(t, func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(500) })
	for _, request := range []openaiimg.GenerateRequest{
		{}, {Prompt: " "}, {Prompt: strings.Repeat("я", 2001)}, {Prompt: "invalid\xff"},
		{Prompt: "Illustration", Size: "4096x4096"}, {Prompt: "Illustration", Size: "1024*1024"},
		{Prompt: "Illustration", Quality: "low"}, {Prompt: "Illustration", Quality: "high"}, {Prompt: "Illustration", Quality: "auto"},
	} {
		if _, err := client.Generate(t.Context(), request); err == nil {
			t.Fatalf("unsupported request accepted: %#v", request)
		}
	}
	if calls != 0 {
		t.Fatal("invalid input reached paid provider")
	}
	for _, size := range []string{"", "auto", "1024x1024", "1024x1536", "1536x1024"} {
		if err := client.Validate(openaiimg.GenerateRequest{Prompt: "Illustration", Size: size}); err != nil {
			t.Fatalf("supported size %s rejected: %v", size, err)
		}
	}
}

func TestGenerateDoesNotFollowAPIKeyRedirects(t *testing.T) {
	targetCalls := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls++; w.WriteHeader(204) }))
	defer target.Close()
	client := apiFixture(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	})
	_, err := client.Generate(t.Context(), openaiimg.GenerateRequest{Prompt: "Illustration"})
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusTemporaryRedirect || targetCalls != 0 {
		t.Fatalf("Qwen API redirect was followed or accepted: %v", err)
	}
}

func TestGenerateRedactsProviderFailureBodiesAndPreservesSafeRequestID(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusOK} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			client := apiFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"request_id":"req-safe-123","code":"synthetic-qwen-key","message":"private prompt and signed URL"}`))
			})
			_, err := client.Generate(t.Context(), openaiimg.GenerateRequest{Prompt: "Illustration"})
			var apiErr *Error
			if !errors.As(err, &apiErr) || apiErr.StatusCode != status || apiErr.RequestID != "req-safe-123" || apiErr.Code != "provider_error" || strings.Contains(err.Error(), "synthetic-qwen-key") || strings.Contains(err.Error(), "private prompt") || strings.Contains(err.Error(), "signed URL") {
				t.Fatalf("provider error lost safe context or leaked body: %v", err)
			}
		})
	}
}

func TestGenerateRejectsUnboundedMalformedAndUntrustedResponsesBeforeDownload(t *testing.T) {
	for _, raw := range []string{
		strings.Repeat("x", maxResponseBytes+1), `{`, `{}`, `{"data":[]}`, `{"data":[{"b64_json":"ignored"}]}`,
		nativeImageResponse(t), nativeImageResponse(t, "https://127.0.0.1/metadata"),
		nativeImageResponse(t, "https://example.com/image.png"), nativeImageResponse(t, officialImageURL, officialImageURL),
		strings.Replace(nativeImageResponse(t, officialImageURL), `"stop"`, `"length"`, 1),
		strings.Replace(nativeImageResponse(t, officialImageURL), `"assistant"`, `"user"`, 1),
		`{"output":{"choices":[]}}`,
	} {
		client := apiFixture(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(raw)) })
		client.images = fetchFunc(func(context.Context, string, mediafetch.Limits) (*mediafetch.Download, error) {
			t.Error("invalid provider response reached image downloader")
			return nil, nil
		})
		if _, err := client.Generate(t.Context(), openaiimg.GenerateRequest{Prompt: "Illustration"}); err == nil {
			t.Fatal("malformed provider response accepted")
		}
	}
}

func TestImageURLAllowsSignedOfficialOSSAndRejectsUnsafeHosts(t *testing.T) {
	for _, raw := range []string{
		officialImageURL,
		"https://dashscope-result-sz.oss-cn-shenzhen.aliyuncs.com/result.png?Expires=1788339600",
		"https://dashscope-result-sgp.oss-ap-southeast-1.aliyuncs.com/result.png?Signature=synthetic%2Bsignature",
		"https://dashscope-result.oss-accelerate.aliyuncs.com/result.png",
	} {
		if !validImageURL(raw) {
			t.Fatalf("official image URL rejected: %s", raw)
		}
	}
	for _, raw := range []string{
		"http://dashscope-result-sz.oss-cn-shenzhen.aliyuncs.com/image.png",
		"https://dashscope-result-sz.oss-cn-shenzhen.aliyuncs.com.evil.com/image.png",
		"https://other-bucket.oss-cn-shenzhen.aliyuncs.com/image.png", "https://127.0.0.1/image.png",
		"https://dashscope-nothex.oss-accelerate.aliyuncs.com/image.png",
		"https://dashscope-463f.oss-accelerate.aliyuncs.com.evil.com/image.png",
		"https://[::1]/image.png", "https://localhost/image.png", "https://a.internal/image.png",
		"https://user:password@dashscope-result-sz.oss-cn-shenzhen.aliyuncs.com/image.png",
		"https://dashscope-result-sz.oss-cn-shenzhen.aliyuncs.com:8443/image.png",
		"https://dashscope-result-sz.oss-cn-shenzhen.aliyuncs.com/image.png#fragment",
		"https://dashscope-result-sz.oss-cn-shenzhen.aliyuncs.com/", "", strings.Repeat("x", 4097),
	} {
		if validImageURL(raw) {
			t.Fatalf("unsafe generated image URL accepted: %s", raw)
		}
	}
}

func TestDownloadRejectsBadMIMEUnboundedPixelsAndCorruptPNG(t *testing.T) {
	good := fixturePNG(t, 512, 512)
	largePixels := append([]byte(nil), good...)
	binary.BigEndian.PutUint32(largePixels[16:20], 100000)
	binary.BigEndian.PutUint32(largePixels[29:33], crc32.ChecksumIEEE(largePixels[12:29]))
	for _, test := range []struct {
		name, mime    string
		data          []byte
		contentLength int64
	}{
		{name: "non PNG MIME", mime: "image/jpeg", data: good},
		{name: "HTML body", mime: "image/png", data: []byte("<html>private error</html>")},
		{name: "empty body", mime: "image/png"},
		{name: "oversize ContentLength", mime: "image/png", data: good, contentLength: maxImageBytes + 1},
		{name: "too few pixels", mime: "image/png", data: fixturePNG(t, 1, 1)},
		{name: "decompression dimensions", mime: "image/png", data: largePixels},
		{name: "truncated PNG", mime: "image/png", data: good[:len(good)-12]},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &Client{images: fetchFunc(func(context.Context, string, mediafetch.Limits) (*mediafetch.Download, error) {
				return &mediafetch.Download{Body: io.NopCloser(bytes.NewReader(test.data)), MIMEType: test.mime, ContentLength: test.contentLength}, nil
			})}
			_, err := client.download(t.Context(), officialImageURL, "req-1")
			var apiErr *Error
			if !errors.As(err, &apiErr) || apiErr.Code != "invalid_image" || strings.Contains(err.Error(), "private error") {
				t.Fatalf("invalid generated file was accepted or exposed: %v", err)
			}
		})
	}
}

func TestDownloadFailureDoesNotExposeSignedURLOrNetworkDetails(t *testing.T) {
	client := &Client{images: fetchFunc(func(context.Context, string, mediafetch.Limits) (*mediafetch.Download, error) {
		return nil, errors.New("Get " + officialImageURL + ": private network detail")
	})}
	_, err := client.download(t.Context(), officialImageURL, "req-1")
	if err == nil || strings.Contains(err.Error(), "Signature") || strings.Contains(err.Error(), "private network") {
		t.Fatalf("download failure was not redacted: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = client.download(ctx, officialImageURL, "req-1")
	if !errors.Is(err, context.Canceled) {
		t.Fatal("download cancellation was lost")
	}
}

type oversizedImageBody struct {
	read   int64
	closed bool
}

func (b *oversizedImageBody) Read(p []byte) (int, error) {
	b.read += int64(len(p))
	clear(p)
	return len(p), nil
}

func (b *oversizedImageBody) Close() error { b.closed = true; return nil }

func TestDownloadBoundsUnknownLengthBodyAndAlwaysCloses(t *testing.T) {
	body := &oversizedImageBody{}
	client := &Client{images: fetchFunc(func(context.Context, string, mediafetch.Limits) (*mediafetch.Download, error) {
		return &mediafetch.Download{Body: body, MIMEType: "image/png", ContentLength: -1}, nil
	})}
	_, err := client.download(t.Context(), officialImageURL, "req-1")
	if err == nil || body.read != maxImageBytes+1 || !body.closed {
		t.Fatalf("unknown-length download was not bounded and closed: read=%d closed=%v err=%v", body.read, body.closed, err)
	}
}

func TestNewRejectsUnsafeBaseURLShapes(t *testing.T) {
	for _, raw := range []string{"relative", "ftp://dashscope-intl.aliyuncs.com", "https://user:password@dashscope-intl.aliyuncs.com", "https://dashscope-intl.aliyuncs.com?key=private", "https://dashscope-intl.aliyuncs.com/#fragment", "https://dashscope-intl.aliyuncs.com/compatible-mode/v1"} {
		if _, err := New(raw, "synthetic-key", "qwen-image-3.0-pro", http.DefaultClient); err == nil {
			t.Fatalf("unsafe base URL shape accepted: %s", raw)
		}
	}
}
