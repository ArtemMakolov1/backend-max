package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// An import may fill missing previews on an existing incomplete imported post,
// but must never replace its content, publication, edit guard or known media.
func enrichMAXHistoryPreviewsTx(ctx context.Context, tx *sql.Tx, ownerID, workspaceID string, channelID int64, item MAXHistoryItem, now time.Time) error {
	post, err := scanPost(tx.QueryRowContext(ctx, `SELECT `+postColumns+` FROM posts
WHERE workspace_id=$1 AND channel_id=$2 AND max_message_id=$3 FOR UPDATE`, workspaceID, channelID, item.MessageID))
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if post.Origin != PostOriginMAXHistory || post.Status != PostStatusPublished || post.MAXHistoryAttachmentsComplete || len(item.Attachments) == 0 {
		return nil
	}
	existing, err := queryPostAttachments(ctx, tx, "WHERE post_id=? ORDER BY position,id", post.ID)
	if err != nil {
		return err
	}
	positions := make(map[int]bool, len(existing))
	tokens := make(map[string]bool, len(existing))
	nextPosition := 0
	for _, attachment := range existing {
		positions[attachment.Position] = true
		tokens[attachment.Type+"\x00"+attachment.ProviderToken] = true
		if attachment.Position >= nextPosition {
			nextPosition = attachment.Position + 1
		}
	}
	limit := MaxPostAttachments
	if len(post.LinkButtons) > 0 {
		limit = MaxPostAttachmentsWithKeyboard
	}
	for _, attachment := range item.Attachments {
		if tokens[attachment.Type+"\x00"+attachment.ProviderToken] {
			continue
		}
		// A formerly missing attachment was excluded from the saved safe subset,
		// so its original index may now be occupied by different known media.
		// Append by identity rather than replacing or moving that known entry.
		if len(positions) >= limit || nextPosition >= limit {
			break
		}
		position := nextPosition
		if _, err := tx.ExecContext(ctx, `INSERT INTO max_history_post_attachments(
owner_id,workspace_id,post_id,type,position,processing_status,size_bytes,mime_type,
width,height,duration_ms,provider_token,remote_url,provider_meta,error_code,created_at,updated_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14::jsonb,'',$15,$15)
ON CONFLICT (post_id,position) DO NOTHING`, ownerID, workspaceID, post.ID, attachment.Type, position,
			AttachmentStatusReady, attachment.SizeBytes, attachment.MIMEType,
			nullableInt(attachment.Width), nullableInt(attachment.Height), nullableInt64(attachment.DurationMS),
			attachment.ProviderToken, attachment.RemoteURL, string(attachment.ProviderMeta), now); err != nil {
			return fmt.Errorf("enrich missing MAX history preview: %w", err)
		}
		positions[position] = true
		tokens[attachment.Type+"\x00"+attachment.ProviderToken] = true
		nextPosition++
	}
	return nil
}
