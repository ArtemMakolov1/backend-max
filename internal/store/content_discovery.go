package store

import (
	"context"
	"fmt"
	"strings"
)

// PublishedContentSample exposes only bounded editorial context. Storage keys,
// upload tokens, remote URLs and attachment metadata never leave this layer.
type PublishedContentSample struct {
	Text       string
	ImageCount int
	VideoCount int
}

func (s *Store) ListContentDiscoverySamplesForWorkspace(
	ctx context.Context, actorUserID, workspaceID string, channelID int64,
) ([]PublishedContentSample, error) {
	if _, err := s.GetChannelForWorkspace(ctx, actorUserID, workspaceID, channelID); err != nil {
		return nil, err
	}
	// Include media-only publications. A bounded window leaves room to skip
	// known local edits that have not actually been delivered to MAX.
	rows, err := s.db.QueryContext(ctx, `SELECT `+postColumns+` FROM posts
WHERE workspace_id=? AND channel_id=? AND status=?
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
	samples := make([]PublishedContentSample, 0, 3)
	for _, post := range posts {
		if post.MAXPublishedFingerprint != "" && post.MAXPublishedFingerprint != publicationFingerprint(post) {
			continue
		}
		text := []rune(strings.TrimSpace(post.Content))
		if len(text) > 400 {
			text = text[:400]
		}
		sample := PublishedContentSample{Text: strings.TrimSpace(string(text))}
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
