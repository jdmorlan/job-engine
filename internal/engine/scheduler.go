package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jdmorlan/job-engine/internal/jobdef"
	"github.com/jdmorlan/job-engine/internal/model"
	"github.com/jdmorlan/job-engine/internal/schedule"
	"github.com/jdmorlan/job-engine/internal/store"
)

// tickInterval is how often the scheduler looks for due windows.
//
// A second, because the shortest interval a job may declare is a second. The
// tick itself touches no storage: due windows are computed against an
// in-memory copy of each schedule's last position, and the database is only
// written when something actually fires.
const tickInterval = time.Second

// DefaultConcurrency caps how many runs execute at once (D8).
const DefaultConcurrency = 4

// maxCatchUpRuns bounds the `all` catch-up policy.
//
// A laptop asleep for a month with `every: 15m` has missed nearly three
// thousand windows, and firing them all is a denial of service against
// yourself. What is dropped is still counted and reported, because "we fired
// 50 and skipped 2830" is a fact somebody needs at 8am.
const maxCatchUpRuns = 50

// scheduleKey identifies one entry in a job's `on:` list.
type scheduleKey struct {
	jobID int64
	index int
}

// scheduleEntry is a parsed schedule and where it last fired.
type scheduleEntry struct {
	job        store.Job
	slug       string
	index      int
	schedule   schedule.Schedule
	catchUp    jobdef.CatchUp
	lastWindow time.Time
}

// Scheduler fires jobs when their own clocks say so.
//
// It queues; it does not execute. D20/C11 moved the worker pool that used to
// live here out of the control plane entirely -- workers pull from the same
// queue over the API, and the only difference between the pool that was here
// and the one that is there is that the remote one is the only one, so it
// cannot be the under-exercised path.
type Scheduler struct {
	engine *Engine

	mu      sync.Mutex
	entries map[scheduleKey]*scheduleEntry
}

func newScheduler(e *Engine) *Scheduler {
	return &Scheduler{
		engine:  e,
		entries: map[scheduleKey]*scheduleEntry{},
	}
}

// RunScheduler starts the schedule loop and the lease reaper, blocking until
// the context is cancelled.
//
// This is the control plane's own loop. It fires schedules into the queue and
// expires leases; between those two the queue sits still until a worker asks
// for something, which is the whole of C11 in one function.
func (e *Engine) RunScheduler(ctx context.Context) error {
	s := newScheduler(e)
	if err := s.reload(ctx); err != nil {
		return err
	}

	e.log.Info("scheduler started", "schedules", len(s.entries))

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	// The reaper runs on its own, slower clock. Expiring a lease declares
	// somebody's running job lost (C6), so it is deliberately not something
	// the one-second tick can do in a hurry.
	reaper := time.NewTicker(LeaseTTL / 3)
	defer reaper.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := s.tick(ctx, e.now()); err != nil {
				// A failing tick must not stop the scheduler: a transient
				// database error at 03:00 should cost one window, not the
				// night.
				e.log.Error("scheduler tick", "error", err)
			}
		case <-reaper.C:
			if _, err := e.ExpireLeases(ctx); err != nil {
				e.log.Error("expiring leases", "error", err)
			}
		case reply := <-e.scheduleReloads:
			// A sync landed new definitions. Rebuilding here rather than in
			// Sync keeps the entries map owned by the one goroutine that reads
			// it, so the schedule table needs no lock of its own.
			err := s.reload(ctx)
			if err != nil {
				// The definitions are already committed, so this is a
				// scheduler problem rather than a sync failure. Keep serving
				// the old table; the next reload gets another chance.
				e.log.Error("rebuilding the schedule table", "error", err)
			} else {
				e.log.Info("schedule table rebuilt", "schedules", len(s.entries))
			}
			reply <- err
		}
	}
}

// Reload rebuilds the schedule table from the loaded definitions.
//
// Called at startup and after every definition load, so editing a job file
// takes effect without a restart.
func (s *Scheduler) reload(ctx context.Context) error {
	jobs, err := s.engine.store.ListJobs(ctx)
	if err != nil {
		return err
	}

	entries := map[scheduleKey]*scheduleEntry{}
	for _, job := range jobs {
		if !job.Runnable() {
			// A misconfigured job keeps its schedule state but is not
			// scheduled. It shows up in `je waiting` as blocked rather than
			// disappearing, which is the difference between a visible problem
			// and a mystery (P1, D10).
			continue
		}
		def, err := jobdef.FromSnapshot(job.Definition)
		if err != nil {
			s.engine.log.Error("parsing stored definition", "job", job.Slug, "error", err)
			continue
		}

		for i, spec := range def.Schedules {
			loc := time.Local
			if spec.Timezone != "" {
				if parsed, err := time.LoadLocation(spec.Timezone); err == nil {
					loc = parsed
				} else {
					s.engine.log.Error("unknown timezone", "job", job.Slug, "tz", spec.Timezone)
					continue
				}
			}

			sched, err := schedule.Parse(schedule.Spec{
				Every:    spec.Every.D,
				Cron:     spec.Cron,
				Location: loc,
			})
			if err != nil {
				s.engine.log.Error("parsing schedule", "job", job.Slug, "index", i, "error", err)
				continue
			}

			key := scheduleKey{jobID: job.ID, index: i}
			entry := &scheduleEntry{
				job: job, slug: job.Slug, index: i,
				schedule: sched, catchUp: spec.CatchUp,
			}

			last, err := s.engine.store.LastWindow(ctx, job.ID, i)
			switch {
			case err == nil:
				entry.lastWindow = last
			case isNoRows(err):
				// First sight of this schedule. Anchor it at now rather than
				// backfilling history -- the same reasoning as seeding a
				// cursor (D14): the recovery from "I wanted the backlog" is
				// cheap, and the recovery from firing a thousand runs is not.
				now := s.engine.now()
				entry.lastWindow = now
				if err := s.engine.store.SetLastWindow(ctx, job.ID, i, now); err != nil {
					return err
				}
				s.engine.recordScheduleStarted(ctx, job, i, sched, now)
			default:
				return err
			}
			entries[key] = entry
		}
	}

	s.mu.Lock()
	s.entries = entries
	s.mu.Unlock()
	return nil
}

// tick fires every window that has come due since the last tick.
//
// A normal tick finds zero or one window. A tick after the laptop woke up
// finds many, and that is the same code path -- which is the point of tracking
// a position on a grid rather than a time since the last run. Catch-up is not
// a special mode; it is an ordinary tick over a longer range.
func (s *Scheduler) tick(ctx context.Context, now time.Time) error {
	s.mu.Lock()
	entries := make([]*scheduleEntry, 0, len(s.entries))
	for _, e := range s.entries {
		entries = append(entries, e)
	}
	s.mu.Unlock()

	for _, entry := range entries {
		if err := s.fireDue(ctx, entry, now); err != nil {
			s.engine.log.Error("firing schedule",
				"job", entry.slug, "index", entry.index, "error", err)
		}
	}
	return nil
}

func (s *Scheduler) fireDue(ctx context.Context, entry *scheduleEntry, now time.Time) error {
	windows, truncated := entry.schedule.Between(entry.lastWindow, now, maxCatchUpRuns)
	if len(windows) == 0 {
		return nil
	}

	newest := windows[len(windows)-1]

	// How many runs to actually create. In normal operation there is exactly
	// one window and all three policies agree; they only differ after a gap,
	// which is precisely when the difference matters.
	var toFire []time.Time
	switch entry.catchUp {
	case jobdef.CatchUpAll:
		toFire = windows
	case jobdef.CatchUpOnce:
		// One run for the whole gap. The job's own cursor (D14) is what makes
		// this correct: it knows how far behind it is and processes the range.
		toFire = windows[len(windows)-1:]
	default: // CatchUpSkip
		if len(windows) == 1 && truncated == 0 {
			toFire = windows
		}
		// Otherwise nothing fires: we resume from now and the gap is recorded.
	}

	for _, window := range toFire {
		if err := s.enqueueWindow(ctx, entry, window); err != nil {
			return err
		}
	}

	// D9: missed windows are recorded as events even when skipped, so a gap in
	// a job's history is explained rather than being an unexplained hole. This
	// is what lets somebody answer "because the machine was asleep" at 8am
	// instead of wondering why the timeline has a gap in it.
	if skipped := (len(windows) + truncated) - len(toFire); skipped > 0 {
		if err := s.engine.recordMissed(ctx, entry, skipped, windows[0], newest); err != nil {
			return err
		}
	}
	entry.lastWindow = newest
	return s.engine.store.SetLastWindow(ctx, entry.job.ID, entry.index, newest)
}

func (s *Scheduler) enqueueWindow(ctx context.Context, entry *scheduleEntry, window time.Time) error {
	payload, _ := json.Marshal(map[string]any{
		"job":      entry.slug,
		"window":   window.Format(time.RFC3339),
		"schedule": entry.schedule.String(),
	})
	cause, _, err := s.engine.publish(ctx, model.Event{
		Type:      EventScheduleFired,
		Source:    model.SourceSchedule,
		Payload:   payload,
		CreatedAt: s.engine.now(),
	})
	if err != nil {
		return err
	}

	prepared, err := s.engine.Enqueue(ctx, entry.slug, RunOptions{TriggeringEvent: &cause})
	if err != nil {
		return err
	}
	if prepared == nil {
		return nil // overlap policy declined; already recorded as an event
	}
	s.engine.log.Info("queued",
		"job", entry.slug, "run", prepared.Run.ID, "window", window.Format(time.RFC3339))
	return nil
}

var errNoSuchRun = errors.New("run vanished between claim and execution")

// prepareClaimed rebuilds the context a claimed run needs to execute.
//
// The run row already records everything that matters -- which definition, which
// cursor version, which event caused it -- so this reads it back rather than
// recomputing. Recomputing would be the bug: a definition reloaded between
// enqueue and execution must not change what this run executes (D11).
func (e *Engine) prepareClaimed(ctx context.Context, run store.Run) (Prepared, error) {
	job, err := e.store.JobByID(ctx, run.JobID)
	if err != nil {
		return Prepared{}, fmt.Errorf("%w: %d", errNoSuchRun, run.ID)
	}
	def, err := jobdef.FromSnapshot(job.Definition)
	if err != nil {
		return Prepared{}, err
	}

	var stateIn store.StateVersion
	if run.StateVersionIn != nil {
		if stateIn, err = e.store.StateAtVersion(ctx, run.JobID, *run.StateVersionIn); err != nil {
			return Prepared{}, err
		}
	}

	var cause model.Event
	if run.TriggeringEventID != nil {
		if cause, err = e.store.EventByID(ctx, *run.TriggeringEventID); err != nil {
			return Prepared{}, err
		}
	}

	return Prepared{Run: run, Job: job, Def: def, StateIn: stateIn, Cause: cause}, nil
}

// recordScheduleStarted notes that the engine has anchored a schedule it had
// never seen before.
//
// Without this the first encounter is invisible, and "why did my new hourly job
// not fire for the last six months of history?" has no answer in the timeline.
// The answer -- we deliberately did not backfill -- should be a row, not
// folklore (P1).
func (e *Engine) recordScheduleStarted(ctx context.Context, job store.Job, index int, sched schedule.Schedule, at time.Time) {
	payload, _ := json.Marshal(map[string]any{
		"job":      job.Slug,
		"schedule": sched.String(),
		"anchored": at.Format(time.RFC3339),
		"next":     sched.Next(at).Format(time.RFC3339),
	})
	if _, _, err := e.publish(ctx, model.Event{
		Type:      EventScheduleStarted,
		Source:    model.SourceSchedule,
		Payload:   payload,
		CreatedAt: at,
	}); err != nil {
		e.log.Error("recording schedule.started", "job", job.Slug, "error", err)
	}
}

// recordMissed explains a gap.
//
// D9 requires this even when the policy is to skip, and the count is the
// interesting part: "3 fired, 2877 skipped while the machine was asleep" is a
// sentence somebody can act on. An absence of runs is not.
func (e *Engine) recordMissed(ctx context.Context, entry *scheduleEntry, skipped int, from, to time.Time) error {
	payload, _ := json.Marshal(map[string]any{
		"job":      entry.slug,
		"schedule": entry.schedule.String(),
		"policy":   string(entry.catchUp),
		"skipped":  skipped,
		"from":     from.Format(time.RFC3339),
		"to":       to.Format(time.RFC3339),
	})
	_, _, err := e.publish(ctx, model.Event{
		Type:      EventScheduleMissed,
		Source:    model.SourceSchedule,
		Payload:   payload,
		CreatedAt: e.now(),
	})
	if err != nil {
		return fmt.Errorf("recording schedule.missed: %w", err)
	}
	e.log.Warn("windows missed",
		"job", entry.slug, "skipped", skipped, "policy", entry.catchUp)
	return nil
}
