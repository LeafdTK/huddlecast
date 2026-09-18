package store

import (
	"context"
	"fmt"
)

var migrations = []string{
	`CREATE TABLE whitelist (
		user_id  TEXT PRIMARY KEY,
		name     TEXT NOT NULL DEFAULT '',
		admin    INTEGER NOT NULL DEFAULT 0,
		added_by TEXT NOT NULL DEFAULT '',
		added_at TEXT NOT NULL
	);
	CREATE TABLE stream_keys (
		key        TEXT PRIMARY KEY,
		owner_id   TEXT NOT NULL,
		label      TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL
	);
	CREATE TABLE accounts (
		name       TEXT PRIMARY KEY,
		status     TEXT NOT NULL,
		last_error TEXT NOT NULL DEFAULT '',
		updated_at TEXT NOT NULL
	);
	CREATE TABLE sessions (
		id           TEXT PRIMARY KEY,
		created_by   TEXT NOT NULL,
		source_type  TEXT NOT NULL,
		source_ref   TEXT NOT NULL,
		presentation TEXT NOT NULL,
		status       TEXT NOT NULL,
		created_at   TEXT NOT NULL,
		ended_at     TEXT NOT NULL DEFAULT ''
	);
	CREATE TABLE targets (
		id             INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id     TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
		channel_id     TEXT NOT NULL,
		channel_name   TEXT NOT NULL DEFAULT '',
		account        TEXT NOT NULL,
		status         TEXT NOT NULL,
		last_error     TEXT NOT NULL DEFAULT '',
		huddle_root_ts TEXT NOT NULL DEFAULT '',
		updated_at     TEXT NOT NULL
	);
	CREATE INDEX targets_session ON targets(session_id);
	CREATE TABLE chat (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id   TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
		channel_id   TEXT NOT NULL,
		channel_name TEXT NOT NULL DEFAULT '',
		user_id      TEXT NOT NULL,
		user_name    TEXT NOT NULL DEFAULT '',
		text         TEXT NOT NULL,
		ts           TEXT NOT NULL,
		received_at  TEXT NOT NULL,
		UNIQUE(channel_id, ts)
	);
	CREATE INDEX chat_session ON chat(session_id, id);`,

	`ALTER TABLE accounts ADD COLUMN cookie_d TEXT NOT NULL DEFAULT '';
	ALTER TABLE accounts ADD COLUMN mode TEXT NOT NULL DEFAULT 'screen';
	ALTER TABLE accounts ADD COLUMN managed INTEGER NOT NULL DEFAULT 0;
	CREATE TABLE recordings (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		stream_key TEXT NOT NULL DEFAULT '',
		name       TEXT NOT NULL,
		storage    TEXT NOT NULL DEFAULT 'local',
		location   TEXT NOT NULL,
		size       INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL,
		UNIQUE(storage, location)
	);
	CREATE INDEX recordings_created ON recordings(created_at DESC);`,

	`ALTER TABLE sessions ADD COLUMN record INTEGER NOT NULL DEFAULT 1;`,
}

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
