package commentbridge

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, e := Open(filepath.Join(t.TempDir(), "bridge.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
func testComment() Comment {
	return Comment{Platform: PlatformBilibili, AccountID: "a", ID: "c", RootID: "root", ParentID: "parent", Text: "comment", IsChargePlus: true}
}

func TestMessageMappingMigrationPreservesReferencesAndReplyIsolation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`
CREATE TABLE comments (platform TEXT NOT NULL, account_id TEXT NOT NULL, comment_id TEXT NOT NULL, root_id TEXT NOT NULL DEFAULT '', parent_id TEXT NOT NULL DEFAULT '', resource_id TEXT NOT NULL, resource_title TEXT NOT NULL, resource_url TEXT NOT NULL, author_id TEXT NOT NULL, author_name TEXT NOT NULL, text TEXT NOT NULL, published_at INTEGER NOT NULL, PRIMARY KEY(platform,account_id,comment_id));
CREATE TABLE feishu_messages (message_id TEXT PRIMARY KEY, chat_id TEXT NOT NULL DEFAULT '', platform TEXT NOT NULL, account_id TEXT NOT NULL, comment_id TEXT NOT NULL, UNIQUE(platform,account_id,comment_id), FOREIGN KEY(platform,account_id,comment_id) REFERENCES comments(platform,account_id,comment_id));
CREATE TABLE preserved_reference (message_id TEXT REFERENCES feishu_messages(message_id));
INSERT INTO comments VALUES('youtube','account','comment','comment','','video','title','url','author','viewer','text',1);
INSERT INTO feishu_messages VALUES('old-message','old-chat','youtube','account','comment');
INSERT INTO preserved_reference VALUES('old-message');`)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { store.Close() }()
	if _, err := store.DB().Exec(`INSERT INTO feishu_messages VALUES('new-message','new-chat','youtube','account','comment')`); err != nil {
		t.Fatal(err)
	}
	var enabled, violations, retained int
	if err := store.DB().QueryRow(`PRAGMA foreign_keys`).Scan(&enabled); err != nil || enabled != 1 {
		t.Fatalf("foreign keys not restored: %d %v", enabled, err)
	}
	if err := store.DB().QueryRow(`SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil || violations != 0 {
		t.Fatalf("reference integrity: %d %v", violations, err)
	}
	if err := store.DB().QueryRow(`SELECT count(*) FROM preserved_reference JOIN feishu_messages USING(message_id)`).Scan(&retained); err != nil || retained != 1 {
		t.Fatalf("old reference lost: %d %v", retained, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"old", "new"} {
		in := IncomingReply{MessageID: name + "-reply", ParentMessageID: name + "-message", ChatID: name + "-chat", UserID: "member", Text: "reply"}
		if added, err := store.AcceptReply(t.Context(), in); err != nil || !added {
			t.Fatalf("%s mapping unavailable: %v", name, err)
		}
		if added, err := store.AcceptReply(t.Context(), in); err != nil || added {
			t.Fatalf("reply replay: %v", err)
		}
		in.MessageID += "-wrong-group"
		in.ChatID = "unrelated"
		if added, err := store.AcceptReply(t.Context(), in); !errors.Is(err, ErrUnmappedReply) || added {
			t.Fatalf("cross-group mapping accepted: %v", err)
		}
	}
	_, err = store.DB().Exec(`INSERT INTO notification_jobs(action_id,platform,account_id,comment_id,status) VALUES('one','youtube','account','comment','sent')`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`INSERT INTO notification_jobs(action_id,platform,account_id,comment_id,status) VALUES('two','youtube','account','comment','pending')`); err == nil {
		t.Fatal("normal notification deduplication was removed")
	}
}
func TestRecordPollBaselineAndClaim(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	c := testComment()
	if e := s.RecordPoll(ctx, PlatformBilibili, "a", []Comment{c}); e != nil {
		t.Fatal(e)
	}
	j, e := s.ClaimNotification(ctx)
	if e != nil || j != nil {
		t.Fatalf("baseline claimed %#v, %v", j, e)
	}
	c.ID = "new"
	if e = s.RecordPoll(ctx, PlatformBilibili, "a", []Comment{c}); e != nil {
		t.Fatal(e)
	}
	j, e = s.ClaimNotification(ctx)
	if e != nil || j == nil {
		t.Fatalf("new comment not claimed: %#v %v", j, e)
	}
	if j.Comment.RootID != "root" || j.Comment.ParentID != "parent" {
		t.Fatalf("thread relationship was not persisted: %#v", j.Comment)
	}
	if !j.Comment.IsChargePlus {
		t.Fatalf("charge-plus flag was not persisted: %#v", j.Comment)
	}
	var stored bool
	if err := s.DB().QueryRowContext(ctx, `SELECT is_charge_plus FROM comments WHERE platform=? AND account_id=? AND comment_id=?`, c.Platform, c.AccountID, c.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !stored {
		t.Fatal("charge-plus flag was not stored")
	}
	if e = s.MarkNotificationSent(ctx, j.ActionID, "f1", "chat-1"); e != nil {
		t.Fatal(e)
	}
}

func TestRecordPollBackfillsMissingVideoTitleWithoutDuplicateNotification(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	comment := testComment()
	comment.Platform = PlatformYouTube
	comment.AccountID = "youtube-account"
	comment.ResourceTitle = ""
	if err := s.RecordPoll(ctx, comment.Platform, comment.AccountID, []Comment{comment}); err != nil {
		t.Fatal(err)
	}
	comment.ResourceTitle = "Resolved title"
	if err := s.RecordPoll(ctx, comment.Platform, comment.AccountID, []Comment{comment}); err != nil {
		t.Fatal(err)
	}
	var title string
	if err := s.DB().QueryRowContext(ctx, `SELECT resource_title FROM comments WHERE platform=? AND account_id=? AND comment_id=?`, comment.Platform, comment.AccountID, comment.ID).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != "Resolved title" {
		t.Fatalf("resource title = %q", title)
	}
	job, err := s.ClaimNotification(ctx)
	if err != nil || job != nil {
		t.Fatalf("backfill created notification: %#v, %v", job, err)
	}
}

func TestRecordPollInvalidArgumentsAndEmptyReplyFailClosed(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.RecordPoll(ctx, PlatformBilibili, "a", "not-comments"); err == nil {
		t.Fatal("accepted invalid RecordPoll arguments")
	}
	if err := s.RecordPoll(ctx, PlatformBilibili, "a", []Comment{{ID: ""}}); err == nil {
		t.Fatal("accepted empty comment id")
	}
	if err := s.RecordPoll(ctx, PlatformBilibili, "a", []Comment{{ID: "large", Text: strings.Repeat("x", maxStoredTextRunes+1)}}); err == nil {
		t.Fatal("accepted oversized comment")
	}
	if _, err := s.AcceptReply(ctx, IncomingReply{MessageID: "in", ParentMessageID: "mapped", Text: "  "}); err == nil {
		t.Fatal("accepted empty reply text")
	}
}

func TestDatabasePermissionsAndReadOnlyDoesNotRecover(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.RecordPoll(ctx, PlatformBilibili, "a", []Comment{testComment()}); err != nil {
		t.Fatal(err)
	}
	c := testComment()
	c.ID = "new"
	if err := s.RecordPoll(ctx, PlatformBilibili, "a", []Comment{c}); err != nil {
		t.Fatal(err)
	}
	if job, err := s.ClaimNotification(ctx); err != nil || job == nil {
		t.Fatalf("claim = %#v, %v", job, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("database mode = %v, err = %v", info.Mode().Perm(), err)
	}
	readOnly, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	counts, err := readOnly.StatusCounts(ctx)
	_ = readOnly.Close()
	if err != nil || counts.Notifications[JobSending] != 1 || counts.Notifications[JobUnknown] != 0 {
		t.Fatalf("read-only status mutated sending job: %#v, %v", counts, err)
	}
	writable, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writable.Close()
	counts, err = writable.StatusCounts(ctx)
	if err != nil || counts.Notifications[JobUnknown] != 1 {
		t.Fatalf("writable recovery = %#v, %v", counts, err)
	}
}

func TestMigrationAddsFailClosedFeishuChatBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.sqlite")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`CREATE TABLE feishu_messages (message_id TEXT PRIMARY KEY, platform TEXT NOT NULL, account_id TEXT NOT NULL, comment_id TEXT NOT NULL, UNIQUE(platform,account_id,comment_id))`)
	if closeErr := legacy.Close(); err != nil || closeErr != nil {
		t.Fatalf("create legacy schema: %v; close: %v", err, closeErr)
	}
	migrated, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	rows, err := migrated.DB().Query("PRAGMA table_info(feishu_messages)")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		if name == "chat_id" {
			found = kind == "TEXT" && notNull == 1
		}
	}
	if err := rows.Err(); err != nil || !found {
		t.Fatalf("chat_id migration missing: found=%v err=%v", found, err)
	}
}

func TestMigrationAddsChargePlusColumnAndDefaultsLegacyRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-comments.sqlite")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`
CREATE TABLE comments (platform TEXT NOT NULL, account_id TEXT NOT NULL, comment_id TEXT NOT NULL, root_id TEXT NOT NULL DEFAULT '', parent_id TEXT NOT NULL DEFAULT '', resource_id TEXT NOT NULL, resource_title TEXT NOT NULL, resource_url TEXT NOT NULL, author_id TEXT NOT NULL, author_name TEXT NOT NULL, text TEXT NOT NULL, published_at INTEGER NOT NULL, PRIMARY KEY(platform,account_id,comment_id));
INSERT INTO comments(platform,account_id,comment_id,root_id,parent_id,resource_id,resource_title,resource_url,author_id,author_name,text,published_at) VALUES('bilibili','a','legacy','legacy','','video','title','https://example.test/video','author','name','old',0);`)
	if closeErr := legacy.Close(); err != nil || closeErr != nil {
		t.Fatalf("create legacy schema: %v; close: %v", err, closeErr)
	}

	migrated, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	var stored bool
	if err := migrated.DB().QueryRow(`SELECT is_charge_plus FROM comments WHERE comment_id='legacy'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored {
		t.Fatal("legacy row unexpectedly became charge-plus")
	}
	rows, err := migrated.DB().Query("PRAGMA table_info(comments)")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	foundChargePlus := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		if name == "is_charge_plus" {
			foundChargePlus = kind == "INTEGER" && notNull == 1
		}
	}
	if err := rows.Err(); err != nil || !foundChargePlus {
		t.Fatalf("comment flag migration missing: charge_plus=%v err=%v", foundChargePlus, err)
	}
	if err := migrated.Close(); err != nil {
		t.Fatal(err)
	}
	// Opening the same 0.1.8-era schema again must not attempt to add the
	// column twice.
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenReadOnlyLegacyDatabaseOmitsManualEngagementOutbox(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-0.1.4.sqlite")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`
CREATE TABLE sources (platform TEXT NOT NULL, account_id TEXT NOT NULL, initialized INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(platform,account_id));
CREATE TABLE comments (platform TEXT NOT NULL, account_id TEXT NOT NULL, comment_id TEXT NOT NULL, root_id TEXT NOT NULL DEFAULT '', parent_id TEXT NOT NULL DEFAULT '', resource_id TEXT NOT NULL, resource_title TEXT NOT NULL, resource_url TEXT NOT NULL, author_id TEXT NOT NULL, author_name TEXT NOT NULL, text TEXT NOT NULL, published_at INTEGER NOT NULL, PRIMARY KEY(platform,account_id,comment_id));
CREATE TABLE notification_jobs (action_id TEXT PRIMARY KEY, platform TEXT NOT NULL, account_id TEXT NOT NULL, comment_id TEXT NOT NULL, status TEXT NOT NULL, error_category TEXT NOT NULL DEFAULT '', external_id TEXT NOT NULL DEFAULT '', UNIQUE(platform,account_id,comment_id), FOREIGN KEY(platform,account_id,comment_id) REFERENCES comments(platform,account_id,comment_id));
CREATE TABLE feishu_messages (message_id TEXT PRIMARY KEY, chat_id TEXT NOT NULL DEFAULT '', platform TEXT NOT NULL, account_id TEXT NOT NULL, comment_id TEXT NOT NULL, UNIQUE(platform,account_id,comment_id), FOREIGN KEY(platform,account_id,comment_id) REFERENCES comments(platform,account_id,comment_id));
CREATE TABLE inbox (message_id TEXT PRIMARY KEY, parent_message_id TEXT NOT NULL, root_message_id TEXT NOT NULL, chat_id TEXT NOT NULL, user_id TEXT NOT NULL, received_at INTEGER NOT NULL);
CREATE TABLE reply_jobs (action_id TEXT PRIMARY KEY, inbound_message_id TEXT NOT NULL UNIQUE, feishu_message_id TEXT NOT NULL, platform TEXT NOT NULL, account_id TEXT NOT NULL, comment_id TEXT NOT NULL, text TEXT NOT NULL, status TEXT NOT NULL, error_category TEXT NOT NULL DEFAULT '', external_id TEXT NOT NULL DEFAULT '', FOREIGN KEY(inbound_message_id) REFERENCES inbox(message_id), FOREIGN KEY(platform,account_id,comment_id) REFERENCES comments(platform,account_id,comment_id));
CREATE TABLE reaction_jobs (action_id TEXT PRIMARY KEY, inbound_message_id TEXT NOT NULL, kind TEXT NOT NULL CHECK(kind IN ('read','ok')), message_id TEXT NOT NULL, emoji_type TEXT NOT NULL, status TEXT NOT NULL, error_category TEXT NOT NULL DEFAULT '', external_id TEXT NOT NULL DEFAULT '', UNIQUE(inbound_message_id,kind), FOREIGN KEY(inbound_message_id) REFERENCES inbox(message_id));
CREATE TABLE engagement_events (event_id TEXT PRIMARY KEY, message_id TEXT NOT NULL, user_id TEXT NOT NULL, kind TEXT NOT NULL CHECK(kind IN ('like')), received_at INTEGER NOT NULL, FOREIGN KEY(message_id) REFERENCES feishu_messages(message_id));
CREATE TABLE engagement_jobs (action_id TEXT PRIMARY KEY, message_id TEXT NOT NULL, platform TEXT NOT NULL, account_id TEXT NOT NULL, comment_id TEXT NOT NULL, kind TEXT NOT NULL CHECK(kind IN ('like')), status TEXT NOT NULL, error_category TEXT NOT NULL DEFAULT '', external_id TEXT NOT NULL DEFAULT '', UNIQUE(platform,account_id,comment_id,kind), FOREIGN KEY(message_id) REFERENCES feishu_messages(message_id), FOREIGN KEY(platform,account_id,comment_id) REFERENCES comments(platform,account_id,comment_id));
CREATE TABLE engagement_feedback_jobs (action_id TEXT PRIMARY KEY, engagement_action_id TEXT NOT NULL UNIQUE, message_id TEXT NOT NULL, emoji_type TEXT NOT NULL, status TEXT NOT NULL, error_category TEXT NOT NULL DEFAULT '', external_id TEXT NOT NULL DEFAULT '', FOREIGN KEY(engagement_action_id) REFERENCES engagement_jobs(action_id), FOREIGN KEY(message_id) REFERENCES feishu_messages(message_id));`)
	if closeErr := legacy.Close(); err != nil || closeErr != nil {
		t.Fatalf("create 0.1.4 schema: %v; close: %v", err, closeErr)
	}

	readOnly, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	counts, err := readOnly.StatusCounts(ctx)
	if err != nil || counts.ManualEngagementFeedbacks[JobPending] != 0 || counts.ManualEngagementFeedbacks[JobUnknown] != 0 {
		_ = readOnly.Close()
		t.Fatalf("legacy status = %#v, %v", counts, err)
	}
	for _, status := range []JobStatus{JobUnknown, JobFailed} {
		jobs, err := readOnly.ListTerminalJobs(ctx, status)
		if err != nil || len(jobs) != 0 {
			_ = readOnly.Close()
			t.Fatalf("legacy terminal %q = %#v, %v", status, jobs, err)
		}
	}
	if err := readOnly.Close(); err != nil {
		t.Fatal(err)
	}

	check, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var manualTableCount int
	if err := check.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='manual_engagement_feedback_jobs'`).Scan(&manualTableCount); err != nil || manualTableCount != 0 {
		t.Fatalf("read-only diagnostics changed schema: count=%d err=%v", manualTableCount, err)
	}
}

func TestDatabaseRejectsUnsafePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.sqlite")
	if err := os.WriteFile(path, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("accepted group-readable database")
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "bridge-link.sqlite")
		if err := os.Symlink(path, link); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(link); err == nil {
			t.Fatal("accepted database symlink")
		}
	}
}
func TestAcceptReplyDeduplicatesAndRequiresMapping(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	c := testComment()
	if e := s.RecordPoll(ctx, PlatformBilibili, "a", []Comment{c}); e != nil {
		t.Fatal(e)
	}
	c.ID = "new"
	if e := s.RecordPoll(ctx, PlatformBilibili, "a", []Comment{c}); e != nil {
		t.Fatal(e)
	}
	n, _ := s.ClaimNotification(ctx)
	if e := s.MarkNotificationSent(ctx, n.ActionID, "f1", "chat-1"); e != nil {
		t.Fatal(e)
	}
	in := IncomingReply{MessageID: "in", ParentMessageID: "f1", ChatID: "chat-1", Text: "reply"}
	added, e := s.AcceptReply(ctx, in)
	if e != nil || !added {
		t.Fatalf("added=%v err=%v", added, e)
	}
	added, e = s.AcceptReply(ctx, in)
	if e != nil || added {
		t.Fatalf("duplicate added=%v err=%v", added, e)
	}
	if _, e = s.AcceptReply(ctx, IncomingReply{MessageID: "other", ParentMessageID: "missing", ChatID: "chat-1", Text: "reply"}); !errors.Is(e, ErrUnmappedReply) {
		t.Fatalf("err=%v", e)
	}
	if _, e = s.AcceptReply(ctx, IncomingReply{MessageID: "thread-chat", ParentMessageID: "other-user", RootMessageID: "f1", ChatID: "chat-1", Text: "reply"}); !errors.Is(e, ErrUnmappedReply) {
		t.Fatalf("root-only mapping was accepted: %v", e)
	}
	if _, e = s.AcceptReply(ctx, IncomingReply{MessageID: "wrong-chat", ParentMessageID: "f1", ChatID: "other-chat", Text: "reply"}); !errors.Is(e, ErrUnmappedReply) {
		t.Fatalf("wrong chat err=%v", e)
	}
	if _, err := s.DB().ExecContext(ctx, `UPDATE feishu_messages SET chat_id='' WHERE message_id='f1'`); err != nil {
		t.Fatal(err)
	}
	if _, e = s.AcceptReply(ctx, IncomingReply{MessageID: "legacy-empty-chat", ParentMessageID: "f1", ChatID: "chat-1", Text: "reply"}); !errors.Is(e, ErrUnmappedReply) {
		t.Fatalf("legacy empty chat err=%v", e)
	}
}

func TestConcurrentDuplicateInboundCreatesOneReply(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.RecordPoll(ctx, PlatformBilibili, "a", []Comment{testComment()}); err != nil {
		t.Fatal(err)
	}
	c := testComment()
	c.ID = "new"
	if err := s.RecordPoll(ctx, PlatformBilibili, "a", []Comment{c}); err != nil {
		t.Fatal(err)
	}
	n, err := s.ClaimNotification(ctx)
	if err != nil || n == nil {
		t.Fatalf("claim notification = %#v, %v", n, err)
	}
	if err := s.MarkNotificationSent(ctx, n.ActionID, "f1", "chat-1"); err != nil {
		t.Fatal(err)
	}
	in := IncomingReply{MessageID: "same-inbound", ParentMessageID: "f1", ChatID: "chat-1", Text: "reply"}
	var accepted int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			added, err := s.AcceptReply(ctx, in)
			if err != nil {
				t.Errorf("AcceptReply: %v", err)
				return
			}
			if added {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if accepted != 1 {
		t.Fatalf("accepted %d duplicate deliveries", accepted)
	}
	job, err := s.ClaimReply(ctx)
	if err != nil || job == nil {
		t.Fatalf("claim reply = %#v, %v", job, err)
	}
	if !job.Comment.IsChargePlus {
		t.Fatalf("reply comment lost charge-plus flag: %#v", job.Comment)
	}
	if second, err := s.ClaimReply(ctx); err != nil || second != nil {
		t.Fatalf("duplicate reply job = %#v, %v", second, err)
	}
	reaction, err := s.ClaimReaction(ctx)
	if err != nil || reaction == nil || reaction.Kind != ReactionRead || reaction.MessageID != in.MessageID || reaction.EmojiType != StatusReading {
		t.Fatalf("read reaction = %#v, %v", reaction, err)
	}
	if second, err := s.ClaimReaction(ctx); err != nil || second != nil {
		t.Fatalf("duplicate read reaction = %#v, %v", second, err)
	}
}

func TestReplySuccessAtomicallyEnqueuesOKReaction(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	prepareMappedInbound(t, s, "in")
	reply, err := s.ClaimReply(ctx)
	if err != nil || reply == nil {
		t.Fatalf("claim reply = %#v, %v", reply, err)
	}
	if err := s.MarkReplySent(ctx, reply.ActionID, "platform-reply"); err != nil {
		t.Fatal(err)
	}
	counts, err := s.StatusCounts(ctx)
	if err != nil || counts.Replies[JobSent] != 1 || counts.Reactions[JobPending] != 2 {
		t.Fatalf("counts = %#v, %v", counts, err)
	}
	read, err := s.ClaimReaction(ctx)
	if err != nil || read == nil || read.Kind != ReactionRead {
		t.Fatalf("read reaction = %#v, %v", read, err)
	}
	if err := s.MarkReactionSent(ctx, read.ActionID, "read-reaction"); err != nil {
		t.Fatal(err)
	}
	ok, err := s.ClaimReaction(ctx)
	if err != nil || ok == nil || ok.Kind != ReactionOK || ok.MessageID != "in" || ok.EmojiType != OK {
		t.Fatalf("ok reaction = %#v, %v", ok, err)
	}
}

func TestReplyFailureDoesNotEnqueueOKReaction(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	prepareMappedInbound(t, s, "in")
	reply, err := s.ClaimReply(ctx)
	if err != nil || reply == nil {
		t.Fatalf("claim reply = %#v, %v", reply, err)
	}
	if err := s.MarkReplyFailed(ctx, reply.ActionID, JobFailed, errors.New("rejected")); err != nil {
		t.Fatal(err)
	}
	counts, err := s.StatusCounts(ctx)
	if err != nil || counts.Reactions[JobPending] != 1 {
		t.Fatalf("counts = %#v, %v", counts, err)
	}
}

func TestReactionRecoveryTerminalListingAndReset(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	prepareMappedInbound(t, s, "in")
	reaction, err := s.ClaimReaction(ctx)
	if err != nil || reaction == nil {
		t.Fatalf("claim reaction = %#v, %v", reaction, err)
	}
	if err := s.RecoverSending(ctx); err != nil {
		t.Fatal(err)
	}
	counts, err := s.StatusCounts(ctx)
	if err != nil || counts.Reactions[JobUnknown] != 1 {
		t.Fatalf("counts = %#v, %v", counts, err)
	}
	jobs, err := s.ListTerminalJobs(ctx, JobUnknown)
	if err != nil || len(jobs) != 1 || jobs[0].Kind != "reaction" || jobs[0].FeishuMessageID != "in" {
		t.Fatalf("terminal jobs = %#v, %v", jobs, err)
	}
	reset, err := s.ResetUnknownToPending(ctx, reaction.ActionID)
	if err != nil || !reset {
		t.Fatalf("reset = %v, %v", reset, err)
	}
	counts, err = s.StatusCounts(ctx)
	if err != nil || counts.Reactions[JobPending] != 1 {
		t.Fatalf("counts after reset = %#v, %v", counts, err)
	}
}

func prepareMappedInbound(t *testing.T, s *Store, inboundMessageID string) {
	t.Helper()
	ctx := context.Background()
	if err := s.RecordPoll(ctx, PlatformBilibili, "a", []Comment{testComment()}); err != nil {
		t.Fatal(err)
	}
	c := testComment()
	c.ID = "new"
	if err := s.RecordPoll(ctx, PlatformBilibili, "a", []Comment{c}); err != nil {
		t.Fatal(err)
	}
	n, err := s.ClaimNotification(ctx)
	if err != nil || n == nil {
		t.Fatalf("claim notification = %#v, %v", n, err)
	}
	if err := s.MarkNotificationSent(ctx, n.ActionID, "f1", "chat-1"); err != nil {
		t.Fatal(err)
	}
	added, err := s.AcceptReply(ctx, IncomingReply{MessageID: inboundMessageID, ParentMessageID: "f1", ChatID: "chat-1", Text: "reply"})
	if err != nil || !added {
		t.Fatalf("accept reply = %v, %v", added, err)
	}
}

func prepareMappedNotification(t *testing.T, s *Store, platform Platform, account, messageID string) {
	t.Helper()
	ctx := context.Background()
	comment := testComment()
	comment.Platform, comment.AccountID = platform, account
	if err := s.RecordPoll(ctx, platform, account, []Comment{comment}); err != nil {
		t.Fatal(err)
	}
	comment.ID = "new"
	if err := s.RecordPoll(ctx, platform, account, []Comment{comment}); err != nil {
		t.Fatal(err)
	}
	job, err := s.ClaimNotification(ctx)
	if err != nil || job == nil {
		t.Fatalf("claim notification = %#v, %v", job, err)
	}
	if err := s.MarkNotificationSent(ctx, job.ActionID, messageID, "chat-1"); err != nil {
		t.Fatal(err)
	}
}

func TestAcceptYouTubeEngagementCreatesOneManualFeedbackJob(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if added, err := s.AcceptEngagement(ctx, IncomingEngagement{EventID: "unknown-event", MessageID: "unknown", UserID: "user", ExpectedChatID: "chat-1", ReactionType: "THUMBSUP"}); added || !errors.Is(err, ErrUnmappedEngagement) {
		t.Fatalf("unmapped = %v, %v", added, err)
	}
	prepareMappedNotification(t, s, PlatformYouTube, "channel", "youtube-message")
	first := IncomingEngagement{EventID: "youtube-event", MessageID: "youtube-message", UserID: "user", ExpectedChatID: "chat-1", ReactionType: "THUMBSUP"}
	if added, err := s.AcceptEngagement(ctx, first); !added || err != nil {
		t.Fatalf("youtube = %v, %v", added, err)
	}
	if added, err := s.AcceptEngagement(ctx, first); added || err != nil {
		t.Fatalf("replayed youtube event = %v, %v", added, err)
	}
	if added, err := s.AcceptEngagement(ctx, IncomingEngagement{EventID: "youtube-event-2", MessageID: "youtube-message", UserID: "other-user", ExpectedChatID: "chat-1", EmojiType: "THUMBSUP"}); added || err != nil {
		t.Fatalf("different user duplicated YouTube instruction = %v, %v", added, err)
	}
	if added, err := s.AcceptEngagement(ctx, IncomingEngagement{EventID: "empty-user", MessageID: "youtube-message", ExpectedChatID: "chat-1", ReactionType: "THUMBSUP"}); added || !errors.Is(err, ErrInvalidEngagement) {
		t.Fatalf("empty user = %v, %v", added, err)
	}
	if added, err := s.AcceptEngagement(ctx, IncomingEngagement{EventID: "wrong-chat", MessageID: "youtube-message", UserID: "user", ExpectedChatID: "other-chat", ReactionType: "THUMBSUP"}); added || !errors.Is(err, ErrUnmappedEngagement) {
		t.Fatalf("wrong chat = %v, %v", added, err)
	}
	if _, err := s.DB().ExecContext(ctx, `UPDATE feishu_messages SET chat_id='' WHERE message_id='youtube-message'`); err != nil {
		t.Fatal(err)
	}
	if added, err := s.AcceptEngagement(ctx, IncomingEngagement{EventID: "legacy-empty-chat", MessageID: "youtube-message", UserID: "user", ExpectedChatID: "chat-1", ReactionType: "THUMBSUP"}); added || !errors.Is(err, ErrUnmappedEngagement) {
		t.Fatalf("legacy empty chat = %v, %v", added, err)
	}
	counts, err := s.StatusCounts(ctx)
	if err != nil || counts.Engagements[JobPending] != 0 || counts.ManualEngagementFeedbacks[JobPending] != 1 {
		t.Fatalf("youtube jobs = %#v, %v", counts, err)
	}

	prepareMappedNotification(t, s, Platform("other"), "other-account", "other-message")
	if added, err := s.AcceptEngagement(ctx, IncomingEngagement{EventID: "other-event", MessageID: "other-message", UserID: "user", ExpectedChatID: "chat-1", ReactionType: "THUMBSUP"}); added || !errors.Is(err, ErrUnsupportedEngagement) {
		t.Fatalf("unknown platform = %v, %v", added, err)
	}
}

func TestManualEngagementFeedbackRecoveryTerminalListingAndReset(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	prepareMappedNotification(t, s, PlatformYouTube, "channel", "youtube-message")
	if added, err := s.AcceptEngagement(ctx, IncomingEngagement{EventID: "youtube-event", MessageID: "youtube-message", UserID: "user", ExpectedChatID: "chat-1", ReactionType: "THUMBSUP"}); err != nil || !added {
		t.Fatalf("accept = %v, %v", added, err)
	}
	job, err := s.ClaimManualEngagementFeedback(ctx)
	if err != nil || job == nil || job.MessageID != "youtube-message" || job.Platform != PlatformYouTube || job.Kind != EngagementLike {
		t.Fatalf("claim = %#v, %v", job, err)
	}
	if err := s.RecoverSending(ctx); err != nil {
		t.Fatal(err)
	}
	if next, err := s.ClaimManualEngagementFeedback(ctx); err != nil || next != nil {
		t.Fatalf("recovered unknown retried = %#v, %v", next, err)
	}
	counts, err := s.StatusCounts(ctx)
	if err != nil || counts.ManualEngagementFeedbacks[JobUnknown] != 1 {
		t.Fatalf("counts = %#v, %v", counts, err)
	}
	jobs, err := s.ListTerminalJobs(ctx, JobUnknown)
	if err != nil || len(jobs) != 1 || jobs[0].Kind != "manual_engagement_feedback" || jobs[0].Platform != PlatformYouTube || jobs[0].CommentID != "new" || jobs[0].FeishuMessageID != "youtube-message" {
		t.Fatalf("terminal jobs = %#v, %v", jobs, err)
	}
	if reset, err := s.ResetUnknownToPending(ctx, job.ActionID); err != nil || !reset {
		t.Fatalf("reset = %v, %v", reset, err)
	}
	counts, err = s.StatusCounts(ctx)
	if err != nil || counts.ManualEngagementFeedbacks[JobPending] != 1 {
		t.Fatalf("counts after reset = %#v, %v", counts, err)
	}
}

func TestEngagementRecoveryMarksSendingUnknown(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	prepareMappedNotification(t, s, PlatformBilibili, "a", "f1")
	if added, err := s.AcceptEngagement(ctx, IncomingEngagement{EventID: "event", MessageID: "f1", UserID: "user", ExpectedChatID: "chat-1", ReactionType: "THUMBSUP"}); err != nil || !added {
		t.Fatalf("accept = %v, %v", added, err)
	}
	job, err := s.ClaimEngagement(ctx)
	if err != nil || job == nil {
		t.Fatalf("claim = %#v, %v", job, err)
	}
	if err := s.RecoverSending(ctx); err != nil {
		t.Fatal(err)
	}
	if next, err := s.ClaimEngagement(ctx); err != nil || next != nil {
		t.Fatalf("recovered unknown retried = %#v, %v", next, err)
	}
	counts, err := s.StatusCounts(ctx)
	if err != nil || counts.Engagements[JobUnknown] != 1 {
		t.Fatalf("counts = %#v, %v", counts, err)
	}
}

func TestEngagementFeedbackRecoveryMarksSendingUnknown(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	prepareMappedNotification(t, s, PlatformBilibili, "a", "f1")
	if added, err := s.AcceptEngagement(ctx, IncomingEngagement{EventID: "event", MessageID: "f1", UserID: "user", ExpectedChatID: "chat-1", ReactionType: "THUMBSUP"}); err != nil || !added {
		t.Fatalf("accept = %v, %v", added, err)
	}
	engagement, err := s.ClaimEngagement(ctx)
	if err != nil || engagement == nil {
		t.Fatalf("claim engagement = %#v, %v", engagement, err)
	}
	if !engagement.Comment.IsChargePlus {
		t.Fatalf("engagement comment lost charge-plus flag: %#v", engagement.Comment)
	}
	if err := s.MarkEngagementSent(ctx, engagement.ActionID, "platform-id"); err != nil {
		t.Fatal(err)
	}
	feedback, err := s.ClaimEngagementFeedback(ctx)
	if err != nil || feedback == nil || feedback.MessageID != "f1" || feedback.EmojiType != OK {
		t.Fatalf("claim feedback = %#v, %v", feedback, err)
	}
	if err := s.RecoverSending(ctx); err != nil {
		t.Fatal(err)
	}
	if next, err := s.ClaimEngagementFeedback(ctx); err != nil || next != nil {
		t.Fatalf("recovered feedback retried = %#v, %v", next, err)
	}
	counts, err := s.StatusCounts(ctx)
	if err != nil || counts.Engagements[JobSent] != 1 || counts.EngagementFeedbacks[JobUnknown] != 1 {
		t.Fatalf("counts = %#v, %v", counts, err)
	}
}

func TestConcurrentPollOnlyCreatesOneJob(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if e := s.RecordPoll(ctx, PlatformBilibili, "a", []Comment{testComment()}); e != nil {
		t.Fatal(e)
	}
	c := testComment()
	c.ID = "new"
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e := s.RecordPoll(ctx, PlatformBilibili, "a", []Comment{c}); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	j, e := s.ClaimNotification(ctx)
	if e != nil || j == nil {
		t.Fatal(e)
	}
	j, e = s.ClaimNotification(ctx)
	if e != nil || j != nil {
		t.Fatalf("duplicate job %#v %v", j, e)
	}
}

func TestCredentialInvalidIncidentDedupAndReasonAwareRecovery(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	created, err := s.RecordCredentialInvalid(ctx, PlatformBilibili, "a", CredentialInvalidLogin)
	if err != nil || !created {
		t.Fatalf("first incident = %v, %v", created, err)
	}
	created, err = s.RecordCredentialInvalid(ctx, PlatformBilibili, "a", CredentialInvalidLogin)
	if err != nil || created {
		t.Fatalf("duplicate incident = %v, %v", created, err)
	}
	created, err = s.RecordCredentialInvalid(ctx, PlatformBilibili, "a", CredentialInvalidCSRF)
	if err != nil || created {
		t.Fatalf("changed reason created a duplicate incident = %v, %v", created, err)
	}
	var state string
	var reason string
	if err := s.DB().QueryRowContext(ctx, `SELECT credential_state,credential_reason FROM source_credential_health WHERE platform=? AND account_id=?`, PlatformBilibili, "a").Scan(&state, &reason); err != nil {
		t.Fatal(err)
	}
	if state != credentialStateInvalid || reason != string(CredentialInvalidLogin) {
		t.Fatalf("health = %q/%q", state, reason)
	}
	if err := s.RecordPoll(ctx, PlatformBilibili, "a", []Comment{}); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRowContext(ctx, `SELECT credential_state FROM source_credential_health WHERE platform=? AND account_id=?`, PlatformBilibili, "a").Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != credentialStateHealthy {
		t.Fatalf("login poll did not recover: %q", state)
	}
	created, err = s.RecordCredentialInvalid(ctx, PlatformBilibili, "a", CredentialInvalidCSRF)
	if err != nil || !created {
		t.Fatalf("csrf incident = %v, %v", created, err)
	}
	if err := s.RecordPoll(ctx, PlatformBilibili, "a", []Comment{}); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRowContext(ctx, `SELECT credential_state FROM source_credential_health WHERE platform=? AND account_id=?`, PlatformBilibili, "a").Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != credentialStateInvalid {
		t.Fatalf("csrf poll incorrectly recovered: %q", state)
	}
	if err := s.RecordCredentialSuccess(ctx, PlatformBilibili, "a"); err != nil {
		t.Fatal(err)
	}
	created, err = s.RecordCredentialInvalid(ctx, PlatformBilibili, "a", CredentialInvalidCSRF)
	if err != nil || !created {
		t.Fatalf("csrf incident after write recovery = %v, %v", created, err)
	}
	counts, err := s.StatusCounts(ctx)
	if err != nil || counts.CredentialAlerts[JobPending] != 3 {
		t.Fatalf("alert counts = %#v, %v", counts, err)
	}
}

func TestCredentialAlertOutboxRecoveryTerminalListingAndReset(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if created, err := s.RecordCredentialInvalid(ctx, PlatformBilibili, "a", CredentialInvalidLogin); err != nil || !created {
		t.Fatalf("record = %v, %v", created, err)
	}
	job, err := s.ClaimCredentialAlert(ctx)
	if err != nil || job == nil || job.Platform != PlatformBilibili || job.AccountID != "a" || job.Reason != CredentialInvalidLogin || job.IncidentSeq != 1 {
		t.Fatalf("claim = %#v, %v", job, err)
	}
	if err := s.RecoverSending(ctx); err != nil {
		t.Fatal(err)
	}
	counts, err := s.StatusCounts(ctx)
	if err != nil || counts.CredentialAlerts[JobUnknown] != 1 {
		t.Fatalf("recovered counts = %#v, %v", counts, err)
	}
	jobs, err := s.ListTerminalJobs(ctx, JobUnknown)
	if err != nil || len(jobs) != 1 || jobs[0].Kind != "credential_alert" || jobs[0].Platform != PlatformBilibili {
		t.Fatalf("terminal jobs = %#v, %v", jobs, err)
	}
	if ok, err := s.ResetUnknownToPending(ctx, jobs[0].ActionID); err != nil || !ok {
		t.Fatalf("reset = %v, %v", ok, err)
	}
	if job, err = s.ClaimCredentialAlert(ctx); err != nil || job == nil {
		t.Fatalf("reclaim = %#v, %v", job, err)
	}
	if err := s.MarkCredentialAlertSent(ctx, job.ActionID, "feishu-alert"); err != nil {
		t.Fatal(err)
	}
	counts, err = s.StatusCounts(ctx)
	if err != nil || counts.CredentialAlerts[JobSent] != 1 {
		t.Fatalf("sent counts = %#v, %v", counts, err)
	}
}

func TestCredentialRefreshStoreLifecycleAndIncidentDeduplication(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	created, err := s.RecordCredentialInvalid(ctx, PlatformBilibili, "a", CredentialInvalidLogin)
	if err != nil || !created {
		t.Fatalf("record invalid = %v, %v", created, err)
	}
	if created, err = s.RecordCredentialInvalid(ctx, PlatformBilibili, "a", CredentialInvalidLogin); err != nil || created {
		t.Fatalf("duplicate invalid created refresh = %v, %v", created, err)
	}
	counts, err := s.StatusCounts(ctx)
	if err != nil || counts.CredentialRefreshes[JobPending] != 1 {
		t.Fatalf("refresh counts = %#v, %v", counts.CredentialRefreshes, err)
	}

	job, err := s.ClaimCredentialRefresh(ctx)
	if err != nil || job == nil || job.Stage != CredentialRefreshStageGenerate || job.Attempt != 1 || job.Status != JobSending {
		t.Fatalf("generate claim = %#v, %v", job, err)
	}
	if err := s.MarkCredentialRefreshGenerated(ctx, job.ActionID, CredentialRefreshSession{Key: "opaque-key", URL: "https://qr.test/opaque", ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	job, err = s.ClaimCredentialRefresh(ctx)
	if err != nil || job == nil || job.Stage != CredentialRefreshStageUpload || job.Session.Key != "opaque-key" || job.Status != JobSending {
		t.Fatalf("upload claim = %#v, %v", job, err)
	}
	if err := s.MarkCredentialRefreshUploaded(ctx, job.ActionID, "image-key"); err != nil {
		t.Fatal(err)
	}
	job, err = s.ClaimCredentialRefresh(ctx)
	if err != nil || job == nil || job.Stage != CredentialRefreshStageSend || job.ImageKey != "image-key" {
		t.Fatalf("send claim = %#v, %v", job, err)
	}
	if err := s.MarkCredentialRefreshSendUnknown(ctx, job.ActionID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `UPDATE credential_refresh_jobs SET next_poll_at=0 WHERE action_id=?`, job.ActionID); err != nil {
		t.Fatal(err)
	}
	job, err = s.ClaimCredentialRefresh(ctx)
	if err != nil || job == nil || job.Stage != CredentialRefreshStagePoll || job.Status != JobSending || job.ErrorCategory != "unknown_outcome" {
		t.Fatalf("poll claim after unknown send = %#v, %v", job, err)
	}
	if err := s.MarkCredentialRefreshPollState(ctx, job.ActionID, CredentialRefreshWaiting); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `UPDATE credential_refresh_jobs SET next_poll_at=0 WHERE action_id=?`, job.ActionID); err != nil {
		t.Fatal(err)
	}
	job, err = s.ClaimCredentialRefresh(ctx)
	if err != nil || job == nil || job.Stage != CredentialRefreshStagePoll {
		t.Fatalf("second poll claim = %#v, %v", job, err)
	}
	if err := s.MarkCredentialRefreshPollState(ctx, job.ActionID, CredentialRefreshExpired); err != nil {
		t.Fatal(err)
	}
	var key, qrURL, image string
	if err := s.DB().QueryRowContext(ctx, `SELECT qrcode_key,qr_url,image_key FROM credential_refresh_jobs WHERE action_id=?`, job.ActionID).Scan(&key, &qrURL, &image); err != nil {
		t.Fatal(err)
	}
	if key != "" || qrURL != "" || image != "" {
		t.Fatalf("temporary QR fields not cleared: key=%q url=%q image=%q", key, qrURL, image)
	}

	counts, err = s.StatusCounts(ctx)
	if err != nil || counts.CredentialRefreshes[JobFailed] != 1 {
		t.Fatalf("terminal refresh counts = %#v, %v", counts.CredentialRefreshes, err)
	}
	jobs, err := s.ListTerminalJobs(ctx, JobFailed)
	if err != nil || len(jobs) != 1 || jobs[0].Kind != "credential_refresh" || jobs[0].Platform != PlatformBilibili {
		t.Fatalf("terminal refresh jobs = %#v, %v", jobs, err)
	}
}

func TestCredentialRefreshUnknownRecoveryRules(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if created, err := s.RecordCredentialInvalid(ctx, PlatformBilibili, "a", CredentialInvalidLogin); err != nil || !created {
		t.Fatalf("record invalid = %v, %v", created, err)
	}
	job, err := s.ClaimCredentialRefresh(ctx)
	if err != nil || job == nil {
		t.Fatalf("claim = %#v, %v", job, err)
	}
	if err := s.MarkCredentialRefreshGenerated(ctx, job.ActionID, CredentialRefreshSession{Key: "key", URL: "https://qr.test/q", ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	job, err = s.ClaimCredentialRefresh(ctx)
	if err != nil || job == nil {
		t.Fatalf("upload claim = %#v, %v", job, err)
	}
	if err := s.MarkCredentialRefreshUploadFailed(ctx, job.ActionID, JobUnknown, errors.New("timeout")); err != nil {
		t.Fatal(err)
	}
	counts, err := s.StatusCounts(ctx)
	if err != nil || counts.CredentialRefreshes[JobUnknown] != 1 {
		t.Fatalf("unknown counts = %#v, %v", counts.CredentialRefreshes, err)
	}
	if _, err := s.ClaimCredentialRefresh(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ResetUnknownToPending(ctx, job.ActionID); err != nil || !ok {
		t.Fatalf("reset unknown = %v, %v", ok, err)
	}
	job, err = s.ClaimCredentialRefresh(ctx)
	if err != nil || job == nil || job.Stage != CredentialRefreshStageUpload {
		t.Fatalf("explicit upload retry = %#v, %v", job, err)
	}

	if _, err := s.DB().ExecContext(ctx, `UPDATE credential_refresh_jobs SET stage=?,status=? WHERE action_id=?`, CredentialRefreshStageGenerate, JobSending, job.ActionID); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverSending(ctx); err != nil {
		t.Fatal(err)
	}
	var status JobStatus
	if err := s.DB().QueryRowContext(ctx, `SELECT status FROM credential_refresh_jobs WHERE action_id=?`, job.ActionID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != JobPending {
		t.Fatalf("generate sending did not resume pending: %q", status)
	}
}
