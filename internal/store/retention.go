package store

import (
	"context"
	"fmt"
	"time"
)

// D13's deletion half.
//
// Three rules shape every query below, and none of them is about SQL:
//
// A run that anything still refers to is kept, whatever its age. job_state
// points at the run that moved a cursor and job state never expires, so a job
// that ran once forty days ago and has not moved since pins that run -- and
// "what set this cursor?" is the question D14 exists to answer. Events point at
// the runs they describe, and events are kept at least as long as runs, so the
// timeline sets the floor rather than the run table. Foreign keys are on, so
// the alternative to obeying this is not a dangling row but a failed sweep.
//
// Everything is capped. A sweep is a job with a timeout like any other, and
// deleting a year of history in one transaction is how you turn housekeeping
// into an outage. What it could not finish it reports, and the next pass
// continues.
//
// And what goes is counted, per job, before it goes. Deletion is the one
// operation that erases its own evidence: thirty days of history for a job that
// ran daily for a year is indistinguishable from a job that started thirty days
// ago, and the rows that would say otherwise are exactly the ones being
// removed (P1).

// RetentionPolicy is how long each kind of history is kept (D13).
type RetentionPolicy struct {
	Runs   time.Duration
	Logs   time.Duration
	Events time.Duration

	// MaxRuns caps one sweep. Zero means the default.
	MaxRuns int
}

// Removed is what one sweep took out.
type Removed struct {
	Runs     int64 `json:"runs"`
	Attempts int64 `json:"attempts"`
	LogLines int64 `json:"log_lines"`
	Events   int64 `json:"events"`

	// StateVersions is cursor history trimmed, and Triggers is spent fan-in
	// state. Neither is bytes anybody notices; both are unbounded without
	// this, which is the only reason they are here.
	StateVersions int64 `json:"state_versions"`
	Triggers      int64 `json:"triggers"`

	// RunsLeft is what the cap stopped this pass from reaching. Reported
	// rather than hidden, because a sweep that quietly does a tenth of the
	// work looks identical to one that had nothing to do.
	RunsLeft int64 `json:"runs_left"`

	// Pinned is runs old enough to remove that something still refers to.
	// Not a problem and not an error -- but the difference between "there was
	// nothing to delete" and "there was, and it is still needed" is worth
	// being able to see.
	Pinned int64 `json:"pinned"`
}

// Any reports whether the sweep removed anything at all.
func (r Removed) Any() bool {
	return r.Runs+r.Attempts+r.LogLines+r.Events+r.StateVersions+r.Triggers > 0
}

// defaultMaxRuns is how many runs one sweep will remove.
//
// Sized so the transaction stays short on a laptop rather than to finish in one
// pass: retention runs daily and a backlog drains over a few days, which is the
// right trade for a database the rest of the engine is writing to.
const defaultMaxRuns = 5000

// SweepLogs deletes the captured output of runs past the log keep period.
//
// Separate from run removal because the periods are separate: logs are the bulk
// of the bytes and may be dropped while the run they belong to is still in the
// history. keepForever is the job ids whose definitions asked to be exempt
// (D13's escape hatch), which the caller resolves because only it can read
// definitions.
func (s *Store) SweepLogs(ctx context.Context, before time.Time, keepForever []int64, limit int) (int64, error) {
	if limit <= 0 {
		limit = defaultMaxRuns
	}
	// The run ids come from the state database and the rows from the logs
	// database, which is why this is two queries and not a join: D4 put them
	// in separate files so that vacuuming one cannot block the other.
	rows, err := s.state.QueryContext(ctx, `
		SELECT id FROM runs
		WHERE ended_at IS NOT NULL AND ended_at < ?
		  AND job_id NOT IN (`+placeholders(len(keepForever))+`)
		ORDER BY ended_at
		LIMIT ?`,
		append(append([]any{formatTime(before)}, ids(keepForever)...), limit)...)
	if err != nil {
		return 0, fmt.Errorf("finding runs whose logs have aged out: %w", err)
	}
	defer rows.Close()

	var runIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, err
		}
		runIDs = append(runIDs, id)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(runIDs) == 0 {
		return 0, nil
	}

	var deleted int64
	// In batches, because SQLite has a limit on how many parameters one
	// statement may carry and a year of runs is well past it.
	for _, chunk := range batches(runIDs, 500) {
		res, err := s.logs.ExecContext(ctx,
			`DELETE FROM logs WHERE run_id IN (`+placeholders(len(chunk))+`)`, ids(chunk)...)
		if err != nil {
			return deleted, fmt.Errorf("deleting logs: %w", err)
		}
		n, _ := res.RowsAffected()
		deleted += n
	}
	return deleted, nil
}

// SweepRuns removes runs past the keep period, with their attempts.
//
// The counts land on the jobs the runs belonged to before the rows go, so that
// `je runs` can say how much history is missing rather than presenting a
// truncated list as the whole story.
func (s *Store) SweepRuns(ctx context.Context, policy RetentionPolicy, keepForever []int64, now time.Time) (Removed, error) {
	limit := policy.MaxRuns
	if limit <= 0 {
		limit = defaultMaxRuns
	}
	runCutoff := formatTime(now.Add(-policy.Runs))
	eventCutoff := formatTime(now.Add(-policy.Events))

	var out Removed
	tx, err := s.state.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()

	// Runs old enough to go, that nothing still points at.
	//
	// The two NOT EXISTS clauses are the rules at the top of this file, in the
	// order they matter: a cursor's provenance, then the timeline. An event
	// surviving its own cutoff keeps its run alive, which is what makes
	// "events are never kept for less time than runs" true in practice rather
	// than only in the config validation.
	// The third exclusion is D13's escape hatch, which has to reach further
	// than its name suggests: a job whose logs are kept forever keeps its runs
	// too. `je logs` is addressed by run id, so deleting the run leaves bytes
	// on the disk that nothing can reach -- which is the worst of both, and was
	// exactly what the first version of this did.
	keepClause := ""
	args := []any{runCutoff, eventCutoff}
	if len(keepForever) > 0 {
		keepClause = ` AND r.job_id NOT IN (` + placeholders(len(keepForever)) + `)`
		args = append(args, ids(keepForever)...)
	}
	args = append(args, limit)

	rows, err := tx.QueryContext(ctx, `
		SELECT r.id, r.job_id FROM runs r
		WHERE r.ended_at IS NOT NULL AND r.ended_at < ?
		  AND NOT EXISTS (SELECT 1 FROM job_state js WHERE js.set_by_run_id = r.id)
		  AND NOT EXISTS (
		        SELECT 1 FROM events e
		        WHERE e.caused_by_run_id = r.id AND e.created_at >= ?)`+keepClause+`
		ORDER BY r.ended_at
		LIMIT ?`, args...)
	if err != nil {
		return out, fmt.Errorf("finding runs past the keep period: %w", err)
	}
	var (
		runIDs []int64
		perJob = map[int64]int64{}
	)
	for rows.Next() {
		var id, jobID int64
		if err := rows.Scan(&id, &jobID); err != nil {
			rows.Close()
			return out, err
		}
		runIDs = append(runIDs, id)
		perJob[jobID]++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}

	if len(runIDs) > 0 {
		for _, chunk := range batches(runIDs, 500) {
			in := placeholders(len(chunk))
			// Attempts first: they are the children, and foreign keys are on.
			res, err := tx.ExecContext(ctx,
				`DELETE FROM attempts WHERE run_id IN (`+in+`)`, ids(chunk)...)
			if err != nil {
				return out, fmt.Errorf("deleting attempts: %w", err)
			}
			n, _ := res.RowsAffected()
			out.Attempts += n

			// Then the events that only exist to describe these runs. They are
			// within the deletion set by construction: a run is only in this
			// list because every event pointing at it is older than the event
			// cutoff.
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM events WHERE caused_by_run_id IN (`+in+`)`, ids(chunk)...); err != nil {
				return out, fmt.Errorf("deleting events for removed runs: %w", err)
			}

			res, err = tx.ExecContext(ctx, `DELETE FROM runs WHERE id IN (`+in+`)`, ids(chunk)...)
			if err != nil {
				return out, fmt.Errorf("deleting runs: %w", err)
			}
			n, _ = res.RowsAffected()
			out.Runs += n
		}

		for jobID, n := range perJob {
			if _, err := tx.ExecContext(ctx,
				`UPDATE jobs SET runs_removed = runs_removed + ? WHERE id = ?`,
				n, jobID); err != nil {
				return out, fmt.Errorf("recording what retention removed: %w", err)
			}
		}
	}

	// What the cap left, and what is old enough to go but still referenced.
	// Both are answers to "why is there still history here?", which is a
	// question a person asks of a sweep that appears not to have worked.
	tail := append([]any{runCutoff, eventCutoff}, ids(keepForever)...)
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM runs r
		WHERE r.ended_at IS NOT NULL AND r.ended_at < ?
		  AND NOT EXISTS (SELECT 1 FROM job_state js WHERE js.set_by_run_id = r.id)
		  AND NOT EXISTS (
		        SELECT 1 FROM events e
		        WHERE e.caused_by_run_id = r.id AND e.created_at >= ?)`+keepClause,
		tail...).Scan(&out.RunsLeft); err != nil {
		return out, err
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM runs r
		WHERE r.ended_at IS NOT NULL AND r.ended_at < ?
		  AND (EXISTS (SELECT 1 FROM job_state js WHERE js.set_by_run_id = r.id)
		       OR EXISTS (
		            SELECT 1 FROM events e
		            WHERE e.caused_by_run_id = r.id AND e.created_at >= ?))`,
		runCutoff, eventCutoff).Scan(&out.Pinned); err != nil {
		return out, err
	}

	return out, tx.Commit()
}

// SweepEvents removes events past the keep period.
//
// An event a surviving run points at is kept, however old: a run whose
// triggering event had been deleted could not answer "why did this run?", which
// is the question D12's causation chain exists for and the reason D13 will not
// let events expire before runs.
func (s *Store) SweepEvents(ctx context.Context, before time.Time, limit int) (int64, error) {
	if limit <= 0 {
		limit = defaultMaxRuns
	}
	res, err := s.state.ExecContext(ctx, `
		DELETE FROM events WHERE id IN (
			SELECT e.id FROM events e
			WHERE e.created_at < ?
			  AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.triggering_event_id = e.id)
			  AND NOT EXISTS (SELECT 1 FROM events c WHERE c.caused_by_event_id = e.id)
			  AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.id = e.caused_by_run_id)
			ORDER BY e.created_at
			LIMIT ?)`, formatTime(before), limit)
	if err != nil {
		return 0, fmt.Errorf("deleting events: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// placeholders renders "?,?,?" for an IN clause, or a form that matches nothing
// when the list is empty.
func placeholders(n int) string {
	if n == 0 {
		return "SELECT NULL WHERE 0"
	}
	out := make([]byte, 0, n*2)
	for i := 0; i < n; i++ {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, '?')
	}
	return string(out)
}

func ids(in []int64) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

// batches splits a list into chunks small enough for one statement's parameter
// limit.
func batches(in []int64, size int) [][]int64 {
	var out [][]int64
	for len(in) > size {
		out = append(out, in[:size])
		in = in[size:]
	}
	if len(in) > 0 {
		out = append(out, in)
	}
	return out
}

// StateHistoryVersions is how many versions of a job's cursor are kept (D13).
//
// The current cursor is never expired at any age: it is tiny, and losing it
// means reprocessing from the beginning. What trims is the history behind it,
// which exists to answer "when did this stop moving?" -- a question a hundred
// versions answers as well as a thousand.
const StateHistoryVersions = 100

// SweepState trims each job's cursor history, keeping the newest versions.
//
// Bounded by count rather than by age, unlike everything else here, and that is
// D13's decision rather than an inconsistency: a job that runs hourly and one
// that runs yearly have wildly different histories in thirty days, and neither
// of them wants the *current* value to be the thing that ages out.
func (s *Store) SweepState(ctx context.Context, keep int) (int64, error) {
	if keep <= 0 {
		keep = StateHistoryVersions
	}
	// The subquery is per job, and the comparison is on version rather than on
	// time: versions are dense and monotonic per job, so "everything below the
	// keep-th newest" needs no window function and no second pass.
	res, err := s.state.ExecContext(ctx, `
		DELETE FROM job_state
		WHERE (job_id, version) IN (
			SELECT js.job_id, js.version FROM job_state js
			WHERE js.version <= (
				SELECT MAX(version) FROM job_state m WHERE m.job_id = js.job_id
			) - ?
		)`, keep)
	if err != nil {
		return 0, fmt.Errorf("trimming cursor history: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// SweepTriggers removes fan-in state that can no longer do anything (D3).
//
// Not in D13, which predates fan-in: trigger_state grows by a row per route per
// correlation key and nothing ever removed one, so a chain that fans in daily
// outgrows the run history it is meant to be smaller than.
//
// Only rows that have fired, or whose window closed before the cutoff. A
// partly-satisfied trigger still inside its window is live work -- it is what
// `je waiting` is showing somebody -- and the cutoff is thirty days against
// windows measured in hours, so the distinction costs nothing and removing it
// would silently drop a pending fan-in.
func (s *Store) SweepTriggers(ctx context.Context, before time.Time) (int64, error) {
	cutoff := formatTime(before)
	res, err := s.state.ExecContext(ctx, `
		DELETE FROM trigger_state
		WHERE window_started_at < ?
		  AND (fired_at IS NOT NULL OR (expires_at IS NOT NULL AND expires_at < ?))`,
		cutoff, cutoff)
	if err != nil {
		return 0, fmt.Errorf("removing spent triggers: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
