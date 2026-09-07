// Additive schema migration for SQLite.
//
// # Why this exists
//
// After the program ships, the database can no longer be wiped and rebuilt —
// user data is persistent. schema.sql uses CREATE TABLE IF NOT EXISTS, which
// only creates NEW tables; it cannot add columns to tables that already
// exist in an old database. This module runs incremental migration scripts
// (ALTER TABLE ADD COLUMN, CREATE INDEX, etc.) on every Open to bring an
// old database up to the latest schema.
//
// # Two sources of truth
//
// When adding a column (or any additive change), the developer updates BOTH:
//   - schema.sql  → full schema for fresh installs (so CREATE TABLE includes
//     the new column — a brand-new database never needs migrations)
//   - migrations/ → additive patch for existing installs (ALTER TABLE ADD
//     COLUMN — brings an old database up to match schema.sql)
//
// # How to add a migration
//
//  1. Edit schema.sql: add the column to the CREATE TABLE statement.
//  2. Create migrations/NNNN_descriptive_name.sql (NNNN = next sequential
//     number, zero-padded to 4 digits, starting at 0001).
//     Example: migrations/0001_add_goal_priority.sql
//     Content: ALTER TABLE goal ADD COLUMN priority INTEGER NOT NULL DEFAULT 0;
//  3. Commit. On the next daemon start, Open() detects the old version,
//     runs the migration in a transaction, and bumps _schema_version.
//
// # Version tracking
//
// A _schema_version meta table (single row, id=1) holds the current version.
// The version UPDATE and the migration SQL run in the same transaction, so a
// crash mid-migration rolls back cleanly — no half-applied state.
package store

import (
	"database/sql"
	"embed"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/eushing/agentwork/internal/logging"
)

//go:embed all:migrations
var migrationsFS embed.FS

// migration is one additive schema patch loaded from migrations/NNNN_name.sql.
type migration struct {
	version int
	name    string
	sql     string
}

// schemaVersionDDL creates the version-tracking meta table. A single row
// (id=1) holds the current schema version, starting at 0. The
// INSERT ... ON CONFLICT DO NOTHING makes it idempotent.
const schemaVersionDDL = `
CREATE TABLE IF NOT EXISTS _schema_version (
	id      INTEGER PRIMARY KEY CHECK (id = 1),
	version INTEGER NOT NULL DEFAULT 0
);
INSERT INTO _schema_version (id, version) VALUES (1, 0)
	ON CONFLICT(id) DO NOTHING;
`

// loadMigrations reads and validates all embedded migration SQL files.
// Files must be named NNNN_descriptive_name.sql (zero-padded sequential
// integers starting at 0001). Gaps in the sequence are rejected.
func loadMigrations() ([]migration, error) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}
	var migs []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		ver, name, ok := splitMigrationName(e.Name())
		if !ok {
			return nil, fmt.Errorf("migration filename %q: expected NNNN_name.sql", e.Name())
		}
		data, err := migrationsFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", e.Name(), err)
		}
		migs = append(migs, migration{version: ver, name: name, sql: string(data)})
	}
	return sortAndValidate(migs)
}

// splitMigrationName parses "0001_add_goal_priority.sql" into (1, "0001_add_goal_priority.sql").
func splitMigrationName(filename string) (version int, name string, ok bool) {
	parts := strings.SplitN(filename, "_", 2)
	if len(parts) != 2 || !strings.HasSuffix(parts[1], ".sql") {
		return 0, "", false
	}
	ver, err := strconv.Atoi(parts[0])
	if err != nil || ver < 1 {
		return 0, "", false
	}
	return ver, filename, true
}

// sortAndValidate orders migrations by version and rejects gaps in the
// 1..N sequence. Migrations are append-only history — deleting or
// renumbering one is a bug.
func sortAndValidate(migs []migration) ([]migration, error) {
	sort.Slice(migs, func(i, j int) bool { return migs[i].version < migs[j].version })
	for i, m := range migs {
		if m.version != i+1 {
			return nil, fmt.Errorf("migration version gap: expected %d, got %d (%s)", i+1, m.version, m.name)
		}
	}
	return migs, nil
}

// migrate is the entry point called by Open. It loads embedded migrations
// and applies them to the database.
func migrate(db *sql.DB, isFresh bool) error {
	migs, err := loadMigrations()
	if err != nil {
		return err
	}
	return migrateDB(db, migs, isFresh)
}

// migrateDB ensures the database is at the latest migration version.
//
// Fresh databases are baselined to the max version (schema.sql already
// created the full schema, so migrations would be redundant). Existing
// databases have pending migrations applied in order, each in its own
// transaction.
func migrateDB(db *sql.DB, migs []migration, isFresh bool) error {
	if _, err := db.Exec(schemaVersionDDL); err != nil {
		return fmt.Errorf("create _schema_version: %w", err)
	}

	maxVersion := 0
	if len(migs) > 0 {
		maxVersion = migs[len(migs)-1].version
	}

	if isFresh {
		return baselineFresh(db, maxVersion)
	}

	current, err := readVersion(db)
	if err != nil {
		return err
	}
	if current >= maxVersion {
		logging.Infof("store: schema at version %d, no migrations needed", current)
		return nil
	}
	return applyPending(db, migs, current, maxVersion)
}

// baselineFresh sets a fresh database's version to maxVersion without
// running any migrations — schema.sql already created the latest schema.
func baselineFresh(db *sql.DB, maxVersion int) error {
	if _, err := db.Exec(
		"UPDATE _schema_version SET version = ? WHERE id = 1", maxVersion,
	); err != nil {
		return fmt.Errorf("baseline schema version to %d: %w", maxVersion, err)
	}
	logging.Infof("store: fresh database, schema at version %d", maxVersion)
	return nil
}

// readVersion returns the current schema version from _schema_version.
func readVersion(db *sql.DB) (int, error) {
	var v int
	if err := db.QueryRow(
		"SELECT version FROM _schema_version WHERE id = 1",
	).Scan(&v); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return v, nil
}

// applyPending runs each migration with version > current, in order.
// Each migration runs in its own transaction: the migration SQL and the
// version UPDATE commit atomically, so a failure rolls back cleanly.
func applyPending(db *sql.DB, migs []migration, current, maxVersion int) error {
	for _, m := range migs {
		if m.version <= current {
			continue
		}
		warnIfDestructive(m)
		logging.Infof("store: migration %s: applying...", m.name)

		if err := applyMigration(db, m); err != nil {
			return err
		}
		logging.Infof("store: migration %s: done", m.name)
	}
	logging.Infof("store: schema at version %d", maxVersion)
	return nil
}

// applyMigration executes one migration SQL plus the version bump inside
// a single transaction. On failure the transaction is rolled back and the
// error is returned — the database stays at the previous version.
func applyMigration(db *sql.DB, m migration) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx for migration %d: %w", m.version, err)
	}
	if _, err := tx.Exec(m.sql); err != nil {
		tx.Rollback()
		return fmt.Errorf("migration %d (%s): %w", m.version, m.name, err)
	}
	if _, err := tx.Exec(
		"UPDATE _schema_version SET version = ? WHERE id = 1", m.version,
	); err != nil {
		tx.Rollback()
		return fmt.Errorf("update version after migration %d: %w", m.version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %d: %w", m.version, err)
	}
	return nil
}

// destructiveRe matches SQL statements that modify or remove existing data
// rather than adding to it (DROP, DELETE FROM, TRUNCATE, RENAME, ALTER ...
// DROP/RENAME). Comment lines (starting with --) are skipped before
// matching. This is a warning-only check — it does not block execution.
var destructiveRe = regexp.MustCompile(
	`(?i)\b(DROP\s+(TABLE|INDEX|VIEW|TRIGGER)|DELETE\s+FROM|TRUNCATE|RENAME\s+TO|ALTER\s+TABLE[^\n]*\b(DROP\s+COLUMN|RENAME\s+(TO|COLUMN)))\b`,
)

// warnIfDestructive scans a migration's SQL for potentially destructive
// statements and logs a warning. It does NOT prevent execution — the
// additive-only policy is a convention enforced by code review, not a hard
// constraint. The warning makes destructive changes visible in the log
// during startup so they can be caught during testing or review.
func warnIfDestructive(m migration) {
	for _, line := range strings.Split(m.sql, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		if destructiveRe.MatchString(trimmed) {
			logging.Warnf("store: migration %s contains potentially destructive statement: %s", m.name, trimmed)
			logging.Warnf("store: proceeding anyway (additive-only is convention, not enforced)")
			return
		}
	}
}
