package engine_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jdmorlan/job-engine/internal/engine"
	"github.com/jdmorlan/job-engine/internal/model"
)

// D7's automatic half, tested through the real seam: a worker claims, runs,
// reports a failure, and the control plane decides whether that was the end of
// the run or the end of one attempt.

// clock is a test clock that starts frozen and moves when the test says so.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func newClock() *clock {
	return &clock{at: time.Date(2026, 9, 4, 3, 0, 0, 0, time.UTC)}
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// countingScript fails until it has been run n times, using a file in a
// scratch directory as the counter.
//
// A real failing process rather than a stubbed result: the whole question is
// what the control plane does with what a worker reports, and the worker
// reports what a process did.
func countingScript(t *testing.T, succeedOn int) string {
	t.Helper()
	counter := filepath.Join(t.TempDir(), "attempts")
	return fmt.Sprintf(`
		n=$(cat %[1]q 2>/dev/null || echo 0)
		n=$((n + 1))
		echo $n > %[1]q
		echo "attempt $n"
		if [ "$n" -ge %[2]d ]; then exit 0; fi
		exit 7
	`, counter, succeedOn)
}

func TestAFailingJobIsRetriedUntilItSucceeds(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	e, _ := jobFixtureAt(t, "flaky", countingScript(t, 3), c.now,
		"retry:", "  max_attempts: 3", "  initial_delay: 10s")

	run, err := e.TriggerRun(ctx, "flaky", engine.RunOptions{Actor: "tester"})
	if err != nil {
		t.Fatalf("triggering: %v", err)
	}

	// Attempt 1 fails. The run is not over: it is waiting out a backoff, and
	// that is a state a person can see rather than a gap in the record.
	if n := drainQueue(t, e); n != 1 {
		t.Fatalf("claimed %d runs on the first pass, want 1", n)
	}
	after1, err := e.Run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after1.Status != model.StatusRetrying {
		t.Fatalf("after one failure the run is %s, want %s", after1.Status, model.StatusRetrying)
	}
	if after1.EndedAt != nil {
		t.Error("a run between attempts has an ended_at, so it will report a duration for work still to come")
	}
	if after1.NextAttemptAt == nil {
		t.Fatal("a retrying run has no next_attempt_at, so nothing says when it comes back")
	}
	if want := c.now().Add(10 * time.Second); !after1.NextAttemptAt.Equal(want) {
		t.Errorf("next attempt at %s, want %s (initial_delay)", after1.NextAttemptAt, want)
	}

	// The backoff is real: nothing may claim the run before its time.
	if n := drainQueue(t, e); n != 0 {
		t.Fatalf("claimed %d runs during the backoff, want 0", n)
	}

	// Attempt 2 fails as well, and waits twice as long (exponential, the default).
	c.advance(10 * time.Second)
	if n := drainQueue(t, e); n != 1 {
		t.Fatalf("claimed %d runs for attempt 2, want 1", n)
	}
	after2, err := e.Run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := c.now().Add(20 * time.Second); after2.NextAttemptAt == nil || !after2.NextAttemptAt.Equal(want) {
		t.Errorf("second backoff ends at %v, want %s (doubled)", after2.NextAttemptAt, want)
	}

	// Attempt 3 succeeds, which is the run succeeding -- the earlier failures
	// are attempts, and the run is the unit of intent (D7).
	c.advance(20 * time.Second)
	if n := drainQueue(t, e); n != 1 {
		t.Fatalf("claimed %d runs for attempt 3, want 1", n)
	}
	detail, err := e.RunDetail(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Run.Status != model.StatusSucceeded {
		t.Fatalf("run %d is %s: %s", run.ID, detail.Run.Status, detail.Run.Error)
	}
	if len(detail.Attempts) != 3 {
		t.Fatalf("%d attempts, want 3", len(detail.Attempts))
	}
	if detail.Run.NextAttemptAt != nil {
		t.Error("a finished run still carries a next_attempt_at")
	}

	// Every attempt has its own outcome and its own cause. That is the whole
	// reason attempts are rows: "did this eventually succeed?" and "how many
	// times did it take?" are different questions.
	wantStatus := []model.Status{model.StatusFailed, model.StatusFailed, model.StatusSucceeded}
	for i, a := range detail.Attempts {
		if a.Status != wantStatus[i] {
			t.Errorf("attempt %d is %s, want %s", a.Number, a.Status, wantStatus[i])
		}
	}
	if detail.Attempts[0].Actor != "tester" {
		t.Errorf("attempt 1 actor = %q, want the person who asked for the run", detail.Attempts[0].Actor)
	}
	for _, a := range detail.Attempts[1:] {
		if a.Actor != "" {
			t.Errorf("attempt %d is attributed to %q, but nobody asked for it", a.Number, a.Actor)
		}
	}

	// Each attempt kept its own logs, so "what did attempt 2 print?" survives.
	for _, n := range []int{1, 2, 3} {
		lines, err := e.Logs(ctx, run.ID, n)
		if err != nil {
			t.Fatal(err)
		}
		if len(lines) != 1 || lines[0].Line != fmt.Sprintf("attempt %d", n) {
			t.Errorf("attempt %d logs = %+v", n, lines)
		}
	}

	// And the timeline says what happened: two retries, then a success.
	types := eventTypes(t, e, run.ID)
	want := []string{engine.EventAttemptFailed, engine.EventAttemptFailed, engine.EventRunSucceeded}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Errorf("timeline = %v, want %v", types, want)
	}
}

func TestAPendingRetrySurvivesARestart(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	e, layout := jobFixtureAt(t, "flaky", countingScript(t, 2), c.now,
		"retry:", "  max_attempts: 2", "  initial_delay: 30s")

	run, err := e.TriggerRun(ctx, "flaky", engine.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	drainQueue(t, e)
	if err := e.Close(ctx); err != nil {
		t.Fatal(err)
	}

	// The control plane goes away mid-backoff -- an upgrade, a reboot, a laptop
	// closing. The retry is a row, not a timer, so it has to still be there.
	e2 := newEngine(t, layout, c.now)
	defer e2.Close(ctx)
	if err := e2.Start(ctx); err != nil {
		t.Fatal(err)
	}

	reopened, err := e2.Run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Status != model.StatusRetrying {
		t.Fatalf("after a restart the run is %s, want %s -- a pending retry was swallowed",
			reopened.Status, model.StatusRetrying)
	}

	c.advance(30 * time.Second)
	if n := drainQueue(t, e2); n != 1 {
		t.Fatalf("claimed %d runs after the restart, want the pending retry", n)
	}
	after, err := e2.Run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != model.StatusSucceeded {
		t.Fatalf("run is %s, want succeeded", after.Status)
	}
}

func TestARunOutOfAttemptsFails(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	e, _ := jobFixtureAt(t, "doomed", "exit 1", c.now,
		"retry:", "  max_attempts: 2", "  initial_delay: 1s")

	run, err := e.TriggerRun(ctx, "doomed", engine.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	drainQueue(t, e)
	c.advance(time.Second)
	drainQueue(t, e)

	detail, err := e.RunDetail(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Run.Status != model.StatusFailed {
		t.Fatalf("run is %s, want failed", detail.Run.Status)
	}
	if len(detail.Attempts) != 2 {
		t.Fatalf("%d attempts, want 2 (max_attempts)", len(detail.Attempts))
	}
	// The last failure is a run.failed, not an attempt.failed: nothing follows
	// it, so it is the run ending rather than the cause of another attempt.
	types := eventTypes(t, e, run.ID)
	want := []string{engine.EventAttemptFailed, engine.EventRunFailed}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Errorf("timeline = %v, want %v", types, want)
	}
}

func TestWithoutARetryPolicyOneFailureEndsTheRun(t *testing.T) {
	e, _ := jobFixture(t, "once", "exit 1")

	result, err := runJob(t, e, "once", engine.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Run.Status != model.StatusFailed {
		t.Fatalf("run is %s, want failed", result.Run.Status)
	}
	// The default is one attempt, and it has to stay that way: an engine that
	// repeats a job nobody told it was safe to repeat is worse than one that
	// does not retry at all.
	for _, ty := range eventTypes(t, e, result.Run.ID) {
		if ty == engine.EventAttemptFailed {
			t.Fatal("a job with no retry policy was retried")
		}
	}
}

func TestAManualRetryAddsAnAttemptAndSaysWho(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	e, _ := jobFixtureAt(t, "flaky", countingScript(t, 2), c.now)

	run, err := e.TriggerRun(ctx, "flaky", engine.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	drainQueue(t, e)
	if failed, err := e.Run(ctx, run.ID); err != nil {
		t.Fatal(err)
	} else if failed.Status != model.StatusFailed {
		t.Fatalf("run is %s, want failed (no retry policy)", failed.Status)
	}

	// A person decides otherwise. Same run, same intent, one more attempt --
	// and max_attempts does not apply, because they made that judgement.
	if _, err := e.RetryRun(ctx, run.ID, "jay"); err != nil {
		t.Fatalf("RetryRun: %v", err)
	}
	if n := drainQueue(t, e); n != 1 {
		t.Fatalf("claimed %d runs after a manual retry, want 1", n)
	}

	detail, err := e.RunDetail(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Run.Status != model.StatusSucceeded {
		t.Fatalf("run is %s, want succeeded", detail.Run.Status)
	}
	if len(detail.Attempts) != 2 {
		t.Fatalf("%d attempts, want 2", len(detail.Attempts))
	}
	// This is the line D7 was written for: attempt 2 says a human intervened.
	if detail.Attempts[1].Actor != "jay" {
		t.Errorf("attempt 2 actor = %q, want jay", detail.Attempts[1].Actor)
	}
	if !hasEvent(t, e, run.ID, engine.EventRetryRequested) {
		t.Error("a manual retry left no retry.requested on the timeline")
	}
}

func TestASucceededRunIsNotRetried(t *testing.T) {
	ctx := context.Background()
	e, _ := jobFixture(t, "fine", "true")

	result, err := runJob(t, e, "fine", engine.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.RetryRun(ctx, result.Run.ID, "jay")
	if err == nil {
		t.Fatal("retrying a successful run was allowed, which would rewrite the record of work that already happened")
	}
	// The refusal has to name the alternative, or it is just a wall (P1).
	if !strings.Contains(err.Error(), "je run "+result.Job.Slug) {
		t.Errorf("refusal does not point at `je run`: %v", err)
	}
}

func TestARunningRunIsNotRetried(t *testing.T) {
	ctx := context.Background()
	e, _ := jobFixture(t, "slow", "sleep 30")
	workerID := ensureWorker(t, e)

	run, err := e.TriggerRun(ctx, "slow", engine.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Claim(ctx, workerID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RetryRun(ctx, run.ID, "jay"); err == nil {
		t.Fatal("a running run was retried, which would put two attempts in flight at once")
	}
}

// eventTypes lists what a run caused, oldest first.
func eventTypes(t *testing.T, e *engine.Engine, runID int64) []string {
	t.Helper()
	detail, err := e.RunDetail(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ev := range detail.Emitted {
		out = append(out, ev.Type)
	}
	return out
}

func hasEvent(t *testing.T, e *engine.Engine, runID int64, ty string) bool {
	t.Helper()
	for _, got := range eventTypes(t, e, runID) {
		if got == ty {
			return true
		}
	}
	return false
}
