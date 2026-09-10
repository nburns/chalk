package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
)

type DB struct {
	db *sql.DB
}

var schemaStmts = []string{
	`CREATE TABLE IF NOT EXISTS entries (
		id         INTEGER  PRIMARY KEY AUTOINCREMENT,
		agent_id   TEXT     NOT NULL DEFAULT '',
		source     TEXT     NOT NULL DEFAULT 'agent',
		type       TEXT     NOT NULL DEFAULT 'note',
		status     TEXT     NOT NULL DEFAULT 'active',
		content    TEXT     NOT NULL,
		tags       TEXT     NOT NULL DEFAULT '',
		expires_at DATETIME,
		created_at DATETIME NOT NULL DEFAULT (datetime('now')),
		updated_at DATETIME NOT NULL DEFAULT (datetime('now'))
	)`,
	`CREATE INDEX IF NOT EXISTS idx_entries_type_status ON entries(type, status)`,
	`CREATE INDEX IF NOT EXISTS idx_entries_status      ON entries(status)`,
	`CREATE INDEX IF NOT EXISTS idx_entries_agent_id    ON entries(agent_id)`,
	`CREATE INDEX IF NOT EXISTS idx_entries_source      ON entries(source)`,
	`CREATE INDEX IF NOT EXISTS idx_entries_created_at  ON entries(created_at)`,
	`CREATE INDEX IF NOT EXISTS idx_entries_expires_at  ON entries(expires_at)`,
	`CREATE TABLE IF NOT EXISTS entry_relations (
		id         INTEGER  PRIMARY KEY AUTOINCREMENT,
		from_id    INTEGER  NOT NULL REFERENCES entries(id) ON DELETE CASCADE,
		to_id      INTEGER  NOT NULL REFERENCES entries(id) ON DELETE CASCADE,
		kind       TEXT     NOT NULL,
		created_at DATETIME NOT NULL DEFAULT (datetime('now')),
		updated_at DATETIME NOT NULL DEFAULT (datetime('now'))
	)`,
	`CREATE INDEX IF NOT EXISTS idx_relations_from_id ON entry_relations(from_id)`,
	`CREATE INDEX IF NOT EXISTS idx_relations_to_id   ON entry_relations(to_id)`,
	`CREATE VIRTUAL TABLE IF NOT EXISTS entries_fts USING fts5(
		content, tags, agent_id,
		content=entries,
		content_rowid=id
	)`,
	`CREATE TRIGGER IF NOT EXISTS entries_ai AFTER INSERT ON entries BEGIN
		INSERT INTO entries_fts(rowid, content, tags, agent_id)
		VALUES (new.id, new.content, new.tags, new.agent_id);
	END`,
	`CREATE TRIGGER IF NOT EXISTS entries_ad AFTER DELETE ON entries BEGIN
		INSERT INTO entries_fts(entries_fts, rowid, content, tags, agent_id)
		VALUES ('delete', old.id, old.content, old.tags, old.agent_id);
	END`,
	// only rebuild FTS when the indexed columns actually change
	`CREATE TRIGGER IF NOT EXISTS entries_au AFTER UPDATE OF content, tags, agent_id ON entries BEGIN
		INSERT INTO entries_fts(entries_fts, rowid, content, tags, agent_id)
		VALUES ('delete', old.id, old.content, old.tags, old.agent_id);
		INSERT INTO entries_fts(rowid, content, tags, agent_id)
		VALUES (new.id, new.content, new.tags, new.agent_id);
	END`,
}

func openDB(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}
	sqldb, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	// single connection serializes writes; WAL still allows external concurrent reads
	sqldb.SetMaxOpenConns(1)

	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
		"PRAGMA temp_store=MEMORY",
		"PRAGMA cache_size=-64000",
	} {
		if _, err := sqldb.Exec(pragma); err != nil {
			return nil, fmt.Errorf("pragma %q: %w", pragma, err)
		}
	}

	d := &DB{db: sqldb}
	if err := d.migrate(); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return d, nil
}

func (d *DB) Close() error {
	return d.db.Close()
}

func (d *DB) migrate() error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range schemaStmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("%.60q: %w", stmt, err)
		}
	}
	return tx.Commit()
}

func (d *DB) reap(ctx context.Context) (int64, error) {
	res, err := d.db.ExecContext(ctx, `
		UPDATE entries
		SET status = 'archived', updated_at = datetime('now')
		WHERE type = 'working_on' AND status = 'active' AND expires_at < datetime('now')`,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (d *DB) queryKV(ctx context.Context, q string) (map[string]int, error) {
	rows, err := d.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := make(map[string]int)
	for rows.Next() {
		var k string
		var v int
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		m[k] = v
	}
	return m, rows.Err()
}

func (d *DB) queryStrings(ctx context.Context, q string) ([]string, error) {
	rows, err := d.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		result = append(result, s)
	}
	return result, rows.Err()
}
