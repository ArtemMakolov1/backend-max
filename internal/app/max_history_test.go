package app

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"maxpilot/backend/internal/maxclient"
	"maxpilot/backend/internal/store"
)

func TestMAXHistoryItemPreservesRoundTripSafeMediaAndLinks(t *testing.T) {
	t.Parallel()

	views := int64(23)
	item, err := maxHistoryItem(maxclient.HistoryMessage{
		MessageID: "mid.history", Text: "  Первая строка  \nвторая", TimestampMillis: 1_785_700_000_123,
		URL: "https://max.ru/channel/mid.history", Views: &views, SenderUserID: "42", SenderIsBot: true,
		Attachments: []maxclient.HistoryAttachment{
			{Type: "image", Token: "image-token", URL: "https://cdn.max.ru/image.jpg", Complete: true,
				Width: intPointer(1200), Height: intPointer(800), Raw: json.RawMessage(`{"type":"image"}`)},
			{Type: "inline_keyboard", Complete: true,
				LinkButtons: []maxclient.LinkButton{{Text: "Подробнее", URL: "https://example.com"}}},
		}, Raw: json.RawMessage(`{"body":{"mid":"mid.history"}}`),
	}, 42)
	if err != nil {
		t.Fatal(err)
	}
	if !item.RoundTrip || item.SenderIsBot == nil || !*item.SenderIsBot || item.Title != "Первая строка" ||
		item.Content != "  Первая строка  \nвторая" || len(item.Attachments) != 1 || len(item.LinkButtons) != 1 ||
		item.Attachments[0].ProviderToken != "image-token" || item.Views == nil || *item.Views != views {
		t.Fatalf("normalized item = %#v", item)
	}
}

func TestMAXHistoryItemKeepsForwardOnlyMessageReadOnly(t *testing.T) {
	t.Parallel()

	item, err := maxHistoryItem(maxclient.HistoryMessage{
		MessageID: "mid.forward", TimestampMillis: 1_785_700_000_123,
		URL: "https://max.ru/channel/mid.forward", SenderUserID: "42", SenderIsBot: true,
		Raw: json.RawMessage(`{"body":null,"link":{"type":"forward","mid":"source"}}`),
	}, 42)
	if err != nil {
		t.Fatal(err)
	}
	if item.RoundTrip || !strings.Contains(item.Content, "mid.forward") {
		t.Fatalf("forward-only item safety = %#v", item)
	}
}

func TestMAXHistoryItemKeepsUnsupportedPublicationReadOnlyWithoutPartialMedia(t *testing.T) {
	t.Parallel()

	item, err := maxHistoryItem(maxclient.HistoryMessage{
		MessageID: "mid.audio", TimestampMillis: 1_785_700_000_123,
		URL: "https://max.ru/channel/mid.audio", SenderUserID: "77",
		Attachments: []maxclient.HistoryAttachment{{Type: "audio", Token: "secret", Complete: false}},
	}, 42)
	if err != nil {
		t.Fatal(err)
	}
	if item.RoundTrip || item.SenderIsBot == nil || *item.SenderIsBot || len(item.Attachments) != 0 ||
		len(item.LinkButtons) != 0 || !strings.Contains(item.Content, "https://max.ru/channel/mid.audio") {
		t.Fatalf("read-only item = %#v", item)
	}
}

func TestMAXHistoryItemRejectsMediaWithoutSafePreviewURLFromEditableSubset(t *testing.T) {
	t.Parallel()

	item, err := maxHistoryItem(maxclient.HistoryMessage{
		MessageID: "mid.video", Text: "Видео", TimestampMillis: 1_785_700_000_123,
		Attachments: []maxclient.HistoryAttachment{{Type: "video", Token: "token", Complete: true}},
	}, 42)
	if err != nil {
		t.Fatal(err)
	}
	if item.RoundTrip || len(item.Attachments) != 0 {
		t.Fatalf("unsafe media was normalized as editable: %#v", item)
	}
}

func TestMAXHistoryItemKeepsFormattedProviderTextReadOnly(t *testing.T) {
	t.Parallel()

	item, err := maxHistoryItem(maxclient.HistoryMessage{
		MessageID: "mid.markup", Text: "Форматированный текст", TimestampMillis: 1_785_700_000_123,
		SenderUserID: "42", SenderIsBot: true,
		Raw: json.RawMessage(`{"body":{"mid":"mid.markup","markup":[{"type":"strong","from":0,"length":17}]}}`),
	}, 42)
	if err != nil {
		t.Fatal(err)
	}
	if item.RoundTrip || item.SenderIsBot == nil || !*item.SenderIsBot {
		t.Fatalf("formatted item safety = %#v", item)
	}
}

func TestMAXHistoryItemKeepsReplyAndForwardLinksReadOnly(t *testing.T) {
	t.Parallel()

	for _, linkType := range []string{"reply", "forward"} {
		t.Run(linkType, func(t *testing.T) {
			item, err := maxHistoryItem(maxclient.HistoryMessage{
				MessageID: "mid." + linkType, Text: "Связанный текст", TimestampMillis: 1_785_700_000_123,
				SenderUserID: "42", SenderIsBot: true,
				Raw: json.RawMessage(`{"body":{"mid":"mid.link"},"link":{"type":"` + linkType + `","mid":"source"}}`),
			}, 42)
			if err != nil {
				t.Fatal(err)
			}
			if item.RoundTrip {
				t.Fatalf("%s link was normalized as editable: %#v", linkType, item)
			}
		})
	}
}

func TestMAXHistoryWriteGuardsRequireCurrentBotAndCompleteAttachments(t *testing.T) {
	t.Parallel()

	trueValue, falseValue := true, false
	for _, test := range []struct {
		name string
		post store.Post
		want error
	}{
		{name: "local", post: store.Post{Origin: store.PostOriginMAXPosty}},
		{name: "current bot", post: store.Post{Origin: store.PostOriginMAXHistory, MAXSenderIsBot: &trueValue, MAXHistoryAttachmentsComplete: true}},
		{name: "another sender", post: store.Post{Origin: store.PostOriginMAXHistory, MAXSenderIsBot: &falseValue}, want: ErrConflict},
		{name: "unknown sender", post: store.Post{Origin: store.PostOriginMAXHistory}, want: ErrConflict},
		{name: "incomplete", post: store.Post{Origin: store.PostOriginMAXHistory, MAXSenderIsBot: &trueValue}, want: ErrConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := requireMAXHistoryEditable(test.post)
			if !errors.Is(err, test.want) || (test.want == nil && err != nil) {
				t.Fatalf("requireMAXHistoryEditable() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestMAXHistoryLiveAuthorshipRequiresCurrentBotIdentity(t *testing.T) {
	t.Parallel()

	current := int64(42)
	trueValue := true
	post := store.Post{
		Origin: store.PostOriginMAXHistory, MAXSenderIsBot: &trueValue,
		MAXHistoryAttachmentsComplete: true,
	}
	if err := requireMAXHistoryCurrentBotMessage(post, maxclient.Message{
		SenderUserID: "42", SenderIsBot: true,
	}, current); err != nil {
		t.Fatalf("current bot rejected: %v", err)
	}
	for _, message := range []maxclient.Message{
		{SenderUserID: "77", SenderIsBot: true},
		{SenderUserID: "42", SenderIsBot: false},
		{},
	} {
		if err := requireMAXHistoryCurrentBotMessage(post, message, current); !errors.Is(err, ErrConflict) {
			t.Fatalf("stale or missing sender accepted: %#v, %v", message, err)
		}
	}
	if err := requireMAXHistoryCurrentBotMessage(store.Post{Origin: store.PostOriginMAXPosty}, maxclient.Message{}, 0); err != nil {
		t.Fatalf("local post rejected: %v", err)
	}
}

func TestMAXHistoryPageCursorIsInclusiveAndEndsStalledBoundaryAsPartial(t *testing.T) {
	t.Parallel()

	complete, next, stalled, err := maxHistoryPageCursor(99, 1000, 0)
	if err != nil || !complete || next != nil || stalled {
		t.Fatalf("99-item cursor = complete %v next %v stalled %v err %v", complete, next, stalled, err)
	}
	complete, next, stalled, err = maxHistoryPageCursor(100, 1000, 0)
	if err != nil || complete || next == nil || *next != 1000 || stalled {
		t.Fatalf("first full cursor = complete %v next %v stalled %v err %v", complete, next, stalled, err)
	}
	complete, next, stalled, err = maxHistoryPageCursor(100, 900, 1000)
	if err != nil || complete || next == nil || *next != 900 || stalled {
		t.Fatalf("advanced cursor = complete %v next %v stalled %v err %v", complete, next, stalled, err)
	}
	complete, next, stalled, err = maxHistoryPageCursor(100, 1000, 1000)
	if err != nil || !complete || next != nil || !stalled {
		t.Fatalf("stalled cursor = complete %v next %v stalled %v err %v", complete, next, stalled, err)
	}
}

func intPointer(value int) *int { return &value }
