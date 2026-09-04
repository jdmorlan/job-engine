package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Job is a loaded definition as the database holds it.
type Job struct {
	ID   int64  `json:"id"`
	Slug string `json:"slug"`

	// Source is which registered place this definition came from (D22).
	// Authority is per source, so every sweep over "what is still here" is
	// scoped by it.
	Source string `json:"source"`

	DefinitionHash string          `json:"definition_hash"`
	Definition     json.RawMessage `json:"definition,omitempty"`
	FilePath       string          `json:"file_path"`
	Enabled        bool            `json:"enabled"`
	LoadedAt       time.Time       `json:"loaded_at"`
	LoadError      string          `json:"load_error,omitempty"`
	ConfigError    string          `json:"config_error,omitempty"`

	// RemovedAt is set when the definition file disappeared. The job keeps its
	// history and stops being schedulable (D19).
	RemovedAt *time.Time `json:"removed_at,omitempty"`

	// Declared maps a field the author wrote to the line it is on, for
	// `je explain` (P3). Stored beside the definition rather than in it,
	// because a line number describes the file and not the job.
	Declared map[string]int `json:"declared,omitempty"`
}

// sourceOrLocal is retained as the one place a missing source is caught.
//
// There is no default any more: every job comes from a registered source, so an
// empty one is a bug in the caller rather than something to paper over with a
// built-in name.
func (j Job) sourceOrLocal() string { return j.Source }

// Removed reports whether this job's definition file is gone.
func (j Job) Removed() bool { return j.RemovedAt != nil }

// Runnable reports whether this job may start a run.
func (j Job) Runnable() bool {
	return j.Enabled && !j.Removed() && j.LoadError == "" && j.ConfigError == ""
}

// UpsertJob records a definition and its snapshot, returning the job row.
//
// Snapshots are deduped by hash (D11): loading the same definition a thousand
// times inserts one job_versions row, so history costs nothing until something
// actually changes.
func (s *Store) UpsertJob(ctx context.Context, j Job) (Job, error) {
	tx, err := s.state.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback() // no-op after a successful Commit

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO job_versions (definition_hash, definition, first_seen_at)
		VALUES (?, ?, ?)
		ON CONFLICT (definition_hash) DO NOTHING`,
		j.DefinitionHash, string(j.Definition), formatTime(time.Now()),
	); err != nil {
		return Job{}, fmt.Errorf("recording definition version: %w", err)
	}

	// The job row is a projection of the file, so re-loading overwrites it.
	// job_versions is the immutable half; this table is the current half.
	declared, err := json.Marshal(j.Declared)
	if err != nil {
		return Job{}, fmt.Errorf("recording declared lines for %s: %w", j.Slug, err)
	}

	err = tx.QueryRowContext(ctx, `
		INSERT INTO jobs (name, source, definition_hash, file_path, enabled, loaded_at, load_error, config_error, declared)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET
			source          = excluded.source,
			definition_hash = excluded.definition_hash,
			file_path       = excluded.file_path,
			enabled         = excluded.enabled,
			loaded_at       = excluded.loaded_at,
			load_error      = excluded.load_error,
			config_error    = excluded.config_error,
			declared        = excluded.declared,
			-- A file that reappears un-tombstones the job, keeping its id and
			-- therefore its whole history and cursor. Reverting a revert has
			-- to be as safe as the revert was.
			removed_at      = NULL
		RETURNING id`,
		j.Slug, j.sourceOrLocal(), j.DefinitionHash, j.FilePath, j.Enabled,
		formatTime(time.Now()), nullString(j.LoadError), nullString(j.ConfigError),
		string(declared),
	).Scan(&j.ID)
	if err != nil {
		return Job{}, fmt.Errorf("upserting job %s: %w", j.Slug, err)
	}
	return j, tx.Commit()
}

const selectJob = `
	SELECT j.id, j.name, j.source, j.definition_hash, v.definition, j.file_path,
	       j.enabled, j.loaded_at, j.load_error, j.config_error, j.removed_at,
	       j.declared
	FROM jobs j
	JOIN job_versions v ON v.definition_hash = j.definition_hash`

// JobBySlug looks up one job. Returns sql.ErrNoRows if there is no such job.
func (s *Store) JobBySlug(ctx context.Context, slug string) (Job, error) {
	return scanJob(s.state.QueryRowContext(ctx, selectJob+` WHERE j.name = ?`, slug))
}

// JobByID looks up one job. Used when executing a claimed run, which knows the
// id rather than the slug.
func (s *Store) JobByID(ctx context.Context, id int64) (Job, error) {
	return scanJob(s.state.QueryRowContext(ctx, selectJob+` WHERE j.id = ?`, id))
}

// ListJobs returns every loaded job, by slug.
func (s *Store) ListJobs(ctx context.Context) ([]Job, error) {
	rows, err := s.state.QueryContext(ctx, selectJob+` ORDER BY j.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// DeleteJobsExcept tombstones jobs whose files have disappeared.
//
// D19 is explicit that deleting a file must never delete history: the job stops
// being schedulable, but its runs, logs and cursor stay. So this disables
// rather than deletes -- reverting a commit must not erase the timeline, since
// the trustworthy timeline is the whole point (P1).
func (s *Store) DeleteJobsExcept(ctx context.Context, keep []string) (int64, error) {
	// A NOT IN with a variable list needs generated placeholders; with an empty
	// keep set the clause collapses to "disable everything", which is correct
	// when every job file has been removed.
	query := `UPDATE jobs SET removed_at = ? WHERE removed_at IS NULL`
	args := make([]any, 0, len(keep)+1)
	args = append(args, formatTime(time.Now()))
	if len(keep) > 0 {
		query += ` AND name NOT IN (?` + repeatComma(len(keep)-1) + `)`
		for _, slug := range keep {
			args = append(args, slug)
		}
	}
	res, err := s.state.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("tombstoning removed jobs: %w", err)
	}
	return res.RowsAffected()
}

func repeatComma(n int) string {
	out := make([]byte, 0, n*3)
	for range n {
		out = append(out, ',', '?')
	}
	return string(out)
}

func scanJob(sc scanner) (Job, error) {
	var (
		j          Job
		definition string
		loadedAt   string
		loadErr    sql.NullString
		configErr  sql.NullString
		removedAt  sql.NullString
		declared   sql.NullString
	)
	if err := sc.Scan(&j.ID, &j.Slug, &j.Source, &j.DefinitionHash, &definition, &j.FilePath,
		&j.Enabled, &loadedAt, &loadErr, &configErr, &removedAt, &declared); err != nil {
		return Job{}, err
	}
	j.Definition = json.RawMessage(definition)
	j.LoadError = loadErr.String
	j.ConfigError = configErr.String
	if declared.Valid && declared.String != "null" {
		if err := json.Unmarshal([]byte(declared.String), &j.Declared); err != nil {
			return Job{}, fmt.Errorf("job %s has unreadable declared lines: %w", j.Slug, err)
		}
	}

	var err error
	if j.RemovedAt, err = parseNullTime(removedAt); err != nil {
		return Job{}, fmt.Errorf("job %s: %w", j.Slug, err)
	}
	if j.LoadedAt, err = parseTime(loadedAt); err != nil {
		return Job{}, fmt.Errorf("job %s has unparseable loaded_at %q: %w", j.Slug, loadedAt, err)
	}
	return j, nil
}
