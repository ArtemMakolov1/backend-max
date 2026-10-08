package app

import (
	"errors"
	"testing"

	"maxpilot/backend/internal/store"
)

func TestMAXVideoAttachmentRequiresPublishedExactUnambiguousTarget(t *testing.T) {
	channelID := int64(7)
	post := store.Post{Status: store.PostStatusPublished, ChannelID: &channelID, MAXMessageID: "root",
		Attachments: []store.PostAttachment{{ID: 1, Type: "video", Source: "upload", ProviderToken: "cached-token"}}}
	if _, err := maxVideoAttachment(post, 1); err != nil {
		t.Fatalf("published upload = %v", err)
	}
	for _, mutate := range []func(*store.Post){
		func(p *store.Post) { p.Status = store.PostStatusDraft },
		func(p *store.Post) { p.MAXMessageID = "" },
		func(p *store.Post) { p.ChannelID = nil },
		func(p *store.Post) {
			p.Attachments = []store.PostAttachment{{ID: 1, Type: "image", Source: "max_history", ProviderToken: "token"}}
		},
		func(p *store.Post) { p.Attachments = []store.PostAttachment{{ID: 1, Type: "video", Source: "upload"}} },
	} {
		copy := post
		mutate(&copy)
		if _, err := maxVideoAttachment(copy, 1); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("ineligible video error = %v", err)
		}
	}
	post.Attachments = append(post.Attachments, store.PostAttachment{ID: 1, Type: "video", Source: "max_history", ProviderToken: "other"})
	if _, err := maxVideoAttachment(post, 1); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("ambiguous attachment ID error = %v", err)
	}
}
