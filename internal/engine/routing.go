package engine

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jdmorlan/job-engine/internal/jobdef"
	"github.com/jdmorlan/job-engine/internal/model"
	"github.com/jdmorlan/job-engine/internal/store"
)

// EventRouteFailed records a rule that matched and could not start its job.
//
// It exists because the alternative is silence, and silence is the failure
// mode chains are most prone to: the upstream job succeeded, the file says
// what happens next, and nothing happened. A row saying which rule it was and
// why it did not fire is the whole difference (P1).
const EventRouteFailed = "route.failed"

// maxFanOut caps how many runs one event may start.
//
// D3 asks for this next to the depth guard, and for a different reason: depth
// catches a cycle, this catches a mistake in breadth -- a `where`-less match on
// a common event type wiring one tick to fifty runs. Ten is well above any
// honest flow and well below anything that hurts.
const maxFanOut = 10

// compiledRoute is a stored rule with its pattern already parsed, so matching
// an event is a comparison rather than a JSON decode per rule per event.
type compiledRoute struct {
	route store.Route
	match jobdef.Match
}

// reloadRoutes rebuilds the in-memory route table from the database.
//
// Routing has to be cheap: every event the engine records is offered to it, and
// most events match nothing. The table changes only on load, so it is read from
// storage once and held, keyed by event type -- an event that matches no rule
// costs one map lookup.
func (e *Engine) reloadRoutes(ctx context.Context) error {
	routes, err := e.store.ActiveRoutes(ctx)
	if err != nil {
		return fmt.Errorf("loading routes: %w", err)
	}

	table := map[string][]compiledRoute{}
	for _, r := range routes {
		var match jobdef.Match
		if err := json.Unmarshal(r.Match, &match); err != nil {
			// The row was written by this program from a validated file, so
			// this is corruption rather than bad input. Skip the rule and say
			// so; refusing to start over one unreadable route would take the
			// whole engine down for a rule nobody may be using.
			e.log.Error("unreadable route", "route", r.ID,
				"chain", r.ChainName, "step", r.StepIndex, "error", err)
			continue
		}
		table[match.Event] = append(table[match.Event], compiledRoute{route: r, match: match})
	}

	e.routesMu.Lock()
	e.routes = table
	e.routesMu.Unlock()

	e.log.Info("routes loaded", "routes", len(routes))
	return nil
}

// publish records an event and fires whatever it triggers.
//
// Every event the engine records goes through here, which is what makes "the
// wiring is in one place" true at runtime as well as in the files: there is one
// function where an event becomes a run, so there is one place to read to know
// what an event can cause.
func (e *Engine) publish(ctx context.Context, ev model.Event) (model.Event, bool, error) {
	stored, deduped, err := e.store.AppendEvent(ctx, ev)
	if err != nil || deduped {
		// A deduped event is not a second occurrence, so it must not cause a
		// second run. That is the whole promise of D16's dedupe key.
		return stored, deduped, err
	}
	e.dispatchRoutes(ctx, stored)
	return stored, false, nil
}

// dispatchRoutes starts a run for every rule the event satisfies.
//
// Failures here are recorded and logged, never returned. The caller is usually
// finishing a run that has already done its work, and failing that run because
// the *next* one could not be queued would misattribute the problem to the job
// that succeeded.
func (e *Engine) dispatchRoutes(ctx context.Context, ev model.Event) {
	matched := e.matching(ev)
	if len(matched) == 0 {
		return
	}

	if ev.Depth >= maxDepth {
		e.recordRouteFailure(ctx, ev, nil, fmt.Sprintf(
			"causation depth %d reached the limit of %d, which is a cycle rather than a flow",
			ev.Depth, maxDepth))
		return
	}
	if len(matched) > maxFanOut {
		e.recordRouteFailure(ctx, ev, nil, fmt.Sprintf(
			"%d rules matched, over the fan-out limit of %d -- none were fired",
			len(matched), maxFanOut))
		return
	}

	for _, cr := range matched {
		route := cr.route
		prepared, err := e.Enqueue(ctx, route.TargetSlug, RunOptions{
			TriggeringEvent: &ev,
			Route:           &route,
		})
		switch {
		case err != nil:
			e.recordRouteFailure(ctx, ev, &route, err.Error())
		case prepared == nil:
			// The overlap policy declined. Already an event of its own, with
			// its reason, so there is nothing to add here.
		default:
			e.log.Info("routed",
				"event", ev.Type, "chain", route.ChainName, "step", route.StepIndex,
				"job", route.TargetSlug, "run", prepared.Run.ID)
		}
	}
}

// matching returns the rules an event satisfies, under the read lock.
func (e *Engine) matching(ev model.Event) []compiledRoute {
	e.routesMu.RLock()
	candidates := e.routes[ev.Type]
	e.routesMu.RUnlock()

	var out []compiledRoute
	for _, cr := range candidates {
		if cr.match.Matches(ev.Type, ev.Payload) {
			out = append(out, cr)
		}
	}
	return out
}

// recordRouteFailure writes the row that keeps a rule from failing silently.
//
// Appended directly rather than published: an event about routing must not
// itself be routed, or a misconfigured chain could feed on its own failures.
func (e *Engine) recordRouteFailure(ctx context.Context, cause model.Event, route *store.Route, reason string) {
	payload := map[string]any{"event": cause.Type, "reason": reason}
	if route != nil {
		payload["job"] = route.TargetSlug
		payload["chain"] = route.ChainName
		payload["step"] = route.StepIndex
		payload["file"] = route.FilePath
	}
	body, _ := json.Marshal(payload)

	if _, _, err := e.store.AppendEvent(ctx, model.Event{
		Type:            EventRouteFailed,
		Source:          model.SourceEngine,
		Payload:         body,
		CausedByEventID: &cause.ID,
		Depth:           cause.Depth + 1,
		CreatedAt:       e.now(),
	}); err != nil {
		e.log.Error("recording "+EventRouteFailed, "error", err)
	}
	e.log.Error("route did not fire", "event", cause.Type, "reason", reason)
}
