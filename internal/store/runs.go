package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jdmorlan/job-engine/internal/model"
)

// Run is a unit of intent, caused by one event, with 1..N attempts (D7).
type Run struct {
	ID                int64
	JobID             int64
	DefinitionHash    string
	TriggeringEventID *int64
	TriggeringRouteID *int64
	RouteHash         string
	Status            model.Status
	QueuedAt          time.Time
	StartedAt         *time.Time
	EndedAt           *time.Time
	AttemptCount      int
	StateVersionIn    *int64
	Output            []byte
	Error             string
}

// Attempt is one execution of a run. It carries its own causation so the
// history can distinguish an automatic retry from a human intervening (D7).
type Attempt struct {
	ID                int64
	RunID             int64
	Number            int
	TriggeringEventID *int64
	Actor             string
	Status            model.Status
	StartedAt         *time.Time
	EndedAt           *time.Time
	ExitCode          *int
	Executor          string
	ContainerID       string
	Error             string
}

// CreateRun inserts a queued run.
func (s *Store) CreateRun(ctx context.Context, r Run) (Run, error) {
	r.QueuedAt = time.Now()
	r.Status = model.StatusQueued
	err := s.state.QueryRowContext(ctx, `
		INSERT INTO runs (job_id, definition_hash, triggering_event_id,
		                  triggering_route_id, route_hash, status, queued_at,
		                  attempt_count, state_version_in)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?)
		RETURNING id`,
		r.JobID, r.DefinitionHash, r.TriggeringEventID, r.TriggeringRouteID,
		nullString(r.RouteHash), string(r.Status), formatTime(r.QueuedAt), r.StateVersionIn,
	).Scan(&r.ID)
	if err != nil {
		return Run{}, fmt.Errorf("creating run: %w", err)
	}
	return r, nil
}

// StartRun marks a run running.
func (s *Store) StartRun(ctx context.Context, runID int64, at time.Time) error {
	_, err := s.state.ExecContext(ctx,
		`UPDATE runs SET status = ?, started_at = ? WHERE id = ?`,
		string(model.StatusRunning), formatTime(at), runID)
	return err
}

// FinishRun records a terminal status and, on success, the structured output.
func (s *Store) FinishRun(ctx context.Context, runID int64, status model.Status, at time.Time, output []byte, runErr string) error {
	if !status.Terminal() {
		return fmt.Errorf("finishing run %d with non-terminal status %q", runID, status)
	}
	var out any
	if len(output) > 0 {
		out = string(output)
	}
	_, err := s.state.ExecContext(ctx, `
		UPDATE runs SET status = ?, ended_at = ?, output = ?, error = ? WHERE id = ?`,
		string(status), formatTime(at), out, nullString(runErr), runID)
	return err
}

// CreateAttempt inserts an attempt and bumps the run's attempt count.
func (s *Store) CreateAttempt(ctx context.Context, a Attempt) (Attempt, error) {
	tx, err := s.state.BeginTx(ctx, nil)
	if err != nil {
		return Attempt{}, err
	}
	defer tx.Rollback()

	// The attempt number is derived inside the transaction rather than passed
	// in, so two callers cannot race to create attempt 2.
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(attempt_number), 0) + 1 FROM attempts WHERE run_id = ?`,
		a.RunID).Scan(&a.Number); err != nil {
		return Attempt{}, err
	}

	started := time.Now()
	a.StartedAt = &started
	a.Status = model.StatusRunning
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO attempts (run_id, attempt_number, triggering_event_id, actor,
		                      status, started_at, executor)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		RETURNING id`,
		a.RunID, a.Number, a.TriggeringEventID, nullString(a.Actor),
		string(a.Status), formatTime(started), nullString(a.Executor),
	).Scan(&a.ID); err != nil {
		return Attempt{}, fmt.Errorf("creating attempt: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE runs SET attempt_count = ? WHERE id = ?`, a.Number, a.RunID); err != nil {
		return Attempt{}, err
	}
	return a, tx.Commit()
}

// FinishAttempt records how one execution ended.
func (s *Store) FinishAttempt(ctx context.Context, id int64, status model.Status, at time.Time, exitCode *int, attemptErr string) error {
	_, err := s.state.ExecContext(ctx, `
		UPDATE attempts SET status = ?, ended_at = ?, exit_code = ?, error = ? WHERE id = ?`,
		string(status), formatTime(at), exitCode, nullString(attemptErr), id)
	return err
}

// RunByID loads one run.
func (s *Store) RunByID(ctx context.Context, id int64) (Run, error) {
	return scanRun(s.state.QueryRowContext(ctx, selectRun+` WHERE id = ?`, id))
}

// RecentRuns lists runs newest first, optionally for one job.
func (s *Store) RecentRuns(ctx context.Context, jobID int64, limit int) ([]Run, error) {
	query := selectRun
	args := []any{}
	if jobID != 0 {
		query += ` WHERE job_id = ?`
		args = append(args, jobID)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.state.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LastSuccessAt reports when a job last succeeded.
//
// D14 serves this to jobs as JE_LAST_SUCCESS_AT. It is deliberately a query
// rather than stored state: it costs nothing, it cannot drift, and keeping it
// unwritable is what stops it being confused with the cursor.
func (s *Store) LastSuccessAt(ctx context.Context, jobID int64) (time.Time, error) {
	var ended sql.NullString
	err := s.state.QueryRowContext(ctx, `
		SELECT ended_at FROM runs
		WHERE job_id = ? AND status = ? AND ended_at IS NOT NULL
		ORDER BY ended_at DESC LIMIT 1`,
		jobID, string(model.StatusSucceeded)).Scan(&ended)
	if err != nil {
		return time.Time{}, err
	}
	if !ended.Valid {
		return time.Time{}, sql.ErrNoRows
	}
	return parseTime(ended.String)
}

// InterruptRunning marks every non-terminal run interrupted.
//
// D5: on startup, anything still "running" is a run we were killed in the
// middle of. It becomes `interrupted`, a distinct state from `failed`, because
// the job did not fail -- we did, and those want different responses.
func (s *Store) InterruptRunning(ctx context.Context, at time.Time) (int64, error) {
	res, err := s.state.ExecContext(ctx, `
		UPDATE runs SET status = ?, ended_at = ?, error = 'engine stopped while this run was in flight'
		WHERE status IN (?, ?)`,
		string(model.StatusInterrupted), formatTime(at),
		string(model.StatusRunning), string(model.StatusQueued))
	if err != nil {
		return 0, err
	}
	if _, err := s.state.ExecContext(ctx, `
		UPDATE attempts SET status = ?, ended_at = ?
		WHERE status IN (?, ?)`,
		string(model.StatusInterrupted), formatTime(at),
		string(model.StatusRunning), string(model.StatusQueued)); err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

const selectRun = `
	SELECT id, job_id, definition_hash, triggering_event_id, triggering_route_id,
	       route_hash, status, queued_at, started_at, ended_at, attempt_count,
	       state_version_in, output, error
	FROM runs`

func scanRun(sc scanner) (Run, error) {
	var (
		r         Run
		status    string
		queuedAt  string
		startedAt sql.NullString
		endedAt   sql.NullString
		routeHash sql.NullString
		output    sql.NullString
		runErr    sql.NullString
	)
	if err := sc.Scan(&r.ID, &r.JobID, &r.DefinitionHash, &r.TriggeringEventID,
		&r.TriggeringRouteID, &routeHash, &status, &queuedAt, &startedAt, &endedAt,
		&r.AttemptCount, &r.StateVersionIn, &output, &runErr); err != nil {
		return Run{}, err
	}
	r.Status = model.Status(status)
	r.RouteHash = routeHash.String
	r.Error = runErr.String
	if output.Valid {
		r.Output = []byte(output.String)
	}

	var err error
	if r.QueuedAt, err = parseTime(queuedAt); err != nil {
		return Run{}, fmt.Errorf("run %d: %w", r.ID, err)
	}
	if r.StartedAt, err = parseNullTime(startedAt); err != nil {
		return Run{}, fmt.Errorf("run %d: %w", r.ID, err)
	}
	if r.EndedAt, err = parseNullTime(endedAt); err != nil {
		return Run{}, fmt.Errorf("run %d: %w", r.ID, err)
	}
	return r, nil
}

func parseNullTime(s sql.NullString) (*time.Time, error) {
	if !s.Valid {
		return nil, nil
	}
	t, err := parseTime(s.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}
