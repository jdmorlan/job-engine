package engine

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"time"

	"github.com/jdmorlan/job-engine/internal/jobdef"
	"github.com/jdmorlan/job-engine/internal/model"
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

	// Retrying is work that failed and will be attempted again, with the
	// clock it is waiting on (D7).
	//
	// It belongs in this view for the reason the view exists: between two
	// attempts a job is neither running nor finished, and a person asking "is
	// anything stuck?" about a job that failed ten minutes ago deserves to see
	// "attempt 2 of 3, next at 14:22" rather than nothing at all.
	Retrying []store.Run `json:"retrying,omitempty"`

	// Unservable is work queued for a capability label no online worker
	// advertises (D20/C8).
	//
	// This is the failure the split introduced and the one it must never be
	// quiet about: the run is queued, the schedule is firing, everything looks
	// busy, and nothing can ever pick it up. An offline worker must produce a
	// visible waiting state, never a silent backlog.
	Unservable []UnservedLabel `json:"unservable,omitempty"`

	// UnservedRuntimes is work whose language no online worker can prepare
	// (D28). The same failure as Unservable one level down: the label matched,
	// a worker would take it, and it would fail on arrival because that
	// machine has no toolchain for it.
	UnservedRuntimes []UnservedRuntime `json:"unserved_runtimes,omitempty"`

	// Triggers is every fan-in that is partly satisfied: what it is still
	// waiting on, and how long the events it already has stay valid (D3).
	//
	// This view is the feature. The mechanism underneath it is a few hundred
	// lines; being able to answer "why hasn't the rollup run?" without reading
	// logs is the thing that makes fan-in worth having at all.
	Triggers []PendingTrigger `json:"triggers,omitempty"`
}

// PendingTrigger is one fan-in step, part way there.
type PendingTrigger struct {
	Chain string `json:"chain"`
	Step  int    `json:"step"`
	Job   string `json:"job"`

	// Satisfied and Waiting are the conditions, described the way the file
	// wrote them, so the view reads like the definition rather than like a
	// row.
	Satisfied []SatisfiedCondition `json:"satisfied"`
	Waiting   []string             `json:"waiting"`

	// Expires is when the oldest satisfying event falls out of the window,
	// after which the trigger cannot complete on what it currently has.
	Expires time.Time `json:"expires"`
}

// SatisfiedCondition is one met condition and what met it.
type SatisfiedCondition struct {
	// Condition is the condition as the file wrote it, including its `where`.
	// The event type alone is not enough to tell two conditions apart -- a
	// fan-in on two jobs is two conditions on run.succeeded -- and a view that
	// showed only the type would say "waiting on run.succeeded, satisfied by
	// run.succeeded", which answers nothing.
	Condition string    `json:"condition"`
	Event     string    `json:"event"`
	EventID   int64     `json:"event_id"`
	At        time.Time `json:"at"`
}

// UnservedRuntime is one language nothing can prepare, and what is stuck on it.
type UnservedRuntime struct {
	Language string   `json:"language"`
	Runs     []int64  `json:"runs"`
	Jobs     []string `json:"jobs"`
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
	if w.Retrying, err = e.store.RecentRunsWithStatus(ctx,
		string(model.StatusRetrying), 50); err != nil {
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

	// D28: and which are waiting on a language nothing can prepare. Checked
	// separately from the label because they are different questions -- a
	// worker can advertise `macos` and still have no pnpm -- and a job can be
	// stuck on either.
	runtimes, err := e.store.RuntimesCovered(ctx, now, LeaseTTL)
	if err != nil {
		return Waiting{}, err
	}
	stuckRuntimes := map[string]*UnservedRuntime{}
	for _, run := range w.Queued {
		job, err := e.store.JobByID(ctx, run.JobID)
		if err != nil {
			continue
		}
		def, err := jobdef.FromSnapshot(job.Definition)
		if err != nil || def.Language == "" || runtimes[def.Language] {
			continue
		}
		entry := stuckRuntimes[def.Language]
		if entry == nil {
			entry = &UnservedRuntime{Language: def.Language}
			stuckRuntimes[def.Language] = entry
		}
		entry.Runs = append(entry.Runs, run.ID)
		if !slices.Contains(entry.Jobs, job.Slug) {
			entry.Jobs = append(entry.Jobs, job.Slug)
		}
	}
	for _, entry := range stuckRuntimes {
		w.UnservedRuntimes = append(w.UnservedRuntimes, *entry)
	}

	if w.Triggers, err = e.pendingTriggers(ctx, now); err != nil {
		return Waiting{}, err
	}
	sort.Slice(w.UnservedRuntimes, func(i, j int) bool {
		return w.UnservedRuntimes[i].Language < w.UnservedRuntimes[j].Language
	})
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
	return len(w.Blocked) > 0 || len(w.Unservable) > 0 || len(w.UnservedRuntimes) > 0
}

func sortScheduled(s []ScheduledWindow) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j].Next.Before(s[j-1].Next); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// pendingTriggers describes every partly-satisfied fan-in (D3).
func (e *Engine) pendingTriggers(ctx context.Context, now time.Time) ([]PendingTrigger, error) {
	state, err := e.store.PendingTriggers(ctx)
	if err != nil {
		return nil, err
	}
	if len(state) == 0 {
		return nil, nil
	}
	routes, err := e.store.ActiveRoutes(ctx)
	if err != nil {
		return nil, err
	}

	var out []PendingTrigger
	for _, r := range routes {
		window, ok := state[r.ID]
		if !ok {
			continue
		}
		var match jobdef.Match
		if err := json.Unmarshal(r.Match, &match); err != nil || !match.IsFanIn() {
			continue
		}

		met := map[int]store.Satisfaction{}
		for _, sat := range window.Satisfied {
			// Only what is still inside the window counts, which is the same
			// question firing asks -- so this view cannot claim a condition is
			// met while the engine disagrees.
			if !sat.SatisfiedAt.Before(now.Add(-match.Within.D)) {
				met[sat.ConditionIndex] = sat
			}
		}
		if len(met) == 0 || len(met) == len(match.AllOf) {
			// Nothing left in the window, or a set that has just fired.
			continue
		}

		pending := PendingTrigger{
			Chain: r.ChainName, Step: r.StepIndex, Job: r.TargetSlug,
			Expires: window.ExpiresAt,
		}
		for i, cond := range match.AllOf {
			if sat, ok := met[i]; ok {
				pending.Satisfied = append(pending.Satisfied, SatisfiedCondition{
					Condition: cond.String(),
					Event:     cond.Event, EventID: sat.EventID, At: sat.SatisfiedAt,
				})
				continue
			}
			pending.Waiting = append(pending.Waiting, cond.String())
		}
		out = append(out, pending)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Chain != out[j].Chain {
			return out[i].Chain < out[j].Chain
		}
		return out[i].Step < out[j].Step
	})
	return out, nil
}
