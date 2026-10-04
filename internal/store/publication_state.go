package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// publicationFingerprint tracks only fields sent to MAX. Cache tokens and
// timestamps for uploaded media are excluded: uploading the same object must
// not make a draft look edited. Gallery order is part of the delivered payload.
func publicationFingerprint(post Post) string {
	type attachment struct {
		Type       string `json:"type"`
		StorageKey string `json:"storage_key,omitempty"`
		Token      string `json:"token,omitempty"`
	}
	payload := struct {
		Content            string       `json:"content"`
		Format             string       `json:"format"`
		Image              string       `json:"image"`
		Attachments        []attachment `json:"attachments"`
		LinkButtons        []LinkButton `json:"link_buttons"`
		DisableLinkPreview bool         `json:"disable_link_preview"`
	}{Content: post.Content, Format: post.Format, Attachments: []attachment{},
		LinkButtons: []LinkButton{}, DisableLinkPreview: post.DisableLinkPreview}
	if len(post.Attachments) == 0 {
		payload.Image = post.ImagePath
		if payload.Image == "" {
			payload.Image = post.ImageURL
		}
	}
	for _, item := range post.Attachments {
		entry := attachment{Type: item.Type, StorageKey: item.StorageKey}
		if item.StorageKey == "" {
			entry.Token = item.ProviderToken
		}
		payload.Attachments = append(payload.Attachments, entry)
	}
	for _, button := range post.LinkButtons {
		payload.LinkButtons = append(payload.LinkButtons, LinkButton{
			Text: strings.TrimSpace(button.Text), URL: strings.TrimSpace(button.URL),
		})
	}
	encoded, _ := json.Marshal(payload) // Only strings, booleans and slices.
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func hydratePublicationState(post *Post) {
	post.PublicationVersionKnown = post.MAXPublishedFingerprint != ""
	post.PublicationHasChanges = post.MAXMessageID != "" &&
		(post.MAXPublishedFingerprint == "" || post.MAXPublishedFingerprint != publicationFingerprint(*post))
}

// NotifyPublicationFailure reuses the workspace inbox for publication failures.
// The failed attempt timestamp deduplicates repeated observers of the same
// failure while allowing a later failed manual attempt to generate a new item.
func (s *Store) NotifyPublicationFailure(ctx context.Context, post Post) error {
	if post.Status != PostStatusFailed || post.WorkspaceID == "" {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := notifyRolesTx(ctx, tx, post.WorkspaceID, "",
		[]string{WorkspaceRoleOwner, WorkspaceRoleEditor}, Notification{
			Kind: "publication.failed", Title: "Не удалось опубликовать пост", Body: post.LastError,
			EntityType: "post", EntityID: fmt.Sprint(post.ID),
			DedupeKey: fmt.Sprintf("publication.failed:%d:%s", post.ID, post.UpdatedAt.UTC().Format(time.RFC3339Nano)),
		}); err != nil {
		return err
	}
	return tx.Commit()
}
