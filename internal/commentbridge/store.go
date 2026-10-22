package commentbridge

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

// Store is the durable, single-process outbox for the comment bridge.
type Store struct{ db *sql.DB }

const maxStoredTextRunes = 20_000

type StatusCounts struct {
	Notifications             map[JobStatus]int
	CredentialAlerts          map[JobStatus]int
	CredentialRefreshes       map[JobStatus]int
	Replies                   map[JobStatus]int
	Reactions                 map[JobStatus]int
	Engagements               map[JobStatus]int
	EngagementFeedbacks       map[JobStatus]int
	ManualEngagementFeedbacks map[JobStatus]int
	Danmaku                   map[JobStatus]int
}

// TerminalJob identifies an operator-actionable job without exposing message text.
type TerminalJob struct {
	Kind            string
	ActionID        string
	Status          JobStatus
	Platform        Platform
	CommentID       string
	FeishuMessageID string
}

func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("comment bridge database path is empty")
	}
	openPath := path
	var initial os.FileInfo
	var created bool
	var err error
	if path != ":memory:" {
		openPath, initial, created, err = prepareDatabaseFile(path)
		if err != nil {
			return nil, err
		}
	}
	keepCreated := false
	defer func() {
		if created && !keepCreated {
			_ = os.Remove(openPath + "-wal")
			_ = os.Remove(openPath + "-shm")
			_ = os.Remove(openPath)
		}
	}()
	db, err := sql.Open("sqlite", openPath)
	if err != nil {
		return nil, fmt.Errorf("open comment bridge sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	for _, p := range []string{"PRAGMA foreign_keys = ON", "PRAGMA busy_timeout = 5000", "PRAGMA journal_mode = WAL"} {
		if _, err := db.ExecContext(context.Background(), p); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := s.RecoverSending(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	if path != ":memory:" {
		current, statErr := os.Lstat(openPath)
		if statErr != nil || !os.SameFile(initial, current) || !privateRegular(current) {
			_ = db.Close()
			return nil, errors.New("comment bridge database changed while opening")
		}
		for _, sidecar := range []string{openPath + "-wal", openPath + "-shm"} {
			if err := validatePrivateSidecar(sidecar); err != nil {
				_ = db.Close()
				return nil, err
			}
		}
	}
	keepCreated = true
	return s, nil
}

// OpenReadOnly opens an existing bridge database without migrations or
// recovery. Operator status commands use it so diagnostics never mutate jobs.
func OpenReadOnly(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" || path == ":memory:" {
		return nil, errors.New("comment bridge read-only database path is invalid")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, errors.New("resolve comment bridge database path")
	}
	initial, err := os.Lstat(abs)
	if err != nil || !privateRegular(initial) {
		return nil, errors.New("comment bridge database must be a regular 0600 file")
	}
	for _, sidecar := range []string{abs + "-wal", abs + "-shm"} {
		if err := validatePrivateSidecar(sidecar); err != nil {
			return nil, err
		}
	}
	dsn := (&url.URL{Scheme: "file", Path: abs, RawQuery: "mode=ro"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, errors.New("open read-only comment bridge database")
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	for _, pragma := range []string{"PRAGMA busy_timeout = 5000", "PRAGMA query_only = ON"} {
		if _, err := db.ExecContext(context.Background(), pragma); err != nil {
			_ = db.Close()
			return nil, errors.New("configure read-only comment bridge database")
		}
	}
	current, err := os.Lstat(abs)
	if err != nil || !os.SameFile(initial, current) || !privateRegular(current) {
		_ = db.Close()
		return nil, errors.New("comment bridge database changed while opening")
	}
	return &Store{db: db}, nil
}

func prepareDatabaseFile(path string) (string, os.FileInfo, bool, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", nil, false, errors.New("resolve comment bridge database path")
	}
	abs = filepath.Clean(abs)
	info, err := os.Lstat(abs)
	if errors.Is(err, os.ErrNotExist) {
		file, createErr := os.OpenFile(abs, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if createErr != nil {
			return "", nil, false, errors.New("create comment bridge database")
		}
		closeErr := file.Close()
		if closeErr != nil {
			_ = os.Remove(abs)
			return "", nil, false, errors.New("create comment bridge database")
		}
		info, err = os.Lstat(abs)
		if err != nil || !privateRegular(info) {
			_ = os.Remove(abs)
			return "", nil, false, errors.New("comment bridge database must be a regular 0600 file")
		}
		return abs, info, true, nil
	}
	if err != nil || !privateRegular(info) {
		return "", nil, false, errors.New("comment bridge database must be a regular 0600 file")
	}
	return abs, info, false, nil
}

func privateRegular(info os.FileInfo) bool {
	return info != nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm() == 0o600
}

func validatePrivateSidecar(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !privateRegular(info) {
		return errors.New("comment bridge database sidecar must be a regular 0600 file")
	}
	return nil
}
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS sources (platform TEXT NOT NULL, account_id TEXT NOT NULL, initialized INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(platform,account_id));
CREATE TABLE IF NOT EXISTS comments (platform TEXT NOT NULL, account_id TEXT NOT NULL, comment_id TEXT NOT NULL, root_id TEXT NOT NULL DEFAULT '', parent_id TEXT NOT NULL DEFAULT '', resource_id TEXT NOT NULL, resource_title TEXT NOT NULL, resource_url TEXT NOT NULL, author_id TEXT NOT NULL, author_name TEXT NOT NULL, text TEXT NOT NULL, published_at INTEGER NOT NULL, is_charge_plus INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(platform,account_id,comment_id));
CREATE TABLE IF NOT EXISTS notification_jobs (action_id TEXT PRIMARY KEY, platform TEXT NOT NULL, account_id TEXT NOT NULL, comment_id TEXT NOT NULL, status TEXT NOT NULL, error_category TEXT NOT NULL DEFAULT '', external_id TEXT NOT NULL DEFAULT '', UNIQUE(platform,account_id,comment_id), FOREIGN KEY(platform,account_id,comment_id) REFERENCES comments(platform,account_id,comment_id));
CREATE TABLE IF NOT EXISTS feishu_messages (message_id TEXT PRIMARY KEY, chat_id TEXT NOT NULL DEFAULT '', platform TEXT NOT NULL, account_id TEXT NOT NULL, comment_id TEXT NOT NULL, FOREIGN KEY(platform,account_id,comment_id) REFERENCES comments(platform,account_id,comment_id));
CREATE TABLE IF NOT EXISTS inbox (message_id TEXT PRIMARY KEY, parent_message_id TEXT NOT NULL, root_message_id TEXT NOT NULL, chat_id TEXT NOT NULL, user_id TEXT NOT NULL, received_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS reply_jobs (action_id TEXT PRIMARY KEY, inbound_message_id TEXT NOT NULL UNIQUE, feishu_message_id TEXT NOT NULL, platform TEXT NOT NULL, account_id TEXT NOT NULL, comment_id TEXT NOT NULL, text TEXT NOT NULL, status TEXT NOT NULL, error_category TEXT NOT NULL DEFAULT '', external_id TEXT NOT NULL DEFAULT '', FOREIGN KEY(inbound_message_id) REFERENCES inbox(message_id), FOREIGN KEY(platform,account_id,comment_id) REFERENCES comments(platform,account_id,comment_id));
CREATE TABLE IF NOT EXISTS reaction_jobs (action_id TEXT PRIMARY KEY, inbound_message_id TEXT NOT NULL, kind TEXT NOT NULL CHECK(kind IN ('read','ok')), message_id TEXT NOT NULL, emoji_type TEXT NOT NULL, status TEXT NOT NULL, error_category TEXT NOT NULL DEFAULT '', external_id TEXT NOT NULL DEFAULT '', UNIQUE(inbound_message_id,kind), FOREIGN KEY(inbound_message_id) REFERENCES inbox(message_id));
CREATE TABLE IF NOT EXISTS engagement_events (event_id TEXT PRIMARY KEY, message_id TEXT NOT NULL, user_id TEXT NOT NULL, kind TEXT NOT NULL CHECK(kind IN ('like')), received_at INTEGER NOT NULL, FOREIGN KEY(message_id) REFERENCES feishu_messages(message_id));
CREATE TABLE IF NOT EXISTS engagement_jobs (action_id TEXT PRIMARY KEY, message_id TEXT NOT NULL, platform TEXT NOT NULL, account_id TEXT NOT NULL, comment_id TEXT NOT NULL, kind TEXT NOT NULL CHECK(kind IN ('like')), status TEXT NOT NULL, error_category TEXT NOT NULL DEFAULT '', external_id TEXT NOT NULL DEFAULT '', UNIQUE(platform,account_id,comment_id,kind), FOREIGN KEY(message_id) REFERENCES feishu_messages(message_id), FOREIGN KEY(platform,account_id,comment_id) REFERENCES comments(platform,account_id,comment_id));
CREATE TABLE IF NOT EXISTS engagement_feedback_jobs (action_id TEXT PRIMARY KEY, engagement_action_id TEXT NOT NULL UNIQUE, message_id TEXT NOT NULL, emoji_type TEXT NOT NULL, status TEXT NOT NULL, error_category TEXT NOT NULL DEFAULT '', external_id TEXT NOT NULL DEFAULT '', FOREIGN KEY(engagement_action_id) REFERENCES engagement_jobs(action_id), FOREIGN KEY(message_id) REFERENCES feishu_messages(message_id));`)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS manual_engagement_feedback_jobs (
	action_id TEXT PRIMARY KEY,
	message_id TEXT NOT NULL,
	platform TEXT NOT NULL,
	account_id TEXT NOT NULL,
	comment_id TEXT NOT NULL,
	kind TEXT NOT NULL CHECK(kind IN ('like')),
	status TEXT NOT NULL,
	error_category TEXT NOT NULL DEFAULT '',
	external_id TEXT NOT NULL DEFAULT '',
	UNIQUE(platform,account_id,comment_id,kind),
	FOREIGN KEY(message_id) REFERENCES feishu_messages(message_id),
	FOREIGN KEY(platform,account_id,comment_id) REFERENCES comments(platform,account_id,comment_id)
);`)
	if err != nil {
		return err
	}
	if err := ensureColumn(s.db, "comments", "root_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := ensureColumn(s.db, "comments", "parent_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := ensureColumn(s.db, "comments", "is_charge_plus", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := ensureColumn(s.db, "feishu_messages", "chat_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.migrateFeishuMessageMappings(ctx); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS source_credential_health (
	platform TEXT NOT NULL,
	account_id TEXT NOT NULL,
	credential_state TEXT NOT NULL,
	credential_reason TEXT NOT NULL DEFAULT '',
	incident_seq INTEGER NOT NULL DEFAULT 0,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY(platform,account_id)
);
CREATE TABLE IF NOT EXISTS credential_alert_jobs (
	action_id TEXT PRIMARY KEY,
	platform TEXT NOT NULL,
	account_id TEXT NOT NULL,
	kind TEXT NOT NULL CHECK(kind IN ('credential_invalid')),
	credential_reason TEXT NOT NULL DEFAULT '',
	incident_seq INTEGER NOT NULL,
	status TEXT NOT NULL,
	error_category TEXT NOT NULL DEFAULT '',
	external_id TEXT NOT NULL DEFAULT '',
	UNIQUE(platform,account_id,kind,incident_seq)
);`)
	if err != nil {
		return err
	}
	if err := ensureColumn(s.db, "source_credential_health", "credential_reason", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := ensureColumn(s.db, "credential_alert_jobs", "credential_reason", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS credential_refresh_jobs (
	action_id TEXT PRIMARY KEY,
	platform TEXT NOT NULL,
	account_id TEXT NOT NULL,
	incident_seq INTEGER NOT NULL,
	stage TEXT NOT NULL CHECK(stage IN ('generate','upload','send','poll')),
	attempt INTEGER NOT NULL DEFAULT 0,
	qrcode_key TEXT NOT NULL DEFAULT '',
	qr_url TEXT NOT NULL DEFAULT '',
	image_key TEXT NOT NULL DEFAULT '',
	expires_at INTEGER NOT NULL DEFAULT 0,
	qr_message_id TEXT NOT NULL DEFAULT '',
	chat_id TEXT NOT NULL DEFAULT '',
	next_poll_at INTEGER NOT NULL DEFAULT 0,
	status TEXT NOT NULL,
	error_category TEXT NOT NULL DEFAULT '',
	UNIQUE(platform,account_id,incident_seq)
);`)
	if err != nil {
		return err
	}
	for _, column := range []struct {
		name string
		decl string
	}{
		{"incident_seq", "INTEGER NOT NULL DEFAULT 0"},
		{"stage", "TEXT NOT NULL DEFAULT 'generate'"},
		{"attempt", "INTEGER NOT NULL DEFAULT 0"},
		{"qrcode_key", "TEXT NOT NULL DEFAULT ''"},
		{"qr_url", "TEXT NOT NULL DEFAULT ''"},
		{"image_key", "TEXT NOT NULL DEFAULT ''"},
		{"expires_at", "INTEGER NOT NULL DEFAULT 0"},
		{"qr_message_id", "TEXT NOT NULL DEFAULT ''"},
		{"chat_id", "TEXT NOT NULL DEFAULT ''"},
		{"next_poll_at", "INTEGER NOT NULL DEFAULT 0"},
		{"status", "TEXT NOT NULL DEFAULT 'pending'"},
		{"error_category", "TEXT NOT NULL DEFAULT ''"},
	} {
		if err := ensureColumn(s.db, "credential_refresh_jobs", column.name, column.decl); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) migrateFeishuMessageMappings(ctx context.Context) (result error) {
	var uniqueIndexes int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_index_list('feishu_messages') WHERE origin='u'`).Scan(&uniqueIndexes); err != nil || uniqueIndexes == 0 {
		return err
	}
	// Explicit resends retain every message mapping; notification_jobs still deduplicates discovery.
	if _, err := s.db.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return err
	}
	defer func() {
		_, err := s.db.ExecContext(context.WithoutCancel(ctx), `PRAGMA foreign_keys=ON`)
		result = errors.Join(result, err)
	}()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `
CREATE TABLE feishu_messages_replacement (message_id TEXT PRIMARY KEY, chat_id TEXT NOT NULL DEFAULT '', platform TEXT NOT NULL, account_id TEXT NOT NULL, comment_id TEXT NOT NULL, FOREIGN KEY(platform,account_id,comment_id) REFERENCES comments(platform,account_id,comment_id));
INSERT INTO feishu_messages_replacement SELECT message_id,chat_id,platform,account_id,comment_id FROM feishu_messages;
DROP TABLE feishu_messages;
ALTER TABLE feishu_messages_replacement RENAME TO feishu_messages;`)
	if err != nil {
		return err
	}
	var violations int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		return err
	}
	if violations != 0 {
		return errors.New("feishu message migration violates existing references")
	}
	return tx.Commit()
}

func ensureColumn(db *sql.DB, table, column, declaration string) error {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = rows.Close()
			return err
		}
		found = found || name == column
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = db.Exec("ALTER TABLE " + table + " ADD COLUMN " + column + " " + declaration)
	return err
}

// RecordPoll records a complete poll. It accepts either (source, comments) or
// (platform, accountID, comments), retaining a small adapter-friendly API.
func (s *Store) RecordPoll(ctx context.Context, args ...any) error {
	var platform Platform
	var account string
	var comments []Comment
	if len(args) == 2 {
		if src, ok := args[0].(CommentSource); ok {
			var commentsOK bool
			comments, commentsOK = args[1].([]Comment)
			if commentsOK {
				platform, account = src.Platform(), src.AccountID()
			}
		}
	}
	if len(args) == 3 {
		platform, _ = args[0].(Platform)
		account, _ = args[1].(string)
		var commentsOK bool
		comments, commentsOK = args[2].([]Comment)
		if !commentsOK {
			platform, account = "", ""
		}
	}
	if platform == "" || account == "" {
		return errors.New("invalid RecordPoll arguments")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var initialized int
	err = tx.QueryRowContext(ctx, "SELECT initialized FROM sources WHERE platform=? AND account_id=?", platform, account).Scan(&initialized)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err = tx.ExecContext(ctx, "INSERT INTO sources(platform,account_id,initialized) VALUES(?,?,0)", platform, account); err != nil {
			return err
		}
		initialized = 0
	} else if err != nil {
		return err
	}
	for _, c := range comments {
		if strings.TrimSpace(c.ID) == "" {
			return errors.New("comment id is required")
		}
		if utf8.RuneCountInString(c.Text) > maxStoredTextRunes || utf8.RuneCountInString(c.ResourceTitle) > 2_000 || utf8.RuneCountInString(c.AuthorName) > 1_000 || len(c.ResourceURL) > 8_192 {
			return errors.New("comment fields exceed storage limits")
		}
		if c.Platform != "" && c.Platform != platform {
			return errors.New("comment platform does not match source")
		}
		if c.AccountID != "" && c.AccountID != account {
			return errors.New("comment account does not match source")
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO comments(platform,account_id,comment_id,root_id,parent_id,resource_id,resource_title,resource_url,author_id,author_name,text,published_at,is_charge_plus) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`, platform, account, c.ID, c.RootID, c.ParentID, c.ResourceID, c.ResourceTitle, c.ResourceURL, c.AuthorID, c.AuthorName, c.Text, c.PublishedAt.UnixNano(), c.IsChargePlus)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 && strings.TrimSpace(c.ResourceTitle) != "" {
			if _, err = tx.ExecContext(ctx, `UPDATE comments SET resource_title=? WHERE platform=? AND account_id=? AND comment_id=? AND resource_title=''`, c.ResourceTitle, platform, account, c.ID); err != nil {
				return err
			}
		}
		if initialized != 0 {
			if n == 1 {
				_, err = tx.ExecContext(ctx, `INSERT INTO notification_jobs(action_id,platform,account_id,comment_id,status) VALUES(?,?,?,?,?)`, actionID("notify", string(platform), account, c.ID), platform, account, c.ID, JobPending)
				if err != nil {
					return err
				}
			}
		}
	}
	if _, err = tx.ExecContext(ctx, "UPDATE sources SET initialized=1 WHERE platform=? AND account_id=?", platform, account); err != nil {
		return err
	}
	if err = markCredentialHealthyTx(ctx, tx, platform, account, false); err != nil {
		return err
	}
	return tx.Commit()
}

const (
	credentialStateHealthy = "healthy"
	credentialStateInvalid = "invalid"
)

// RecordCredentialInvalid records one credential-invalid incident and queues
// one body-free Feishu alert for it. It is idempotent while the source remains
// invalid for the same structured reason; a successful login poll or a
// successful Bilibili reply/engagement re-arms the source for a later incident.
// The variadic reason keeps the two-argument API source-compatible for older
// callers while new adapters should always pass a closed-set reason.
func (s *Store) RecordCredentialInvalid(ctx context.Context, platform Platform, accountID string, reasons ...CredentialInvalidReason) (bool, error) {
	platform = Platform(strings.TrimSpace(string(platform)))
	accountID = strings.TrimSpace(accountID)
	if platform == "" || accountID == "" {
		return false, errors.New("invalid credential source")
	}
	reason := CredentialInvalidUnknown
	if len(reasons) > 0 && reasons[0] != "" {
		reason = reasons[0]
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	now := time.Now().UnixNano()
	if _, err = tx.ExecContext(ctx, `INSERT INTO source_credential_health(platform,account_id,credential_state,credential_reason,incident_seq,updated_at) VALUES(?,?,?,?,?,?) ON CONFLICT(platform,account_id) DO NOTHING`, platform, accountID, credentialStateHealthy, "", 0, now); err != nil {
		return false, err
	}
	var state string
	var currentReason CredentialInvalidReason
	var incidentSeq int64
	if err = tx.QueryRowContext(ctx, `SELECT credential_state,credential_reason,incident_seq FROM source_credential_health WHERE platform=? AND account_id=?`, platform, accountID).Scan(&state, &currentReason, &incidentSeq); err != nil {
		return false, err
	}
	// Once a source is invalid, a second structured cause is still part of the
	// same operator incident. Keep the first cause as the recovery rule and do
	// not create another alert until that incident has been proven healthy.
	if state == credentialStateInvalid {
		return false, tx.Commit()
	}
	incidentSeq++
	if _, err = tx.ExecContext(ctx, `UPDATE source_credential_health SET credential_state=?,credential_reason=?,incident_seq=?,updated_at=? WHERE platform=? AND account_id=?`, credentialStateInvalid, reason, incidentSeq, now, platform, accountID); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO credential_alert_jobs(action_id,platform,account_id,kind,credential_reason,incident_seq,status) VALUES(?,?,?,?,?,?,?) ON CONFLICT(platform,account_id,kind,incident_seq) DO NOTHING`, actionID("credential-alert", string(platform), accountID, strconv.FormatInt(incidentSeq, 10)), platform, accountID, "credential_invalid", reason, incidentSeq, JobPending)
	if err != nil {
		return false, err
	}
	created, _ := result.RowsAffected()
	if _, err = tx.ExecContext(ctx, `INSERT INTO credential_refresh_jobs(action_id,platform,account_id,incident_seq,stage,attempt,status) VALUES(?,?,?,?,?,?,?) ON CONFLICT(platform,account_id,incident_seq) DO NOTHING`, actionID("credential-refresh", string(platform), accountID, strconv.FormatInt(incidentSeq, 10)), platform, accountID, incidentSeq, CredentialRefreshStageGenerate, 1, JobPending); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return created == 1, nil
}

// markCredentialHealthyTx is called only after a complete successful poll has
// been persisted, keeping recovery and the next invalid incident atomic with
// the poll transaction.
func markCredentialHealthyTx(ctx context.Context, tx *sql.Tx, platform Platform, accountID string, recoverCSRF bool) error {
	var state string
	var reason CredentialInvalidReason
	if err := tx.QueryRowContext(ctx, `SELECT credential_state,credential_reason FROM source_credential_health WHERE platform=? AND account_id=?`, platform, accountID).Scan(&state, &reason); errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx, `INSERT INTO source_credential_health(platform,account_id,credential_state,credential_reason,incident_seq,updated_at) VALUES(?,?,?,?,?,?)`, platform, accountID, credentialStateHealthy, "", 0, time.Now().UnixNano())
		return err
	} else if err != nil {
		return err
	} else if !recoverCSRF && state == credentialStateInvalid && reason == CredentialInvalidCSRF {
		// A successful list/read operation cannot prove that the CSRF token is
		// usable for an external write. Leave the incident active until a
		// successful Bilibili reply or engagement calls RecordCredentialSuccess.
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE source_credential_health SET credential_state=?,credential_reason=?,updated_at=? WHERE platform=? AND account_id=?`, credentialStateHealthy, "", time.Now().UnixNano(), platform, accountID)
	return err
}

// RecordCredentialSuccess re-arms a source after a successful external write.
// The bridge calls this only for Bilibili reply/engagement success, which is
// the proof needed to recover a CSRF incident.
func (s *Store) RecordCredentialSuccess(ctx context.Context, platform Platform, accountID string) error {
	platform = Platform(strings.TrimSpace(string(platform)))
	accountID = strings.TrimSpace(accountID)
	if platform == "" || accountID == "" {
		return errors.New("invalid credential source")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = markCredentialHealthyTx(ctx, tx, platform, accountID, true); err != nil {
		return err
	}
	return tx.Commit()
}

func actionID(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (s *Store) ClaimNotification(ctx context.Context) (*NotificationJob, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	job, err := scanNotification(tx.QueryRowContext(ctx, `SELECT n.action_id,c.platform,c.account_id,c.comment_id,c.root_id,c.parent_id,c.resource_id,c.resource_title,c.resource_url,c.author_id,c.author_name,c.text,c.published_at,c.is_charge_plus,n.status FROM notification_jobs n JOIN comments c USING(platform,account_id,comment_id) WHERE n.status=? ORDER BY n.rowid LIMIT 1`, JobPending))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r, err := tx.ExecContext(ctx, "UPDATE notification_jobs SET status=? WHERE action_id=? AND status=?", JobSending, job.ActionID, JobPending)
	if err != nil {
		return nil, err
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return nil, nil
	}
	job.Status = JobSending
	return job, tx.Commit()
}

// ClaimCredentialAlert claims one pending body-free credential alert.
func (s *Store) ClaimCredentialAlert(ctx context.Context) (*CredentialAlertJob, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	job, err := scanCredentialAlert(tx.QueryRowContext(ctx, `SELECT action_id,platform,account_id,credential_reason,incident_seq,status FROM credential_alert_jobs WHERE status=? ORDER BY rowid LIMIT 1`, JobPending))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE credential_alert_jobs SET status=? WHERE action_id=? AND status=?`, JobSending, job.ActionID, JobPending)
	if err != nil {
		return nil, err
	}
	updated, _ := result.RowsAffected()
	if updated != 1 {
		return nil, nil
	}
	job.Status = JobSending
	return job, tx.Commit()
}

func (s *Store) MarkCredentialAlertSent(ctx context.Context, actionID, externalID string) error {
	if strings.TrimSpace(externalID) == "" {
		return errors.New("empty feishu credential alert id")
	}
	return s.finishCredentialAlert(ctx, actionID, JobSent, externalID)
}

func (s *Store) MarkCredentialAlertFailed(ctx context.Context, actionID string, status JobStatus, err error) error {
	if status != JobFailed && status != JobUnknown {
		return errors.New("invalid terminal credential alert status")
	}
	return s.finishCredentialAlert(ctx, actionID, status, "")
}

func (s *Store) finishCredentialAlert(ctx context.Context, actionID string, status JobStatus, externalID string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE credential_alert_jobs SET status=?,external_id=?,error_category=? WHERE action_id=? AND status=?`, status, externalID, terminalCategory(status), actionID, JobSending)
	if err != nil {
		return err
	}
	updated, _ := result.RowsAffected()
	if updated != 1 {
		return sql.ErrNoRows
	}
	return nil
}

const (
	maxCredentialRefreshKeyLength   = 1_024
	maxCredentialRefreshURLLength   = 16_384
	maxCredentialRefreshImageLength = 1_024
	maxCredentialRefreshMessageID   = 1_024
	credentialRefreshPollInterval   = 2 * time.Second
)

// ClaimCredentialRefresh claims the next pending QR-refresh stage. The
// incident attempt is assigned when the incident/reset is created; the durable
// action ID remains stable while stage-specific notifier keys include it.
func (s *Store) ClaimCredentialRefresh(ctx context.Context) (*CredentialRefreshJob, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now().UnixNano()
	job, err := scanCredentialRefresh(tx.QueryRowContext(ctx, `SELECT action_id,platform,account_id,incident_seq,stage,attempt,qrcode_key,qr_url,image_key,expires_at,qr_message_id,chat_id,next_poll_at,status,error_category FROM credential_refresh_jobs WHERE status=? AND (stage<>? OR next_poll_at<=?) ORDER BY rowid LIMIT 1`, JobPending, CredentialRefreshStagePoll, now))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if job.Stage == "" {
		job.Stage = CredentialRefreshStageGenerate
	}
	if job.Stage == CredentialRefreshStageGenerate {
		// Older additive migrations may have created a zero-valued attempt;
		// normalize that one legacy row while claiming it.
		if job.Attempt < 1 {
			_, err = tx.ExecContext(ctx, `UPDATE credential_refresh_jobs SET status=?,attempt=1 WHERE action_id=? AND status=?`, JobSending, job.ActionID, JobPending)
			job.Attempt = 1
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE credential_refresh_jobs SET status=? WHERE action_id=? AND status=?`, JobSending, job.ActionID, JobPending)
		}
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE credential_refresh_jobs SET status=? WHERE action_id=? AND status=?`, JobSending, job.ActionID, JobPending)
	}
	if err != nil {
		return nil, err
	}
	job.Status = JobSending
	return job, tx.Commit()
}

func validateCredentialRefreshSession(session CredentialRefreshSession) error {
	if strings.TrimSpace(session.Key) == "" || strings.TrimSpace(session.URL) == "" {
		return errors.New("credential refresh session key and URL are required")
	}
	if len(session.Key) > maxCredentialRefreshKeyLength || len(session.URL) > maxCredentialRefreshURLLength {
		return errors.New("credential refresh session exceeds storage limits")
	}
	if session.ExpiresAt.IsZero() {
		return errors.New("credential refresh session expiry is required")
	}
	return nil
}

func validateCredentialRefreshValue(value string, limit int, field string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("empty credential refresh " + field)
	}
	if len(value) > limit {
		return errors.New("credential refresh " + field + " exceeds storage limits")
	}
	return nil
}

// MarkCredentialRefreshGenerated persists only the short-lived QR session and
// advances the job to the upload stage.
func (s *Store) MarkCredentialRefreshGenerated(ctx context.Context, actionID string, session CredentialRefreshSession) error {
	if strings.TrimSpace(actionID) == "" {
		return errors.New("empty credential refresh action id")
	}
	if err := validateCredentialRefreshSession(session); err != nil {
		return err
	}
	r, err := s.db.ExecContext(ctx, `UPDATE credential_refresh_jobs SET stage=?,qrcode_key=?,qr_url=?,image_key='',expires_at=?,next_poll_at=0,status=?,error_category='' WHERE action_id=? AND stage=? AND status=?`, CredentialRefreshStageUpload, strings.TrimSpace(session.Key), strings.TrimSpace(session.URL), session.ExpiresAt.UnixNano(), JobPending, actionID, CredentialRefreshStageGenerate, JobSending)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n != 1 {
		return sql.ErrNoRows
	}
	return nil
}

// MarkCredentialRefreshUploaded advances an uploaded QR to the Feishu send
// stage. imageKey is an opaque Feishu identifier, never the image bytes.
func (s *Store) MarkCredentialRefreshUploaded(ctx context.Context, actionID, imageKey string) error {
	imageKey = strings.TrimSpace(imageKey)
	if err := validateCredentialRefreshValue(imageKey, maxCredentialRefreshImageLength, "image key"); err != nil {
		return err
	}
	r, err := s.db.ExecContext(ctx, `UPDATE credential_refresh_jobs SET stage=?,image_key=?,next_poll_at=0,status=?,error_category='' WHERE action_id=? AND stage=? AND status=?`, CredentialRefreshStageSend, imageKey, JobPending, actionID, CredentialRefreshStageUpload, JobSending)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n != 1 {
		return sql.ErrNoRows
	}
	return nil
}

// MarkCredentialRefreshUploadFailed records an upload outcome. An unknown
// upload remains terminal and retains the session for an explicit operator
// reset; a deterministic failure clears all short-lived session material.
func (s *Store) MarkCredentialRefreshUploadFailed(ctx context.Context, actionID string, status JobStatus, err error) error {
	return s.finishCredentialRefreshFailure(ctx, actionID, CredentialRefreshStageUpload, status, err)
}

func (s *Store) markCredentialRefreshSent(ctx context.Context, actionID, messageID, chatID string) error {
	messageID = strings.TrimSpace(messageID)
	if err := validateCredentialRefreshValue(messageID, maxCredentialRefreshMessageID, "message id"); err != nil {
		return err
	}
	r, err := s.db.ExecContext(ctx, `UPDATE credential_refresh_jobs SET stage=?,qr_message_id=?,chat_id=?,next_poll_at=0,status=?,error_category='' WHERE action_id=? AND stage=? AND status=?`, CredentialRefreshStagePoll, messageID, chatID, JobPending, actionID, CredentialRefreshStageSend, JobSending)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n != 1 {
		return sql.ErrNoRows
	}
	return nil
}

// MarkCredentialRefreshSent records the QR message and moves the job into its
// safe polling stage. chatID is optional for source compatibility, but the
// running bridge always supplies its configured target chat so retry replies
// cannot cross group boundaries.
func (s *Store) MarkCredentialRefreshSent(ctx context.Context, actionID, messageID string, chatIDs ...string) error {
	chatID := ""
	if len(chatIDs) > 0 {
		chatID = strings.TrimSpace(chatIDs[0])
	}
	if chatID != "" && len(chatID) > 1_024 {
		return errors.New("credential refresh chat id exceeds storage limits")
	}
	return s.markCredentialRefreshSent(ctx, actionID, messageID, chatID)
}

// MarkCredentialRefreshSendUnknown deliberately enters poll: the QR send may
// have reached Feishu even when the response was lost. The safe category is
// retained for status diagnostics, but the QR session is not retried blindly.
func (s *Store) MarkCredentialRefreshSendUnknown(ctx context.Context, actionID string) error {
	r, err := s.db.ExecContext(ctx, `UPDATE credential_refresh_jobs SET stage=?,status=?,next_poll_at=?,error_category=? WHERE action_id=? AND stage=? AND status=?`, CredentialRefreshStagePoll, JobPending, time.Now().Add(credentialRefreshPollInterval).UnixNano(), "unknown_outcome", actionID, CredentialRefreshStageSend, JobSending)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n != 1 {
		return sql.ErrNoRows
	}
	return nil
}

// MarkCredentialRefreshPollState records a safe QR poll result. Waiting and
// scanned remain pending; succeeded/expired are terminal and clear the
// short-lived session fields.
func (s *Store) MarkCredentialRefreshPollState(ctx context.Context, actionID string, state CredentialRefreshState) error {
	switch state {
	case CredentialRefreshWaiting, CredentialRefreshScanned:
		r, err := s.db.ExecContext(ctx, `UPDATE credential_refresh_jobs SET status=?,next_poll_at=? WHERE action_id=? AND stage=? AND status=?`, JobPending, time.Now().Add(credentialRefreshPollInterval).UnixNano(), actionID, CredentialRefreshStagePoll, JobSending)
		if err != nil {
			return err
		}
		if n, _ := r.RowsAffected(); n != 1 {
			return sql.ErrNoRows
		}
		return nil
	case CredentialRefreshSucceeded:
		return s.finishCredentialRefresh(ctx, actionID, JobSent, "")
	case CredentialRefreshExpired:
		return s.finishCredentialRefresh(ctx, actionID, JobFailed, "expired")
	default:
		return errors.New("invalid credential refresh poll state")
	}
}

// MarkCredentialRefreshPollFailed marks a deterministic poll failure. Read
// failures are not retried automatically once this method is chosen by the
// bridge; an operator may explicitly reset the resulting terminal job.
func (s *Store) MarkCredentialRefreshPollFailed(ctx context.Context, actionID string, status JobStatus, err error) error {
	return s.finishCredentialRefreshFailure(ctx, actionID, CredentialRefreshStagePoll, status, err)
}

// MarkCredentialRefreshFailed is used for a missing adapter or a failure
// before the stage-specific external call. It preserves a previously sent QR
// message while clearing any still-live session material.
func (s *Store) MarkCredentialRefreshFailed(ctx context.Context, actionID string, status JobStatus, err error) error {
	if status != JobFailed && status != JobUnknown {
		return errors.New("invalid terminal credential refresh status")
	}
	category := terminalCategory(status)
	if status == JobFailed {
		category = "external_error"
	}
	r, execErr := s.db.ExecContext(ctx, `UPDATE credential_refresh_jobs SET status=?,error_category=?,next_poll_at=0,qrcode_key=CASE WHEN ?=? THEN '' ELSE qrcode_key END,qr_url=CASE WHEN ?=? THEN '' ELSE qr_url END,image_key=CASE WHEN ?=? THEN '' ELSE image_key END,expires_at=CASE WHEN ?=? THEN 0 ELSE expires_at END WHERE action_id=? AND status=?`, status, category, status, JobFailed, status, JobFailed, status, JobFailed, status, JobFailed, actionID, JobSending)
	if execErr != nil {
		return execErr
	}
	if n, _ := r.RowsAffected(); n != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) finishCredentialRefreshFailure(ctx context.Context, actionID string, stage CredentialRefreshStage, status JobStatus, _ error) error {
	if status != JobFailed && status != JobUnknown {
		return errors.New("invalid terminal credential refresh status")
	}
	category := terminalCategory(status)
	if status == JobFailed {
		category = "external_error"
	}
	r, err := s.db.ExecContext(ctx, `UPDATE credential_refresh_jobs SET status=?,error_category=?,next_poll_at=0,qrcode_key=CASE WHEN ?=? THEN '' ELSE qrcode_key END,qr_url=CASE WHEN ?=? THEN '' ELSE qr_url END,image_key=CASE WHEN ?=? THEN '' ELSE image_key END,expires_at=CASE WHEN ?=? THEN 0 ELSE expires_at END WHERE action_id=? AND stage=? AND status=?`, status, category, status, JobFailed, status, JobFailed, status, JobFailed, status, JobFailed, actionID, stage, JobSending)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) finishCredentialRefresh(ctx context.Context, actionID string, status JobStatus, category string) error {
	if status != JobSent && status != JobFailed {
		return errors.New("invalid credential refresh terminal status")
	}
	if category == "" && status == JobFailed {
		category = "external_error"
	}
	r, err := s.db.ExecContext(ctx, `UPDATE credential_refresh_jobs SET status=?,error_category=?,next_poll_at=0,qrcode_key='',qr_url='',image_key='',expires_at=0 WHERE action_id=? AND stage=? AND status=?`, status, category, actionID, CredentialRefreshStagePoll, JobSending)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n != 1 {
		return sql.ErrNoRows
	}
	return nil
}

// ResetCredentialRefresh starts a new QR generation attempt for a failed job
// only when the operator quotes the exact QR message that produced it.
func (s *Store) ResetCredentialRefresh(ctx context.Context, actionID, quoteMessageID string) (bool, error) {
	actionID = strings.TrimSpace(actionID)
	quoteMessageID = strings.TrimSpace(quoteMessageID)
	if actionID == "" || quoteMessageID == "" {
		return false, errors.New("credential refresh action and quote message are required")
	}
	r, err := s.db.ExecContext(ctx, `UPDATE credential_refresh_jobs SET stage=?,status=?,attempt=attempt+1,qrcode_key='',qr_url='',image_key='',expires_at=0,next_poll_at=0,qr_message_id='',error_category='' WHERE action_id=? AND qr_message_id=? AND status=?`, CredentialRefreshStageGenerate, JobPending, actionID, quoteMessageID, JobFailed)
	if err != nil {
		return false, err
	}
	n, _ := r.RowsAffected()
	return n == 1, nil
}

func (s *Store) MarkNotificationSent(ctx context.Context, action, messageID, chatID string) error {
	return s.finishNotification(ctx, action, JobSent, messageID, chatID)
}

// Webhooks return no message ID and must never authorize quoted replies or reactions.
func (s *Store) MarkWebhookNotificationSent(ctx context.Context, action string) error {
	r, err := s.db.ExecContext(ctx, `UPDATE notification_jobs SET status=?,external_id='',error_category='' WHERE action_id=? AND platform=? AND status=?`, JobSent, action, PlatformYouTube, JobSending)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) MarkNotificationFailed(ctx context.Context, action string, status JobStatus, err error) error {
	if status != JobFailed && status != JobUnknown {
		return errors.New("invalid terminal notification status")
	}
	return s.finishNotification(ctx, action, status, "", "")
}
func (s *Store) finishNotification(ctx context.Context, action string, status JobStatus, messageID, chatID string) error {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var p, a, c string
	if e = tx.QueryRowContext(ctx, "SELECT platform,account_id,comment_id FROM notification_jobs WHERE action_id=? AND status=?", action, JobSending).Scan(&p, &a, &c); e != nil {
		return e
	}
	if status == JobSent {
		messageID, chatID = strings.TrimSpace(messageID), strings.TrimSpace(chatID)
		if messageID == "" || chatID == "" {
			return errors.New("empty feishu notification identity")
		}
		if _, e = tx.ExecContext(ctx, "INSERT INTO feishu_messages(message_id,chat_id,platform,account_id,comment_id) VALUES(?,?,?,?,?)", messageID, chatID, p, a, c); e != nil {
			return e
		}
	}
	_, e = tx.ExecContext(ctx, "UPDATE notification_jobs SET status=?, external_id=?, error_category=? WHERE action_id=?", status, messageID, terminalCategory(status), action)
	if e != nil {
		return e
	}
	return tx.Commit()
}

func (s *Store) AcceptReply(ctx context.Context, in IncomingReply) (bool, error) {
	if strings.TrimSpace(in.MessageID) == "" {
		return false, errors.New("empty inbound message id")
	}
	in.ChatID = strings.TrimSpace(in.ChatID)
	in.Text = strings.TrimSpace(in.Text)
	if in.Text == "" || utf8.RuneCountInString(in.Text) > 10_000 {
		return false, ErrInvalidReply
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return false, e
	}
	defer tx.Rollback()
	var count int
	if e = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM inbox WHERE message_id=?", in.MessageID).Scan(&count); e != nil {
		return false, e
	}
	if count > 0 {
		return false, tx.Commit()
	}
	// Only the direct parent may resolve a platform action. RootID is retained
	// for audit/context, but accepting it as a fallback would turn any later
	// conversation inside that thread into an accidental platform reply.
	parentID := strings.TrimSpace(in.ParentMessageID)
	if parentID == "" || in.ChatID == "" {
		return false, ErrUnmappedReply
	}
	// A QR refresh reply is an operator control message, never a platform
	// reply. Only an exact retry command quoting a terminal QR message may
	// reset the existing incident job.
	if in.Text == CredentialRefreshRetryText {
		var refreshActionID string
		var refreshStatus JobStatus
		var refreshChatID string
		refreshErr := tx.QueryRowContext(ctx, `SELECT action_id,status,chat_id FROM credential_refresh_jobs WHERE qr_message_id=?`, parentID).Scan(&refreshActionID, &refreshStatus, &refreshChatID)
		if refreshErr == nil {
			if refreshStatus != JobFailed || refreshChatID == "" || refreshChatID != in.ChatID {
				return false, ErrUnmappedReply
			}
			if in.ReceivedAt.IsZero() {
				in.ReceivedAt = time.Now()
			}
			if _, e = tx.ExecContext(ctx, "INSERT INTO inbox(message_id,parent_message_id,root_message_id,chat_id,user_id,received_at) VALUES(?,?,?,?,?,?)", in.MessageID, in.ParentMessageID, in.RootMessageID, in.ChatID, in.UserID, in.ReceivedAt.UnixNano()); e != nil {
				return false, e
			}
			r, updateErr := tx.ExecContext(ctx, `UPDATE credential_refresh_jobs SET stage=?,status=?,attempt=attempt+1,qrcode_key='',qr_url='',image_key='',expires_at=0,next_poll_at=0,qr_message_id='',error_category='' WHERE action_id=? AND qr_message_id=? AND status=?`, CredentialRefreshStageGenerate, JobPending, refreshActionID, parentID, JobFailed)
			if updateErr != nil {
				return false, updateErr
			}
			if n, _ := r.RowsAffected(); n != 1 {
				return false, ErrUnmappedReply
			}
			if _, e = tx.ExecContext(ctx, "INSERT INTO reaction_jobs(action_id,inbound_message_id,kind,message_id,emoji_type,status) VALUES(?,?,?,?,?,?)", actionID("reaction", string(ReactionRead), in.MessageID), in.MessageID, ReactionRead, in.MessageID, StatusReading, JobPending); e != nil {
				return false, e
			}
			return true, tx.Commit()
		}
		if !errors.Is(refreshErr, sql.ErrNoRows) {
			return false, refreshErr
		}
	}
	var p, a, c, msg string
	e = tx.QueryRowContext(ctx, `SELECT platform,account_id,comment_id,message_id FROM feishu_messages WHERE message_id=? AND chat_id=?`, parentID, in.ChatID).Scan(&p, &a, &c, &msg)
	if errors.Is(e, sql.ErrNoRows) {
		return false, ErrUnmappedReply
	}
	if e != nil {
		return false, e
	}
	if in.ReceivedAt.IsZero() {
		in.ReceivedAt = time.Now()
	}
	if _, e = tx.ExecContext(ctx, "INSERT INTO inbox(message_id,parent_message_id,root_message_id,chat_id,user_id,received_at) VALUES(?,?,?,?,?,?)", in.MessageID, in.ParentMessageID, in.RootMessageID, in.ChatID, in.UserID, in.ReceivedAt.UnixNano()); e != nil {
		return false, e
	}
	_, e = tx.ExecContext(ctx, "INSERT INTO reply_jobs(action_id,inbound_message_id,feishu_message_id,platform,account_id,comment_id,text,status) VALUES(?,?,?,?,?,?,?,?)", actionID("reply", in.MessageID), in.MessageID, msg, p, a, c, in.Text, JobPending)
	if e != nil {
		return false, e
	}
	_, e = tx.ExecContext(ctx, "INSERT INTO reaction_jobs(action_id,inbound_message_id,kind,message_id,emoji_type,status) VALUES(?,?,?,?,?,?)", actionID("reaction", string(ReactionRead), in.MessageID), in.MessageID, ReactionRead, in.MessageID, StatusReading, JobPending)
	if e != nil {
		return false, e
	}
	return true, tx.Commit()
}

// AcceptEngagement stores each Feishu event once and creates at most one
// platform action for a comment/kind pair. Unknown bot messages are ignored so
// reactions elsewhere in a chat can never become platform writes.
func (s *Store) AcceptEngagement(ctx context.Context, in IncomingEngagement) (bool, error) {
	in.EventID = strings.TrimSpace(in.EventID)
	in.MessageID = strings.TrimSpace(in.MessageID)
	in.UserID = strings.TrimSpace(in.UserID)
	in.ExpectedChatID = strings.TrimSpace(in.ExpectedChatID)
	reactionType := strings.TrimSpace(in.ReactionType)
	if reactionType == "" {
		reactionType = strings.TrimSpace(in.EmojiType)
	}
	if in.EventID == "" || in.MessageID == "" || in.UserID == "" || in.ExpectedChatID == "" || reactionType != "THUMBSUP" {
		return false, ErrInvalidEngagement
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var platform Platform
	var accountID, commentID, messageID string
	err = tx.QueryRowContext(ctx, `SELECT platform,account_id,comment_id,message_id FROM feishu_messages WHERE message_id=? AND chat_id=?`, in.MessageID, in.ExpectedChatID).Scan(&platform, &accountID, &commentID, &messageID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrUnmappedEngagement
	}
	if err != nil {
		return false, err
	}
	if platform != PlatformBilibili && platform != PlatformYouTube {
		return false, ErrUnsupportedEngagement
	}
	if in.ReceivedAt.IsZero() {
		in.ReceivedAt = time.Now()
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO engagement_events(event_id,message_id,user_id,kind,received_at) VALUES(?,?,?,?,?) ON CONFLICT(event_id) DO NOTHING`, in.EventID, messageID, in.UserID, EngagementLike, in.ReceivedAt.UnixNano())
	if err != nil {
		return false, err
	}
	inserted, _ := result.RowsAffected()
	if inserted != 1 {
		return false, tx.Commit()
	}
	table := "engagement_jobs"
	actionPrefix := "engagement"
	if platform == PlatformYouTube {
		table = "manual_engagement_feedback_jobs"
		actionPrefix = "manual-engagement-feedback"
	}
	result, err = tx.ExecContext(ctx, "INSERT INTO "+table+"(action_id,message_id,platform,account_id,comment_id,kind,status) VALUES(?,?,?,?,?,?,?) ON CONFLICT(platform,account_id,comment_id,kind) DO NOTHING", actionID(actionPrefix, string(platform), accountID, commentID, string(EngagementLike)), messageID, platform, accountID, commentID, EngagementLike, JobPending)
	if err != nil {
		return false, err
	}
	created, _ := result.RowsAffected()
	return created == 1, tx.Commit()
}

func (s *Store) ClaimReply(ctx context.Context) (*ReplyJob, error) {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	j, e := scanReply(tx.QueryRowContext(ctx, `SELECT r.action_id,r.inbound_message_id,r.feishu_message_id,c.platform,c.account_id,c.comment_id,c.root_id,c.parent_id,c.resource_id,c.resource_title,c.resource_url,c.author_id,c.author_name,c.text,c.published_at,c.is_charge_plus,r.text,r.status FROM reply_jobs r JOIN comments c USING(platform,account_id,comment_id) WHERE r.status=? ORDER BY r.rowid LIMIT 1`, JobPending))
	if errors.Is(e, sql.ErrNoRows) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	r, e := tx.ExecContext(ctx, "UPDATE reply_jobs SET status=? WHERE action_id=? AND status=?", JobSending, j.ActionID, JobPending)
	if e != nil {
		return nil, e
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return nil, nil
	}
	j.Status = JobSending
	return j, tx.Commit()
}
func (s *Store) MarkReplySent(ctx context.Context, action, id string) error {
	return s.finishReply(ctx, action, JobSent, id)
}
func (s *Store) MarkReplyFailed(ctx context.Context, action string, status JobStatus, err error) error {
	if status != JobFailed && status != JobUnknown {
		return errors.New("invalid terminal reply status")
	}
	return s.finishReply(ctx, action, status, "")
}
func (s *Store) finishReply(ctx context.Context, action string, status JobStatus, id string) error {
	if status == JobSent && strings.TrimSpace(id) == "" {
		return errors.New("empty platform reply id")
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var inboundMessageID string
	e = tx.QueryRowContext(ctx, "SELECT inbound_message_id FROM reply_jobs WHERE action_id=? AND status=?", action, JobSending).Scan(&inboundMessageID)
	if e != nil {
		return e
	}
	if status == JobSent {
		if _, e = tx.ExecContext(ctx, "INSERT INTO reaction_jobs(action_id,inbound_message_id,kind,message_id,emoji_type,status) VALUES(?,?,?,?,?,?)", actionID("reaction", string(ReactionOK), inboundMessageID), inboundMessageID, ReactionOK, inboundMessageID, OK, JobPending); e != nil {
			return e
		}
	}
	r, e := tx.ExecContext(ctx, "UPDATE reply_jobs SET status=?,external_id=?,error_category=? WHERE action_id=? AND status=?", status, id, terminalCategory(status), action, JobSending)
	if e != nil {
		return e
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

func (s *Store) ClaimReaction(ctx context.Context) (*ReactionJob, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	job, err := scanReaction(tx.QueryRowContext(ctx, `SELECT action_id,inbound_message_id,kind,message_id,emoji_type,status FROM reaction_jobs WHERE status=? ORDER BY rowid LIMIT 1`, JobPending))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r, err := tx.ExecContext(ctx, "UPDATE reaction_jobs SET status=? WHERE action_id=? AND status=?", JobSending, job.ActionID, JobPending)
	if err != nil {
		return nil, err
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return nil, nil
	}
	job.Status = JobSending
	return job, tx.Commit()
}

func (s *Store) MarkReactionSent(ctx context.Context, action, id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("empty feishu reaction id")
	}
	return s.finishReaction(ctx, action, JobSent, id)
}

func (s *Store) MarkReactionFailed(ctx context.Context, action string, status JobStatus, err error) error {
	if status != JobFailed && status != JobUnknown {
		return errors.New("invalid terminal reaction status")
	}
	return s.finishReaction(ctx, action, status, "")
}

func (s *Store) finishReaction(ctx context.Context, action string, status JobStatus, id string) error {
	r, err := s.db.ExecContext(ctx, "UPDATE reaction_jobs SET status=?,external_id=?,error_category=? WHERE action_id=? AND status=?", status, id, terminalCategory(status), action, JobSending)
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) ClaimEngagement(ctx context.Context) (*EngagementJob, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	job, err := scanEngagement(tx.QueryRowContext(ctx, `SELECT e.action_id,e.message_id,c.platform,c.account_id,c.comment_id,c.root_id,c.parent_id,c.resource_id,c.resource_title,c.resource_url,c.author_id,c.author_name,c.text,c.published_at,c.is_charge_plus,e.kind,e.status FROM engagement_jobs e JOIN comments c USING(platform,account_id,comment_id) WHERE e.status=? ORDER BY e.rowid LIMIT 1`, JobPending))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r, err := tx.ExecContext(ctx, `UPDATE engagement_jobs SET status=? WHERE action_id=? AND status=?`, JobSending, job.ActionID, JobPending)
	if err != nil {
		return nil, err
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return nil, nil
	}
	job.Status = JobSending
	return job, tx.Commit()
}

// MarkEngagementSent makes the platform result and the Feishu OK receipt
// durable in one transaction. The receipt is a distinct outbox so its failure
// can never change a successful platform engagement.
func (s *Store) MarkEngagementSent(ctx context.Context, action, externalID string) error {
	if strings.TrimSpace(externalID) == "" {
		return errors.New("empty platform engagement id")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var messageID string
	if err = tx.QueryRowContext(ctx, `SELECT message_id FROM engagement_jobs WHERE action_id=? AND status=?`, action, JobSending).Scan(&messageID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO engagement_feedback_jobs(action_id,engagement_action_id,message_id,emoji_type,status) VALUES(?,?,?,?,?)`, actionID("engagement-feedback", action), action, messageID, OK, JobPending); err != nil {
		return err
	}
	r, err := tx.ExecContext(ctx, `UPDATE engagement_jobs SET status=?,external_id=?,error_category=? WHERE action_id=? AND status=?`, JobSent, externalID, "", action, JobSending)
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

func (s *Store) MarkEngagementFailed(ctx context.Context, action string, status JobStatus, err error) error {
	if status != JobFailed && status != JobUnknown {
		return errors.New("invalid terminal engagement status")
	}
	r, execErr := s.db.ExecContext(ctx, `UPDATE engagement_jobs SET status=?,external_id='',error_category=? WHERE action_id=? AND status=?`, status, terminalCategory(status), action, JobSending)
	if execErr != nil {
		return execErr
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) ClaimEngagementFeedback(ctx context.Context) (*EngagementFeedbackJob, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	job, err := scanEngagementFeedback(tx.QueryRowContext(ctx, `SELECT action_id,engagement_action_id,message_id,emoji_type,status FROM engagement_feedback_jobs WHERE status=? ORDER BY rowid LIMIT 1`, JobPending))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r, err := tx.ExecContext(ctx, `UPDATE engagement_feedback_jobs SET status=? WHERE action_id=? AND status=?`, JobSending, job.ActionID, JobPending)
	if err != nil {
		return nil, err
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return nil, nil
	}
	job.Status = JobSending
	return job, tx.Commit()
}

func (s *Store) MarkEngagementFeedbackSent(ctx context.Context, action, id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("empty feishu engagement feedback id")
	}
	return s.finishEngagementFeedback(ctx, action, JobSent, id)
}

func (s *Store) MarkEngagementFeedbackFailed(ctx context.Context, action string, status JobStatus, err error) error {
	if status != JobFailed && status != JobUnknown {
		return errors.New("invalid terminal engagement feedback status")
	}
	return s.finishEngagementFeedback(ctx, action, status, "")
}

func (s *Store) finishEngagementFeedback(ctx context.Context, action string, status JobStatus, id string) error {
	r, err := s.db.ExecContext(ctx, `UPDATE engagement_feedback_jobs SET status=?,external_id=?,error_category=? WHERE action_id=? AND status=?`, status, id, terminalCategory(status), action, JobSending)
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return sql.ErrNoRows
	}
	return nil
}

// ClaimManualEngagementFeedback claims one human-facing manual engagement
// instruction. Unlike an engagement feedback reaction, this posts a reply to
// the original Feishu notification and never calls a platform adapter.
func (s *Store) ClaimManualEngagementFeedback(ctx context.Context) (*ManualEngagementFeedbackJob, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	job, err := scanManualEngagementFeedback(tx.QueryRowContext(ctx, `SELECT action_id,message_id,platform,account_id,comment_id,kind,status FROM manual_engagement_feedback_jobs WHERE status=? ORDER BY rowid LIMIT 1`, JobPending))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r, err := tx.ExecContext(ctx, `UPDATE manual_engagement_feedback_jobs SET status=? WHERE action_id=? AND status=?`, JobSending, job.ActionID, JobPending)
	if err != nil {
		return nil, err
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return nil, nil
	}
	job.Status = JobSending
	return job, tx.Commit()
}

func (s *Store) MarkManualEngagementFeedbackSent(ctx context.Context, action, id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("empty feishu manual engagement feedback id")
	}
	return s.finishManualEngagementFeedback(ctx, action, JobSent, id)
}

func (s *Store) MarkManualEngagementFeedbackFailed(ctx context.Context, action string, status JobStatus, err error) error {
	if status != JobFailed && status != JobUnknown {
		return errors.New("invalid terminal manual engagement feedback status")
	}
	return s.finishManualEngagementFeedback(ctx, action, status, "")
}

func (s *Store) finishManualEngagementFeedback(ctx context.Context, action string, status JobStatus, id string) error {
	r, err := s.db.ExecContext(ctx, `UPDATE manual_engagement_feedback_jobs SET status=?,external_id=?,error_category=? WHERE action_id=? AND status=?`, status, id, terminalCategory(status), action, JobSending)
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func terminalCategory(s JobStatus) string {
	if s == JobUnknown {
		return "unknown_outcome"
	}
	if s == JobFailed {
		return "external_error"
	}
	return ""
}
func (s *Store) RecoverSending(ctx context.Context) error {
	_, e := s.db.ExecContext(ctx, "UPDATE notification_jobs SET status=?,error_category=? WHERE status=?", JobUnknown, "interrupted", JobSending)
	if e != nil {
		return e
	}
	_, e = s.db.ExecContext(ctx, "UPDATE reply_jobs SET status=?,error_category=? WHERE status=?", JobUnknown, "interrupted", JobSending)
	if e != nil {
		return e
	}
	_, e = s.db.ExecContext(ctx, "UPDATE reaction_jobs SET status=?,error_category=? WHERE status=?", JobUnknown, "interrupted", JobSending)
	if e != nil {
		return e
	}
	_, e = s.db.ExecContext(ctx, "UPDATE engagement_jobs SET status=?,error_category=? WHERE status=?", JobUnknown, "interrupted", JobSending)
	if e != nil {
		return e
	}
	_, e = s.db.ExecContext(ctx, "UPDATE engagement_feedback_jobs SET status=?,error_category=? WHERE status=?", JobUnknown, "interrupted", JobSending)
	if e != nil {
		return e
	}
	_, e = s.db.ExecContext(ctx, "UPDATE manual_engagement_feedback_jobs SET status=?,error_category=? WHERE status=?", JobUnknown, "interrupted", JobSending)
	if e != nil {
		return e
	}
	_, e = s.db.ExecContext(ctx, "UPDATE credential_alert_jobs SET status=?,error_category=? WHERE status=?", JobUnknown, "interrupted", JobSending)
	if e != nil {
		return e
	}
	// Generate and poll are safe reads and can resume after a restart.
	if _, e = s.db.ExecContext(ctx, "UPDATE credential_refresh_jobs SET status=?,error_category=? WHERE status=? AND stage IN (?,?)", JobPending, "interrupted_read", JobSending, CredentialRefreshStageGenerate, CredentialRefreshStagePoll); e != nil {
		return e
	}
	// Upload and send are external writes. A crash leaves their outcome
	// unknown, so an operator must explicitly reset them before another call.
	_, e = s.db.ExecContext(ctx, "UPDATE credential_refresh_jobs SET status=?,error_category=? WHERE status=? AND stage IN (?,?)", JobUnknown, "interrupted", JobSending, CredentialRefreshStageUpload, CredentialRefreshStageSend)
	return e
}

// StatusCounts provides a body-free health snapshot of all outboxes.
func (s *Store) StatusCounts(ctx context.Context) (StatusCounts, error) {
	out := StatusCounts{Notifications: map[JobStatus]int{}, CredentialAlerts: map[JobStatus]int{}, CredentialRefreshes: map[JobStatus]int{}, Replies: map[JobStatus]int{}, Reactions: map[JobStatus]int{}, Engagements: map[JobStatus]int{}, EngagementFeedbacks: map[JobStatus]int{}, ManualEngagementFeedbacks: map[JobStatus]int{}}
	queries := []struct {
		sql string
		dst map[JobStatus]int
	}{
		{"SELECT status,COUNT(*) FROM notification_jobs GROUP BY status", out.Notifications},
		{"SELECT status,COUNT(*) FROM reply_jobs GROUP BY status", out.Replies},
		{"SELECT status,COUNT(*) FROM reaction_jobs GROUP BY status", out.Reactions},
		{"SELECT status,COUNT(*) FROM engagement_jobs GROUP BY status", out.Engagements},
		// Engagement feedback is still a Feishu reaction and therefore is
		// included in Reactions for existing health dashboards.
		{"SELECT status,COUNT(*) FROM engagement_feedback_jobs GROUP BY status", out.Reactions},
		{"SELECT status,COUNT(*) FROM engagement_feedback_jobs GROUP BY status", out.EngagementFeedbacks},
	}
	credentialOutbox, err := s.tableExists(ctx, "credential_alert_jobs")
	if err != nil {
		return StatusCounts{}, err
	}
	if credentialOutbox {
		queries = append(queries, struct {
			sql string
			dst map[JobStatus]int
		}{"SELECT status,COUNT(*) FROM credential_alert_jobs GROUP BY status", out.CredentialAlerts})
	}
	credentialRefreshOutbox, err := s.tableExists(ctx, "credential_refresh_jobs")
	if err != nil {
		return StatusCounts{}, err
	}
	if credentialRefreshOutbox {
		queries = append(queries, struct {
			sql string
			dst map[JobStatus]int
		}{"SELECT status,COUNT(*) FROM credential_refresh_jobs GROUP BY status", out.CredentialRefreshes})
	}
	manualOutbox, err := s.tableExists(ctx, "manual_engagement_feedback_jobs")
	if err != nil {
		return StatusCounts{}, err
	}
	if manualOutbox {
		queries = append(queries, struct {
			sql string
			dst map[JobStatus]int
		}{"SELECT status,COUNT(*) FROM manual_engagement_feedback_jobs GROUP BY status", out.ManualEngagementFeedbacks})
	}
	danmakuOutbox, err := s.tableExists(ctx, "danmaku_jobs")
	if err != nil {
		return StatusCounts{}, err
	}
	if danmakuOutbox {
		out.Danmaku = map[JobStatus]int{}
		queries = append(queries, struct {
			sql string
			dst map[JobStatus]int
		}{"SELECT status,COUNT(*) FROM danmaku_jobs GROUP BY status", out.Danmaku})
	}
	for _, q := range queries {
		rows, err := s.db.QueryContext(ctx, q.sql)
		if err != nil {
			return StatusCounts{}, err
		}
		for rows.Next() {
			var status JobStatus
			var n int
			if err := rows.Scan(&status, &n); err != nil {
				_ = rows.Close()
				return StatusCounts{}, err
			}
			q.dst[status] += n
		}
		if err := rows.Close(); err != nil {
			return StatusCounts{}, err
		}
	}
	return out, nil
}

// ListTerminalJobs only lists unknown or failed work and never returns bodies.
func (s *Store) ListTerminalJobs(ctx context.Context, status JobStatus) ([]TerminalJob, error) {
	if status != JobUnknown && status != JobFailed {
		return nil, errors.New("terminal status must be unknown or failed")
	}
	queries := []string{
		"SELECT 'notification',action_id,status,platform,comment_id,external_id FROM notification_jobs WHERE status=?",
		"SELECT 'reply',action_id,status,platform,comment_id,feishu_message_id FROM reply_jobs WHERE status=?",
		"SELECT 'reaction',r.action_id,r.status,'','',r.message_id FROM reaction_jobs r WHERE r.status=?",
		"SELECT 'engagement',action_id,status,platform,comment_id,message_id FROM engagement_jobs WHERE status=?",
		"SELECT 'engagement_feedback',action_id,status,'','',message_id FROM engagement_feedback_jobs WHERE status=?",
	}
	args := []any{status, status, status, status, status}
	credentialOutbox, err := s.tableExists(ctx, "credential_alert_jobs")
	if err != nil {
		return nil, err
	}
	if credentialOutbox {
		queries = append(queries, "SELECT 'credential_alert',action_id,status,platform,'',external_id FROM credential_alert_jobs WHERE status=?")
		args = append(args, status)
	}
	credentialRefreshOutbox, err := s.tableExists(ctx, "credential_refresh_jobs")
	if err != nil {
		return nil, err
	}
	if credentialRefreshOutbox {
		queries = append(queries, "SELECT 'credential_refresh',action_id,status,platform,'',qr_message_id FROM credential_refresh_jobs WHERE status=?")
		args = append(args, status)
	}
	manualOutbox, err := s.tableExists(ctx, "manual_engagement_feedback_jobs")
	if err != nil {
		return nil, err
	}
	if manualOutbox {
		queries = append(queries, "SELECT 'manual_engagement_feedback',action_id,status,platform,comment_id,message_id FROM manual_engagement_feedback_jobs WHERE status=?")
		args = append(args, status)
	}
	danmakuOutbox, err := s.tableExists(ctx, "danmaku_jobs")
	if err != nil {
		return nil, err
	}
	if danmakuOutbox {
		queries = append(queries, "SELECT 'danmaku',action_id,status,'bilibili','',external_id FROM danmaku_jobs WHERE status=?")
		args = append(args, status)
	}
	rows, err := s.db.QueryContext(ctx, strings.Join(queries, " UNION ALL ")+" ORDER BY action_id", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TerminalJob
	for rows.Next() {
		var j TerminalJob
		if err := rows.Scan(&j.Kind, &j.ActionID, &j.Status, &j.Platform, &j.CommentID, &j.FeishuMessageID); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// tableExists permits read-only diagnostics to remain compatible with
// pre-outbox databases. It performs no schema changes, and any catalog query
// failure remains visible to the caller.
func (s *Store) tableExists(ctx context.Context, table string) (bool, error) {
	var found int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ResetUnknownToPending is deliberately a single-action, explicit operator action.
func (s *Store) ResetUnknownToPending(ctx context.Context, actionID string) (bool, error) {
	if strings.TrimSpace(actionID) == "" {
		return false, errors.New("empty action id")
	}
	tables := []string{"notification_jobs", "reply_jobs", "reaction_jobs", "engagement_jobs", "engagement_feedback_jobs"}
	for _, optional := range []string{"manual_engagement_feedback_jobs", "credential_alert_jobs", "credential_refresh_jobs"} {
		exists, err := s.tableExists(ctx, optional)
		if err != nil {
			return false, err
		}
		if exists {
			tables = append(tables, optional)
		}
	}
	for _, table := range tables {
		r, err := s.db.ExecContext(ctx, "UPDATE "+table+" SET status=?,error_category='' WHERE action_id=? AND status=?", JobPending, actionID, JobUnknown)
		if err != nil {
			return false, err
		}
		n, _ := r.RowsAffected()
		if n == 1 {
			return true, nil
		}
	}
	return false, nil
}
func (s *Store) RetryUnknown(ctx context.Context, actionID string) (bool, error) {
	return s.ResetUnknownToPending(ctx, actionID)
}

type row interface{ Scan(...any) error }

func scanComment(r row) (Comment, error) {
	var c Comment
	var t int64
	e := r.Scan(&c.Platform, &c.AccountID, &c.ID, &c.RootID, &c.ParentID, &c.ResourceID, &c.ResourceTitle, &c.ResourceURL, &c.AuthorID, &c.AuthorName, &c.Text, &t, &c.IsChargePlus)
	c.PublishedAt = time.Unix(0, t)
	return c, e
}
func scanNotification(r row) (*NotificationJob, error) {
	var j NotificationJob
	var t int64
	e := r.Scan(&j.ActionID, &j.Comment.Platform, &j.Comment.AccountID, &j.Comment.ID, &j.Comment.RootID, &j.Comment.ParentID, &j.Comment.ResourceID, &j.Comment.ResourceTitle, &j.Comment.ResourceURL, &j.Comment.AuthorID, &j.Comment.AuthorName, &j.Comment.Text, &t, &j.Comment.IsChargePlus, &j.Status)
	j.Comment.PublishedAt = time.Unix(0, t)
	return &j, e
}

func scanCredentialAlert(r row) (*CredentialAlertJob, error) {
	var j CredentialAlertJob
	err := r.Scan(&j.ActionID, &j.Platform, &j.AccountID, &j.Reason, &j.IncidentSeq, &j.Status)
	return &j, err
}

func scanCredentialRefresh(r row) (*CredentialRefreshJob, error) {
	var j CredentialRefreshJob
	var stage string
	var attempt int
	var key, rawURL, imageKey, messageID, chatID string
	var expiresAt, nextPollAt int64
	err := r.Scan(&j.ActionID, &j.Platform, &j.AccountID, &j.IncidentSeq, &stage, &attempt, &key, &rawURL, &imageKey, &expiresAt, &messageID, &chatID, &nextPollAt, &j.Status, &j.ErrorCategory)
	j.Stage = CredentialRefreshStage(stage)
	j.Attempt = attempt
	j.Session = CredentialRefreshSession{Key: key, URL: rawURL}
	if expiresAt != 0 {
		j.Session.ExpiresAt = time.Unix(0, expiresAt)
	}
	j.ImageKey = imageKey
	j.QRMessageID = messageID
	j.ChatID = chatID
	if nextPollAt != 0 {
		j.NextPollAt = time.Unix(0, nextPollAt)
	}
	return &j, err
}

func scanReply(r row) (*ReplyJob, error) {
	var j ReplyJob
	var t int64
	e := r.Scan(&j.ActionID, &j.IncomingMessage, &j.FeishuMessageID, &j.Comment.Platform, &j.Comment.AccountID, &j.Comment.ID, &j.Comment.RootID, &j.Comment.ParentID, &j.Comment.ResourceID, &j.Comment.ResourceTitle, &j.Comment.ResourceURL, &j.Comment.AuthorID, &j.Comment.AuthorName, &j.Comment.Text, &t, &j.Comment.IsChargePlus, &j.Text, &j.Status)
	j.Comment.PublishedAt = time.Unix(0, t)
	return &j, e
}

func scanReaction(r row) (*ReactionJob, error) {
	var j ReactionJob
	err := r.Scan(&j.ActionID, &j.IncomingMessage, &j.Kind, &j.MessageID, &j.EmojiType, &j.Status)
	return &j, err
}

func scanEngagement(r row) (*EngagementJob, error) {
	var j EngagementJob
	var t int64
	err := r.Scan(&j.ActionID, &j.FeishuMessageID, &j.Comment.Platform, &j.Comment.AccountID, &j.Comment.ID, &j.Comment.RootID, &j.Comment.ParentID, &j.Comment.ResourceID, &j.Comment.ResourceTitle, &j.Comment.ResourceURL, &j.Comment.AuthorID, &j.Comment.AuthorName, &j.Comment.Text, &t, &j.Comment.IsChargePlus, &j.Kind, &j.Status)
	j.Comment.PublishedAt = time.Unix(0, t)
	return &j, err
}

func scanEngagementFeedback(r row) (*EngagementFeedbackJob, error) {
	var j EngagementFeedbackJob
	err := r.Scan(&j.ActionID, &j.EngagementActionID, &j.MessageID, &j.EmojiType, &j.Status)
	return &j, err
}

func scanManualEngagementFeedback(r row) (*ManualEngagementFeedbackJob, error) {
	var j ManualEngagementFeedbackJob
	err := r.Scan(&j.ActionID, &j.MessageID, &j.Platform, &j.AccountID, &j.CommentID, &j.Kind, &j.Status)
	return &j, err
}
