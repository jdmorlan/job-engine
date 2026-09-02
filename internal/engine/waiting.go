package engine

import (
	"context"
	"slices"
	"sort"
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
	Scheduled []ScheduledWindow `json:"scheduled"`

	// Queued is work waiting for a worker, i.e. behind the concurrency cap.
	Queued []store.Run `json:"queued"`

	// Blocked is jobs that cannot run at all -- a missing secret, a file that
	// does not parse. These are the ones that will never resolve themselves.
	Blocked []store.Job `json:"blocked"`

	// Running is what is executing right now, for context.
	Running []store.Run `json:"running"`

	// Unservable is work queued for a capability label no online worker
	// advertises (D20/C8).
	//
	// This is the failure the split introduced and the one it must never be
	// quiet about: the run is queued, the schedule is firing, everything looks
	// busy, and nothing can ever pick it up. An offline worker must produce a
	// visible waiting state, never a silent backlog.
	Unservable []UnservedLabel `json:"unservable,omitempty"`
}

// UnservedLabel is one capability nothing is serving, and what is stuck on it.
type UnservedLabel struct {
	Label string   `json:"label"`
	Runs  []int64  `json:"runs"`
	Jobs  []string `json:"jobs"`
}

// ScheduledWindow is one upcoming firing.
type ScheduledWindow struct {
	Job      string         `json:"job"`
	Schedule string         `json:"schedule"`
	Next     time.Time      `json:"next"`
	CatchUp  jobdef.CatchUp `json:"catch_up"`
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
		if job.Removed() {
			// Deliberately gone, not stuck. Reporting a deleted job as
			// needing attention would make `je waiting` exit 3 forever after
			// any tidy-up, which trains people to ignore the exit code.
			continue
		}
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

	// C8: which of the queued runs is waiting on a label nothing serves.
	covered, err := e.store.LabelsCovered(ctx, now, LeaseTTL)
	if err != nil {
		return Waiting{}, err
	}
	stuck := map[string]*UnservedLabel{}
	for _, run := range w.Queued {
		if covered[run.RunsOn] {
			continue
		}
		entry := stuck[run.RunsOn]
		if entry == nil {
			entry = &UnservedLabel{Label: run.RunsOn}
			stuck[run.RunsOn] = entry
		}
		entry.Runs = append(entry.Runs, run.ID)
		if job, err := e.store.JobByID(ctx, run.JobID); err == nil {
			if !slices.Contains(entry.Jobs, job.Slug) {
				entry.Jobs = append(entry.Jobs, job.Slug)
			}
		}
	}
	for _, entry := range stuck {
		w.Unservable = append(w.Unservable, *entry)
	}
	sort.Slice(w.Unservable, func(i, j int) bool {
		return w.Unservable[i].Label < w.Unservable[j].Label
	})
	return w, nil
}

// NeedsAttention reports whether anything here is a problem a human should look
// at, which is what gives `je waiting` and `je status` a meaningful exit code
// (P1: a query with an exit code rather than a vibe).
// Unservable counts here because a run nobody can take is stuck in exactly the
// way Blocked is: it will not resolve on its own, and no amount of waiting
// helps. It is arguably the worse of the two, because everything about it looks
// like ordinary queueing.
func (w Waiting) NeedsAttention() bool {
	return len(w.Blocked) > 0 || len(w.Unservable) > 0
}

func sortScheduled(s []ScheduledWindow) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j].Next.Before(s[j-1].Next); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
