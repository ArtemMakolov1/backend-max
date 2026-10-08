package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

func historyPreviewReviewItem(mid string, now time.Time) MAXHistoryItem {
	sender := false
	return MAXHistoryItem{Title: "Saved original title", Content: "Saved original content", MessageID: mid,
		URL: "https://max.ru/channel/" + mid, PublishedAt: now.Add(-time.Hour), SenderIsBot: &sender,
		RoundTrip: false, Raw: json.RawMessage(`{}`), Attachments: []MAXHistoryAttachment{{
			Type: PostAttachmentImage, ProviderToken: "known-image-token", RemoteURL: "https://media.max.ru/known.jpg",
			ProviderMeta: json.RawMessage(`{}`),
		}},
	}
}

func seedHistoryPreviewReview(t *testing.T, s *Store, workspace Workspace, channel Channel, item MAXHistoryItem, now time.Time) Post {
	t.Helper()
	claim, err := s.ClaimMAXHistoryImport(t.Context(), "test-owner", workspace.ID, channel.ID, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApplyMAXHistoryPage(t.Context(), "test-owner", workspace.ID, claim.Generation, []MAXHistoryItem{item}, nil, true, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	return maxHistoryPostByMessageID(t, s, workspace.ID, item.MessageID)
}

func TestMAXHistoryEnrichmentKeepsSavedPublicationAndKnownMedia(t *testing.T) {
	s, workspace, channel := openMAXHistoryStoreTest(t, "history-preview-enrichment", 1)
	now := time.Now().UTC().Truncate(time.Microsecond)
	item := historyPreviewReviewItem("mid.preview-preserve", now)
	before := seedHistoryPreviewReview(t, s, workspace, channel, item, now)
	if len(before.Attachments) != 1 || before.MAXHistoryAttachmentsComplete {
		t.Fatal("fixture must be an incomplete imported original with one known image")
	}

	incoming := item
	incoming.Title = "Different remote title"
	incoming.Content = "Different remote content"
	incoming.PublishedAt = now.Add(-30 * time.Minute)
	incoming.RoundTrip = true
	sender := true
	incoming.SenderIsBot = &sender
	incoming.Attachments = []MAXHistoryAttachment{
		{Type: PostAttachmentImage, ProviderToken: "known-image-token", RemoteURL: "https://media.max.ru/changed-known.jpg", ProviderMeta: json.RawMessage(`{}`)},
		{Type: PostAttachmentVideo, ProviderToken: "missing-video-token", RemoteURL: "https://v.oneme.ru/missing.mp4", ProviderMeta: json.RawMessage(`{}`)},
		{Type: PostAttachmentImage, ProviderToken: "known-image-token", RemoteURL: "https://media.max.ru/duplicate.jpg", ProviderMeta: json.RawMessage(`{}`)},
	}
	for run := 0; run < 2; run++ {
		claim, err := s.ClaimMAXHistoryImport(t.Context(), "history-editor", workspace.ID, channel.ID, now.Add(time.Duration(3+run*2)*time.Second), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.ApplyMAXHistoryPage(t.Context(), "history-editor", workspace.ID, claim.Generation, []MAXHistoryItem{incoming}, nil, true, now.Add(time.Duration(4+run*2)*time.Second)); err != nil {
			t.Fatal(err)
		}
		after, err := s.GetPostForWorkspace(t.Context(), "history-viewer", workspace.ID, before.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(after.Attachments) != 2 {
			t.Fatalf("run %d gallery has %d entries; missing media should be added once", run, len(after.Attachments))
		}
		if !reflect.DeepEqual(after.Attachments[0], before.Attachments[0]) {
			t.Fatal("existing media ID, URL, token, order or metadata was overwritten")
		}
		added := after.Attachments[1]
		if added.ID <= 0 || added.Position != 1 || added.Type != PostAttachmentVideo || added.ProviderToken != "missing-video-token" || added.URL != "https://v.oneme.ru/missing.mp4" || added.Source != PostAttachmentSourceMAXHistory {
			t.Fatalf("wrong added preview: %#v", added)
		}
		// Compare every other field, including content/title, publication root/time,
		// compatibility image URL, sender and the independent read-only guard.
		after.Attachments = before.Attachments
		if !reflect.DeepEqual(after, before) {
			t.Fatal("enrichment changed the saved publication rather than only missing previews")
		}
		replacement := "Attempt to edit a read-only original"
		if _, err = s.UpdatePostForWorkspaceIfUnchanged(t.Context(), "history-editor", workspace.ID, after, PostChanges{Content: &replacement}); !errors.Is(err, ErrConflict) {
			t.Fatalf("incomplete imported original became editable: %v", err)
		}
	}
}

func TestMAXHistoryEnrichmentExcludesCompleteManagedBusyAndChangedRoots(t *testing.T) {
	for _, kind := range []string{"complete", "managed", "publishing", "republished", "other-channel", "other-workspace"} {
		t.Run(kind, func(t *testing.T) {
			s, workspace, channel := openMAXHistoryStoreTest(t, "history-preview-excluded-"+kind, 1)
			now := time.Now().UTC().Truncate(time.Microsecond)
			item := historyPreviewReviewItem("mid.preview-excluded", now)
			if kind == "complete" {
				item.RoundTrip = true
			}
			var before Post
			if kind == "managed" {
				var err error
				published := item.PublishedAt
				before, err = s.CreatePostForWorkspace(t.Context(), "test-owner", workspace.ID, Post{Title: item.Title, Content: item.Content, Status: PostStatusPublished, ChannelID: &channel.ID, MAXMessageID: item.MessageID, PublishedAt: &published})
				if err != nil {
					t.Fatal(err)
				}
			} else {
				before = seedHistoryPreviewReview(t, s, workspace, channel, item, now)
			}
			if kind == "publishing" || kind == "republished" {
				column, value := "status", PostStatusPublishing
				if kind == "republished" {
					column, value = "max_message_id", "mid.new-publication"
				}
				if _, err := s.db.ExecContext(t.Context(), "UPDATE posts SET "+column+"=$1 WHERE id=$2", value, before.ID); err != nil {
					t.Fatal(err)
				}
				var err error
				before, err = s.GetPostForWorkspace(t.Context(), "test-owner", workspace.ID, before.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			incoming := item
			incoming.Attachments = append(append([]MAXHistoryAttachment(nil), item.Attachments...), MAXHistoryAttachment{Type: PostAttachmentVideo, ProviderToken: "should-not-add", RemoteURL: "https://media.max.ru/should-not-add.mp4", ProviderMeta: json.RawMessage(`{}`)})
			candidateWorkspace, candidateChannel := workspace.ID, channel.ID
			if kind == "other-channel" {
				candidateChannel++
			}
			if kind == "other-workspace" {
				candidateWorkspace = "unrelated-workspace"
			}
			tx, err := s.db.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			if err = enrichMAXHistoryPreviewsTx(t.Context(), tx, before.UserID, candidateWorkspace, candidateChannel, incoming, now.Add(2*time.Second)); err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(); err != nil {
				t.Fatal(err)
			}
			after, err := s.GetPostForWorkspace(t.Context(), "history-viewer", workspace.ID, before.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("%s publication was enriched despite the exclusion", kind)
			}
		})
	}
}

func TestMAXHistoryEnrichmentRecoversEarlierMissingPreviewWithoutMovingKnownMedia(t *testing.T) {
	s, workspace, channel := openMAXHistoryStoreTest(t, "history-preview-earlier-missing", 1)
	now := time.Now().UTC().Truncate(time.Microsecond)
	item := historyPreviewReviewItem("mid.preview-earlier-missing", now)
	// The first provider attachment was unavailable on the previous import.
	// Its safe subset therefore stored the second attachment at position zero.
	item.Attachments = []MAXHistoryAttachment{{Type: PostAttachmentImage, ProviderToken: "known-second-image", RemoteURL: "https://media.max.ru/known-second.jpg", ProviderMeta: json.RawMessage(`{}`)}}
	before := seedHistoryPreviewReview(t, s, workspace, channel, item, now)
	incoming := item
	incoming.Attachments = []MAXHistoryAttachment{
		{Type: PostAttachmentVideo, ProviderToken: "recovered-first-video", RemoteURL: "https://v.oneme.ru/recovered-first.mp4", ProviderMeta: json.RawMessage(`{}`)},
		item.Attachments[0],
	}
	claim, err := s.ClaimMAXHistoryImport(t.Context(), "history-editor", workspace.ID, channel.ID, now.Add(3*time.Second), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApplyMAXHistoryPage(t.Context(), "history-editor", workspace.ID, claim.Generation, []MAXHistoryItem{incoming}, nil, true, now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	after, err := s.GetPostForWorkspace(t.Context(), "history-viewer", workspace.ID, before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Attachments) != 2 {
		t.Fatalf("earlier missing preview was not recovered: gallery has %d entries", len(after.Attachments))
	}
	if !reflect.DeepEqual(after.Attachments[0], before.Attachments[0]) || after.Attachments[1].ProviderToken != "recovered-first-video" {
		t.Fatal("recovering the earlier preview moved or replaced known media")
	}
	after.Attachments = before.Attachments
	if !reflect.DeepEqual(after, before) {
		t.Fatal("recovering the earlier preview changed saved publication data")
	}
}
