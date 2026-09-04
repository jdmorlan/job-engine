package engine_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jdmorlan/job-engine/internal/engine"
	"github.com/jdmorlan/job-engine/internal/model"
)

// P2: the engine's own work is jobs. These tests are the forcing function that
// rider names -- if retention is awkward to express as a job, the job format
// has a hole, and it should be this repository that finds it.

func TestTheEngineShipsItsOwnJobs(t *testing.T) {
	ctx := context.Background()
	e := newEngine(t, tempLayout(t), nil)
	defer e.Close(ctx)
	if err := e.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	jobs, err := e.Jobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var retention string
	for _, j := range jobs {
		if !engine.IsSystem(j.Slug) {
			t.Errorf("a fresh engine loaded a job that is not its own: %s", j.Slug)
			continue
		}
		if j.LoadError != "" || j.ConfigError != "" {
			t.Fatalf("%s does not load: %s%s", j.Slug, j.LoadError, j.ConfigError)
		}
		if strings.HasSuffix(j.Slug, "/retention") {
			retention = j.Slug
		}
	}
	if retention == "" {
		t.Fatalf("no retention job among %d system job(s)", len(jobs))
	}

	// It must be a real job in every sense the rest of the engine cares about,
	// or P2 is decoration: schedulable, explainable, dispatchable.
	x, err := e.Explain(ctx, retention)
	if err != nil {
		t.Fatalf("explaining the engine's own job: %v", err)
	}
	if len(x.Triggers) == 0 {
		t.Error("the retention job has nothing that starts it")
	}
}

// The engine registers its own source at every start, so removing it would be
// undone by the next restart. A command whose effect expires on reboot is
// worse than one that refuses (P1).
func TestTheSystemSourceCannotBeUnregistered(t *testing.T) {
	ctx := context.Background()
	e := newEngine(t, tempLayout(t), nil)
	defer e.Close(ctx)
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RemoveSource(ctx, "system"); err == nil {
		t.Fatal("the engine's own source was unregistered")
	}
}

// An upgrade changes definitions that are compiled into the binary, and there
// is no sync anybody would think to run. Loading at every start is what makes
// the new version's jobs the ones that run.
func TestSystemJobsFollowTheBinaryAcrossAnUpgrade(t *testing.T) {
	ctx := context.Background()
	layout := tempLayout(t)

	first := newEngine(t, layout, nil)
	if err := first.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(ctx); err != nil {
		t.Fatal(err)
	}

	// The same data directory, a different version. The source's revision is
	// the engine's version rather than a commit, so this is what a source
	// pointing at new code looks like here (D11).
	upgraded, err := engine.New(engine.Options{Layout: layout, Version: "v9.9.9"})
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close(ctx)
	if err := upgraded.Start(ctx); err != nil {
		t.Fatal(err)
	}

	sources, err := upgraded.Sources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sources {
		if s.Name != "system" {
			continue
		}
		if s.Revision != "v9.9.9" {
			t.Fatalf("the system source still reports revision %q after an upgrade", s.Revision)
		}
		if s.LastError != "" {
			t.Fatalf("the system source failed to load: %s", s.LastError)
		}
		return
	}
	t.Fatal("no system source after restarting")
}

// The whole point of the shape: a system job is dispatched to a worker like any
// other, and the worker is told to run it as itself (P2, C1, C11).
func TestASystemJobIsDispatchedLikeAnyOther(t *testing.T) {
	ctx := context.Background()
	e := newEngine(t, tempLayout(t), nil)
	defer e.Close(ctx)
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	workerID := ensureWorker(t, e)

	run, err := e.TriggerRun(ctx, "system/retention", engine.RunOptions{Actor: "tester"})
	if err != nil {
		t.Fatalf("triggering the engine's own job: %v", err)
	}
	d, err := e.Claim(ctx, workerID)
	if err != nil {
		t.Fatal(err)
	}
	if d == nil {
		t.Fatal("no worker could take the engine's own job")
	}
	if d.RunID != run.ID {
		t.Fatalf("claimed run %d, want %d", d.RunID, run.ID)
	}
	if !d.System {
		t.Error("the dispatch does not say this is the engine's own work, so the worker " +
			"would run whatever `je` is on its PATH, against no data directory")
	}
	// No tree, and nothing to fetch. Offering either would send the worker
	// looking for a directory that has never existed.
	if d.SourceRoot != "" || d.SourceName != "" || d.SourceRevision != "" {
		t.Errorf("the dispatch offers a tree: root=%q name=%q revision=%q",
			d.SourceRoot, d.SourceName, d.SourceRevision)
	}
}

// A run of a system job is an ordinary run, which is the property that makes
// this worth doing at all: it has a status, an attempt, and a place in history.
func TestSweepIsRecordedOnTheTimeline(t *testing.T) {
	ctx := context.Background()
	e := newEngine(t, tempLayout(t), nil)
	defer e.Close(ctx)
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := e.Sweep(ctx, "tester"); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	events, err := e.RecentEvents(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Type != engine.EventRetentionSwept {
			continue
		}
		if ev.Actor != "tester" {
			t.Errorf("the sweep is attributed to %q", ev.Actor)
		}
		if ev.Source != model.SourceEngine {
			t.Errorf("sweep event source = %q", ev.Source)
		}
		return
	}
	t.Fatal("a sweep left nothing on the timeline; deletion is the one operation " +
		"that erases its own evidence, so it has to write down that it happened")
}
