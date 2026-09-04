package engine_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jdmorlan/job-engine/internal/engine"
)

// D13, tested against history that is actually old rather than against a
// cutoff moved to meet it: the engine's clock is the test's, so a run really
// does end forty days before the sweep looks at it.

func TestOldRunsAreRemovedAndRecentOnesAreNot(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	e, _ := jobFixtureAt(t, "chatty", "echo hello", c.now)

	// Two runs forty days apart, so one is past a thirty day keep period and
	// the other is not.
	old, err := runJob(t, e, "chatty", engine.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c.advance(40 * 24 * time.Hour)
	recent, err := runJob(t, e, "chatty", engine.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := e.Logs(ctx, old.Run.ID, 1); err != nil {
		t.Fatal(err)
	}

	swept, err := e.Sweep(ctx, engine.DefaultPolicy, "tester")
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if swept.Removed.Runs != 1 {
		t.Fatalf("removed %d run(s), want 1 (the forty day old one)", swept.Removed.Runs)
	}
	if swept.Removed.LogLines == 0 {
		t.Error("the removed run's output is still stored")
	}

	if _, err := e.Run(ctx, old.Run.ID); err == nil {
		t.Error("a run past the keep period is still in the history")
	}
	if _, err := e.Run(ctx, recent.Run.ID); err != nil {
		t.Errorf("a run inside the keep period was removed: %v", err)
	}

	// The count survives the rows, which is the only way anybody can tell a
	// truncated history from a short one (P1).
	job, err := e.Job(ctx, old.Job.Slug)
	if err != nil {
		t.Fatal(err)
	}
	if job.RunsRemoved != 1 {
		t.Errorf("the job records %d removed run(s), want 1", job.RunsRemoved)
	}
}

// C2: job state never expires and points at the run that moved the cursor, so
// that run is kept however old it is. "What set this cursor?" is the question
// D14 exists to answer.
func TestARunACursorPointsAtIsKept(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	script := `echo '{"since":"2026-01-01T00:00:00Z"}' > "$JOB_STATE_OUT_FILE"`
	e, _ := jobFixtureAt(t, "ingest", script, c.now)

	moved, err := runJob(t, e, "ingest", engine.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if moved.StateOut == nil {
		t.Fatal("the fixture did not move its cursor, so this test proves nothing")
	}

	c.advance(90 * 24 * time.Hour)
	swept, err := e.Sweep(ctx, engine.DefaultPolicy, "tester")
	if err != nil {
		t.Fatal(err)
	}
	if swept.Removed.Runs != 0 {
		t.Errorf("removed %d run(s); the run that set the current cursor must be kept",
			swept.Removed.Runs)
	}
	if swept.Removed.Pinned == 0 {
		t.Error("the kept run is not reported as pinned, so nobody can tell why it is still here")
	}
	if _, err := e.Run(ctx, moved.Run.ID); err != nil {
		t.Fatalf("the run that set the cursor was deleted: %v", err)
	}

	// And the cursor itself is untouched: losing it means reprocessing from
	// the beginning, which is the one thing retention must never cost.
	if _, err := e.CurrentState(ctx, moved.Job.ID); err != nil {
		t.Fatalf("the cursor did not survive a sweep: %v", err)
	}
}

// D13's escape hatch. A job that says its output matters keeps it, and the
// deployment's period does not apply.
func TestKeepLogsForeverExemptsAJob(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	e, _ := jobFixtureAt(t, "audit", "echo an audit trail", c.now, "keep_logs: forever")

	run, err := runJob(t, e, "audit", engine.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c.advance(90 * 24 * time.Hour)
	if _, err := e.Sweep(ctx, engine.DefaultPolicy, "tester"); err != nil {
		t.Fatal(err)
	}

	lines, err := e.Logs(ctx, run.Run.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) == 0 {
		t.Fatal("keep_logs: forever did not keep the logs")
	}

	// And the run with them. `je logs` is addressed by run id, so a kept log
	// whose run has been deleted is bytes on the disk that nothing can reach --
	// the worst of both, and what the first version of this did.
	if _, err := e.Run(ctx, run.Run.ID); err != nil {
		t.Fatalf("the run whose logs are kept forever was deleted: %v", err)
	}
}

// D13's coupling rule, which is the reason it is a rule and not a footgun:
// events expiring before runs breaks `je why` for runs still in the history.
func TestEventsMayNotBeKeptForLessTimeThanRuns(t *testing.T) {
	ctx := context.Background()
	e := newEngine(t, tempLayout(t), nil)
	defer e.Close(ctx)
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}

	_, err := e.Sweep(ctx, engine.Policy{
		Runs: 30 * engine.Day, Logs: 30 * engine.Day, Events: 7 * engine.Day,
	}, "tester")
	if err == nil {
		t.Fatal("a policy that expires the timeline before the runs it explains was accepted")
	}
	if !strings.Contains(err.Error(), "je why") {
		t.Errorf("the refusal does not say what breaks: %v", err)
	}
}

func TestPeriodsAreWrittenTheWayPeopleWriteThem(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{"30d", 30 * engine.Day},
		{"1d", engine.Day},
		{"12h", 12 * time.Hour},
		{"90m", 90 * time.Minute},
	} {
		got, err := engine.ParsePeriod(tc.in)
		if err != nil {
			t.Errorf("%q: %v", tc.in, err)
		} else if got != tc.want {
			t.Errorf("%q = %s, want %s", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"", "0d", "-3d", "soon", "30 days", "1w"} {
		if _, err := engine.ParsePeriod(bad); err == nil {
			t.Errorf("%q was accepted as a period", bad)
		}
	}
}

// A sweep is a job with a timeout, so it stops when it has done enough and says
// what is left. A cap that stops quietly looks exactly like one with nothing to
// do (P1).
func TestASweepReportsWhatItCouldNotFinish(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	e, _ := jobFixtureAt(t, "chatty", "true", c.now)

	for i := 0; i < 3; i++ {
		if _, err := runJob(t, e, "chatty", engine.RunOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	c.advance(40 * 24 * time.Hour)

	policy := engine.DefaultPolicy
	policy.MaxRuns = 1
	swept, err := e.Sweep(ctx, policy, "tester")
	if err != nil {
		t.Fatal(err)
	}
	if swept.Removed.Runs != 1 {
		t.Fatalf("removed %d run(s) with a cap of 1", swept.Removed.Runs)
	}
	if swept.Removed.RunsLeft != 2 {
		t.Errorf("reports %d run(s) left, want 2", swept.Removed.RunsLeft)
	}
}
