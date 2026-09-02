package engine

import (
	"context"

	"github.com/jdmorlan/job-engine/internal/store"
)

// The read-side of the engine. These exist so the CLI and the API share one
// path to the data, rather than the CLI growing its own queries the moment the
// API is inconvenient (D15).

// Jobs returns every loaded job.
func (e *Engine) Jobs(ctx context.Context) ([]store.Job, error) {
	return e.store.ListJobs(ctx)
}

// Job returns one job by slug.
func (e *Engine) Job(ctx context.Context, slug string) (store.Job, error) {
	return e.store.JobBySlug(ctx, slug)
}

// Runs returns recent runs, newest first. A zero jobID means all jobs.
func (e *Engine) Runs(ctx context.Context, jobID int64, limit int) ([]store.Run, error) {
	if limit <= 0 || limit > 1000 {
		limit = 20
	}
	return e.store.RecentRuns(ctx, jobID, limit)
}

// Run returns one run.
func (e *Engine) Run(ctx context.Context, id int64) (store.Run, error) {
	return e.store.RunByID(ctx, id)
}

// Logs returns the captured output of one attempt.
func (e *Engine) Logs(ctx context.Context, runID int64, attempt int) ([]store.LogLine, error) {
	return e.store.ReadLogs(ctx, runID, attempt)
}

// CurrentState returns a job's cursor. sql.ErrNoRows means it has never run.
func (e *Engine) CurrentState(ctx context.Context, jobID int64) (store.StateVersion, error) {
	return e.store.CurrentState(ctx, jobID)
}

// StateHistory returns the cursor's movement over time.
//
// D14 insists this is part of the feature rather than a follow-up: a cursor
// that moves silently is the dragon. "The cursor stopped moving on Tuesday even
// though runs kept succeeding" is a bug class that is invisible everywhere else
// and obvious here.
func (e *Engine) StateHistory(ctx context.Context, jobID int64, limit int) ([]store.StateVersion, error) {
	if limit <= 0 || limit > 1000 {
		limit = 20
	}
	return e.store.StateHistory(ctx, jobID, limit)
}
