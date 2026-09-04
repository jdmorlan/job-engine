package engine_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jdmorlan/job-engine/internal/engine"
	"github.com/jdmorlan/job-engine/internal/model"
)

// runScheduler starts the scheduler, attaches a worker, and stops both once
// cond holds or a deadline passes. Polling rather than sleeping a fixed time,
// so the test is neither flaky nor slower than it needs to be.
//
// The worker is not optional scaffolding. D20/C11 means the scheduler only
// queues -- a control plane with nothing attached runs nothing, which is the
// behaviour C8 requires it to be loud about, and a scheduler test that did not
// attach one would wait forever for runs that were never going to happen.
func runScheduler(t *testing.T, e *engine.Engine, cond func() bool) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.RunScheduler(ctx) }()

	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		pumpWorker(t, e, ctx)
	}()
	defer func() { <-workerDone }()

	deadline := time.After(20 * time.Second)
	for !cond() {
		select {
		case err := <-done:
			cancel()
			t.Fatalf("scheduler exited early: %v", err)
		case <-deadline:
			cancel()
			<-done
			t.Fatal("condition never became true")
		case <-time.After(20 * time.Millisecond):
		}
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("RunScheduler: %v", err)
	}
}

func succeededCount(t *testing.T, e *engine.Engine, slug string) int {
	t.Helper()
	ctx := context.Background()
	job, err := e.Job(ctx, slug)
	if err != nil {
		return 0
	}
	runs, err := e.Runs(ctx, job.ID, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	for _, r := range runs {
		if r.Status == model.StatusSucceeded {
			n++
		}
	}
	return n
}

func runCount(t *testing.T, e *engine.Engine, slug string) int {
	t.Helper()
	ctx := context.Background()
	job, err := e.Job(ctx, slug)
	if err != nil {
		return 0
	}
	runs, err := e.Runs(ctx, job.ID, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return len(runs)
}

func TestScheduleFiresOnItsGrid(t *testing.T) {
	// A one-second interval so the test is quick; the grid logic is identical
	// at 15m and is unit-tested in internal/schedule.
	e, _ := jobFixture(t, "ticker", `echo tick`, "on:", "  - every: 1s")

	runScheduler(t, e, func() bool { return succeededCount(t, e, "ticker") >= 2 })
}

// TestScheduleIsAnchoredNotBackfilled pins the first-sight behaviour. A new
// hourly job must not fire once for every hour since the epoch.
func TestScheduleIsAnchoredNotBackfilled(t *testing.T) {
	ctx := context.Background()
	e, _ := jobFixture(t, "hourly", `echo hi`, "on:", "  - cron: \"0 * * * *\"")

	// Start and stop immediately: reload anchors the schedule, and nothing is
	// due within the next hour.
	runScheduler(t, e, func() bool {
		events, err := e.RecentEvents(ctx, 50)
		if err != nil {
			return false
		}
		for _, ev := range events {
			if ev.Type == engine.EventScheduleStarted {
				return true
			}
		}
		return false
	})

	if n := runCount(t, e, "hourly"); n != 0 {
		t.Errorf("a newly seen schedule fired %d times; it must anchor, not backfill", n)
	}
}

// TestCatchUpPolicies is the heart of D9. All three agree in normal operation
// and only differ after a gap, which is exactly when the difference matters.
func TestCatchUpPolicies(t *testing.T) {
	tests := []struct {
		policy   string
		wantRuns int
		reason   string
	}{
		{"skip", 0, "a gap is skipped entirely; the job resumes from now"},
		{"once", 1, "one run catches up on the whole gap, using the job's cursor"},
		{"all", 4, "one run per missed window, serialised by the queue"},
	}

	for _, tc := range tests {
		t.Run(tc.policy, func(t *testing.T) {
			ctx := context.Background()
			extra := []string{"on:", "  - every: 1m", "    catch_up: " + tc.policy}
			if tc.policy == "all" {
				// catch_up: all requires overlap: queue -- the engine refuses
				// the contradictory combination at load time.
				extra = append(extra, "overlap: queue")
			}

			// A frozen clock, so the gap is exactly four windows and no real
			// minute boundary can slide past mid-test.
			frozen := time.Now().Truncate(time.Minute).Add(30 * time.Second)
			e, _ := jobFixtureAt(t, "gappy", `echo hi`,
				func() time.Time { return frozen }, extra...)

			job, err := e.Job(ctx, "gappy")
			if err != nil {
				t.Fatal(err)
			}
			// Simulate the laptop having been asleep across four windows.
			past := frozen.Truncate(time.Minute).Add(-4 * time.Minute)
			if err := engine.SetLastWindowForTest(e, job.ID, 0, past); err != nil {
				t.Fatal(err)
			}

			runScheduler(t, e, func() bool {
				if tc.wantRuns == 0 {
					// Nothing should fire; wait for the missed event instead.
					events, _ := e.RecentEvents(ctx, 50)
					for _, ev := range events {
						if ev.Type == engine.EventScheduleMissed {
							return true
						}
					}
					return false
				}
				return runCount(t, e, "gappy") >= tc.wantRuns
			})

			got := runCount(t, e, "gappy")
			if got != tc.wantRuns {
				t.Errorf("%s: %d runs, want %d (%s)", tc.policy, got, tc.wantRuns, tc.reason)
			}
		})
	}
}

// TestMissedWindowsAreRecorded is D9's other half: a gap must be explained
// rather than being an unexplained hole in the timeline.
func TestMissedWindowsAreRecorded(t *testing.T) {
	ctx := context.Background()
	e, _ := jobFixture(t, "sleeper", `echo hi`,
		"on:", "  - every: 1m", "    catch_up: skip")

	job, err := e.Job(ctx, "sleeper")
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-10 * time.Minute).Truncate(time.Minute)
	if err := engine.SetLastWindowForTest(e, job.ID, 0, past); err != nil {
		t.Fatal(err)
	}

	var missed model.Event
	runScheduler(t, e, func() bool {
		events, _ := e.RecentEvents(ctx, 50)
		for _, ev := range events {
			if ev.Type == engine.EventScheduleMissed {
				missed = ev
				return true
			}
		}
		return false
	})

	var payload struct {
		Job     string `json:"job"`
		Policy  string `json:"policy"`
		Skipped int    `json:"skipped"`
	}
	if err := json.Unmarshal(missed.Payload, &payload); err != nil {
		t.Fatalf("parsing schedule.missed payload: %v", err)
	}
	if payload.Job != qual("sleeper") || payload.Policy != "skip" {
		t.Errorf("payload = %+v", payload)
	}
	// Ten minutes on a one-minute grid. The exact count depends on where the
	// truncation lands, but it must be a real number rather than zero -- the
	// count is the whole point of the event.
	if payload.Skipped < 9 {
		t.Errorf("skipped = %d, want about 10", payload.Skipped)
	}
}

// TestOverlapSkipIsRecorded covers D8. A job that quietly does not run is the
// most confusing thing a scheduler can do, so the decision is an event.
func TestOverlapSkipIsRecorded(t *testing.T) {
	ctx := context.Background()
	// The job sleeps longer than its own interval, so the second window
	// necessarily overlaps the first.
	e, _ := jobFixture(t, "slowloop", `sleep 3`,
		"on:", "  - every: 1s", "overlap: skip")

	var skipped model.Event
	runScheduler(t, e, func() bool {
		events, _ := e.RecentEvents(ctx, 100)
		for _, ev := range events {
			if ev.Type == engine.EventRunSkipped {
				skipped = ev
				return true
			}
		}
		return false
	})

	if !strings.Contains(string(skipped.Payload), "slowloop") {
		t.Errorf("run.skipped payload does not name the job: %s", skipped.Payload)
	}
	// The reason has to be in there, or the event is just a different flavour
	// of silence.
	if !strings.Contains(string(skipped.Payload), "overlap") {
		t.Errorf("run.skipped payload does not explain why: %s", skipped.Payload)
	}
}

func TestWaitingShowsScheduledAndBlocked(t *testing.T) {
	ctx := context.Background()
	e, _ := jobFixture(t, "scheduled", `echo hi`, "on:", "  - every: 1h")

	w, err := e.Waiting(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Scheduled) != 1 {
		t.Fatalf("scheduled = %+v, want one entry", w.Scheduled)
	}
	if w.Scheduled[0].Job != qual("scheduled") {
		t.Errorf("job = %q", w.Scheduled[0].Job)
	}
	if !w.Scheduled[0].Next.After(time.Now()) {
		t.Errorf("next window %s is not in the future", w.Scheduled[0].Next)
	}
	if w.NeedsAttention() {
		t.Error("a healthy engine reported that it needs attention")
	}
}

func TestWaitingReportsBlockedJobs(t *testing.T) {
	ctx := context.Background()
	e, _ := jobFixture(t, "blocked", `echo hi`, "secrets: [MISSING_TOKEN]")

	w, err := e.Waiting(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Blocked) != 1 {
		t.Fatalf("blocked = %+v, want one entry", w.Blocked)
	}
	if !w.NeedsAttention() {
		t.Error("a blocked job did not set the attention flag; `je waiting` would exit 0")
	}
}
