package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func now() string { return time.Now().UTC().Format(time.RFC3339) }

type WhitelistEntry struct {
	UserID  string
	Name    string
	Admin   bool
	AddedBy string
	AddedAt string
}

func (s *Store) IsWhitelisted(ctx context.Context, userID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM whitelist WHERE user_id = ?`, userID).Scan(&n)
	return n > 0, err
}

func (s *Store) IsAdmin(ctx context.Context, userID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM whitelist WHERE user_id = ? AND admin = 1`, userID).Scan(&n)
	return n > 0, err
}

func (s *Store) UpsertWhitelist(ctx context.Context, e WhitelistEntry) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO whitelist (user_id, name, admin, added_by, added_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET name = excluded.name, admin = MAX(admin, excluded.admin)`,
		e.UserID, e.Name, e.Admin, e.AddedBy, now())
	return err
}

func (s *Store) RemoveWhitelist(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM whitelist WHERE user_id = ?`, userID)
	return err
}

func (s *Store) ListWhitelist(ctx context.Context) ([]WhitelistEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT user_id, name, admin, added_by, added_at FROM whitelist ORDER BY added_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WhitelistEntry
	for rows.Next() {
		var e WhitelistEntry
		if err := rows.Scan(&e.UserID, &e.Name, &e.Admin, &e.AddedBy, &e.AddedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

type StreamKey struct {
	Key       string
	OwnerID   string
	Label     string
	CreatedAt string
}

func (s *Store) CreateStreamKey(ctx context.Context, k StreamKey) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO stream_keys (key, owner_id, label, created_at) VALUES (?, ?, ?, ?)`,
		k.Key, k.OwnerID, k.Label, now())
	return err
}

func (s *Store) GetStreamKey(ctx context.Context, key string) (*StreamKey, error) {
	var k StreamKey
	err := s.db.QueryRowContext(ctx, `SELECT key, owner_id, label, created_at FROM stream_keys WHERE key = ?`, key).
		Scan(&k.Key, &k.OwnerID, &k.Label, &k.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &k, err
}

func (s *Store) DeleteStreamKey(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM stream_keys WHERE key = ?`, key)
	return err
}

func (s *Store) ListStreamKeys(ctx context.Context, ownerID string) ([]StreamKey, error) {
	q := `SELECT key, owner_id, label, created_at FROM stream_keys`
	var args []any
	if ownerID != "" {
		q += ` WHERE owner_id = ?`
		args = append(args, ownerID)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY created_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StreamKey
	for rows.Next() {
		var k StreamKey
		if err := rows.Scan(&k.Key, &k.OwnerID, &k.Label, &k.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

type AccountState struct {
	Name      string
	Status    string
	LastError string
	UpdatedAt string
}

func (s *Store) SetAccountState(ctx context.Context, name, status, lastError string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO accounts (name, status, last_error, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET status = excluded.status, last_error = excluded.last_error, updated_at = excluded.updated_at`,
		name, status, lastError, now())
	return err
}

func (s *Store) ListAccountStates(ctx context.Context) ([]AccountState, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, status, last_error, updated_at FROM accounts ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccountState
	for rows.Next() {
		var a AccountState
		if err := rows.Scan(&a.Name, &a.Status, &a.LastError, &a.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

type Identity struct {
	Name      string
	CookieD   string
	Mode      string
	Managed   bool
	Status    string
	LastError string
	UpdatedAt string
}

func (s *Store) UpsertIdentity(ctx context.Context, id Identity) error {
	if id.Mode == "" {
		id.Mode = "screen"
	}
	if id.Status == "" {
		id.Status = "pending"
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO accounts (name, status, last_error, updated_at, cookie_d, mode, managed)
		VALUES (?, ?, '', ?, ?, ?, 1)
		ON CONFLICT(name) DO UPDATE SET cookie_d = excluded.cookie_d, mode = excluded.mode, managed = 1, updated_at = excluded.updated_at`,
		id.Name, id.Status, now(), id.CookieD, id.Mode)
	return err
}

func (s *Store) DeleteIdentity(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM accounts WHERE name = ? AND managed = 1`, name)
	return err
}

func (s *Store) ListIdentities(ctx context.Context) ([]Identity, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, cookie_d, mode, managed, status, last_error, updated_at FROM accounts WHERE managed = 1 ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Identity
	for rows.Next() {
		var id Identity
		if err := rows.Scan(&id.Name, &id.CookieD, &id.Mode, &id.Managed, &id.Status, &id.LastError, &id.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

type Recording struct {
	ID        int64
	StreamKey string
	Name      string
	Storage   string
	Location  string
	Size      int64
	CreatedAt string
}

func (s *Store) AddRecording(ctx context.Context, r Recording) (int64, error) {
	if r.Storage == "" {
		r.Storage = "local"
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO recordings (stream_key, name, storage, location, size, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(storage, location) DO UPDATE SET size = excluded.size, stream_key = excluded.stream_key`,
		r.StreamKey, r.Name, r.Storage, r.Location, r.Size, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) ListRecordings(ctx context.Context, limit int) ([]Recording, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, stream_key, name, storage, location, size, created_at FROM recordings ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Recording
	for rows.Next() {
		var r Recording
		if err := rows.Scan(&r.ID, &r.StreamKey, &r.Name, &r.Storage, &r.Location, &r.Size, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type Session struct {
	ID           string
	CreatedBy    string
	SourceType   string
	SourceRef    string
	Presentation string
	Status       string
	CreatedAt    string
	EndedAt      string
	Record       bool
}

type Target struct {
	ID           int64
	SessionID    string
	ChannelID    string
	ChannelName  string
	Account      string
	Status       string
	LastError    string
	HuddleRootTS string
	UpdatedAt    string
}

func (s *Store) CreateSession(ctx context.Context, sess Session) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (id, created_by, source_type, source_ref, presentation, status, created_at, ended_at, record)
		VALUES (?, ?, ?, ?, ?, ?, ?, '', ?)`,
		sess.ID, sess.CreatedBy, sess.SourceType, sess.SourceRef, sess.Presentation, sess.Status, now(), sess.Record)
	return err
}

func (s *Store) SetSessionRecord(ctx context.Context, id string, record bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET record = ? WHERE id = ?`, record, id)
	return err
}

func (s *Store) UpdateSession(ctx context.Context, id, presentation, status string) error {
	ended := ""
	if status == "stopped" {
		ended = now()
	}
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET presentation = ?, status = ?, ended_at = CASE WHEN ? = '' THEN ended_at ELSE ? END WHERE id = ?`,
		presentation, status, ended, ended, id)
	return err
}

func (s *Store) GetSession(ctx context.Context, id string) (*Session, error) {
	var x Session
	err := s.db.QueryRowContext(ctx, `SELECT id, created_by, source_type, source_ref, presentation, status, created_at, ended_at, record FROM sessions WHERE id = ?`, id).
		Scan(&x.ID, &x.CreatedBy, &x.SourceType, &x.SourceRef, &x.Presentation, &x.Status, &x.CreatedAt, &x.EndedAt, &x.Record)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &x, err
}

func (s *Store) ListSessions(ctx context.Context, onlyRunning bool, limit int) ([]Session, error) {
	q := `SELECT id, created_by, source_type, source_ref, presentation, status, created_at, ended_at, record FROM sessions`
	if onlyRunning {
		q += ` WHERE status = 'running'`
	}
	q += ` ORDER BY created_at DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var x Session
		if err := rows.Scan(&x.ID, &x.CreatedBy, &x.SourceType, &x.SourceRef, &x.Presentation, &x.Status, &x.CreatedAt, &x.EndedAt, &x.Record); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) AddTarget(ctx context.Context, t Target) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO targets (session_id, channel_id, channel_name, account, status, last_error, huddle_root_ts, updated_at)
		VALUES (?, ?, ?, ?, ?, '', '', ?)`,
		t.SessionID, t.ChannelID, t.ChannelName, t.Account, t.Status, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdateTargetStatus(ctx context.Context, id int64, status, lastError string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE targets SET status = ?, last_error = ?, updated_at = ? WHERE id = ?`, status, lastError, now(), id)
	return err
}

func (s *Store) SetTargetHuddleRoot(ctx context.Context, sessionID, channelID, rootTS string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE targets SET huddle_root_ts = ? WHERE session_id = ? AND channel_id = ?`, rootTS, sessionID, channelID)
	return err
}

func (s *Store) DeleteTarget(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM targets WHERE id = ?`, id)
	return err
}

func (s *Store) ListTargets(ctx context.Context, sessionID string) ([]Target, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, session_id, channel_id, channel_name, account, status, last_error, huddle_root_ts, updated_at FROM targets WHERE session_id = ? ORDER BY id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Target
	for rows.Next() {
		var t Target
		if err := rows.Scan(&t.ID, &t.SessionID, &t.ChannelID, &t.ChannelName, &t.Account, &t.Status, &t.LastError, &t.HuddleRootTS, &t.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

type ChatMessage struct {
	ID          int64
	SessionID   string
	ChannelID   string
	ChannelName string
	UserID      string
	UserName    string
	Text        string
	TS          string
	ReceivedAt  string
}

func (s *Store) AddChat(ctx context.Context, m ChatMessage) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO chat (session_id, channel_id, channel_name, user_id, user_name, text, ts, received_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		m.SessionID, m.ChannelID, m.ChannelName, m.UserID, m.UserName, m.Text, m.TS, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) ListChat(ctx context.Context, sessionID string, afterID int64, limit int) ([]ChatMessage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, channel_id, channel_name, user_id, user_name, text, ts, received_at
		FROM chat WHERE session_id = ? AND id > ? ORDER BY id LIMIT ?`, sessionID, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatMessage
	for rows.Next() {
		var m ChatMessage
		if err := rows.Scan(&m.ID, &m.SessionID, &m.ChannelID, &m.ChannelName, &m.UserID, &m.UserName, &m.Text, &m.TS, &m.ReceivedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
