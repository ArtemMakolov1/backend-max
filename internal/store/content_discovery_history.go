package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"regexp"
	"strings"
	"unicode"

	"maxpilot/backend/internal/contentdedup"
)

const publishedContentIndexVersion = 1
const publishedContentBackfillPageSize = 128

// PublishedDiscoveryReference is a private editorial projection. Provider
// tokens, media/storage addresses, authors and raw revision data are excluded.
type PublishedDiscoveryReference struct {
	Title      string
	Text       string
	SourceURLs []string
}

var publishedURLPattern = regexp.MustCompile(`(?i)https?://[^\s<>"']+`)
var publishedMarkupPattern = regexp.MustCompile(`<[^>]*>|[*_~+#^\x60]`)

func boundedPublishedText(text string, limit int) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) > limit {
		runes = runes[:limit]
	}
	return string(runes)
}

// Post.Title is an editable internal label and is not part of the MAX delivery
// fingerprint. Derive event labels exclusively from confirmed published prose.
func publishedContentTitle(content string) string {
	visible := publishedURLPattern.ReplaceAllString(html.UnescapeString(content), " ")
	visible = strings.TrimSpace(publishedMarkupPattern.ReplaceAllString(visible, " "))
	if end := strings.IndexAny(visible, ".!?\n"); end >= 0 {
		visible = visible[:end]
	}
	return boundedPublishedText(strings.Join(strings.Fields(visible), " "), 200)
}

func publishedSourceURLs(content string, buttons []LinkButton) []string {
	urls := make([]string, 0)
	seen := make(map[string]bool)
	add := func(raw string) {
		value := contentdedup.CanonicalSourceURL(strings.TrimRight(html.UnescapeString(raw), ".,;:!?)]}"))
		if value != "" && !seen[value] && len(urls) < 100 {
			seen[value] = true
			urls = append(urls, value)
		}
	}
	for _, raw := range publishedURLPattern.FindAllString(content, -1) {
		add(raw)
	}
	for _, button := range buttons {
		add(button.URL)
	}
	return urls
}

func publishedSourceHash(raw string) string {
	canonical := contentdedup.CanonicalSourceURL(raw)
	if canonical == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(digest[:])
}

// recordPublishedContentTx is called only at a confirmed MAX delivery/import
// boundary. Historical versions remain indexed when the editor changes later.
func recordPublishedContentTx(ctx context.Context, tx *sql.Tx, post Post, proof string) error {
	if post.WorkspaceID == "" || post.ChannelID == nil || *post.ChannelID <= 0 {
		return nil
	}
	urls := publishedSourceURLs(post.Content, post.LinkButtons)
	hashes := make([]string, 0, len(urls))
	for _, raw := range urls {
		hashes = append(hashes, publishedSourceHash(raw))
	}
	encodedURLs, err := json.Marshal(urls)
	if err != nil {
		return fmt.Errorf("encode publication index sources: %w", err)
	}
	content := boundedPublishedText(post.Content, 4000)
	title := publishedContentTitle(post.Content)
	digest := sha256.Sum256([]byte(content + "\x00" + string(encodedURLs)))
	_, err = tx.ExecContext(ctx, `INSERT INTO published_content_index
(workspace_id,channel_id,post_id,content_hash,publication_fingerprint,proof,title,content,source_urls,source_hashes)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb,$10::text[])
ON CONFLICT(workspace_id,channel_id,post_id,content_hash) DO UPDATE SET
publication_fingerprint=EXCLUDED.publication_fingerprint,
proof=CASE WHEN published_content_index.proof='confirmed' THEN 'confirmed' ELSE EXCLUDED.proof END`,
		post.WorkspaceID, *post.ChannelID, post.ID, hex.EncodeToString(digest[:]), post.MAXPublishedFingerprint,
		proof, title, content, string(encodedURLs), hashes)
	if err != nil {
		return fmt.Errorf("index confirmed publication content: %w", err)
	}
	return nil
}

func recordImportedPublishedContentTx(ctx context.Context, tx *sql.Tx, workspaceID string, channelID, postID int64, item MAXHistoryItem) error {
	return recordPublishedContentTx(ctx, tx, Post{ID: postID, WorkspaceID: workspaceID, ChannelID: &channelID,
		Title: item.Title, Content: item.Content, Format: FormatMarkdown, LinkButtons: item.LinkButtons}, "imported")
}

// EnsurePublishedContentIndexForWorkspace scans the historical rows once, in
// bounded pages. The durable state lock serializes concurrent first requests;
// all subsequent requests read one state row. New deliveries/imports maintain
// the index incrementally in the same transaction as their publication write.
func (s *Store) EnsurePublishedContentIndexForWorkspace(ctx context.Context, actorUserID, workspaceID string, channelID int64) error {
	if _, err := s.GetChannelForWorkspace(ctx, actorUserID, workspaceID, channelID); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := resolveWorkspaceAccess(ctx, tx, actorUserID, workspaceID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO published_content_index_state(workspace_id,channel_id,version)
VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, workspaceID, channelID, publishedContentIndexVersion)
	if err != nil {
		return fmt.Errorf("initialize publication index state: %w", err)
	}
	var cursor int64
	var version int
	var complete bool
	if err := tx.QueryRowContext(ctx, `SELECT version,last_post_id,complete FROM published_content_index_state
WHERE workspace_id=$1 AND channel_id=$2 FOR UPDATE`, workspaceID, channelID).Scan(&version, &cursor, &complete); err != nil {
		return fmt.Errorf("lock publication index state: %w", err)
	}
	if version == publishedContentIndexVersion && complete {
		return tx.Commit()
	}
	if version != publishedContentIndexVersion {
		cursor = 0
	}
	for {
		posts, err := readPublishedIndexBackfillPage(ctx, tx, workspaceID, channelID, cursor)
		if err != nil {
			return err
		}
		if len(posts) == 0 {
			break
		}
		for _, post := range posts {
			post.Attachments, err = queryPostAttachments(ctx, tx, "WHERE post_id=? ORDER BY position,id", post.ID)
			if err != nil {
				return err
			}
			if post.MAXPublishedFingerprint != "" && post.MAXPublishedFingerprint == publicationFingerprint(post) {
				if err := recordPublishedContentTx(ctx, tx, post, "confirmed"); err != nil {
					return err
				}
			} else if err := backfillPublishedRevisionTx(ctx, tx, post); err != nil {
				return err
			}
			cursor = post.ID
		}
	}
	if _, err := resolveWorkspaceAccess(ctx, tx, actorUserID, workspaceID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE published_content_index_state SET version=$3,last_post_id=$4,complete=TRUE,
unverified_posts=(SELECT count(*) FROM posts p WHERE p.workspace_id=$1 AND p.channel_id=$2
AND (p.status='published' OR p.max_message_id<>'') AND NOT EXISTS(SELECT 1 FROM published_content_index i
WHERE i.workspace_id=p.workspace_id AND i.channel_id=p.channel_id AND i.post_id=p.id))
WHERE workspace_id=$1 AND channel_id=$2`, workspaceID, channelID, publishedContentIndexVersion, cursor)
	if err != nil {
		return fmt.Errorf("complete publication index backfill: %w", err)
	}
	return tx.Commit()
}

func readPublishedIndexBackfillPage(ctx context.Context, tx *sql.Tx, workspaceID string, channelID, cursor int64) ([]Post, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+postColumns+` FROM posts
WHERE workspace_id=$1 AND channel_id=$2 AND id>$3 AND (status=$4 OR max_message_id<>'')
ORDER BY id LIMIT $5`, workspaceID, channelID, cursor, PostStatusPublished, publishedContentBackfillPageSize)
	if err != nil {
		return nil, fmt.Errorf("read historical publication index page: %w", err)
	}
	defer func() { _ = rows.Close() }()
	posts := make([]Post, 0, publishedContentBackfillPageSize)
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
	return posts, nil
}

// A dirty editable row is never an editorial history source. Recover only an
// immutable matching delivered revision, or a first imported revision whose
// tenant-scoped MAX history mapping proves a real publication. Unknown legacy
// local deliveries remain unindexed rather than treating an initial draft as
// authoritative published content.
func backfillPublishedRevisionTx(ctx context.Context, tx *sql.Tx, current Post) error {
	var imported bool
	if current.Origin == PostOriginMAXHistory {
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM max_history_messages
WHERE workspace_id=$1 AND channel_id=$2 AND post_id=$3 AND max_message_id=$4)`,
			current.WorkspaceID, *current.ChannelID, current.ID, current.MAXMessageID).Scan(&imported); err != nil {
			return fmt.Errorf("verify historical publication mapping: %w", err)
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT revision_number,snapshot FROM post_revisions
WHERE workspace_id=$1 AND post_id=$2 ORDER BY revision_number DESC`, current.WorkspaceID, current.ID)
	if err != nil {
		return fmt.Errorf("read publication index revisions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var selected *Post
	proof := ""
	for rows.Next() {
		var number int
		var raw []byte
		if err := rows.Scan(&number, &raw); err != nil {
			_ = rows.Close()
			return err
		}
		var snapshot struct {
			Title              string       `json:"title"`
			Content            string       `json:"content"`
			Format             string       `json:"format"`
			ImageURL           string       `json:"image_url"`
			ImagePath          string       `json:"image_path"`
			LinkButtons        []LinkButton `json:"link_buttons"`
			DisableLinkPreview bool         `json:"disable_link_preview"`
			Origin             string       `json:"origin"`
			Attachments        []struct {
				Type       string `json:"type"`
				StorageKey string `json:"storage_key"`
			} `json:"attachments"`
		}
		if json.Unmarshal(raw, &snapshot) != nil || snapshot.Format == "" {
			continue
		}
		post := Post{ID: current.ID, WorkspaceID: current.WorkspaceID, ChannelID: current.ChannelID,
			Title: snapshot.Title, Content: snapshot.Content, Format: snapshot.Format, ImageURL: snapshot.ImageURL,
			ImagePath: snapshot.ImagePath, LinkButtons: snapshot.LinkButtons, DisableLinkPreview: snapshot.DisableLinkPreview,
			MAXPublishedFingerprint: current.MAXPublishedFingerprint}
		for _, attachment := range snapshot.Attachments {
			post.Attachments = append(post.Attachments, PostAttachment{Type: attachment.Type, StorageKey: attachment.StorageKey})
		}
		if current.MAXPublishedFingerprint != "" && publicationFingerprint(post) == current.MAXPublishedFingerprint {
			selected, proof = &post, "confirmed"
			break
		}
		if number == 1 && imported && snapshot.Origin == PostOriginMAXHistory {
			selected, proof = &post, "imported"
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if selected != nil {
		return recordPublishedContentTx(ctx, tx, *selected, proof)
	}
	return nil
}

// FindPublishedDiscoveryReferences reads a small, indexed shortlist. Russian
// stemming and URL hashes select proven snapshots. The caller checks all local
// references deterministically, then projects at most three nearest snapshots
// for bounded semantic comparisons; it never reads the whole channel history.
func (s *Store) FindPublishedDiscoveryReferences(ctx context.Context, actorUserID, workspaceID string, channelID int64, title, text, sourceURL string) ([]PublishedDiscoveryReference, error) {
	if _, err := s.GetChannelForWorkspace(ctx, actorUserID, workspaceID, channelID); err != nil {
		return nil, err
	}
	input := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}
		return ' '
	}, boundedPublishedText(title+" "+text, 4200))
	rows, err := s.db.QueryContext(ctx, `WITH candidate_terms AS (
SELECT tsvector_to_array(to_tsvector('russian',$3)) AS terms
),candidate AS (
SELECT terms,to_tsquery('russian',array_to_string(terms,' | ')) AS query FROM candidate_terms
)
SELECT title,content,source_urls FROM published_content_index,candidate
WHERE workspace_id=$1 AND channel_id=$2
AND (source_hashes @> ARRAY[$4]::text[] OR search_vector @@ candidate.query)
ORDER BY (source_hashes @> ARRAY[$4]::text[]) DESC,
cardinality(ARRAY(SELECT unnest(tsvector_to_array(search_vector)) INTERSECT SELECT unnest(candidate.terms))) DESC,
ts_rank_cd(search_vector,candidate.query) DESC,post_id DESC
LIMIT 50`, workspaceID, channelID, input, publishedSourceHash(sourceURL))
	if err != nil {
		return nil, fmt.Errorf("match indexed published content: %w", err)
	}
	defer func() { _ = rows.Close() }()
	references := make([]PublishedDiscoveryReference, 0)
	for rows.Next() {
		var reference PublishedDiscoveryReference
		var urls []byte
		if err := rows.Scan(&reference.Title, &reference.Text, &urls); err != nil {
			return nil, err
		}
		if json.Unmarshal(urls, &reference.SourceURLs) != nil {
			return nil, fmt.Errorf("invalid publication index sources")
		}
		references = append(references, reference)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if _, err := s.GetChannelForWorkspace(ctx, actorUserID, workspaceID, channelID); err != nil {
		return nil, err
	}
	return references, nil
}
