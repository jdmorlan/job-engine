package engine_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jdmorlan/job-engine/internal/engine"
	"github.com/jdmorlan/job-engine/internal/model"
	"github.com/jdmorlan/job-engine/internal/paths"
	"github.com/jdmorlan/job-engine/internal/store"
)

// chainFixture writes a set of job files and chain files, and loads them.
//
// jobs maps a slug to the shell it runs; chains maps a chain name to the body
// of chains/<name>.yaml.
func chainFixture(t *testing.T, jobs, chains map[string]string) (*engine.Engine, paths.Layout) {
	t.Helper()

	dir := t.TempDir()
	layout := paths.Layout{Data: dir}

	e := newEngine(t, layout, nil)
	t.Cleanup(func() { e.Close(context.Background()) })

	// A repository, served from a directory the test edits. Definitions reach
	// the engine the only way they can now: by being fetched.
	tree := filepath.Join(dir, "repo")
	if err := os.MkdirAll(filepath.Join(tree, "chains"), 0o755); err != nil {
		t.Fatal(err)
	}
	for slug, script := range jobs {
		body := "command: [\"/bin/sh\", \"-c\", " + quote(script) + "]\n"
		write(t, filepath.Join(tree, slug+".yaml"), body)
	}
	for name, body := range chains {
		write(t, filepath.Join(tree, "chains", name+".yaml"), body)
	}

	hub := newHub(t)
	hub.Add("you/"+testSource, tree)
	rememberFixture(e, tree, hub)
	engine.SetGitHubBaseURLForTest(e, hub.URL)

	if _, err := e.AddSource(context.Background(), store.Source{
		Name: testSource, Kind: store.SourceKindGitHub, Location: "you/" + testSource,
	}); err != nil {
		t.Fatalf("registering the fixture source: %v", err)
	}
	return e, layout
}

// testSource is what fixture definitions are registered under. Every job it
// loads is named <testSource>/<slug>, because a job always comes from a source
// somebody named now.
const testSource = "src"

// qual is the name a fixture job is known by.
func qual(slug string) string { return testSource + "/" + slug }

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func quote(s string) string {
	out := []byte{'"'}
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"', '\\':
			out = append(out, '\\', s[i])
		case '\n':
			out = append(out, '\\', 'n')
		default:
			out = append(out, s[i])
		}
	}
	return string(append(out, '"'))
}

// drainQueue executes everything the queue holds, the way a worker would.
//
// A chain step is queued by the control plane the moment the previous step's
// run.succeeded lands, so the interesting part of every test below is what is
// waiting here afterwards.
func drainQueue(t *testing.T, e *engine.Engine) int {
	t.Helper()
	ctx := context.Background()
	workerID := ensureWorker(t, e)

	executed := 0
	for range 20 {
		dispatch, err := e.Claim(ctx, workerID)
		if err != nil {
			t.Fatalf("claiming: %v", err)
		}
		if dispatch == nil {
			return executed
		}
		completion := executeDispatch(t, e, *dispatch, ctx)
		if err := e.Complete(ctx, dispatch.RunID, workerID, completion); err != nil {
			t.Fatalf("completing run %d: %v", dispatch.RunID, err)
		}
		executed++
	}
	t.Fatal("the queue never emptied, which means something is triggering itself")
	return executed
}

func TestChainRunsEveryStepInOrder(t *testing.T) {
	ctx := context.Background()
	e, _ := chainFixture(t,
		map[string]string{
			"extract":   `echo extracting`,
			"normalize": `echo normalizing`,
			"rollup":    `echo rolling up`,
		},
		map[string]string{"daily": `
description: extract, normalize, roll up
steps:
  - on: { event: run.succeeded, where: { job: extract } }
    run: normalize
  - on: { event: run.succeeded, where: { job: normalize } }
    run: rollup
`})

	if _, err := runJob(t, e, "extract", engine.RunOptions{Actor: "tester"}); err != nil {
		t.Fatalf("running the first job: %v", err)
	}
	// One run per remaining step: extract's success queued normalize, and
	// normalize's success queued rollup while this loop was still running.
	if executed := drainQueue(t, e); executed != 2 {
		t.Fatalf("the chain ran %d further steps, want 2", executed)
	}

	view, err := e.Chain(ctx, "daily")
	if err != nil {
		t.Fatalf("reading the chain: %v", err)
	}
	if view.State != engine.ChainComplete {
		t.Fatalf("state = %q, want complete", view.State)
	}
	if view.Trigger == nil || view.Trigger.Job != qual("extract") {
		t.Fatalf("trigger = %+v, want the extract run that set it off", view.Trigger)
	}
	if len(view.Steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(view.Steps))
	}
	for _, s := range view.Steps {
		if s.Run == nil {
			t.Fatalf("step %d (%s) did not fire", s.Step, s.Job)
		}
		if s.Run.Status != model.StatusSucceeded {
			t.Fatalf("step %d (%s) = %s", s.Step, s.Job, s.Run.Status)
		}
	}
	// End-to-end is the number no job-level view can produce, so it has to be
	// real rather than the last step's duration wearing a different name.
	if view.Duration <= 0 {
		t.Errorf("duration = %s, want the span from the trigger to the last step", view.Duration)
	}
}

func TestARunRecordsWhichRuleFiredIt(t *testing.T) {
	ctx := context.Background()
	e, _ := chainFixture(t,
		map[string]string{"extract": `echo one`, "rollup": `echo two`},
		map[string]string{"daily": `
steps:
  - on: { event: run.succeeded, where: { job: extract } }
    run: rollup
`})

	if _, err := runJob(t, e, "extract", engine.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	drainQueue(t, e)

	view, err := e.Chain(ctx, "daily")
	if err != nil {
		t.Fatal(err)
	}
	run, err := e.Run(ctx, view.Steps[0].Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	// D11: "why did this run?" has to survive the file changing, so the run
	// carries both the rule's id and its hash.
	if run.TriggeringRouteID == nil || *run.TriggeringRouteID != view.Steps[0].Route {
		t.Errorf("triggering_route_id = %v, want route %d", run.TriggeringRouteID, view.Steps[0].Route)
	}
	if run.RouteHash == "" {
		t.Error("the run did not record the hash of the rule that fired it")
	}
}

func TestAFailedStepStopsTheChainWithoutCancellingAnything(t *testing.T) {
	ctx := context.Background()
	e, _ := chainFixture(t,
		map[string]string{
			"extract":   `echo extracting`,
			"normalize": `echo failing; exit 1`,
			"rollup":    `echo should not happen`,
		},
		map[string]string{"daily": `
steps:
  - on: { event: run.succeeded, where: { job: normalize } }
    run: rollup
  - on: { event: run.succeeded, where: { job: extract } }
    run: normalize
`})

	if _, err := runJob(t, e, "extract", engine.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if executed := drainQueue(t, e); executed != 1 {
		t.Fatalf("ran %d runs after extract, want just the failing normalize", executed)
	}

	runs, err := e.Runs(ctx, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		detail, err := e.RunDetail(ctx, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		if detail.JobSlug == "rollup" {
			t.Fatal("rollup ran even though the step before it failed")
		}
	}

	// The steps are deliberately written out of execution order in the file,
	// to pin that a chain's order comes from its steps and not from luck.
	view, err := e.Chain(ctx, "daily")
	if err != nil {
		t.Fatal(err)
	}
	if view.State != engine.ChainStopped {
		t.Fatalf("state = %q, want stopped", view.State)
	}
	if len(view.Failed) != 1 || view.Failed[0] != 2 {
		t.Fatalf("failed steps = %v, want just step 2 (normalize)", view.Failed)
	}
}

func TestAStepMatchesFieldsOfAJobEmittedEvent(t *testing.T) {
	ctx := context.Background()
	e, _ := chainFixture(t,
		map[string]string{
			"ingest": `echo '{"type":"weather.ingested","payload":{"count":41,"station":"north"}}' >> "$JOB_EVENTS_FILE"`,
			"rollup": `echo rolling up`,
			"alert":  `echo alerting`,
		},
		map[string]string{
			"north": `
steps:
  - on: { event: weather.ingested, where: { station: north } }
    run: rollup
`,
			"south": `
steps:
  - on: { event: weather.ingested, where: { station: south } }
    run: alert
`})

	if _, err := runJob(t, e, "ingest", engine.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if executed := drainQueue(t, e); executed != 1 {
		t.Fatalf("ran %d jobs, want only the chain whose `where` matched", executed)
	}

	north, err := e.Chain(ctx, "north")
	if err != nil {
		t.Fatal(err)
	}
	if north.State != engine.ChainComplete {
		t.Fatalf("north = %q, want complete", north.State)
	}
	south, err := e.Chain(ctx, "south")
	if err != nil {
		t.Fatal(err)
	}
	if south.State != engine.ChainNeverRun {
		t.Fatalf("south = %q, want never run -- station was north", south.State)
	}
}

func TestARemovedChainFileStopsFiringAndKeepsItsHistory(t *testing.T) {
	ctx := context.Background()
	e, _ := chainFixture(t,
		map[string]string{"extract": `echo one`, "rollup": `echo two`},
		map[string]string{"daily": `
steps:
  - on: { event: run.succeeded, where: { job: extract } }
    run: rollup
`})

	if _, err := runJob(t, e, "extract", engine.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	drainQueue(t, e)

	if err := os.Remove(filepath.Join(chainsDir(e), "daily.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Sync(ctx); err != nil {
		t.Fatalf("reloading without the chain file: %v", err)
	}

	if _, err := runJob(t, e, "extract", engine.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if executed := drainQueue(t, e); executed != 0 {
		t.Fatalf("%d runs fired from a chain file that is gone", executed)
	}

	// The rule stopped firing; the runs it caused are still there. D19: a
	// deleted file must never delete history.
	runs, err := e.Runs(ctx, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 3 {
		t.Fatalf("runs = %d, want 3 (two extracts and the rollup the chain caused)", len(runs))
	}
}

func TestARuleThatCannotFireSaysSoInsteadOfNothing(t *testing.T) {
	ctx := context.Background()
	e, _ := chainFixture(t,
		map[string]string{"extract": `echo one`, "rollup": `echo two`},
		map[string]string{"daily": `
steps:
  - on: { event: run.succeeded, where: { job: extract } }
    run: rollup
`})

	// The target exists and cannot run: it declares a secret nothing has set,
	// which is D10's misconfigured state. Rewritten after the first load
	// rather than before it, so the chain is wired to a job that was fine when
	// the wiring was written -- which is how this happens in practice.
	write(t, filepath.Join(treeDir(e), "rollup.yaml"),
		"command: [\"echo\", \"hi\"]\nsecrets: [MISSING_TOKEN]\n")
	if _, err := e.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := runJob(t, e, "extract", engine.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if executed := drainQueue(t, e); executed != 0 {
		t.Fatalf("%d runs started for a job that cannot run", executed)
	}

	// The failure mode this guards against is silence: the upstream job
	// succeeded, the file says what happens next, and nothing happens. A row
	// naming the rule and the reason is the whole difference (P1).
	events, err := e.RecentEvents(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Type != engine.EventRouteFailed {
			continue
		}
		if !strings.Contains(string(ev.Payload), "MISSING_TOKEN") {
			t.Errorf("route.failed does not say why: %s", ev.Payload)
		}
		if !strings.Contains(string(ev.Payload), `"chain":"src/daily"`) {
			t.Errorf("route.failed does not name the chain: %s", ev.Payload)
		}
		return
	}
	t.Fatal("a rule matched, could not fire, and left no record of it")
}

func TestOneEventFansOutToEveryMatchingStep(t *testing.T) {
	ctx := context.Background()
	e, _ := chainFixture(t,
		map[string]string{
			"extract":  `echo extracting`,
			"report-a": `echo a`,
			"report-b": `echo b`,
			"report-c": `echo c`,
			"report-d": `echo d`,
		},
		map[string]string{"reports": `
description: one extract, four reports off the back of it
steps:
  - on: { event: run.succeeded, where: { job: extract } }
    run: report-a
  - on: { event: run.succeeded, where: { job: extract } }
    run: report-b
  - on: { event: run.succeeded, where: { job: extract } }
    run: report-c
  - on: { event: run.succeeded, where: { job: extract } }
    run: report-d
`})

	if _, err := runJob(t, e, "extract", engine.RunOptions{}); err != nil {
		t.Fatal(err)
	}

	// All four are queued by the one run.succeeded event, before any of them
	// starts: fan-out is not a feature, it is what happens when four rules
	// match one event.
	queued, err := e.Waiting(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued.Queued) != 4 {
		t.Fatalf("queued = %d, want all four reports waiting at once", len(queued.Queued))
	}

	if executed := drainQueue(t, e); executed != 4 {
		t.Fatalf("ran %d reports, want 4", executed)
	}

	view, err := e.Chain(ctx, "reports")
	if err != nil {
		t.Fatal(err)
	}
	if view.State != engine.ChainComplete {
		t.Fatalf("state = %q, want complete", view.State)
	}
	// Every step hangs off the same trigger, so the view has to find them from
	// one run rather than by walking a line.
	for _, s := range view.Steps {
		if s.Run == nil {
			t.Errorf("step %d (%s) was not found by the chain view", s.Step, s.Job)
		}
	}
	if view.Trigger == nil || view.Trigger.Job != qual("extract") {
		t.Fatalf("trigger = %+v", view.Trigger)
	}
}
