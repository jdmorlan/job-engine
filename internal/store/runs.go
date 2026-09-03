package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jdmorlan/job-engine/internal/model"
)

// Run is a unit of intent, caused by one event, with 1..N attempts (D7).
type Run struct {
	ID                int64  `json:"id"`
	JobID             int64  `json:"job_id"`
	DefinitionHash    string `json:"definition_hash"`
	TriggeringEventID *int64 `json:"triggering_event_id,omitempty"`
	TriggeringRouteID *int64 `json:"triggering_route_id,omitempty"`

	// SourceRevision is the commit this run's code came from, for a job whose
	// source is fetched (D22). Empty for a job on local disk, which has no
	// revision to record.
	SourceRevision string          `json:"source_revision,omitempty"`
	RouteHash      string          `json:"route_hash,omitempty"`
	Status         model.Status    `json:"status"`
	QueuedAt       time.Time       `json:"queued_at"`
	StartedAt      *time.Time      `json:"started_at,omitempty"`
	EndedAt        *time.Time      `json:"ended_at,omitempty"`
	AttemptCount   int             `json:"attempt_count"`
	StateVersionIn *int64          `json:"state_version_in,omitempty"`
	Output         json.RawMessage `json:"output,omitempty"`
	Error          string          `json:"error,omitempty"`

	// Overlap is the policy in force when this run was queued (D8).
	Overlap string `json:"overlap"`

	// RunsOn is the capability label a worker must advertise to take this run
	// (D20/C3). Snapshotted at enqueue time for the same reason Overlap is: a
	// definition reloaded between enqueue and dispatch must not move a run
	// that is already waiting.
	RunsOn string `json:"runs_on"`

	// WorkerID is who holds the lease, and LeaseExpiresAt is until when (C5).
	// Both are nil for a queued run.
	WorkerID       *string    `json:"worker_id,omitempty"`
	LeaseExpiresAt *time.Time `json:"lease_expires_at,omitempty"`
}

// Attempt is one execution of a run. It carries its own causation so the
// history can distinguish an automatic retry from a human intervening (D7).
type Attempt struct {
	ID                int64        `json:"id"`
	RunID             int64        `json:"run_id"`
	Number            int          `json:"number"`
	TriggeringEventID *int64       `json:"triggering_event_id,omitempty"`
	Actor             string       `json:"actor,omitempty"`
	Status            model.Status `json:"status"`
	StartedAt         *time.Time   `json:"started_at,omitempty"`
	EndedAt           *time.Time   `json:"ended_at,omitempty"`
	ExitCode          *int         `json:"exit_code,omitempty"`
	Executor          string       `json:"executor,omitempty"`
	ContainerID       string       `json:"container_id,omitempty"`
	Error             string       `json:"error,omitempty"`
}

// CreateRun inserts a queued run.
func (s *Store) CreateRun(ctx context.Context, r Run) (Run, error) {
	r.QueuedAt = time.Now()
	r.Status = model.StatusQueued
	err := s.state.QueryRowContext(ctx, `
		INSERT INTO runs (job_id, definition_hash, triggering_event_id,
		                  triggering_route_id, route_hash, source_revision,
		                  status, queued_at, attempt_count, state_version_in,
		                  overlap, runs_on)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?)
		RETURNING id`,
		r.JobID, r.DefinitionHash, r.TriggeringEventID, r.TriggeringRouteID,
		nullString(r.RouteHash), nullString(r.SourceRevision),
		string(r.Status), formatTime(r.QueuedAt), r.StateVersionIn,
		r.Overlap, r.RunsOn,
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

// AttemptsForRun lists a run's attempts in order.
//
// D7 makes this worth showing: the attempt list is where "did a human have to
// intervene?" is answerable, because attempt 3 says it was a manual retry
// while 1 and 2 say they were automatic.
func (s *Store) AttemptsForRun(ctx context.Context, runID int64) ([]Attempt, error) {
	rows, err := s.state.QueryContext(ctx, `
		SELECT id, run_id, attempt_number, triggering_event_id, actor, status,
		       started_at, ended_at, exit_code, executor, container_id, error
		FROM attempts WHERE run_id = ? ORDER BY attempt_number`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Attempt
	for rows.Next() {
		var (
			a           Attempt
			actor       sql.NullString
			status      string
			startedAt   sql.NullString
			endedAt     sql.NullString
			executor    sql.NullString
			containerID sql.NullString
			attemptErr  sql.NullString
		)
		if err := rows.Scan(&a.ID, &a.RunID, &a.Number, &a.TriggeringEventID, &actor,
			&status, &startedAt, &endedAt, &a.ExitCode, &executor, &containerID, &attemptErr); err != nil {
			return nil, err
		}
		a.Actor, a.Status = actor.String, model.Status(status)
		a.Executor, a.ContainerID, a.Error = executor.String, containerID.String, attemptErr.String

		var err error
		if a.StartedAt, err = parseNullTime(startedAt); err != nil {
			return nil, err
		}
		if a.EndedAt, err = parseNullTime(endedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
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

// RecentRunsWithStatus lists runs in one state, newest first.
func (s *Store) RecentRunsWithStatus(ctx context.Context, status string, limit int) ([]Run, error) {
	rows, err := s.state.QueryContext(ctx,
		selectRun+` WHERE status = ? ORDER BY id DESC LIMIT ?`, status, limit)
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

// LatestRunByRoute returns the most recent run a rule started.
//
// It is how a chain view finds its way in: chains are not runtime entities
// (D17), so there is no chain instance to look up -- the newest run of the
// first step is the closest thing to "the last time this flow happened", and
// everything else is reachable from it by causation.
func (s *Store) LatestRunByRoute(ctx context.Context, routeID int64) (Run, error) {
	return scanRun(s.state.QueryRowContext(ctx, selectRun+`
		WHERE triggering_route_id = ? ORDER BY id DESC LIMIT 1`, routeID))
}

// RunTriggeredBy returns the run that one run caused through one rule.
//
// This is the join that makes end-to-end anything expressible: a run points at
// the event that caused it, and that event points at the run that emitted it,
// so the whole flow is one hop at a time through facts that were recorded for
// their own reasons rather than for this query.
func (s *Store) RunTriggeredBy(ctx context.Context, causeRunID, routeID int64) (Run, error) {
	return scanRun(s.state.QueryRowContext(ctx, `
		SELECT r.id, r.job_id, r.definition_hash, r.triggering_event_id,
		       r.triggering_route_id, r.route_hash, r.source_revision, r.status,
		       r.queued_at, r.started_at, r.ended_at, r.attempt_count,
		       r.state_version_in, r.output, r.error, r.overlap, r.runs_on,
		       r.worker_id, r.lease_expires_at
		FROM runs r
		JOIN events e ON e.id = r.triggering_event_id
		WHERE e.caused_by_run_id = ? AND r.triggering_route_id = ?
		ORDER BY r.id DESC LIMIT 1`, causeRunID, routeID))
}

const selectRun = `
	SELECT id, job_id, definition_hash, triggering_event_id, triggering_route_id,
	       route_hash, source_revision, status, queued_at, started_at, ended_at,
	       attempt_count, state_version_in, output, error, overlap, runs_on,
	       worker_id, lease_expires_at
	FROM runs`

func scanRun(sc scanner) (Run, error) {
	var (
		r         Run
		status    string
		queuedAt  string
		startedAt sql.NullString
		endedAt   sql.NullString
		routeHash sql.NullString
		revision  sql.NullString
		output    sql.NullString
		runErr    sql.NullString
		workerID  sql.NullString
		leaseEnds sql.NullString
	)
	if err := sc.Scan(&r.ID, &r.JobID, &r.DefinitionHash, &r.TriggeringEventID,
		&r.TriggeringRouteID, &routeHash, &revision, &status, &queuedAt, &startedAt,
		&endedAt, &r.AttemptCount, &r.StateVersionIn, &output, &runErr, &r.Overlap,
		&r.RunsOn, &workerID, &leaseEnds); err != nil {
		return Run{}, err
	}
	r.SourceRevision = revision.String
	if workerID.Valid {
		r.WorkerID = &workerID.String
	}
	r.Status = model.Status(status)
	r.RouteHash = routeHash.String
	r.Error = runErr.String
	if output.Valid {
		r.Output = json.RawMessage(output.String)
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
	if r.LeaseExpiresAt, err = parseNullTime(leaseEnds); err != nil {
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
