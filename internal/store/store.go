// Package store is the SQLite persistence layer.
//
// Single-file SQLite (WAL mode) at ~/.agentwork/agentwork.db by default.
// Schema is applied on Open via schema.sql (CREATE TABLE IF NOT EXISTS) for
// fresh installs, plus additive migrations from migrations/ for existing
// databases (see migrate.go).
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// Store wraps the SQLite handle.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path and applies
// the schema. If path is empty, defaults to ~/.agentwork/agentwork.db.
func Open(path string) (*Store, error) {
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("home dir: %w", err)
		}
		dir := filepath.Join(home, ".agentwork")
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", dir, err)
		}
		path = filepath.Join(dir, "agentwork.db")
	}

	// DSN pragmas apply to every pooled connection. foreign_keys,
	// journal_mode, and busy_timeout are per-connection in SQLite, so setting
	// them in schema.sql only affects the one connection that runs it — pooled
	// connections would silently get foreign_keys=0 and busy_timeout=0. The
	// DSN form modernc.org/sqlite supports is ?_pragma=name(value).
	// busy_timeout=5000: when two writes collide (rare under WAL), block up to
	// 5s for the lock instead of returning SQLITE_BUSY immediately — without
	// this, concurrent task handlers persisting messages get (5) SQLITE_BUSY.
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// Single writer + many readers is fine for SQLite WAL; one connection
	// for writes keeps the model simple. Pool a few for concurrent reads.
	db.SetMaxOpenConns(8)
	if path == ":memory:" {
		// An in-memory database is PER-CONNECTION in SQLite — a pool would
		// fragment the schema across connections ("no such table" on queries
		// that land on a different connection than the one that ran the
		// schema). Tests only: pin a single connection.
		db.SetMaxOpenConns(1)
	}

	// Detect a fresh database (no user tables) BEFORE applying schema —
	// after schema.sql runs, all tables exist and the count is meaningless.
	var tableCount int
	if err := db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`,
	).Scan(&tableCount); err != nil {
		db.Close()
		return nil, fmt.Errorf("check existing tables: %w", err)
	}
	isFresh := tableCount == 0

	// Apply full schema (CREATE TABLE IF NOT EXISTS). On a fresh install this
	// creates every table; on an existing install it is a no-op for tables
	// that already exist (new tables added to schema.sql are created here).
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}

	// Run additive migrations (ALTER TABLE ADD COLUMN, new indexes, etc.)
	// for existing databases. Fresh installs are baselined to the latest
	// version since schema.sql already reflects the current state.
	if err := migrate(db, isFresh); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return &Store{db: db}, nil
}

// DB exposes the underlying handle for handlers/services that run queries.
func (s *Store) DB() *sql.DB { return s.db }

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Ping verifies the connection is alive.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }
