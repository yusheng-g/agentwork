package store

import (
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// openTestDB opens an in-memory SQLite (single connection) for migration
// tests. The caller is responsible for applying schema or setting up tables
// as needed by the specific test scenario.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func schemaVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v int
	if err := db.QueryRow("SELECT version FROM _schema_version WHERE id = 1").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func columnExists(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	var count int
	err := db.QueryRow(
		`SELECT count(*) FROM pragma_table_info(?) WHERE name = ?`,
		table, column,
	).Scan(&count)
	if err != nil {
		t.Fatal(err)
	}
	return count > 0
}

// TestMigrateDB_FreshBaseline: a fresh database (no user tables) is
// baselined to the latest version without running any migration SQL. The
// schema.sql is expected to already contain the full schema — migrations
// would be redundant.
func TestMigrateDB_FreshBaseline(t *testing.T) {
	db := openTestDB(t)
	migs := []migration{
		{version: 1, name: "0001_add_col.sql", sql: "ALTER TABLE t ADD COLUMN c1 INTEGER DEFAULT 0;"},
		{version: 2, name: "0002_add_col.sql", sql: "ALTER TABLE t ADD COLUMN c2 TEXT DEFAULT '';"},
	}

	if _, err := db.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}

	// isFresh=true: schema.sql already created the full schema, so
	// migrations must NOT run — baseline directly to version 2.
	if err := migrateDB(db, migs, true); err != nil {
		t.Fatal(err)
	}

	if v := schemaVersion(t, db); v != 2 {
		t.Fatalf("fresh baseline: got version %d, want 2", v)
	}
	if columnExists(t, db, "t", "c1") {
		t.Fatal("fresh baseline: migration 1 should not have run, but column c1 exists")
	}
	if columnExists(t, db, "t", "c2") {
		t.Fatal("fresh baseline: migration 2 should not have run, but column c2 exists")
	}
}

// TestMigrateDB_ExistingRunsPending: an existing database at version 0 has
// all pending migrations applied in order, each adding its column.
func TestMigrateDB_ExistingRunsPending(t *testing.T) {
	db := openTestDB(t)
	migs := []migration{
		{version: 1, name: "0001_add_c1.sql", sql: "ALTER TABLE t ADD COLUMN c1 INTEGER DEFAULT 0;"},
		{version: 2, name: "0002_add_c2.sql", sql: "ALTER TABLE t ADD COLUMN c2 TEXT DEFAULT '';"},
	}

	// Existing database: table t already exists from a pre-migration schema.
	if _, err := db.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}

	if err := migrateDB(db, migs, false); err != nil {
		t.Fatal(err)
	}

	if v := schemaVersion(t, db); v != 2 {
		t.Fatalf("existing: got version %d, want 2", v)
	}
	if !columnExists(t, db, "t", "c1") {
		t.Fatal("existing: column c1 should exist after migration 1")
	}
	if !columnExists(t, db, "t", "c2") {
		t.Fatal("existing: column c2 should exist after migration 2")
	}
}

// TestMigrateDB_IdempotentReRun: calling migrateDB again after all
// migrations are applied is a no-op — no errors, no re-execution.
func TestMigrateDB_IdempotentReRun(t *testing.T) {
	db := openTestDB(t)
	migs := []migration{
		{version: 1, name: "0001_add_c1.sql", sql: "ALTER TABLE t ADD COLUMN c1 INTEGER DEFAULT 0;"},
	}
	if _, err := db.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, migs, false); err != nil {
		t.Fatal(err)
	}
	if v := schemaVersion(t, db); v != 1 {
		t.Fatalf("first run: got version %d, want 1", v)
	}

	if err := migrateDB(db, migs, false); err != nil {
		t.Fatal(err)
	}
	if v := schemaVersion(t, db); v != 1 {
		t.Fatalf("re-run: got version %d, want 1 (unchanged)", v)
	}
}

// TestMigrateDB_PartialThenResume: if the database is at version 1 and a
// new migration 2 is added, only migration 2 runs on the next Open.
func TestMigrateDB_PartialThenResume(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}

	// First Open: only migration 1 exists.
	migsV1 := []migration{
		{version: 1, name: "0001_add_c1.sql", sql: "ALTER TABLE t ADD COLUMN c1 INTEGER DEFAULT 0;"},
	}
	if err := migrateDB(db, migsV1, false); err != nil {
		t.Fatal(err)
	}
	if !columnExists(t, db, "t", "c1") {
		t.Fatal("first run: column c1 should exist")
	}

	// Second Open: migration 2 was added in a new release.
	migsV2 := []migration{
		{version: 1, name: "0001_add_c1.sql", sql: "ALTER TABLE t ADD COLUMN c1 INTEGER DEFAULT 0;"},
		{version: 2, name: "0002_add_c2.sql", sql: "ALTER TABLE t ADD COLUMN c2 TEXT DEFAULT '';"},
	}
	if err := migrateDB(db, migsV2, false); err != nil {
		t.Fatal(err)
	}
	if v := schemaVersion(t, db); v != 2 {
		t.Fatalf("second run: got version %d, want 2", v)
	}
	if !columnExists(t, db, "t", "c2") {
		t.Fatal("second run: column c2 should exist")
	}
	// Migration 1 did NOT re-run — re-adding c1 would error "duplicate
	// column". No error means it was correctly skipped.
}

// TestMigrateDB_EmptyMigrations: with zero migration files, migrateDB
// succeeds and sets version=0 (baseline) or leaves it at 0 (existing).
func TestMigrateDB_EmptyMigrations(t *testing.T) {
	t.Run("fresh", func(t *testing.T) {
		db := openTestDB(t)
		if _, err := db.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY)"); err != nil {
			t.Fatal(err)
		}
		if err := migrateDB(db, nil, true); err != nil {
			t.Fatal(err)
		}
		if v := schemaVersion(t, db); v != 0 {
			t.Fatalf("fresh empty: got version %d, want 0", v)
		}
	})
	t.Run("existing", func(t *testing.T) {
		db := openTestDB(t)
		if _, err := db.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY)"); err != nil {
			t.Fatal(err)
		}
		if err := migrateDB(db, nil, false); err != nil {
			t.Fatal(err)
		}
		if v := schemaVersion(t, db); v != 0 {
			t.Fatalf("existing empty: got version %d, want 0", v)
		}
	})
}

// TestMigrateDB_FailureRollsBack: a migration that errors (e.g. invalid
// SQL) is rolled back — the version is NOT bumped and Open returns an error.
func TestMigrateDB_FailureRollsBack(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	migs := []migration{
		{version: 1, name: "0001_add_c1.sql", sql: "ALTER TABLE t ADD COLUMN c1 INTEGER DEFAULT 0;"},
		{version: 2, name: "0002_bad.sql", sql: "ALTER TABLE nonexistent ADD COLUMN x;"},
	}
	err := migrateDB(db, migs, false)
	if err == nil {
		t.Fatal("expected error from bad migration, got nil")
	}
	if !strings.Contains(err.Error(), "0002_bad") {
		t.Fatalf("error should name the failing migration, got: %v", err)
	}
	// Version should be 1 (migration 1 succeeded, migration 2 failed and
	// rolled back).
	if v := schemaVersion(t, db); v != 1 {
		t.Fatalf("after failure: got version %d, want 1 (migration 2 rolled back)", v)
	}
	// Column c1 exists (migration 1 committed), column x does not.
	if !columnExists(t, db, "t", "c1") {
		t.Fatal("column c1 should exist (migration 1 committed before failure)")
	}
}

// TestSortAndValidate_GapDetected: a gap in version numbers (1, 3) is
// rejected — migrations must be contiguous 1..N.
func TestSortAndValidate_GapDetected(t *testing.T) {
	migs := []migration{
		{version: 1, name: "0001_a.sql", sql: ""},
		{version: 3, name: "0003_c.sql", sql: ""},
	}
	_, err := sortAndValidate(migs)
	if err == nil {
		t.Fatal("expected gap error, got nil")
	}
	if !strings.Contains(err.Error(), "gap") {
		t.Fatalf("error should mention gap, got: %v", err)
	}
}

// TestSortAndValidate_OutOfOrder: migrations provided out of order are
// sorted correctly.
func TestSortAndValidate_OutOfOrder(t *testing.T) {
	migs := []migration{
		{version: 3, name: "0003_c.sql", sql: ""},
		{version: 1, name: "0001_a.sql", sql: ""},
		{version: 2, name: "0002_b.sql", sql: ""},
	}
	sorted, err := sortAndValidate(migs)
	if err != nil {
		t.Fatal(err)
	}
	if sorted[0].version != 1 || sorted[1].version != 2 || sorted[2].version != 3 {
		t.Fatalf("not sorted: %v %v %v", sorted[0].version, sorted[1].version, sorted[2].version)
	}
}

// TestSplitMigrationName: filename parsing accepts valid names and rejects
// malformed ones.
func TestSplitMigrationName(t *testing.T) {
	tests := []struct {
		filename string
		wantVer  int
		wantOk   bool
	}{
		{"0001_add_col.sql", 1, true},
		{"0042_index_run_cost.sql", 42, true},
		{"9999_z.sql", 9999, true},
		{"001_no_suffix", 0, false},
		{"add_col.sql", 0, false},
		{"0001.sql", 0, false},
		{"abc_name.sql", 0, false},
		{"0000_zero.sql", 0, false},
	}
	for _, tt := range tests {
		ver, _, ok := splitMigrationName(tt.filename)
		if ok != tt.wantOk || (ok && ver != tt.wantVer) {
			t.Errorf("splitMigrationName(%q) = (%d, %v), want (%d, %v)",
				tt.filename, ver, ok, tt.wantVer, tt.wantOk)
		}
	}
}

// TestWarnIfDestructive does not panic on destructive SQL. It only verifies
// the function runs without error (the warning goes to the log, which we
// don't capture here — the point is it does NOT block execution).
func TestWarnIfDestructive_NoPanic(t *testing.T) {
	migs := []migration{
		{name: "0001_ok.sql", sql: "ALTER TABLE t ADD COLUMN c INTEGER DEFAULT 0;"},
		{name: "0002_drop.sql", sql: "DROP TABLE t;"},
		{name: "0003_delete.sql", sql: "DELETE FROM t WHERE id = 1;"},
		{name: "0004_comment.sql", sql: "-- this DROP is just a comment\nALTER TABLE t ADD COLUMN x;"},
	}
	for _, m := range migs {
		warnIfDestructive(m) // should not panic
	}
}

// TestOpen_FreshInMemory: a fresh :memory: database opened via Open gets the
// full schema and is baselined to the current max migration version.
func TestOpen_FreshInMemory(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	// The version should match the highest migration file version.
	migs, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	wantVersion := 0
	if len(migs) > 0 {
		wantVersion = migs[len(migs)-1].version
	}
	if v := schemaVersion(t, st.DB()); v != wantVersion {
		t.Fatalf("fresh :memory: version: got %d, want %d", v, wantVersion)
	}

	var count int
	err = st.DB().QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='goal'`,
	).Scan(&count)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("fresh :memory: goal table should exist, got count=%d", count)
	}
}

// TestOpen_ReopenKeepsVersion: opening an existing file DB does not re-run
// migrations or reset the version. This simulates a daemon restart.
func TestOpen_ReopenKeepsVersion(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/test.db"

	// First open: creates the DB, applies schema, baselines to max version.
	st1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st1.Close()

	// Second open: existing DB, version should be unchanged.
	st2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()

	migs, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	wantVersion := 0
	if len(migs) > 0 {
		wantVersion = migs[len(migs)-1].version
	}
	if v := schemaVersion(t, st2.DB()); v != wantVersion {
		t.Fatalf("reopen: got version %d, want %d", v, wantVersion)
	}
}
