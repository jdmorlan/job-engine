package engine

import (
	"context"

	"github.com/jdmorlan/job-engine/internal/jobdef"
	"github.com/jdmorlan/job-engine/internal/model"
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

// RunDetail is everything worth knowing about one run.
//
// It exists so the daemon path and the in-process path can print the same
// summary. Without it `je run` against a daemon would show less than `je run`
// without one, and two renderings of the same event is how a tool starts
// feeling untrustworthy.
//
// It is also the shape D12's run-detail view wants, and what `je why` will
// build on: the cursor before and after, what the run emitted, and every
// attempt with its own causation.
type RunDetail struct {
	Run      store.Run           `json:"run"`
	JobSlug  string              `json:"job"`
	Attempts []store.Attempt     `json:"attempts"`
	StateIn  *store.StateVersion `json:"state_in,omitempty"`
	StateOut *store.StateVersion `json:"state_out,omitempty"`
	Emitted  []model.Event       `json:"emitted,omitempty"`

	// PrimaryCursor is the key the definition nominated for display (D14).
	PrimaryCursor string `json:"primary_cursor,omitempty"`
}

// RunDetail assembles the full picture of a run.
func (e *Engine) RunDetail(ctx context.Context, id int64) (RunDetail, error) {
	run, err := e.store.RunByID(ctx, id)
	if err != nil {
		return RunDetail{}, err
	}

	detail := RunDetail{Run: run}

	job, err := e.store.JobByID(ctx, run.JobID)
	if err != nil {
		return RunDetail{}, err
	}
	detail.JobSlug = job.Slug
	if def, err := jobdef.FromSnapshot(job.Definition); err == nil {
		detail.PrimaryCursor = def.State.PrimaryCursor
	}

	if detail.Attempts, err = e.store.AttemptsForRun(ctx, id); err != nil {
		return RunDetail{}, err
	}
	if detail.Emitted, err = e.store.EventsCausedByRun(ctx, id); err != nil {
		return RunDetail{}, err
	}

	// The cursor this run started from, and the one it committed if it did.
	// Read by version rather than "current", because by the time anyone looks
	// at an old run the current cursor has moved on -- and showing today's
	// value next to a week-old run is worse than showing none.
	if run.StateVersionIn != nil {
		if in, err := e.store.StateAtVersion(ctx, run.JobID, *run.StateVersionIn); err == nil {
			detail.StateIn = &in
		}
		if out, err := e.store.StateAtVersion(ctx, run.JobID, *run.StateVersionIn+1); err == nil &&
			out.SetByRun != nil && *out.SetByRun == id {
			detail.StateOut = &out
		}
	}
	return detail, nil
}
