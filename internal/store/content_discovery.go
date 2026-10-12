package store

import (
	"context"
	"fmt"
	"strings"
)

// PublishedContentSample exposes only bounded editorial context. Storage keys,
// upload tokens, remote URLs and attachment metadata never leave this layer.
type PublishedContentSample struct {
	Text           string
	Format         string
	CharacterCount int
	ImageCount     int
	VideoCount     int
}

func (s *Store) ListContentDiscoverySamplesForWorkspace(
	ctx context.Context, actorUserID, workspaceID string, channelID int64,
) ([]PublishedContentSample, error) {
	posts, err := s.listPublishedChannelContextPosts(ctx, actorUserID, workspaceID, channelID)
	if err != nil {
		return nil, err
	}
	samples := make([]PublishedContentSample, 0, 3)
	for _, post := range posts {
		if post.MAXPublishedFingerprint != "" && post.MAXPublishedFingerprint != publicationFingerprint(post) {
			continue
		}
		text := []rune(strings.TrimSpace(post.Content))
		characterCount := len(text)
		if len(text) > 400 {
			text = text[:400]
		}
		sample := PublishedContentSample{Text: strings.TrimSpace(string(text)), Format: post.Format, CharacterCount: characterCount}
		for _, attachment := range post.Attachments {
			switch attachment.Type {
			case PostAttachmentImage:
				sample.ImageCount++
			case PostAttachmentVideo:
				sample.VideoCount++
			}
		}
		if sample.ImageCount == 0 && (post.ImagePath != "" || post.ImageURL != "") {
			sample.ImageCount = 1
		}
		// Normal posts already obey MAX's twelve-attachment limit. Keep this
		// projection bounded even for older malformed imported metadata.
		sample.ImageCount = min(sample.ImageCount, MaxPostAttachments)
		sample.VideoCount = min(sample.VideoCount, MaxPostAttachments-sample.ImageCount)
		if sample.Text == "" && sample.ImageCount == 0 && sample.VideoCount == 0 {
			continue
		}
		samples = append(samples, sample)
		if len(samples) == 3 {
			break
		}
	}
	return samples, nil
}

// ListChannelStyleSamplesForWorkspace returns at most three recent published
// texts and their original lengths. Callers bound the text snippets sent to AI;
// image-only posts and unpublished local edits cannot describe writing style.
func (s *Store) ListChannelStyleSamplesForWorkspace(
	ctx context.Context, actorUserID, workspaceID string, channelID int64,
) ([]PublishedContentSample, error) {
	posts, err := s.listPublishedChannelContextPosts(ctx, actorUserID, workspaceID, channelID)
	if err != nil {
		return nil, err
	}
	samples := make([]PublishedContentSample, 0, 3)
	for _, post := range posts {
		if post.MAXPublishedFingerprint != "" && post.MAXPublishedFingerprint != publicationFingerprint(post) {
			continue
		}
		text := []rune(strings.TrimSpace(post.Content))
		if len(text) == 0 {
			continue
		}
		samples = append(samples, PublishedContentSample{Text: string(text), Format: post.Format, CharacterCount: len(text)})
		if len(samples) == 3 {
			break
		}
	}
	return samples, nil
}

func (s *Store) listPublishedChannelContextPosts(
	ctx context.Context, actorUserID, workspaceID string, channelID int64,
) ([]Post, error) {
	if _, err := s.GetChannelForWorkspace(ctx, actorUserID, workspaceID, channelID); err != nil {
		return nil, err
	}
	// Unknown imported fingerprints require the immutable original import
	// revision to prove the selected text. UpdatedAt is not proof: importing or
	// editing the local title also updates it. Unknown local deliveries are skipped.
	// A bounded window includes media-only posts and leaves room for dirty rows.
	rows, err := s.db.QueryContext(ctx, `SELECT `+postColumns+` FROM posts
WHERE workspace_id=? AND channel_id=? AND status=?
AND (max_published_fingerprint<>'' OR (
  origin='max_history' AND EXISTS (
    SELECT 1 FROM post_revisions r
    JOIN max_history_messages h ON h.workspace_id=r.workspace_id AND h.post_id=r.post_id
    WHERE r.workspace_id=posts.workspace_id AND r.post_id=posts.id AND r.revision_number=1
      AND r.snapshot->>'origin'='max_history'
      AND r.snapshot->>'content'=posts.content AND r.snapshot->>'format'=posts.format
  )
))
ORDER BY published_at DESC NULLS LAST,id DESC LIMIT 10`, workspaceID, channelID, PostStatusPublished)
	if err != nil {
		return nil, fmt.Errorf("list content discovery publications: %w", err)
	}
	defer func() { _ = rows.Close() }()
	posts := make([]Post, 0, 10)
	for rows.Next() {
		post, err := scanPost(rows)
		if err != nil {
			return nil, err
		}
		posts = append(posts, post)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := s.hydratePostAttachments(ctx, posts); err != nil {
		return nil, err
	}
	return posts, nil
}
