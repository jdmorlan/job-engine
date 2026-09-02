package engine_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jdmorlan/job-engine/internal/engine"
	"github.com/jdmorlan/job-engine/internal/model"
	"github.com/jdmorlan/job-engine/internal/paths"
)

// newEngine builds an engine over a throwaway data directory.
//
// It does not register a cleanup that closes the engine, because several tests
// need to close and reopen to exercise restart behaviour. Closing is the
// test's job.
func newEngine(t *testing.T, layout paths.Layout, now func() time.Time) *engine.Engine {
	t.Helper()
	e, err := engine.New(engine.Options{Layout: layout, Version: "test", Now: now})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	return e
}

func tempLayout(t *testing.T) paths.Layout {
	t.Helper()
	dir := t.TempDir()
	return paths.Layout{Data: dir, Jobs: dir + "/jobs"}
}

func TestStartAndStopAreRecorded(t *testing.T) {
	ctx := context.Background()
	layout := tempLayout(t)

	e := newEngine(t, layout, nil)
	if err := e.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := e.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen and read the timeline back. D16's promise is that the engine's own
	// downtime is a queryable fact rather than a hole, so the events must
	// survive the process that wrote them.
	e2 := newEngine(t, layout, nil)
	defer e2.Close(ctx)

	events, err := e2.RecentEvents(ctx, 10)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	// Newest first, and e2 has not started yet, so we expect stop then start.
	want := []string{model.EventEngineStopped, model.EventEngineStarted}
	if len(events) != len(want) {
		t.Fatalf("got %d events, want %d: %+v", len(events), len(want), events)
	}
	for i, w := range want {
		if events[i].Type != w {
			t.Errorf("event %d: got %q, want %q", i, events[i].Type, w)
		}
		if events[i].Source != model.SourceEngine {
			t.Errorf("event %d: source %q, want %q", i, events[i].Source, model.SourceEngine)
		}
	}
}

func TestDowntimeIsMeasuredAcrossRestart(t *testing.T) {
	ctx := context.Background()
	layout := tempLayout(t)

	// A controllable clock, because the whole point of this behaviour is what
	// happens across a gap and we are not going to sleep for one.
	clock := time.Date(2026, 9, 2, 3, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }

	e := newEngine(t, layout, now)
	if err := e.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := e.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The laptop sleeps for four hours (D15: macOS suspends the daemon, so
	// this is the normal case, not an exotic one).
	clock = clock.Add(4 * time.Hour)

	e2 := newEngine(t, layout, now)
	defer e2.Close(ctx)
	if err := e2.Start(ctx); err != nil {
		t.Fatalf("Start after gap: %v", err)
	}

	health := e2.Health(context.Background())
	if health.LastDowntime != 4*time.Hour {
		t.Errorf("LastDowntime = %s, want 4h", health.LastDowntime)
	}
	if health.UncleanStop {
		t.Error("UncleanStop = true after a clean shutdown")
	}
}

func TestUncleanStopIsDetected(t *testing.T) {
	ctx := context.Background()
	layout := tempLayout(t)

	// Start, then abandon the engine without Close -- the shape of a crash or
	// a SIGKILL. Releasing the lock directly is how we simulate the kernel
	// doing it for us on process exit.
	e := newEngine(t, layout, nil)
	if err := e.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	crash(t, e)

	e2 := newEngine(t, layout, nil)
	defer e2.Close(ctx)
	if err := e2.Start(ctx); err != nil {
		t.Fatalf("Start after crash: %v", err)
	}
	if !e2.Health(context.Background()).UncleanStop {
		t.Error("UncleanStop = false after a start with no matching stop")
	}
}

func TestSecondEngineIsRefused(t *testing.T) {
	ctx := context.Background()
	layout := tempLayout(t)

	e := newEngine(t, layout, nil)
	defer e.Close(ctx)

	// D18: two engines on one database double-fire every schedule. This must
	// be a refusal at startup, not something discovered from duplicate runs.
	_, err := engine.New(engine.Options{Layout: layout})
	if err == nil {
		t.Fatal("second engine started; expected it to be refused")
	}
	if !strings.Contains(err.Error(), "already using") {
		t.Errorf("error does not explain the conflict: %v", err)
	}
}

func TestEmitDeduplicates(t *testing.T) {
	ctx := context.Background()
	e := newEngine(t, tempLayout(t), nil)
	defer e.Close(ctx)
	if err := e.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	key := "motion-2026-09-02T03:00"
	first, deduped, err := e.Emit(ctx, model.Event{
		Type:      "homekit.motion",
		Source:    model.SourceCLI,
		Payload:   json.RawMessage(`{"room":"office"}`),
		DedupeKey: &key,
	})
	if err != nil {
		t.Fatalf("first Emit: %v", err)
	}
	if deduped {
		t.Fatal("first emit reported as a duplicate")
	}

	second, deduped, err := e.Emit(ctx, model.Event{
		Type:      "homekit.motion",
		Source:    model.SourceCLI,
		DedupeKey: &key,
	})
	if err != nil {
		t.Fatalf("second Emit: %v", err)
	}
	if !deduped {
		t.Error("second emit with the same key was not deduped")
	}
	if second.ID != first.ID {
		t.Errorf("deduped emit returned event %d, want the original %d", second.ID, first.ID)
	}
	// The original must come back untouched, payload included -- a dedupe is
	// "this already happened", not "here is a blank event with the same id".
	if string(second.Payload) != `{"room":"office"}` {
		t.Errorf("deduped emit lost the original payload: %q", second.Payload)
	}
}

func TestEmitRequiresAType(t *testing.T) {
	ctx := context.Background()
	e := newEngine(t, tempLayout(t), nil)
	defer e.Close(ctx)

	if _, _, err := e.Emit(ctx, model.Event{Source: model.SourceCLI}); err == nil {
		t.Error("emitting a typeless event succeeded; expected an error")
	}
}

// crash releases the engine's resources the way process death would, without
// giving it the chance to write engine.stopped.
func crash(t *testing.T, e *engine.Engine) {
	t.Helper()
	if err := engine.Abandon(e); err != nil {
		t.Fatalf("abandoning engine: %v", err)
	}
}
