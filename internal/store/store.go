// Package store owns every SQL statement in the program.
//
// Q1 settled this deliberately: SQLite only, no storage adapter, no interface
// until a second implementation exists. The discipline that replaces the
// abstraction is containment -- nothing outside this package writes a query.
// That keeps a future port a contained project rather than an archaeology dig,
// without paying the abstraction tax now.
package store

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"net/url"
	"sort"
	"strings"

	"github.com/jdmorlan/job-engine/internal/paths"

	_ "modernc.org/sqlite" // pure Go: no cgo, so the binary stays static (D4)
)

//go:embed migrations
var migrations embed.FS

// Store is the engine's persistence. It holds two databases, per D4: state
// (runs, events, definitions, cursors) and logs (captured job output).
//
// A Store is safe for concurrent use.
type Store struct {
	state *sql.DB
	logs  *sql.DB

	// logsPath is kept because reclaiming space is the one operation whose
	// result is a fact about the file rather than about the rows (D13), and a
	// store that has to be told where its own database is has a way of being
	// called wrong.
	logsPath string
}

// Open connects to both databases, creating and migrating them as needed.
//
// The caller must already hold the data directory lock (see internal/lockfile).
// Open does not take it, because read-only tooling like `je db` wants to open
// the file without claiming to be the daemon.
func Open(l paths.Layout) (*Store, error) {
	state, err := openDB(l.StateDB())
	if err != nil {
		return nil, fmt.Errorf("opening state db: %w", err)
	}
	// The logs database is born knowing how to give space back (D13). Set in
	// the DSN because auto_vacuum can only be chosen before the first table
	// exists -- after that it takes a full VACUUM to change, which is what
	// ReclaimLogSpace does once for a database that predates this.
	//
	// Only the logs database. State is small, its rows are the history rather
	// than its volume, and auto_vacuum's pointer-map pages are a cost with
	// nothing to buy there.
	logs, err := openDB(l.LogsDB(), "auto_vacuum(INCREMENTAL)")
	if err != nil {
		state.Close()
		return nil, fmt.Errorf("opening logs db: %w", err)
	}

	s := &Store{state: state, logs: logs, logsPath: l.LogsDB()}
	if err := s.migrate(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// Close releases both database handles.
func (s *Store) Close() error {
	var errs []error
	if s.state != nil {
		errs = append(errs, s.state.Close())
	}
	if s.logs != nil {
		errs = append(errs, s.logs.Close())
	}
	return joinErrs(errs)
}

func openDB(path string, extra ...string) (*sql.DB, error) {
	// Pragmas go in the DSN rather than in a post-open Exec, so that every
	// connection the pool creates gets them. A pragma set on one connection
	// does not apply to the next one, which is a classic and very quiet bug.
	//
	//   journal_mode(WAL)  readers do not block the writer (D4)
	//   busy_timeout(5000) wait rather than fail on a momentary conflict
	//   foreign_keys(1)    SQLite has them off by default; the schema means them
	//   synchronous(NORMAL) the WAL-safe setting; FULL costs an fsync per commit
	q := url.Values{}
	for _, p := range append([]string{
		"journal_mode(WAL)",
		"busy_timeout(5000)",
		"foreign_keys(1)",
		"synchronous(NORMAL)",
	}, extra...) {
		q.Add("_pragma", p)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}

	// One connection, deliberately. The engine is a single writer by design
	// (D20 C1), and serialising through one connection means we never see
	// SQLITE_BUSY at all. If read concurrency ever becomes a real bottleneck
	// the fix is a separate read-only pool, not loosening this.
	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// migrate applies any unapplied migration files to each database.
//
// Migrations are embedded .sql files applied in filename order, each in its own
// transaction, recorded in schema_migrations. Deliberately not a library: the
// whole mechanism is forty lines and a dependency here would be one we could
// never remove.
func (s *Store) migrate() error {
	for _, m := range []struct {
		name string
		db   *sql.DB
	}{
		{"state", s.state},
		{"logs", s.logs},
	} {
		if err := migrateDB(m.db, "migrations/"+m.name); err != nil {
			return fmt.Errorf("migrating %s db: %w", m.name, err)
		}
	}
	return nil
}

func migrateDB(db *sql.DB, dir string) error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name       TEXT PRIMARY KEY,
			applied_at TEXT NOT NULL
		)`); err != nil {
		return err
	}

	applied := map[string]bool{}
	rows, err := db.Query(`SELECT name FROM schema_migrations`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		applied[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}

	entries, err := fs.ReadDir(migrations, dir)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	pending := false
	for _, name := range names {
		if !applied[name] {
			pending = true
			break
		}
	}
	if !pending {
		return nil
	}

	// Foreign keys off for the duration, which is what SQLite's own procedure
	// for schema changes prescribes -- and here it is not optional. SQLite
	// refuses `ALTER TABLE ... ADD COLUMN ... NOT NULL DEFAULT x REFERENCES y`
	// while foreign keys are enforced, but *only when the table already has
	// rows*: it cannot check the constraint for the rows it is about to
	// backfill. 0007 does exactly that to `jobs`, `chains` and `routes`.
	//
	// That row-dependence is why this shipped: a fresh database migrates
	// cleanly and every test started from one, so the failure only appeared on
	// a real install with real history -- the one case nothing exercised.
	//
	// The pragma has to be outside a transaction, where it is a silent no-op,
	// and the single connection (SetMaxOpenConns(1)) is what makes setting it
	// here reliable.
	// Recorded before anything is disabled, so a database that was already
	// inconsistent is not blamed on the migration that ran next.
	before, err := danglingReferences(db)
	if err != nil {
		return err
	}

	if _, err := db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("disabling foreign keys for migration: %w", err)
	}
	defer db.Exec(`PRAGMA foreign_keys = ON`)

	for _, name := range names {
		if applied[name] {
			continue
		}
		body, err := fs.ReadFile(migrations, dir+"/"+name)
		if err != nil {
			return err
		}
		// One transaction per migration: a failed migration leaves the
		// database exactly as it was, so the next start retries cleanly
		// instead of finding a half-applied schema.
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("%s: %w", name, err)
		}
		if _, err := tx.Exec(
			`INSERT INTO schema_migrations (name, applied_at) VALUES (?, datetime('now'))`,
			name,
		); err != nil {
			tx.Rollback()
			return fmt.Errorf("%s: recording migration: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}

	// Nothing enforced foreign keys while those ran, so check what the
	// migrations actually did -- but only hold them responsible for damage they
	// caused. A database that arrives already inconsistent is not this
	// migration's doing, and refusing to start over it would replace the bug
	// this fix exists for with a different one on the same upgrade path.
	after, err := danglingReferences(db)
	if err != nil {
		return err
	}
	if introduced := added(before, after); len(introduced) > 0 {
		return fmt.Errorf("migration left %d dangling reference(s): %s",
			len(introduced), strings.Join(introduced, ", "))
	}
	return nil
}

// danglingReferences lists every row whose foreign key does not resolve, each
// as a stable "table row N -> parent" string so two snapshots can be compared.
func danglingReferences(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		return nil, fmt.Errorf("checking foreign keys: %w", err)
	}
	defer rows.Close()

	var broken []string
	for rows.Next() {
		// table, rowid, referred table, and which foreign key of that table.
		var table, parent string
		var rowid sql.NullInt64
		var fkid int
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return nil, fmt.Errorf("checking foreign keys: %w", err)
		}
		broken = append(broken, fmt.Sprintf("%s row %d -> %s", table, rowid.Int64, parent))
	}
	return broken, rows.Err()
}

// added returns the entries in after that were not already in before.
func added(before, after []string) []string {
	had := make(map[string]bool, len(before))
	for _, b := range before {
		had[b] = true
	}
	var out []string
	for _, a := range after {
		if !had[a] {
			out = append(out, a)
		}
	}
	return out
}

func joinErrs(errs []error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
