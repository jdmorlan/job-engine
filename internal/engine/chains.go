package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/jdmorlan/job-engine/internal/jobdef"
	"github.com/jdmorlan/job-engine/internal/model"
	"github.com/jdmorlan/job-engine/internal/store"
)

// ChainState is what happened to a chain's most recent pass.
//
// These are derived from runs and events at read time, never stored. That is
// the discipline D17 asks for: a chain is a display grouping, so it may have a
// *rendered* state and must not have a persisted one -- the moment a chain has
// its own status column, something has to maintain it, and we have a state
// machine and a DAG engine.
type ChainState string

const (
	// ChainEmpty is a chain file with no steps yet. It loads, and it wires
	// nothing -- distinct from "never run", which is a chain that would fire
	// if its event happened.
	ChainEmpty     ChainState = "no steps"
	ChainNeverRun  ChainState = "never run"
	ChainRunning   ChainState = "running"
	ChainComplete  ChainState = "complete"
	ChainStopped   ChainState = "stopped"
	ChainUnstarted ChainState = "stalled"
)

// ChainView is one chain's most recent pass, step by step.
type ChainView struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	FilePath    string `json:"file_path"`

	Steps []ChainStep `json:"steps"`

	// Trigger is the run that set this pass off, when the first step matched
	// something a run emitted. Nil when the chain starts from a schedule, an
	// external `je emit`, or nothing has happened yet.
	Trigger *ChainRun `json:"trigger,omitempty"`

	State ChainState `json:"state"`

	// Duration is end to end: from the start of whatever set the chain off to
	// the end of the last step that ran. This is the number no job-level view
	// can produce, and the reason a chain is worth naming at all -- "the chain
	// takes 40 minutes now, it used to take 5" is not a question you can ask
	// about anonymous rules.
	Duration time.Duration `json:"duration_ns,omitempty"`

	// Failed lists the steps whose runs did not succeed, 1-based and ascending.
	//
	// A list rather than a single index, because a chain is a set of rules and
	// not a line: four reports hanging off one extract can fail
	// independently, and naming only the first would describe a smaller
	// problem than the one that happened. There is no runtime consequence to
	// any of it -- steps downstream of a failure simply never fire, because
	// their triggering event never happens.
	Failed []int `json:"failed_steps,omitempty"`
}

// ChainStep is one rule and the run it most recently started.
type ChainStep struct {
	Step  int          `json:"step"`
	Job   string       `json:"job"`
	On    jobdef.Match `json:"on"`
	Run   *ChainRun    `json:"run,omitempty"`
	Route int64        `json:"route_id"`
}

// ChainRun is the part of a run a chain view shows.
type ChainRun struct {
	ID        int64        `json:"id"`
	Job       string       `json:"job"`
	Status    model.Status `json:"status"`
	StartedAt *time.Time   `json:"started_at,omitempty"`
	EndedAt   *time.Time   `json:"ended_at,omitempty"`
}

// Chains returns every loaded chain with its most recent pass.
func (e *Engine) Chains(ctx context.Context) ([]ChainView, error) {
	chains, err := e.store.ListChains(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ChainView, 0, len(chains))
	for _, c := range chains {
		view, err := e.chainView(ctx, c)
		if err != nil {
			return nil, err
		}
		out = append(out, view)
	}
	return out, nil
}

// Chain returns one chain. sql.ErrNoRows means there is no such chain.
func (e *Engine) Chain(ctx context.Context, name string) (ChainView, error) {
	c, err := e.resolveChain(ctx, name)
	if err != nil {
		return ChainView{}, err
	}
	return e.chainView(ctx, c)
}

// chainView reconstructs the latest pass by walking causation forwards.
//
// There is no chain instance to look up, because there is no such row -- so
// this finds the newest run the first step started and follows the recorded
// causation from there: a run points at the event that caused it, and that
// event points at the run that emitted it. Every fact it uses was recorded for
// its own reasons; none of it exists to make this view possible, which is why
// the view can exist without a runtime entity behind it.
func (e *Engine) chainView(ctx context.Context, c store.Chain) (ChainView, error) {
	view := ChainView{
		Name:        c.Name,
		Description: c.Description,
		FilePath:    c.FilePath,
		State:       ChainNeverRun,
	}

	routes, err := e.store.RoutesForChain(ctx, c.Name)
	if err != nil {
		return ChainView{}, err
	}
	for _, r := range routes {
		if r.RemovedAt != nil {
			continue
		}
		var match jobdef.Match
		_ = json.Unmarshal(r.Match, &match)
		view.Steps = append(view.Steps, ChainStep{
			Step: r.StepIndex, Job: r.TargetSlug, On: match, Route: r.ID,
		})
	}
	if len(view.Steps) == 0 {
		view.State = ChainEmpty
		return view, nil
	}

	// Anchor on the newest run any of this chain's rules started, then walk
	// causation outwards from it -- backwards to whatever set the pass off,
	// forwards through the steps it went on to cause.
	//
	// Anchoring on the newest run rather than on the first step is what makes
	// this work when a chain does not happen to be a straight line: the steps
	// in a file are a set of rules, not a sequence, and two of them may hang
	// off the same job. The order they are written in is for a human to read.
	anchor, ok, err := e.anchorRun(ctx, view.Steps)
	if err != nil {
		return ChainView{}, err
	}
	if !ok {
		return view, nil
	}

	byRoute := map[int64]int{} // route id -> index into view.Steps
	for i, s := range view.Steps {
		byRoute[s.Route] = i
	}

	// Backwards, until a run this chain did not cause. That run is the
	// trigger: the schedule-fired ingest, the manual `je run`, the job in
	// another chain that happens to feed this one.
	//
	// Everything passed through on the way up joins the forward search below,
	// the trigger included. That is not tidiness: the anchor is the *newest*
	// run, and in a fan-out its siblings are not downstream of it -- they hang
	// off the same parent. Searching forward from the anchor alone finds one
	// of five and calls the pass stalled.
	found := map[int64]store.Run{} // route id -> run
	frontier := []store.Run{anchor}
	current := anchor
	for {
		if step, ok := byRoute[routeIDOf(current)]; ok {
			found[view.Steps[step].Route] = current
		}
		previous, err := e.causingRun(ctx, current)
		if err != nil {
			return ChainView{}, err
		}
		if previous == nil {
			break
		}
		frontier = append(frontier, *previous)
		if _, ok := byRoute[routeIDOf(*previous)]; !ok {
			if view.Trigger, err = e.chainRun(ctx, *previous); err != nil {
				return ChainView{}, err
			}
			break
		}
		current = *previous
	}

	// Forwards, breadth first, so a job that fans out to five steps shows all
	// five rather than whichever the anchor happened to be.
	for len(frontier) > 0 {
		from := frontier[0]
		frontier = frontier[1:]
		for _, step := range view.Steps {
			if _, done := found[step.Route]; done {
				continue
			}
			run, err := e.store.RunTriggeredBy(ctx, from.ID, step.Route)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			} else if err != nil {
				return ChainView{}, err
			}
			found[step.Route] = run
			frontier = append(frontier, run)
		}
	}

	for i, step := range view.Steps {
		run, ok := found[step.Route]
		if !ok {
			continue
		}
		if view.Steps[i].Run, err = e.chainRun(ctx, run); err != nil {
			return ChainView{}, err
		}
	}

	view.State, view.Failed = chainState(view.Steps)
	view.Duration = chainDuration(view)
	return view, nil
}

// anchorRun is the newest run any of the chain's rules started.
func (e *Engine) anchorRun(ctx context.Context, steps []ChainStep) (store.Run, bool, error) {
	var (
		newest store.Run
		found  bool
	)
	for _, s := range steps {
		run, err := e.store.LatestRunByRoute(ctx, s.Route)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		} else if err != nil {
			return store.Run{}, false, err
		}
		if !found || run.ID > newest.ID {
			newest, found = run, true
		}
	}
	return newest, found, nil
}

// causingRun returns the run whose event caused this one, if a run did.
//
// A schedule, an external `je emit` and a manual `je run` all leave this nil,
// which is the honest answer: the chain was set off by something that is not a
// run, and inventing one to draw a tree would be a lie about causation.
func (e *Engine) causingRun(ctx context.Context, run store.Run) (*store.Run, error) {
	if run.TriggeringEventID == nil {
		return nil, nil
	}
	event, err := e.store.EventByID(ctx, *run.TriggeringEventID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if event.CausedByRunID == nil {
		return nil, nil
	}
	cause, err := e.store.RunByID(ctx, *event.CausedByRunID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return &cause, nil
}

func routeIDOf(r store.Run) int64 {
	if r.TriggeringRouteID == nil {
		return 0
	}
	return *r.TriggeringRouteID
}

// chainState reads the outcome off the steps that ran.
//
// A failure anywhere is the answer, whatever else is missing: D17 gives a
// chain no runtime consequence, so the steps after a failed one have no run
// for exactly one reason -- the event they wait for never happened. Reporting
// those as separately stalled would turn one problem into three.
func chainState(steps []ChainStep) (ChainState, []int) {
	var (
		failed  []int
		running bool
		missing bool
	)
	for i, s := range steps {
		switch {
		case s.Run == nil:
			missing = true
		case !s.Run.Status.Terminal():
			running = true
		case s.Run.Status != model.StatusSucceeded:
			failed = append(failed, i+1)
		}
	}
	switch {
	case len(failed) > 0:
		// A failure is the answer even if something else is still going: the
		// pass did not do what the file says it does, and that is the fact
		// somebody needs first.
		return ChainStopped, failed
	case running:
		return ChainRunning, nil
	case missing:
		// Everything that ran succeeded and a step still has no run. The rule
		// did not fire: an overlap skip, or a route.failed. Both are recorded
		// events, so this only has to refuse to call it complete.
		return ChainUnstarted, nil
	default:
		return ChainComplete, nil
	}
}

// chainDuration measures from whatever set the chain off to the last thing that
// finished.
func chainDuration(v ChainView) time.Duration {
	var start, end *time.Time
	consider := func(r *ChainRun) {
		if r == nil {
			return
		}
		if r.StartedAt != nil && (start == nil || r.StartedAt.Before(*start)) {
			start = r.StartedAt
		}
		if r.EndedAt != nil && (end == nil || r.EndedAt.After(*end)) {
			end = r.EndedAt
		}
	}
	consider(v.Trigger)
	for _, s := range v.Steps {
		consider(s.Run)
	}
	if start == nil || end == nil {
		return 0
	}
	return end.Sub(*start)
}

func (e *Engine) chainRun(ctx context.Context, run store.Run) (*ChainRun, error) {
	job, err := e.store.JobByID(ctx, run.JobID)
	if err != nil {
		return nil, err
	}
	return &ChainRun{
		ID:        run.ID,
		Job:       job.Slug,
		Status:    run.Status,
		StartedAt: run.StartedAt,
		EndedAt:   run.EndedAt,
	}, nil
}
