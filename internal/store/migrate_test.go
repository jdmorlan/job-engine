package store

import (
	"database/sql"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// Every migration has to survive meeting a database that already has rows in
// it, which is the only kind of database an upgrade ever runs against.
//
// This is not a hypothetical. 0007 adds `source TEXT NOT NULL DEFAULT 'local'
// REFERENCES sources(name)` to `jobs`, and SQLite refuses that while foreign
// keys are enforced -- but only when the table is non-empty, because it cannot
// check the constraint for rows it is about to backfill. A fresh database
// migrated cleanly, so v0.4.0 shipped a control plane that could not start on
// any install that had ever loaded a job.
//
// Applying every prefix of the migration list and seeding rows before the rest
// is what turns that from a bug somebody finds in production into a test
// failure.
func TestMigrationsApplyToADatabaseThatHasRows(t *testing.T) {
	names := stateMigrations(t)

	for cut := 1; cut < len(names); cut++ {
		t.Run("rows before "+names[cut], func(t *testing.T) {
			db := openTestDB(t)

			applyThrough(t, db, names[:cut])
			seedJob(t, db)

			// The real path, which is what must cope.
			if err := migrateDB(db, "migrations/state"); err != nil {
				t.Fatalf("migrating a database with rows in it: %v", err)
			}

			assertNoDanglingReferences(t, db)
			if got := count(t, db, "jobs"); got != 1 {
				t.Fatalf("the seeded job did not survive: jobs = %d, want 1", got)
			}
		})
	}
}

// A fresh database is the easy case, and worth pinning so the fix above cannot
// regress it while making the hard case pass.
func TestMigrationsApplyToAnEmptyDatabase(t *testing.T) {
	db := openTestDB(t)
	if err := migrateDB(db, "migrations/state"); err != nil {
		t.Fatalf("migrating an empty database: %v", err)
	}
	assertNoDanglingReferences(t, db)
}

// Foreign keys must be enforcing again once migration is done. Turning them off
// is a migration-time concession and would be a silent loss of integrity if it
// leaked into normal operation.
func TestForeignKeysAreOnAfterMigrating(t *testing.T) {
	db := openTestDB(t)
	if err := migrateDB(db, "migrations/state"); err != nil {
		t.Fatal(err)
	}
	var on int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&on); err != nil {
		t.Fatal(err)
	}
	if on != 1 {
		t.Fatal("foreign keys are off after migrating; they must be back on")
	}
}

// A database that arrives already inconsistent still has to start. Refusing
// would swap the bug this file exists for -- an upgrade that cannot boot -- for
// a different one on exactly the same path, and a dangling row in history is no
// reason to stop scheduling work.
func TestAnAlreadyInconsistentDatabaseStillMigrates(t *testing.T) {
	db := openTestDB(t)
	names := stateMigrations(t)
	applyThrough(t, db, names[:1])

	// A job pointing at a version that does not exist, which is only insertable
	// because foreign keys are off -- the same way such a row would arrive in
	// the wild.
	if _, err := db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO jobs (name, definition_hash, file_path, enabled, loaded_at)
		VALUES ('orphan', 'missing-hash', 'orphan.yaml', 1, datetime('now'))`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		t.Fatal(err)
	}

	if err := migrateDB(db, "migrations/state"); err != nil {
		t.Fatalf("a pre-existing dangling reference must not block migrating: %v", err)
	}

	// Still dangling, still reported by the pragma -- unfixed, but not fatal,
	// and not attributed to the migration.
	broken, err := danglingReferences(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(broken) != 1 {
		t.Fatalf("expected the pre-existing dangling row to survive, got %v", broken)
	}
}

func stateMigrations(t *testing.T) []string {
	t.Helper()
	entries, err := fs.ReadDir(migrations, "migrations/state")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) < 2 {
		t.Fatalf("expected several migrations, found %d", len(names))
	}
	return names
}

// openTestDB mirrors openDB's pragmas, since foreign_keys(1) is the whole
// reason the bug this file exists for was possible.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", "file:"+path+
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"+
		"&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

// applyThrough puts the database into the state an older release left it in.
func applyThrough(t *testing.T, db *sql.DB, names []string) {
	t.Helper()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	// Off here for the same reason the real runner turns it off: this is
	// replaying schema history, not running the engine.
	if _, err := db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		body, err := fs.ReadFile(migrations, "migrations/state/"+name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(body)); err != nil {
			t.Fatalf("applying %s: %v", name, err)
		}
		if _, err := db.Exec(
			`INSERT INTO schema_migrations (name, applied_at) VALUES (?, datetime('now'))`,
			name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		t.Fatal(err)
	}
}

// seedJob inserts using only the columns `jobs` has had since 0001, so this
// keeps working as later migrations add more.
func seedJob(t *testing.T, db *sql.DB) {
	t.Helper()
	// jobs.definition_hash references job_versions, so a job needs a version to
	// point at -- which is also what makes this a realistic row rather than one
	// that only exists because the constraint was off.
	if _, err := db.Exec(`INSERT INTO job_versions (definition_hash, definition, first_seen_at)
		VALUES ('abc123', '{}', datetime('now'))`); err != nil {
		t.Fatalf("seeding a job version: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO jobs (name, definition_hash, file_path, enabled, loaded_at)
		VALUES ('weather-ingest', 'abc123', 'weather.yaml', 1, datetime('now'))`); err != nil {
		t.Fatalf("seeding a job: %v", err)
	}
}

func assertNoDanglingReferences(t *testing.T, db *sql.DB) {
	t.Helper()
	broken, err := danglingReferences(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(broken) > 0 {
		t.Fatalf("dangling references after migrating: %v", broken)
	}
}

func count(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(fmt.Sprintf(`SELECT count(*) FROM %s`, table)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
