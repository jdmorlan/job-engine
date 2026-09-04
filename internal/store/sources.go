package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Source kinds (D22).
//
// One kind, and the constant stays so that adding a second is a change in one
// place rather than a string appearing everywhere.
//
// The directory kind is gone, along with the built-in `local` source that was
// a directory inside the engine's own data directory. Definitions live in a
// repository you own and reach the engine as a registered source; the data
// directory holds engine state and a cache of fetched trees, and nothing a
// person authored. The deciding argument is that a directory source never
// travelled: a job whose code sat on the control plane's disk could only run on
// a worker that shared that disk, so the moment there were two machines the
// kind was already broken.
const SourceKindGitHub = "github"

// SourceKindSystem is the engine's own work, defined as jobs (P2).
//
// Not an exception to D27 so much as its limiting case. The rule is that code
// which cannot travel is not a source; these definitions run `je`, which is on
// every worker by definition because C10 requires the worker to match the
// control plane. There is no location to fetch, no token, and no ref: the
// revision is the engine's version, so a run still records exactly which
// definition of its own work the engine executed (D11).
const SourceKindSystem = "system"

// Source is a named place definitions come from.
type Source struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Location string `json:"location,omitempty"`
	Subpath  string `json:"subpath,omitempty"`

	// Ref is what was asked for -- a branch, a tag, a commit -- and Revision is
	// what it resolved to. Both are recorded because the difference between
	// them is the whole risk of tracking a moving branch: the revision is what
	// actually ran, and D11 stops being true for remote jobs without it.
	Ref      string `json:"ref,omitempty"`
	Revision string `json:"revision,omitempty"`

	// TokenSecret names a secret in the local store (D10), never a token. The
	// value is read when fetching and is never returned by the API.
	TokenSecret string `json:"token_secret,omitempty"`

	SyncedAt  *time.Time `json:"synced_at,omitempty"`
	LastError string     `json:"last_error,omitempty"`
	RemovedAt *time.Time `json:"removed_at,omitempty"`
	AddedAt   time.Time  `json:"added_at"`
}

// Removed reports whether this registration is gone. The row stays so the jobs
// it loaded keep a readable name.
func (s Source) Removed() bool { return s.RemovedAt != nil }

// Qualify returns the identity of a definition from this source.
//
// Always prefixed now. The built-in source used to qualify to a bare slug so
// that somebody's first job did not have to know sources were a concept -- a
// trade that made sense while there was a source you got without asking. There
// is not, so every job comes from a source somebody named, and the name is
// worth carrying.
func (s Source) Qualify(slug string) string { return s.Name + "/" + slug }

// SourceOfName splits a qualified name back into its source and slug. An
// unqualified name has no source, which is a caller's problem to report.
func SourceOfName(qualified string) (source, slug string) {
	for i := range qualified {
		if qualified[i] == '/' {
			return qualified[:i], qualified[i+1:]
		}
	}
	return "", qualified
}

// UpsertSource registers a source or updates its registration.
func (s *Store) UpsertSource(ctx context.Context, src Source) error {
	_, err := s.state.ExecContext(ctx, `
		INSERT INTO sources (name, kind, location, subpath, ref, revision, token_secret, added_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET
			kind         = excluded.kind,
			location     = excluded.location,
			subpath      = excluded.subpath,
			ref          = excluded.ref,
			token_secret = excluded.token_secret,
			-- Re-registering a name brings it back, keeping every job id and
			-- therefore every job's history. Same rule as a job file that
			-- reappears.
			removed_at   = NULL`,
		src.Name, src.Kind, src.Location, src.Subpath, src.Ref, src.Revision,
		src.TokenSecret, formatTime(time.Now()),
	)
	if err != nil {
		return fmt.Errorf("registering source %s: %w", src.Name, err)
	}
	return nil
}

// RecordSourceSync stores the outcome of one source's load.
//
// An error is recorded rather than returned to nobody: a source that stopped
// loading has to be visible in `je source` afterwards, or a repo silently stops
// updating and everything looks fine (P1).
func (s *Store) RecordSourceSync(ctx context.Context, name, revision, syncErr string) error {
	_, err := s.state.ExecContext(ctx, `
		UPDATE sources SET revision = ?, synced_at = ?, last_error = ? WHERE name = ?`,
		revision, formatTime(time.Now()), nullString(syncErr), name)
	return err
}

// DeleteSource unregisters a source.
//
// It tombstones rather than deletes, for a reason the foreign key also
// insists on: the jobs it loaded keep their history and their names still
// carry this source, so `weather/ingest` in a run list a year from now needs
// `weather` to still be something the database can describe (D19, D22).
func (s *Store) DeleteSource(ctx context.Context, name string) error {
	res, err := s.state.ExecContext(ctx,
		`UPDATE sources SET removed_at = ? WHERE name = ? AND removed_at IS NULL`,
		formatTime(time.Now()), name)
	if err != nil {
		return fmt.Errorf("removing source %s: %w", name, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

const selectSource = `
	SELECT name, kind, location, subpath, ref, revision, token_secret,
	       synced_at, last_error, removed_at, added_at
	FROM sources`

// Sources returns every registered source, by name.
func (s *Store) Sources(ctx context.Context) ([]Source, error) {
	rows, err := s.state.QueryContext(ctx,
		selectSource+` WHERE removed_at IS NULL ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("listing sources: %w", err)
	}
	defer rows.Close()

	var out []Source
	for rows.Next() {
		src, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	return out, rows.Err()
}

// SourceByName returns one live registration. sql.ErrNoRows means it is not
// registered, which includes one that was unregistered: its row survives for
// the history, and it is not a place definitions come from any more.
func (s *Store) SourceByName(ctx context.Context, name string) (Source, error) {
	return scanSource(s.state.QueryRowContext(ctx,
		selectSource+` WHERE name = ? AND removed_at IS NULL`, name))
}

func scanSource(sc scanner) (Source, error) {
	var (
		src       Source
		syncedAt  sql.NullString
		lastError sql.NullString
		removedAt sql.NullString
		addedAt   string
	)
	if err := sc.Scan(&src.Name, &src.Kind, &src.Location, &src.Subpath, &src.Ref,
		&src.Revision, &src.TokenSecret, &syncedAt, &lastError, &removedAt,
		&addedAt); err != nil {
		return Source{}, err
	}
	src.LastError = lastError.String

	var err error
	if src.SyncedAt, err = parseNullTime(syncedAt); err != nil {
		return Source{}, fmt.Errorf("source %s: %w", src.Name, err)
	}
	if src.RemovedAt, err = parseNullTime(removedAt); err != nil {
		return Source{}, fmt.Errorf("source %s: %w", src.Name, err)
	}
	// Written by the migration with datetime('now'), which is not this
	// package's format, so a parse failure here is cosmetic rather than fatal.
	if src.AddedAt, err = parseTime(addedAt); err != nil {
		src.AddedAt = time.Time{}
	}
	return src, nil
}

// JobSlugsInSource returns the qualified names this source currently owns,
// which is what the per-source tombstone sweep compares against.
func (s *Store) DeleteJobsExceptInSource(ctx context.Context, source string, keep []string) (int64, error) {
	// Scoped to one source on purpose (D22): a weather repo that will not parse
	// keeps its last good tree serving and must not tombstone the home jobs on
	// its way past.
	query := `UPDATE jobs SET removed_at = ? WHERE removed_at IS NULL AND source = ?`
	args := []any{formatTime(time.Now()), source}
	if len(keep) > 0 {
		query += ` AND name NOT IN (?` + repeatComma(len(keep)-1) + `)`
		for _, name := range keep {
			args = append(args, name)
		}
	}
	res, err := s.state.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("tombstoning removed jobs in %s: %w", source, err)
	}
	return res.RowsAffected()
}
