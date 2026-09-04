package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jdmorlan/job-engine/internal/jobdef"

	"github.com/jdmorlan/job-engine/internal/model"
	"github.com/jdmorlan/job-engine/internal/store"
)

// EventRetentionSwept records one pass of the sweep (D13).
//
// The counts are on the event because deletion is the one operation that
// erases its own evidence: after a sweep, thirty days of history for a job that
// ran daily for a year is indistinguishable from a job that started thirty days
// ago. What was removed has to be written down as it goes, or the question
// "was there more?" has no answer anywhere (P1).
const EventRetentionSwept = "retention.swept"

// Sweep is what `je retention sweep` asks the control plane to do.
//
// It runs here rather than in the CLI for the reason C1 gives: the worker
// executing the job has no access to the store, and should not. The job is a
// client making one request, exactly as a person would.
type Sweep struct {
	// Removed is what went, counted by kind.
	Removed store.Removed `json:"removed"`

	// Space is what the logs database gave back to the disk.
	Space store.LogSpace `json:"space"`

	// Policy is what was in force, echoed back so the job's own log records
	// the periods it ran under rather than only their effect.
	Policy Policy `json:"policy"`
}

// Policy is how long each kind of history is kept (D13).
//
// It arrives with the request rather than being read from a config file or a
// settings table, because it lives in the argv of the `system/retention` job.
// That is the whole reason the job framing was worth the trouble: retention is
// the first setting that belongs to a deployment rather than to a machine or to
// somebody's repository, and a job file is a place that already exists, that
// `je explain` already renders, and that already has somewhere to put a
// validation rule.
type Policy struct {
	Runs   time.Duration `json:"runs"`
	Logs   time.Duration `json:"logs"`
	Events time.Duration `json:"events"`

	// MaxRuns caps how much one sweep removes. Zero means the default.
	//
	// A work budget rather than a keep period, and it is here rather than
	// hidden in the store because it is a real choice: a sweep is a job with a
	// timeout, deleting a year of history in one transaction is how
	// housekeeping becomes an outage, and somebody draining a backlog on
	// purpose should be able to say so.
	MaxRuns int `json:"max_runs,omitempty"`
}

// DefaultPolicy is D13's answer, in your words: "records could be 30 days...
// logs could be 30 days as well."
var DefaultPolicy = Policy{Runs: 30 * Day, Logs: 30 * Day, Events: 30 * Day}

// Day is 24 hours exactly.
//
// Retention measures an age rather than a calendar, so a day here is a fixed
// span and never a local one. Deliberately not added to jobdef's Duration,
// where `every: 1d` would look like a schedule and mean 24 hours -- which is
// the same thing except across a daylight-saving boundary, and "the same thing
// except twice a year" is the sort of quiet wrongness P1 rules out.
const Day = 24 * time.Hour

// Validate enforces D13's one coupling rule.
func (p Policy) Validate() error {
	switch {
	case p.Runs <= 0 || p.Logs <= 0 || p.Events <= 0:
		return fmt.Errorf("%w: every period must be positive", ErrBadPolicy)
	case p.Events < p.Runs:
		// The causation chain reads through events (D12), so events expiring
		// first would break "why did this run?" for runs still in the history.
		// A rule rather than a footgun, exactly as D13 asked.
		return fmt.Errorf(
			"%w: events are kept for %s and runs for %s: events may never be kept "+
				"for less time than runs, or `je why` breaks for runs that are "+
				"still listed", ErrBadPolicy, p.Events, p.Runs)
	}
	return nil
}

// Sweep removes history past its keep period and returns the space (D13).
//
// Deleting rows is not yet here; this pass reclaims what previous deletions
// already freed, which on a database that predates D13 also means converting it
// so that reclaiming is possible at all. That ordering is deliberate: the space
// half is the half that silently does nothing when it is wrong, so it is worth
// having working and visible before anything starts removing rows.
func (e *Engine) Sweep(ctx context.Context, policy Policy, actor string) (Sweep, error) {
	if err := policy.Validate(); err != nil {
		return Sweep{}, err
	}
	now := e.now()

	// Logs first, and by run rather than by timestamp, so that a job asking to
	// keep its output can be exempted (D13). The same exemption keeps those
	// jobs' *runs*: logs are addressed by run id, so a kept log whose run has
	// gone is bytes nobody can reach. The ids come from the state
	// database and the rows live in the other one, which is why the exemption
	// is resolved here: only the engine can read definitions.
	keep, err := e.jobsKeepingLogsForever(ctx)
	if err != nil {
		return Sweep{}, err
	}
	lines, err := e.store.SweepLogs(ctx, now.Add(-policy.Logs), keep, policy.MaxRuns)
	if err != nil {
		return Sweep{}, err
	}

	removed, err := e.store.SweepRuns(ctx, store.RetentionPolicy{
		Runs: policy.Runs, Logs: policy.Logs, Events: policy.Events,
		MaxRuns: policy.MaxRuns,
	}, keep, now)
	if err != nil {
		return Sweep{}, err
	}
	removed.LogLines = lines

	// Events last. A run that has just gone releases the events that described
	// it, so sweeping them after costs a pass and saves a day of waiting.
	events, err := e.store.SweepEvents(ctx, now.Add(-policy.Events), policy.MaxRuns)
	if err != nil {
		return Sweep{}, err
	}
	removed.Events += events

	// And only then the space, because reclaiming before deleting would return
	// whatever the last sweep freed and none of this one's.
	space, err := e.store.ReclaimLogSpace(ctx, 0)
	if err != nil {
		return Sweep{}, err
	}
	out := Sweep{Removed: removed, Space: space, Policy: policy}

	payload, _ := json.Marshal(map[string]any{
		"runs":         removed.Runs,
		"attempts":     removed.Attempts,
		"log_lines":    removed.LogLines,
		"events":       removed.Events,
		"runs_left":    removed.RunsLeft,
		"pinned":       removed.Pinned,
		"converted":    space.Converted,
		"bytes_freed":  space.Reclaimed(),
		"logs_db_size": space.BytesAfter,
	})
	if _, _, err := e.publish(ctx, model.Event{
		Type:      EventRetentionSwept,
		Source:    model.SourceEngine,
		Payload:   payload,
		Actor:     actor,
		CreatedAt: e.now(),
	}); err != nil {
		return out, fmt.Errorf("recording %s: %w", EventRetentionSwept, err)
	}
	e.log.Info("retention swept",
		"runs", removed.Runs, "events", removed.Events, "log_lines", removed.LogLines,
		"bytes_freed", space.Reclaimed(), "runs_left", removed.RunsLeft)
	return out, nil
}

// jobsKeepingLogsForever resolves D13's escape hatch to job ids.
//
// Read from the loaded definitions rather than stored on the job row, for the
// reason every other derived fact here is: a cached copy of what a file says is
// a thing that can disagree with the file.
func (e *Engine) jobsKeepingLogsForever(ctx context.Context) ([]int64, error) {
	jobs, err := e.store.ListJobs(ctx)
	if err != nil {
		return nil, err
	}
	var keep []int64
	for _, j := range jobs {
		def, err := jobdef.FromSnapshot(j.Definition)
		if err != nil {
			// A job whose snapshot will not parse is already visible as broken
			// (D10). Keeping its logs is the conservative reading of an
			// unreadable definition: retention should not delete on the
			// strength of a file it could not understand.
			keep = append(keep, j.ID)
			continue
		}
		if def.KeepLogs.Forever() {
			keep = append(keep, j.ID)
		}
	}
	return keep, nil
}

// ErrBadPolicy marks a retention policy this deployment will not accept, so a
// caller can tell "you asked for something incoherent" from "the sweep failed".
var ErrBadPolicy = errors.New("invalid retention policy")

// ParsePeriod reads a retention period: 30d, 12h, 90m.
//
// Its own parser rather than jobdef's Duration, for one reason: days. A
// retention period is naturally written in days and a job timeout never is, and
// teaching the shared type about `d` would quietly make `every: 1d` legal in a
// schedule -- where it would mean exactly 24 hours, which is what people expect
// except across a daylight-saving boundary.
func ParsePeriod(s string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("%q is not a period; use forms like 30d, 12h", s)
		}
		return time.Duration(n) * Day, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%q is not a period; use forms like 30d, 12h", s)
	}
	return d, nil
}
