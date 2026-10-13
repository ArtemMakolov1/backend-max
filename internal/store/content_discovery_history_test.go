package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestPublishedDiscoveryIndexBackfillsThousandRowsOnceAndScopesIndexedReads(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, workspace, channel := openMAXHistoryStoreTest(t, "published-index-thousand", 4)
	// Simulate a pre-index installation with immutable historical revisions,
	// not 1,000 new publication operations that already maintain the index.
	_, err := s.db.ExecContext(ctx, `INSERT INTO posts(owner_id,workspace_id,channel_id,title,content,format,status,max_message_id,published_at,created_at,updated_at)
SELECT $3,$1,$2,'История '||n,
CASE WHEN n=1 THEN 'Русский инженер изобрёл керосиновый двигатель с рекордной экономичностью.' ELSE 'Несвязанная историческая заметка номер '||n END,
'markdown','published','historical-'||n,CURRENT_TIMESTAMP - n * INTERVAL '1 minute',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP
FROM generate_series(1,1000) AS n`, workspace.ID, channel.ID, workspace.CompatOwnerUserID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO post_revisions(workspace_id,post_id,revision_number,author_user_id,snapshot)
SELECT workspace_id,id,1,'test-owner',jsonb_build_object('title',title,'content',content,'format',format,
'image_path','','image_url','','attachments','[]'::jsonb,'link_buttons','[]'::jsonb,'disable_link_preview',FALSE)
FROM posts WHERE workspace_id=$1 AND channel_id=$2`, workspace.ID, channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,content FROM posts WHERE workspace_id=$1 AND channel_id=$2`, workspace.ID, channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	var fingerprints []string
	for rows.Next() {
		var id int64
		var text string
		if err := rows.Scan(&id, &text); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		fingerprints = append(fingerprints, publicationFingerprint(Post{Content: text, Format: FormatMarkdown}))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE posts p SET max_published_fingerprint=f.fingerprint
FROM UNNEST($1::bigint[],$2::text[]) f(id,fingerprint) WHERE p.id=f.id`, ids, fingerprints); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsurePublishedContentIndexForWorkspace(ctx, "history-editor", workspace.ID, channel.ID); err != nil {
		t.Fatal(err)
	}
	var indexed int
	var complete bool
	var cursor int64
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM published_content_index WHERE workspace_id=$1 AND channel_id=$2`, workspace.ID, channel.ID).Scan(&indexed); err != nil || indexed != 1000 {
		t.Fatalf("historical index count=%d err=%v", indexed, err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT complete,last_post_id FROM published_content_index_state WHERE workspace_id=$1 AND channel_id=$2`, workspace.ID, channel.ID).Scan(&complete, &cursor); err != nil || !complete || cursor <= 0 {
		t.Fatalf("backfill completion=%t cursor=%d err=%v", complete, cursor, err)
	}
	var schema string
	if err := s.db.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	testURL, err := withSearchPath(os.Getenv("TEST_DATABASE_URL"), schema)
	if err != nil {
		t.Fatal(err)
	}
	tracer := &publishedIndexQueryTracer{}
	tracedDB, err := openPostgresWithTracer(testURL, tracer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tracedDB.Close() })
	indexedStore := &Store{db: &postgresDB{DB: tracedDB}}
	if _, err := s.db.ExecContext(ctx, `UPDATE posts SET content='PRIVATE unpublished editable body' WHERE workspace_id=$1 AND channel_id=$2`, workspace.ID, channel.ID); err != nil {
		t.Fatal(err)
	}
	if err := indexedStore.EnsurePublishedContentIndexForWorkspace(ctx, "history-editor", workspace.ID, channel.ID); err != nil {
		t.Fatal(err)
	}
	references, err := indexedStore.FindPublishedDiscoveryReferences(ctx, "history-editor", workspace.ID, channel.ID,
		"Керосиновый двигатель", "Русский инженер изобрёл экономичный керосиновый двигатель", "")
	if err != nil || len(references) == 0 || len(references) > 50 {
		t.Fatalf("bounded indexed reference lookup=%d err=%v", len(references), err)
	}
	found := false
	for _, reference := range references {
		if strings.Contains(reference.Text, "керосиновый двигатель") {
			found = true
		}
		if strings.Contains(reference.Text, "PRIVATE") {
			t.Fatal("a second lookup rescanned unpublished editable rows")
		}
	}
	if !found {
		t.Fatal("publication outside the latest ten disappeared from indexed history")
	}
	if reads := tracer.historyReads.Load(); reads != 0 {
		t.Fatalf("repeat indexed lookup executed %d source-history reads", reads)
	}
	var ginIndexes int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM pg_index i
JOIN pg_class c ON c.oid=i.indexrelid JOIN pg_am a ON a.oid=c.relam
WHERE i.indrelid='published_content_index'::regclass AND a.amname='gin'`).Scan(&ginIndexes); err != nil || ginIndexes != 2 {
		t.Fatalf("text/source GIN indexes=%d err=%v", ginIndexes, err)
	}
	if _, err := s.FindPublishedDiscoveryReferences(ctx, "foreign-user", workspace.ID, channel.ID, "керосин", "", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign reader accessed historical index: %v", err)
	}
	foreignWorkspace, err := s.CreateWorkspace(ctx, "test-owner", Workspace{Name: "Different history"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.FindPublishedDiscoveryReferences(ctx, "test-owner", foreignWorkspace.ID, channel.ID, "керосин", "", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("history crossed workspace boundary: %v", err)
	}
	other, err := s.CreateChannelForWorkspace(ctx, "test-owner", workspace.ID, Channel{MAXChatID: "other-history", Title: "Other", VerifiedMAXOwnerID: "100", IsChannel: true, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	if references, err := s.FindPublishedDiscoveryReferences(ctx, "test-owner", workspace.ID, other.ID, "керосин", "", ""); err != nil || len(references) != 0 {
		t.Fatalf("history crossed channel boundary: %d %v", len(references), err)
	}
}

func TestPublishedDiscoveryIndexFindsShortPublishedCaptionWithoutOriginalSourceURL(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, workspace, channel := openMAXHistoryStoreTest(t, "published-index-short-caption", 4)
	data, err := os.ReadFile("../contentdedup/testdata/published_aviation_rewrite.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Reference struct {
			Text       string
			SourceURLs []string
		}
		Candidate, Article struct {
			Title, Text, SourceURL string
		}
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	post, err := s.CreatePostForWorkspace(ctx, "test-owner", workspace.ID, Post{ChannelID: &channel.ID,
		Title: "Новая публикация", Content: fixture.Reference.Text, Format: FormatMarkdown})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimForPublishing(ctx, post.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkPublished(ctx, post.ID, "published-short-caption", ""); err != nil {
		t.Fatal(err)
	}
	// Recreate a pre-index installation with a known, unchanged publication.
	// The original article URL never appeared in the delivered channel caption.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM published_content_index WHERE workspace_id=$1 AND channel_id=$2`, workspace.ID, channel.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsurePublishedContentIndexForWorkspace(ctx, "test-owner", workspace.ID, channel.ID); err != nil {
		t.Fatal(err)
	}
	for name, candidate := range map[string]struct{ Title, Text, SourceURL string }{
		"short rewritten card": fixture.Candidate,
		"long article": {Title: fixture.Article.Title,
			Text:      fixture.Article.Text + strings.Repeat(" Научные исследования продолжаются, подробности описаны в публикации.", 80),
			SourceURL: fixture.Article.SourceURL},
	} {
		t.Run(name, func(t *testing.T) {
			refs, err := s.FindPublishedDiscoveryReferences(ctx, "test-owner", workspace.ID, channel.ID, candidate.Title, candidate.Text, candidate.SourceURL)
			if err != nil || len(refs) != 1 {
				t.Fatalf("known caption absent from indexed text shortlist: count=%d err=%v", len(refs), err)
			}
			if refs[0].Text != fixture.Reference.Text || refs[0].Title != publishedContentTitle(fixture.Reference.Text) {
				t.Fatal("shortlist did not preserve the proven published body and visible title")
			}
			for _, source := range refs[0].SourceURLs {
				if publishedSourceHash(source) == publishedSourceHash(candidate.SourceURL) {
					t.Fatal("fixture unexpectedly supplied exact source URL authority")
				}
			}
		})
	}
	var complete bool
	var unverified int
	if err := s.db.QueryRowContext(ctx, `SELECT complete,unverified_posts FROM published_content_index_state WHERE workspace_id=$1 AND channel_id=$2`, workspace.ID, channel.ID).Scan(&complete, &unverified); err != nil || !complete || unverified != 0 {
		t.Fatalf("confirmed caption was treated as unverified legacy history: complete=%t unverified=%d err=%v", complete, unverified, err)
	}
}

func TestPublishedDiscoveryIndexRanksDistinctEventTermsBeforeRepetitionAndCapsLocalShortlist(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, workspace, channel := openMAXHistoryStoreTest(t, "published-index-nearest", 4)
	publish := func(content string) {
		t.Helper()
		post, err := s.CreatePostForWorkspace(ctx, "test-owner", workspace.ID, Post{ChannelID: &channel.ID,
			Title: "Internal label", Content: content, Format: FormatMarkdown})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ClaimForPublishing(ctx, post.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.MarkPublished(ctx, post.ID, fmt.Sprintf("nearest-post-%d", post.ID), ""); err != nil {
			t.Fatal(err)
		}
	}
	specific := "Учёные получили авиационный керосин из древесных опилок и подсолнечной лузги. Каталитическая переработка биомассы превращает растительные отходы в горючее для самолётов."
	priority := "Независимый опубликованный прибор. https://example.com/priority-source?utm_source=old"
	publish(specific)
	publish(priority)
	for index := 0; index < 55; index++ {
		publish("Самолёт. " + strings.Repeat("Самолёт ", 250) + fmt.Sprint(index))
	}
	if err := s.EnsurePublishedContentIndexForWorkspace(ctx, "history-editor", workspace.ID, channel.ID); err != nil {
		t.Fatal(err)
	}
	refs, err := s.FindPublishedDiscoveryReferences(ctx, "history-editor", workspace.ID, channel.ID,
		"Керосин из опилок и подсолнечной лузги", "Авиационное горючее для самолётов получили переработкой биомассы и растительных отходов. Самолёт.", "")
	if err != nil || len(refs) != 50 || refs[0].Text != specific {
		t.Fatalf("distinct event terms lost to generic repetition: count=%d err=%v", len(refs), err)
	}
	refs, err = s.FindPublishedDiscoveryReferences(ctx, "history-editor", workspace.ID, channel.ID,
		"Самолёт", "Новая авиационная разработка", "https://example.com/priority-source?utm_campaign=next")
	if err != nil || len(refs) != 50 || refs[0].Text != priority {
		t.Fatalf("canonical source did not rank before text matches: count=%d err=%v", len(refs), err)
	}
	if err := s.RemoveWorkspaceMember(ctx, "test-owner", workspace.ID, "history-editor"); err != nil {
		t.Fatal(err)
	}
	if refs, err := s.FindPublishedDiscoveryReferences(ctx, "history-editor", workspace.ID, channel.ID, "керосин", "", ""); !errors.Is(err, ErrNotFound) || refs != nil {
		t.Fatalf("revoked member received cached published references: count=%d err=%v", len(refs), err)
	}
}

func TestPublishedDiscoveryIndexTracksOnlyConfirmedDeliveriesAndRetainsPriorVersions(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, workspace, channel := openMAXHistoryStoreTest(t, "published-index-lifecycle", 4)
	post, err := s.CreatePostForWorkspace(ctx, "test-owner", workspace.ID, Post{ChannelID: &channel.ID,
		Title: "Керосиновый двигатель", Content: "Российский инженер создал керосиновый двигатель. [Источник](https://example.com/engine?utm_source=old)", Format: FormatMarkdown})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnsurePublishedContentIndexForWorkspace(ctx, "test-owner", workspace.ID, channel.ID); err != nil {
		t.Fatal(err)
	}
	refs, err := s.FindPublishedDiscoveryReferences(ctx, "test-owner", workspace.ID, channel.ID, "керосиновый двигатель", "", "https://example.com/engine")
	if err != nil || len(refs) != 0 {
		t.Fatal("an unpublished draft entered the index")
	}
	if _, err := s.ClaimForPublishing(ctx, post.ID); err != nil {
		t.Fatal(err)
	}
	post, err = s.MarkPublished(ctx, post.ID, "published-engine", "")
	if err != nil {
		t.Fatal(err)
	}
	refs, err = s.FindPublishedDiscoveryReferences(ctx, "test-owner", workspace.ID, channel.ID, "", "", "https://example.com/engine?utm_campaign=new")
	if err != nil || len(refs) != 1 || !strings.Contains(refs[0].Text, "керосиновый двигатель") {
		t.Fatalf("confirmed source URL was not indexed canonically: %+v %v", refs, err)
	}
	changed := "PRIVATE local edit about a submarine that never reached MAX"
	post, err = s.UpdatePost(ctx, post.ID, PostChanges{Content: &changed})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := s.ClaimPublishedForUpdate(ctx, post)
	if err != nil {
		t.Fatal(err)
	}
	post, err = s.ReleasePublishedUpdate(ctx, claim, "Update rejected")
	if err != nil {
		t.Fatal(err)
	}
	refs, err = s.FindPublishedDiscoveryReferences(ctx, "test-owner", workspace.ID, channel.ID, "submarine", "", "")
	if err != nil || len(refs) != 0 {
		t.Fatal("a failed publication update entered the index")
	}
	confirmed := "Инженеры создали новую исследовательскую подводную лодку. https://example.com/submarine"
	post, err = s.UpdatePost(ctx, post.ID, PostChanges{Content: &confirmed})
	if err != nil {
		t.Fatal(err)
	}
	claim, err = s.ClaimPublishedForUpdate(ctx, post)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReleasePublishedUpdate(ctx, claim, ""); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"https://example.com/engine", "https://example.com/submarine"} {
		if refs, err := s.FindPublishedDiscoveryReferences(ctx, "test-owner", workspace.ID, channel.ID, "", "", source); err != nil || len(refs) != 1 {
			t.Fatalf("historical delivered version %s lost: %d %v", source, len(refs), err)
		}
	}
}

func TestPublishedDiscoveryIndexImportedOriginalSurvivesUnpublishedEdits(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, workspace, channel := openMAXHistoryStoreTest(t, "published-index-imported", 4)
	now := time.Now().UTC()
	claim, err := s.ClaimMAXHistoryImport(ctx, "test-owner", workspace.ID, channel.ID, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ApplyMAXHistoryPage(ctx, "test-owner", workspace.ID, claim.Generation, []MAXHistoryItem{{
		MessageID: "engine-import", Title: "Импортированный двигатель", Content: "Изобретатель создал керосиновый двигатель. https://example.com/imported-engine",
		PublishedAt: now, RoundTrip: true, Raw: json.RawMessage(`{}`),
	}}, nil, true, now)
	if err != nil {
		t.Fatal(err)
	}
	posts, err := s.ListPostsForWorkspace(ctx, "test-owner", workspace.ID, PostStatusPublished, &channel.ID)
	if err != nil || len(posts) != 1 {
		t.Fatalf("imported posts=%d err=%v", len(posts), err)
	}
	changed := "Private unpublished edit"
	if _, err := s.db.ExecContext(ctx, `UPDATE posts SET content=$1 WHERE id=$2`, changed, posts[0].ID); err != nil {
		t.Fatal(err)
	}
	// Remove the new index to exercise initial backfill of a dirty imported
	// publication from the immutable first revision, not its editable body.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM published_content_index WHERE workspace_id=$1 AND channel_id=$2`, workspace.ID, channel.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsurePublishedContentIndexForWorkspace(ctx, "test-owner", workspace.ID, channel.ID); err != nil {
		t.Fatal(err)
	}
	refs, err := s.FindPublishedDiscoveryReferences(ctx, "test-owner", workspace.ID, channel.ID, "", "", "https://example.com/imported-engine")
	if err != nil || len(refs) != 1 || strings.Contains(refs[0].Text, "Private") {
		t.Fatalf("dirty import recovery failed: %+v %v", refs, err)
	}
	var proof string
	if err := s.db.QueryRowContext(ctx, `SELECT proof FROM published_content_index WHERE workspace_id=$1 AND channel_id=$2`, workspace.ID, channel.ID).Scan(&proof); err != nil || proof != "confirmed" {
		// Text-only immutable import revisions also reproduce the confirmed
		// fingerprint; unsupported galleries use the explicit imported proof.
		t.Fatalf("immutable import proof=%q err=%v", proof, err)
	}
}

func TestPublishedDiscoveryBackfillRecoversDeliveredBAndNeverInitialAOrDirtyC(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, workspace, channel := openMAXHistoryStoreTest(t, "published-index-three-versions", 4)
	post, err := s.CreatePostForWorkspace(ctx, "test-owner", workspace.ID, Post{ChannelID: &channel.ID,
		Title: "Внутреннее имя", Content: "INITIAL_UNPUBLISHED_A https://example.com/private-a", Format: FormatMarkdown})
	if err != nil {
		t.Fatal(err)
	}
	delivered := "Инженеры создали керосиновый двигатель. https://example.com/published-b"
	post, err = s.UpdatePost(ctx, post.ID, PostChanges{Content: &delivered})
	if err != nil {
		t.Fatal(err)
	}
	revision, err := s.CreatePostRevision(ctx, "test-owner", workspace.ID, post.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimForPublishing(ctx, post.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkPublished(ctx, post.ID, "three-versions", ""); err != nil {
		t.Fatal(err)
	}
	dirty := "DIRTY_UNPUBLISHED_C https://example.com/private-c"
	if _, err := s.UpdatePost(ctx, post.ID, PostChanges{Content: &dirty}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM published_content_index WHERE workspace_id=$1`, workspace.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsurePublishedContentIndexForWorkspace(ctx, "test-owner", workspace.ID, channel.ID); err != nil {
		t.Fatal(err)
	}
	refs, err := s.FindPublishedDiscoveryReferences(ctx, "test-owner", workspace.ID, channel.ID, "", "", "https://example.com/published-b")
	if err != nil || len(refs) != 1 || refs[0].Text != delivered {
		t.Fatalf("confirmed historical revision B was not recovered: %+v %v", refs, err)
	}
	var privateCount int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM published_content_index WHERE workspace_id=$1
AND (content LIKE '%INITIAL_UNPUBLISHED%' OR content LIKE '%DIRTY_UNPUBLISHED%')`, workspace.ID).Scan(&privateCount); err != nil || privateCount != 0 {
		t.Fatalf("initial draft A or local edit C entered historical authority: %d %v", privateCount, err)
	}
	// A legacy publication without an immutable delivered revision is explicitly
	// incomplete, not assigned the first draft as a convenient false fallback.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM post_revisions WHERE id=$1`, revision.ID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"published_content_index", "published_content_index_state"} {
		if _, err := s.db.ExecContext(ctx, "DELETE FROM "+table+" WHERE workspace_id=$1", workspace.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.EnsurePublishedContentIndexForWorkspace(ctx, "test-owner", workspace.ID, channel.ID); err != nil {
		t.Fatal(err)
	}
	var unverified int
	if err := s.db.QueryRowContext(ctx, `SELECT unverified_posts FROM published_content_index_state WHERE workspace_id=$1 AND channel_id=$2`, workspace.ID, channel.ID).Scan(&unverified); err != nil || unverified != 1 {
		t.Fatalf("unknown legacy publication not marked explicitly: %d %v", unverified, err)
	}
	var indexed int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM published_content_index WHERE workspace_id=$1`, workspace.ID).Scan(&indexed); err != nil || indexed != 0 {
		t.Fatalf("unproven original draft used as published authority: %d %v", indexed, err)
	}
}

// Retain only a query class/count: SQL arguments and editorial text are never
// recorded by this test tracer.
type publishedIndexQueryTracer struct{ historyReads atomic.Int64 }

func (t *publishedIndexQueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "FROM posts") || strings.Contains(data.SQL, "FROM post_revisions") {
		t.historyReads.Add(1)
	}
	return ctx
}
func (*publishedIndexQueryTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestPublishedDiscoveryBackfillNeverUsesPrivateEditedInternalTitle(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, workspace, channel := openMAXHistoryStoreTest(t, "published-index-title-only", 4)
	post, err := s.CreatePostForWorkspace(ctx, "test-owner", workspace.ID, Post{ChannelID: &channel.ID,
		Title: "INITIAL_PRIVATE_INTERNAL_TITLE", Content: "**Опубликованный двигатель изобретателя.** https://example.com/title-only", Format: FormatMarkdown})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimForPublishing(ctx, post.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkPublished(ctx, post.ID, "title-only-publication", ""); err != nil {
		t.Fatal(err)
	}
	title := "NEW_PRIVATE_UNPUBLISHED_INTERNAL_TITLE"
	if _, err := s.UpdatePost(ctx, post.ID, PostChanges{Title: &title}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM published_content_index WHERE workspace_id=$1`, workspace.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsurePublishedContentIndexForWorkspace(ctx, "test-owner", workspace.ID, channel.ID); err != nil {
		t.Fatal(err)
	}
	refs, err := s.FindPublishedDiscoveryReferences(ctx, "test-owner", workspace.ID, channel.ID, "", "", "https://example.com/title-only")
	if err != nil || len(refs) != 1 || refs[0].Title != "Опубликованный двигатель изобретателя" {
		t.Fatalf("index title lacks published-content authority: %+v %v", refs, err)
	}
	encoded, err := json.Marshal(refs)
	if err != nil || strings.Contains(string(encoded), "PRIVATE") {
		t.Fatal("a local internal title entered publication identity")
	}
}
