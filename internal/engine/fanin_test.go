package engine_test

import (
	"context"
	"github.com/jdmorlan/job-engine/internal/store"
	"testing"

	"github.com/jdmorlan/job-engine/internal/engine"
)

// Fan-in: a step that waits for two things, and runs when both have landed
// inside its window (D3).
//
// The property that matters is that one event is not enough and two are, in
// either order -- which is the whole feature and also the thing that is easy to
// get subtly wrong when the state lives in a row.
func TestAStepWaitsForEveryConditionBeforeItRuns(t *testing.T) {
	ctx := context.Background()
	e, _ := chainFixture(t,
		map[string]string{
			"extract-weather": `echo weather`,
			"extract-power":   `echo power`,
			"rollup":          `echo rollup`,
		},
		map[string]string{"daily": `
steps:
  - on:
      all_of:
        - { event: run.succeeded, where: { job: src/extract-weather } }
        - { event: run.succeeded, where: { job: src/extract-power } }
      within: 6h
    run: rollup
`})

	runsOf := func(slug string) int {
		t.Helper()
		job := jobBySlugRaw(t, e, slug)
		runs, err := e.Runs(ctx, job.ID, 50)
		if err != nil {
			t.Fatal(err)
		}
		return len(runs)
	}
	// Trigger a job and let a worker take it, which is what produces the
	// run.succeeded event a condition matches on.
	land := func(slug string) {
		t.Helper()
		if _, err := e.TriggerRun(ctx, slug, engine.RunOptions{}); err != nil {
			t.Fatal(err)
		}
		drainQueue(t, e)
	}

	// One half of the pair. The rollup must not run: a fan-in that fires on the
	// first condition is just an ordinary trigger with extra words.
	land(qual("extract-weather"))
	if got := runsOf(qual("rollup")); got != 0 {
		t.Fatalf("rollup ran %d times after one condition; it needs both", got)
	}

	// And it says so, which is the part D3 calls the feature.
	waiting, err := e.Waiting(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(waiting.Triggers) != 1 {
		t.Fatalf("pending triggers = %v, want the half-satisfied rollup", waiting.Triggers)
	}
	pending := waiting.Triggers[0]
	if len(pending.Satisfied) != 1 || len(pending.Waiting) != 1 {
		t.Errorf("satisfied %v, waiting on %v -- want one of each",
			pending.Satisfied, pending.Waiting)
	}
	if pending.Job != qual("rollup") {
		t.Errorf("pending trigger is for %q, want %q", pending.Job, qual("rollup"))
	}

	// The other half completes the set.
	land(qual("extract-power"))
	if got := runsOf(qual("rollup")); got != 1 {
		t.Fatalf("rollup ran %d times after both conditions, want 1", got)
	}

	// And the pending view is empty again, because the events were consumed.
	if waiting, err = e.Waiting(ctx); err != nil {
		t.Fatal(err)
	}
	if len(waiting.Triggers) != 0 {
		t.Errorf("a fired trigger is still shown as waiting: %v", waiting.Triggers)
	}

	// once_per_window: the same pair must not fire it twice. One more of the
	// first condition leaves it half satisfied again rather than re-firing.
	land(qual("extract-weather"))
	if got := runsOf(qual("rollup")); got != 1 {
		t.Errorf("rollup ran %d times; the consumed pair fired it again", got)
	}
}

// jobBySlugRaw looks a job up by its full name, without qualifying it again.
func jobBySlugRaw(t *testing.T, e *engine.Engine, slug string) store.Job {
	t.Helper()
	jobs, err := e.Jobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if j.Slug == slug {
			return j
		}
	}
	t.Fatalf("no job called %q", slug)
	return store.Job{}
}
