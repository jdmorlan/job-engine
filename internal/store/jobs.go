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
	ID             int64           `json:"id"`
	Slug           string          `json:"slug"`
	DefinitionHash string          `json:"definition_hash"`
	Definition     json.RawMessage `json:"definition,omitempty"`
	FilePath       string          `json:"file_path"`
	Enabled        bool            `json:"enabled"`
	LoadedAt       time.Time       `json:"loaded_at"`
	LoadError      string          `json:"load_error,omitempty"`
	ConfigError    string          `json:"config_error,omitempty"`
}

// Runnable reports whether this job may start a run.
func (j Job) Runnable() bool {
	return j.Enabled && j.LoadError == "" && j.ConfigError == ""
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
	err = tx.QueryRowContext(ctx, `
		INSERT INTO jobs (name, definition_hash, file_path, enabled, loaded_at, load_error, config_error)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET
			definition_hash = excluded.definition_hash,
			file_path       = excluded.file_path,
			enabled         = excluded.enabled,
			loaded_at       = excluded.loaded_at,
			load_error      = excluded.load_error,
			config_error    = excluded.config_error
		RETURNING id`,
		j.Slug, j.DefinitionHash, j.FilePath, j.Enabled,
		formatTime(time.Now()), nullString(j.LoadError), nullString(j.ConfigError),
	).Scan(&j.ID)
	if err != nil {
		return Job{}, fmt.Errorf("upserting job %s: %w", j.Slug, err)
	}
	return j, tx.Commit()
}

const selectJob = `
	SELECT j.id, j.name, j.definition_hash, v.definition, j.file_path,
	       j.enabled, j.loaded_at, j.load_error, j.config_error
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
	query := `UPDATE jobs SET enabled = 0, load_error = 'definition file removed' WHERE enabled = 1`
	args := make([]any, 0, len(keep))
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
	)
	if err := sc.Scan(&j.ID, &j.Slug, &j.DefinitionHash, &definition, &j.FilePath,
		&j.Enabled, &loadedAt, &loadErr, &configErr); err != nil {
		return Job{}, err
	}
	j.Definition = json.RawMessage(definition)
	j.LoadError = loadErr.String
	j.ConfigError = configErr.String

	t, err := parseTime(loadedAt)
	if err != nil {
		return Job{}, fmt.Errorf("job %s has unparseable loaded_at %q: %w", j.Slug, loadedAt, err)
	}
	j.LoadedAt = t
	return j, nil
}
