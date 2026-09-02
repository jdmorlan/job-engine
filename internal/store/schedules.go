package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jdmorlan/job-engine/internal/jobdef"
	"github.com/jdmorlan/job-engine/internal/model"
)

// LastWindow returns when a schedule last fired, or sql.ErrNoRows if the
// engine has never seen it.
//
// "Never seen it" is a meaningful state rather than an error: a schedule
// encountered for the first time must not backfill its entire history, the
// same way a cursor is seeded rather than started from the epoch (D14).
func (s *Store) LastWindow(ctx context.Context, jobID int64, index int) (time.Time, error) {
	var raw string
	err := s.state.QueryRowContext(ctx,
		`SELECT last_window_at FROM schedule_state WHERE job_id = ? AND schedule_index = ?`,
		jobID, index).Scan(&raw)
	if err != nil {
		return time.Time{}, err
	}
	return parseTime(raw)
}

// SetLastWindow advances a schedule's position on its grid.
func (s *Store) SetLastWindow(ctx context.Context, jobID int64, index int, window time.Time) error {
	_, err := s.state.ExecContext(ctx, `
		INSERT INTO schedule_state (job_id, schedule_index, last_window_at, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (job_id, schedule_index) DO UPDATE SET
			last_window_at = excluded.last_window_at,
			updated_at     = excluded.updated_at`,
		jobID, index, formatTime(window), formatTime(time.Now()))
	if err != nil {
		return fmt.Errorf("recording schedule window: %w", err)
	}
	return nil
}

// ActiveRunCount reports how many runs are queued or running, for the global
// concurrency cap (D8).
func (s *Store) ActiveRunCount(ctx context.Context) (int, error) {
	var n int
	err := s.state.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM runs WHERE status = ?`, string(model.StatusRunning)).Scan(&n)
	return n, err
}

// JobHasActiveRun reports whether a job is already running or waiting to, which
// is what the overlap policy keys off (D8).
func (s *Store) JobHasActiveRun(ctx context.Context, jobID int64) (bool, error) {
	var n int
	err := s.state.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM runs WHERE job_id = ? AND status IN (?, ?)`,
		jobID, string(model.StatusQueued), string(model.StatusRunning)).Scan(&n)
	return n > 0, err
}

// ClaimNextQueuedRun atomically takes the oldest queued run and marks it
// running, returning sql.ErrNoRows when the queue is empty.
//
// Atomic because several workers pull from the same queue, and two workers
// executing one run would double-fire the job -- the same failure the data
// directory lock prevents between processes, one level down.
func (s *Store) ClaimNextQueuedRun(ctx context.Context, at time.Time) (Run, error) {
	tx, err := s.state.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()

	var id int64
	// Oldest first, so the queue is fair and a job that has been waiting does
	// not starve behind newer arrivals.
	//
	// The NOT EXISTS clause is what makes `overlap: queue` mean what it says:
	// a second run of the same job waits for the first to finish rather than
	// running beside it. Without it the worker pool would happily claim both,
	// and a queued backlog would execute concurrently -- which is the one
	// thing the author ruled out by not choosing `allow`.
	err = tx.QueryRowContext(ctx, `
		SELECT r.id FROM runs r
		WHERE r.status = ?
		  AND (r.overlap = ? OR NOT EXISTS (
		        SELECT 1 FROM runs o WHERE o.job_id = r.job_id AND o.status = ?))
		ORDER BY r.queued_at, r.id LIMIT 1`,
		string(model.StatusQueued), string(jobdef.OverlapAllow), string(model.StatusRunning),
	).Scan(&id)
	if err != nil {
		return Run{}, err
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE runs SET status = ?, started_at = ? WHERE id = ?`,
		string(model.StatusRunning), formatTime(at), id); err != nil {
		return Run{}, err
	}

	run, err := scanRun(tx.QueryRowContext(ctx, selectRun+` WHERE id = ?`, id))
	if err != nil {
		return Run{}, err
	}
	return run, tx.Commit()
}

// QueuedRuns lists runs waiting for a worker, oldest first. This is what
// `je waiting` renders (P1).
func (s *Store) QueuedRuns(ctx context.Context) ([]Run, error) {
	rows, err := s.state.QueryContext(ctx,
		selectRun+` WHERE status = ? ORDER BY queued_at, id`, string(model.StatusQueued))
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
