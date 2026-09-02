package engine

import (
	"context"
	"time"

	"github.com/jdmorlan/job-engine/internal/jobdef"
	"github.com/jdmorlan/job-engine/internal/schedule"
	"github.com/jdmorlan/job-engine/internal/store"
)

// Waiting is everything the engine intends to do but has not done yet.
//
// P1 names this the largest gap in every job engine its author knows of: they
// can all show you what ran, and none can show you what *didn't* and what it is
// waiting for. It is the negative space, and it is deliberately one view rather
// than three, because "is anything stuck?" is one question.
type Waiting struct {
	// Scheduled is what the clock will fire next, soonest first.
	Scheduled []ScheduledWindow

	// Queued is work waiting for a worker, i.e. behind the concurrency cap.
	Queued []store.Run

	// Blocked is jobs that cannot run at all -- a missing secret, a file that
	// does not parse. These are the ones that will never resolve themselves.
	Blocked []store.Job

	// Running is what is executing right now, for context.
	Running []store.Run
}

// ScheduledWindow is one upcoming firing.
type ScheduledWindow struct {
	Job      string
	Schedule string
	Next     time.Time
	CatchUp  jobdef.CatchUp
}

// Waiting reports the engine's intended future work.
func (e *Engine) Waiting(ctx context.Context) (Waiting, error) {
	var w Waiting
	now := e.now()

	jobs, err := e.store.ListJobs(ctx)
	if err != nil {
		return Waiting{}, err
	}

	for _, job := range jobs {
		if !job.Runnable() {
			w.Blocked = append(w.Blocked, job)
			continue
		}
		def, err := jobdef.FromSnapshot(job.Definition)
		if err != nil {
			continue
		}
		for _, spec := range def.Schedules {
			loc := time.Local
			if spec.Timezone != "" {
				if parsed, err := time.LoadLocation(spec.Timezone); err == nil {
					loc = parsed
				}
			}
			sched, err := schedule.Parse(schedule.Spec{
				Every: spec.Every.D, Cron: spec.Cron, Location: loc,
			})
			if err != nil {
				continue
			}
			w.Scheduled = append(w.Scheduled, ScheduledWindow{
				Job:      job.Slug,
				Schedule: sched.String(),
				Next:     sched.Next(now),
				CatchUp:  spec.CatchUp,
			})
		}
	}

	// Soonest first: the question is "what happens next", so that is the top
	// of the list.
	sortScheduled(w.Scheduled)

	if w.Queued, err = e.store.QueuedRuns(ctx); err != nil {
		return Waiting{}, err
	}
	if w.Running, err = e.store.RecentRunsWithStatus(ctx, "running", 50); err != nil {
		return Waiting{}, err
	}
	return w, nil
}

// NeedsAttention reports whether anything here is a problem a human should look
// at, which is what gives `je waiting` and `je status` a meaningful exit code
// (P1: a query with an exit code rather than a vibe).
func (w Waiting) NeedsAttention() bool { return len(w.Blocked) > 0 }

func sortScheduled(s []ScheduledWindow) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j].Next.Before(s[j-1].Next); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
