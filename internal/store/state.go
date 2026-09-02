package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// StateVersion is one committed value of a job's cursor (D14).
//
// The table is append-only, so this type is immutable history rather than a
// mutable row. "The cursor stopped moving on Tuesday even though runs kept
// succeeding" is a bug class that is invisible everywhere else and obvious
// here, and it is only obvious because we keep the versions.
type StateVersion struct {
	JobID      int64
	Version    int64
	Value      json.RawMessage
	SetByRun   *int64
	SetByActor string
	CreatedAt  time.Time
}

// Summary renders a cursor for display, preferring the key the author declared
// as primary_cursor so status views show what they said matters (D14).
//
// State stays opaque JSON to the engine; this only decides what to *show*,
// which is the whole job of the primary_cursor hint.
func (v StateVersion) Summary(primary string) string {
	var fields map[string]any
	if err := json.Unmarshal(v.Value, &fields); err != nil {
		return string(v.Value)
	}
	if got, ok := fields[primary]; ok {
		return fmt.Sprintf("%v", got)
	}
	return string(v.Value)
}

// ActorEngine marks the cursor version the engine seeded on a first run, as
// distinct from one a job or a person set.
const ActorEngine = "engine"

// CurrentState returns a job's cursor, or sql.ErrNoRows if it has none yet.
func (s *Store) CurrentState(ctx context.Context, jobID int64) (StateVersion, error) {
	return scanStateVersion(s.state.QueryRowContext(ctx, selectState+`
		WHERE job_id = ? ORDER BY version DESC LIMIT 1`, jobID))
}

// StateAtVersion returns one specific version.
//
// D14 requires `je retry` of an old run to replay with the state that run
// originally started from, not today's. That is what this is for, and getting
// it wrong in either direction causes silent data gaps.
func (s *Store) StateAtVersion(ctx context.Context, jobID, version int64) (StateVersion, error) {
	return scanStateVersion(s.state.QueryRowContext(ctx, selectState+`
		WHERE job_id = ? AND version = ?`, jobID, version))
}

// StateHistory returns recent versions, newest first.
func (s *Store) StateHistory(ctx context.Context, jobID int64, limit int) ([]StateVersion, error) {
	rows, err := s.state.QueryContext(ctx, selectState+`
		WHERE job_id = ? ORDER BY version DESC LIMIT ?`, jobID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []StateVersion
	for rows.Next() {
		v, err := scanStateVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// CommitState appends a new cursor version and returns it.
//
// Callers are responsible for the commit-on-success rule (D14) -- this function
// is the mechanism, and the engine is what decides whether to invoke it. That
// split is deliberate: the rule is a semantic decision that belongs where the
// run's outcome is known, not buried in a SQL helper.
func (s *Store) CommitState(ctx context.Context, v StateVersion) (StateVersion, error) {
	if len(v.Value) == 0 {
		return StateVersion{}, fmt.Errorf("refusing to commit empty state for job %d", v.JobID)
	}
	if !json.Valid(v.Value) {
		return StateVersion{}, fmt.Errorf("job %d state is not valid JSON", v.JobID)
	}

	tx, err := s.state.BeginTx(ctx, nil)
	if err != nil {
		return StateVersion{}, err
	}
	defer tx.Rollback()

	// Derived inside the transaction so two commits cannot claim one version.
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) + 1 FROM job_state WHERE job_id = ?`,
		v.JobID).Scan(&v.Version); err != nil {
		return StateVersion{}, err
	}

	v.CreatedAt = time.Now()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO job_state (job_id, version, value, set_by_run_id, set_by_actor, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		v.JobID, v.Version, string(v.Value), v.SetByRun,
		nullString(v.SetByActor), formatTime(v.CreatedAt),
	); err != nil {
		return StateVersion{}, fmt.Errorf("committing state: %w", err)
	}
	return v, tx.Commit()
}

const selectState = `
	SELECT job_id, version, value, set_by_run_id, set_by_actor, created_at
	FROM job_state`

func scanStateVersion(sc scanner) (StateVersion, error) {
	var (
		v         StateVersion
		value     string
		actor     sql.NullString
		createdAt string
	)
	if err := sc.Scan(&v.JobID, &v.Version, &value, &v.SetByRun, &actor, &createdAt); err != nil {
		return StateVersion{}, err
	}
	v.Value = json.RawMessage(value)
	v.SetByActor = actor.String

	t, err := parseTime(createdAt)
	if err != nil {
		return StateVersion{}, fmt.Errorf("state v%d: %w", v.Version, err)
	}
	v.CreatedAt = t
	return v, nil
}
