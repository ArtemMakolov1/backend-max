package maxclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

type fallbackRoundTripFunc func(*http.Request) (*http.Response, error)

func (f fallbackRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestGetChatByLinkFallsBackToCredentiallessPublicPage(t *testing.T) {
	t.Parallel()
	var calls []string
	transport := fallbackRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls = append(calls, request.URL.Host+request.URL.EscapedPath())
		switch {
		case request.URL.Host == "platform-api2.max.ru" && request.URL.Path == "/chats/se13549123_biz":
			if got := request.Header.Get("Authorization"); got != "bot-secret" {
				t.Errorf("initial API Authorization = %q", got)
			}
			return fallbackResponse(request, http.StatusNotFound, "application/json",
				`{"code":"chat.not.found","message":"Chat not found by link: se13549123_biz"}`), nil
		case request.URL.Host == "max.ru" && request.URL.Path == "/se13549123_biz":
			if request.URL.Scheme != "https" || request.URL.RawQuery != "" {
				t.Errorf("unsafe public URL = %s", request.URL.String())
			}
			if got := request.Header.Get("Authorization"); got != "" {
				t.Errorf("public page received Authorization = %q", got)
			}
			if got := request.Header.Get("Cookie"); got != "" {
				t.Errorf("public page received Cookie = %q", got)
			}
			return fallbackResponse(request, http.StatusOK, "text/html; charset=utf-8",
				`<!doctype html><script>window.__DATA__={"channelId":76868796016845}</script>`), nil
		case request.URL.Host == "platform-api2.max.ru" && request.URL.Path == "/chats/-76868796016845":
			if got := request.Header.Get("Authorization"); got != "bot-secret" {
				t.Errorf("numeric API Authorization = %q", got)
			}
			return fallbackResponse(request, http.StatusOK, "application/json",
				`{"chat_id":-76868796016845,"owner_id":32202189,"type":"channel","status":"active","title":"Тестовый канал","link":"https://max.ru/se13549123_biz"}`), nil
		default:
			t.Fatalf("unexpected request: %s %s", request.Method, request.URL.String())
			return nil, errors.New("unexpected request")
		}
	})
	client := mustClient(t, "https://platform-api2.max.ru", "bot-secret", &http.Client{Transport: transport})

	chat, err := client.GetChatByLink(context.Background(),
		"https://max.ru/se13549123_biz?access_token=must-not-leak#ignored")
	if err != nil {
		t.Fatal(err)
	}
	if chat.ChatID != "-76868796016845" || chat.OwnerID != "32202189" || chat.Link != "https://max.ru/se13549123_biz" {
		t.Fatalf("resolved chat = %#v", chat)
	}
	wantCalls := []string{
		"platform-api2.max.ru/chats/se13549123_biz",
		"max.ru/se13549123_biz",
		"platform-api2.max.ru/chats/-76868796016845",
	}
	if strings.Join(calls, "\n") != strings.Join(wantCalls, "\n") {
		t.Fatalf("request order = %#v, want %#v", calls, wantCalls)
	}
}

func TestGetChatByLinkPublicFallbackIsBounded(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	transport := fallbackRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		if request.URL.Host == "max.ru" {
			if got := request.Header.Get("Authorization"); got != "" {
				t.Errorf("public page received Authorization = %q", got)
			}
			return fallbackResponse(request, http.StatusOK, "text/html",
				strings.Repeat("x", maxPublicPageBytes+1)), nil
		}
		return fallbackResponse(request, http.StatusNotFound, "application/json",
			`{"code":"chat.not.found","message":"not found"}`), nil
	})
	client := mustClient(t, "https://platform-api2.max.ru", "bot-secret", &http.Client{Transport: transport})

	_, err := client.GetChatByLink(context.Background(), "se13549123_biz")
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code != "chat.not.found" || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("GetChatByLink() error = %T %v", err, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("request count = %d, want API plus bounded public fetch", calls.Load())
	}
}

func TestGetChatByLinkPublicFallbackRejectsCanonicalMismatch(t *testing.T) {
	t.Parallel()
	transport := fallbackRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch {
		case request.URL.Host == "max.ru":
			return fallbackResponse(request, http.StatusOK, "text/html", `<script>channelId:76868796016845</script>`), nil
		case request.URL.Path == "/chats/-76868796016845":
			return fallbackResponse(request, http.StatusOK, "application/json",
				`{"chat_id":-76868796016845,"type":"channel","status":"active","link":"https://max.ru/different_channel"}`), nil
		default:
			return fallbackResponse(request, http.StatusNotFound, "application/json",
				`{"code":"chat.not.found","message":"not found"}`), nil
		}
	})
	client := mustClient(t, "https://platform-api2.max.ru", "bot-secret", &http.Client{Transport: transport})

	_, err := client.GetChatByLink(context.Background(), "se13549123_biz")
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code != "chat.not.found" || !strings.Contains(err.Error(), "canonical API link") {
		t.Fatalf("GetChatByLink() error = %T %v", err, err)
	}
}

func TestGetChatByLinkPublicFallbackDoesNotFollowRedirect(t *testing.T) {
	t.Parallel()
	var redirectTargetCalls atomic.Int32
	transport := fallbackRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host == "max.ru" {
			response := fallbackResponse(request, http.StatusFound, "text/html", "")
			response.Header.Set("Location", "https://evil.example/stolen")
			return response, nil
		}
		if request.URL.Host == "evil.example" {
			redirectTargetCalls.Add(1)
			return fallbackResponse(request, http.StatusOK, "text/html", `channelId:1`), nil
		}
		return fallbackResponse(request, http.StatusNotFound, "application/json",
			`{"code":"chat.not.found","message":"not found"}`), nil
	})
	client := mustClient(t, "https://platform-api2.max.ru", "bot-secret", &http.Client{Transport: transport})

	_, err := client.GetChatByLink(context.Background(), "se13549123_biz")
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code != "chat.not.found" {
		t.Fatalf("GetChatByLink() error = %T %v", err, err)
	}
	if redirectTargetCalls.Load() != 0 {
		t.Fatal("public fallback followed a redirect")
	}
}

func TestParsePublicChannelIDValidation(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		body string
		want string
	}{
		"plain":                {body: `channelId:76868796016845`, want: "-76868796016845"},
		"quoted":               {body: `{"channelId":"76868796016845"}`, want: "-76868796016845"},
		"escaped duplicate":    {body: `\"channelId\":76868796016845; \"channelId\":76868796016845`, want: "-76868796016845"},
		"missing":              {body: `{"chatId":76868796016845}`},
		"zero":                 {body: `channelId:0`},
		"negative":             {body: `channelId:-76868796016845`},
		"overflow":             {body: `channelId:9999999999999999999`},
		"ambiguous":            {body: `channelId:1; channelId:2`},
		"identifier substring": {body: `otherchannelId:76868796016845`},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := parsePublicChannelID([]byte(test.body))
			if test.want == "" {
				if err == nil {
					t.Fatalf("parsePublicChannelID(%q) = %q, want error", test.body, got)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("parsePublicChannelID(%q) = %q, %v; want %q", test.body, got, err, test.want)
			}
		})
	}
}

func TestGetChatByLinkRejectsOversizedSlugBeforeOutboundRequest(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	transport := fallbackRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("unexpected outbound request")
	})
	client := mustClient(t, "https://platform-api2.max.ru", "bot-secret", &http.Client{Transport: transport})

	if _, err := client.GetChatByLink(context.Background(), strings.Repeat("a", 129)); err == nil {
		t.Fatal("GetChatByLink accepted an oversized slug")
	}
	if calls.Load() != 0 {
		t.Fatalf("outbound request count = %d, want zero", calls.Load())
	}
}

func fallbackResponse(request *http.Request, status int, contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}
