package engine_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jdmorlan/job-engine/internal/engine"
	"github.com/jdmorlan/job-engine/internal/model"
	"github.com/jdmorlan/job-engine/internal/paths"
)

// jobFixture writes a job file into a fresh data directory and returns an
// engine with it loaded.
//
// Jobs are /bin/sh scripts rather than Python or Node so the tests depend on
// nothing that might be missing from a build machine.
func jobFixture(t *testing.T, name, script string, extra ...string) (*engine.Engine, paths.Layout) {
	t.Helper()
	return jobFixtureAt(t, name, script, nil, extra...)
}

// jobFixtureAt is jobFixture with a controllable clock.
//
// The scheduler computes due windows against the engine's clock rather than
// the wall, so freezing it makes catch-up exactly testable: the gap is whatever
// the test says it is, and no real minute boundary can slide past mid-test and
// add a window nobody asked for.
func jobFixtureAt(t *testing.T, name, script string, now func() time.Time, extra ...string) (*engine.Engine, paths.Layout) {
	t.Helper()

	dir := t.TempDir()
	layout := paths.Layout{Data: dir, Jobs: filepath.Join(dir, "jobs")}
	if err := os.MkdirAll(layout.Jobs, 0o755); err != nil {
		t.Fatal(err)
	}

	body := fmt.Sprintf("command: [\"/bin/sh\", \"-c\", %q]\n", script)
	for _, line := range extra {
		body += line + "\n"
	}
	if err := os.WriteFile(filepath.Join(layout.Jobs, name+".yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	e := newEngine(t, layout, now)
	t.Cleanup(func() { e.Close(context.Background()) })

	if _, err := e.LoadFromDisk(context.Background()); err != nil {
		t.Fatalf("loading definitions: %v", err)
	}
	return e, layout
}

func TestSuccessfulRunCommitsEverything(t *testing.T) {
	ctx := context.Background()
	script := `
		echo "hello from the job"
		echo '{"since":"2026-09-02T10:00:00Z"}' > "$JOB_STATE_OUT_FILE"
		echo '{"rows":41}' > "$JOB_OUTPUT_FILE"
		echo '{"type":"weather.ingested","payload":{"count":41}}' >> "$JOB_EVENTS_FILE"
	`
	e, _ := jobFixture(t, "ingest", script)

	result, err := runJob(t, e, "ingest", engine.RunOptions{Actor: "tester"})
	if err != nil {
		t.Fatalf("running the job: %v", err)
	}
	if result.Run.Status != model.StatusSucceeded {
		t.Fatalf("status = %s, error = %s", result.Run.Status, result.Run.Error)
	}

	if result.StateOut == nil {
		t.Fatal("cursor did not move on a successful run")
	}
	if got := result.StateOut.Summary("since"); got != "2026-09-02T10:00:00Z" {
		t.Errorf("cursor = %q", got)
	}
	if string(result.Output) != `{"rows":41}` {
		t.Errorf("output = %s", result.Output)
	}
	if len(result.Emitted) != 1 || result.Emitted[0].Type != "weather.ingested" {
		t.Fatalf("emitted = %+v", result.Emitted)
	}
	// An emitted event inherits the run's causation, so the chain stays intact
	// without the job doing anything (D17).
	if result.Emitted[0].CausedByRunID == nil || *result.Emitted[0].CausedByRunID != result.Run.ID {
		t.Error("emitted event is not attributed to the run that emitted it")
	}

	lines, err := e.Logs(ctx, result.Run.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0].Line != "hello from the job" {
		t.Errorf("logs = %+v", lines)
	}
}

func TestFailedRunCommitsNothing(t *testing.T) {
	ctx := context.Background()
	// The job does all three things and then fails. None may survive. This is
	// the bug class D14 exists to eliminate: a failure that still advances the
	// cursor silently skips every record in between.
	script := `
		echo '{"since":"2099-01-01T00:00:00Z"}' > "$JOB_STATE_OUT_FILE"
		echo '{"rows":1}' > "$JOB_OUTPUT_FILE"
		echo '{"type":"should.not.exist","payload":{}}' >> "$JOB_EVENTS_FILE"
		exit 3
	`
	e, _ := jobFixture(t, "flaky", script)

	result, err := runJob(t, e, "flaky", engine.RunOptions{})
	if err != nil {
		t.Fatalf("running the job: %v", err)
	}
	if result.Run.Status != model.StatusFailed {
		t.Fatalf("status = %s, want failed", result.Run.Status)
	}
	if result.StateOut != nil {
		t.Errorf("cursor advanced on a failed run to %s", result.StateOut.Value)
	}
	if len(result.Output) != 0 {
		t.Errorf("output was kept from a failed run: %s", result.Output)
	}
	if len(result.Emitted) != 0 {
		t.Errorf("events were emitted from a failed run: %+v", result.Emitted)
	}

	// The cursor in the database is still only the seed.
	history, err := e.StateHistory(ctx, result.Job.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 {
		t.Fatalf("state has %d versions after a failed run, want only the seed", len(history))
	}

	events, err := e.RecentEvents(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Type == "should.not.exist" {
			t.Fatal("an event from a failed run reached the log")
		}
	}
}

func TestCursorIsSeededOnFirstRun(t *testing.T) {
	ctx := context.Background()
	// The job just echoes what it was given, and never sets state.
	e, _ := jobFixture(t, "seeded", `echo "$JE_STATE"`,
		"state:", "  primary_cursor: watermark")

	result, err := runJob(t, e, "seeded", engine.RunOptions{})
	if err != nil {
		t.Fatalf("running the job: %v", err)
	}
	if result.StateIn == nil {
		t.Fatal("no state was supplied to the job")
	}

	var seeded map[string]string
	if err := json.Unmarshal(result.StateIn.Value, &seeded); err != nil {
		t.Fatal(err)
	}
	ts, ok := seeded["watermark"]
	if !ok {
		t.Fatalf("seed does not use the declared primary cursor: %s", result.StateIn.Value)
	}
	if _, err := time.Parse(time.RFC3339, ts); err != nil {
		t.Errorf("seed %q is not a timestamp: %v", ts, err)
	}

	// The seed is a seed, never a maintained value: a run that does not set
	// state leaves the cursor exactly where it was.
	if result.StateOut != nil {
		t.Error("the engine advanced the cursor by itself")
	}

	second, err := runJob(t, e, "seeded", engine.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(second.StateIn.Value) != string(result.StateIn.Value) {
		t.Errorf("the seed moved between runs: %s then %s",
			result.StateIn.Value, second.StateIn.Value)
	}

	// And it is stored, so its origin is visible in the history rather than
	// being magic (P1).
	history, err := e.StateHistory(ctx, result.Job.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].SetByActor != "engine" {
		t.Errorf("seed is not recorded as an engine commit: %+v", history)
	}
}

func TestLastSuccessAtIsServedButNotOnTheFirstRun(t *testing.T) {
	e, _ := jobFixture(t, "reporter", `echo "last=${JE_LAST_SUCCESS_AT:-never}"`)

	first, err := runJob(t, e, "reporter", engine.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := logText(t, e, first.Run.ID, 1); got != "last=never" {
		t.Errorf("first run saw %q, want last=never", got)
	}

	second, err := runJob(t, e, "reporter", engine.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := logText(t, e, second.Run.ID, 1)
	if !strings.HasPrefix(got, "last=2") {
		t.Errorf("second run saw %q, want a timestamp", got)
	}
}

func TestTimeoutIsDistinctFromFailure(t *testing.T) {
	// D8 keeps timed_out separate from failed because they call for different
	// responses: one is a slow job, the other is a broken one.
	e, _ := jobFixture(t, "slow", `sleep 30`, "timeout: 300ms")

	start := time.Now()
	result, err := runJob(t, e, "slow", engine.RunOptions{})
	if err != nil {
		t.Fatalf("running the job: %v", err)
	}
	elapsed := time.Since(start)

	if result.Run.Status != model.StatusTimedOut {
		t.Errorf("status = %s, want timed_out", result.Run.Status)
	}
	if !result.TimedOut {
		t.Error("result does not report the timeout")
	}
	// Generous, but it must be nowhere near the 30s the job asked to sleep for.
	if elapsed > 15*time.Second {
		t.Errorf("timeout took %s to take effect", elapsed)
	}
}

func TestMalformedEventsFileFailsTheRunWithoutCommitting(t *testing.T) {
	// Everything is parsed before anything is written, so a bad events line
	// cannot leave a committed cursor behind. This is the atomicity that
	// separate channels make possible.
	script := `
		echo '{"since":"2026-09-02T10:00:00Z"}' > "$JOB_STATE_OUT_FILE"
		echo '{"type":"fine","payload":{}}' >> "$JOB_EVENTS_FILE"
		echo 'not json at all' >> "$JOB_EVENTS_FILE"
	`
	e, _ := jobFixture(t, "badevents", script)

	result, err := runJob(t, e, "badevents", engine.RunOptions{})
	if err != nil {
		t.Fatalf("running the job: %v", err)
	}
	if result.Run.Status != model.StatusFailed {
		t.Fatalf("status = %s, want failed", result.Run.Status)
	}
	// P1: the error must name the line, not just the file.
	if !strings.Contains(result.Run.Error, "line 2") {
		t.Errorf("error does not name the offending line: %q", result.Run.Error)
	}
	if result.StateOut != nil {
		t.Error("the cursor was committed despite a malformed events channel")
	}
}

func TestOversizedStateIsRejected(t *testing.T) {
	// 64KB is the cap, because state travels in the environment.
	script := `
		printf '{"blob":"' > "$JOB_STATE_OUT_FILE"
		for i in $(seq 1 700); do printf '%0100d' 0 >> "$JOB_STATE_OUT_FILE"; done
		printf '"}' >> "$JOB_STATE_OUT_FILE"
	`
	e, _ := jobFixture(t, "fat", script)

	result, err := runJob(t, e, "fat", engine.RunOptions{})
	if err != nil {
		t.Fatalf("running the job: %v", err)
	}
	if result.Run.Status != model.StatusFailed {
		t.Fatalf("status = %s, want failed", result.Run.Status)
	}
	if !strings.Contains(result.Run.Error, "limit") {
		t.Errorf("error does not explain the cap: %q", result.Run.Error)
	}
}

func TestJobEnvironmentIsExactlyTheProtocol(t *testing.T) {
	ctx := context.Background()
	// D10: a job must not inherit the engine's environment by accident.
	t.Setenv("A_SECRET_THE_JOB_MUST_NOT_SEE", "hunter2")
	e, _ := jobFixture(t, "env", `echo "${A_SECRET_THE_JOB_MUST_NOT_SEE:-absent}"; echo "job=$JOB_ID attempt=$ATTEMPT by=$TRIGGERED_BY"`)

	result, err := runJob(t, e, "env", engine.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	lines, err := e.Logs(ctx, result.Run.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 {
		t.Fatalf("got %d log lines: %+v", len(lines), lines)
	}
	if lines[0].Line != "absent" {
		t.Errorf("the job inherited an unrelated environment variable: %q", lines[0].Line)
	}
	if want := "job=env attempt=1 by=run.requested"; lines[1].Line != want {
		t.Errorf("protocol variables = %q, want %q", lines[1].Line, want)
	}
}

func TestMisconfiguredJobWillNotRun(t *testing.T) {
	// D10's pit of success: a job declaring a secret that cannot be supplied is
	// visibly broken rather than failing cryptically at 3am.
	e, _ := jobFixture(t, "needy", `echo hi`, "secrets: [SOME_TOKEN]")

	if _, err := runJob(t, e, "needy", engine.RunOptions{}); err == nil {
		t.Fatal("a misconfigured job ran")
	} else if !strings.Contains(err.Error(), "misconfigured") {
		t.Errorf("error = %v", err)
	}
}

func logText(t *testing.T, e *engine.Engine, runID int64, attempt int) string {
	t.Helper()
	lines, err := e.Logs(context.Background(), runID, attempt)
	if err != nil {
		t.Fatal(err)
	}
	parts := make([]string, len(lines))
	for i, l := range lines {
		parts[i] = l.Line
	}
	return strings.Join(parts, "\n")
}

func TestCancellationIsInterruptedNotFailed(t *testing.T) {
	// D5 makes `interrupted` a distinct state from `failed` because the job did
	// not fail -- we did, and those want different responses. It is also what
	// on_interrupt keys off.
	e, _ := jobFixture(t, "longrunner", `sleep 30`)

	// The cancellation now belongs to the worker, which is where the process
	// is (D20/C11). It reports `interrupted` explicitly rather than the engine
	// inferring it from its own context, because after the split the engine no
	// longer has a context that has anything to do with the job.
	result, err := runJobInterrupted(t, e, "longrunner", 200*time.Millisecond)
	if err != nil {
		t.Fatalf("running the job: %v", err)
	}
	if result.Run.Status != model.StatusInterrupted {
		t.Errorf("status = %s, want interrupted", result.Run.Status)
	}
	if result.TimedOut {
		t.Error("a cancelled run was reported as timed out")
	}
}

func TestDeclaredSecretIsInjectedAndRedacted(t *testing.T) {
	ctx := context.Background()
	e, _ := jobFixture(t, "tokenuser",
		`echo "token is $STATION_API_KEY"; echo "and nothing else: ${OTHER_TOKEN:-absent}"`,
		"secrets: [STATION_API_KEY]")

	if err := e.Secrets().Set("STATION_API_KEY", "sk-live-abcdef123456"); err != nil {
		t.Fatal(err)
	}
	// A second secret the job did NOT declare. It must not leak in.
	if err := e.Secrets().Set("OTHER_TOKEN", "sk-other-999999"); err != nil {
		t.Fatal(err)
	}
	// Reload so the load-time secret check sees the new values.
	if _, err := e.LoadFromDisk(ctx); err != nil {
		t.Fatal(err)
	}

	result, err := runJob(t, e, "tokenuser", engine.RunOptions{})
	if err != nil {
		t.Fatalf("running the job: %v", err)
	}
	if result.Run.Status != model.StatusSucceeded {
		t.Fatalf("status = %s: %s", result.Run.Status, result.Run.Error)
	}

	logged := logText(t, e, result.Run.ID, 1)

	// D10: redaction happens on write, so the value never reaches the database.
	if strings.Contains(logged, "sk-live-abcdef123456") {
		t.Errorf("the secret value was stored in the logs: %q", logged)
	}
	if !strings.Contains(logged, "token is ***") {
		t.Errorf("the secret was not redacted: %q", logged)
	}
	// Only declared secrets are injected.
	if !strings.Contains(logged, "and nothing else: absent") {
		t.Errorf("an undeclared secret reached the job: %q", logged)
	}
}

func TestMissingSecretIsALoadTimeError(t *testing.T) {
	ctx := context.Background()
	// D10's whole point: you find out when the file is loaded, not at 3am.
	e, _ := jobFixture(t, "needstoken", `echo hi`, "secrets: [NEVER_SET]")

	jobs, err := e.Jobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("got %d jobs", len(jobs))
	}
	if jobs[0].Runnable() {
		t.Fatal("a job with an unset secret is marked runnable")
	}
	// The error has to say which secret and how to fix it, or it is just a
	// different flavour of confusion.
	if !strings.Contains(jobs[0].ConfigError, "NEVER_SET") {
		t.Errorf("config error does not name the secret: %q", jobs[0].ConfigError)
	}
	if !strings.Contains(jobs[0].ConfigError, "je secret set") {
		t.Errorf("config error does not say how to fix it: %q", jobs[0].ConfigError)
	}

	if _, err := runJob(t, e, "needstoken", engine.RunOptions{}); err == nil {
		t.Error("a job with an unset secret ran anyway")
	}
}

func TestSecretBecomingAvailableClearsTheError(t *testing.T) {
	ctx := context.Background()
	e, _ := jobFixture(t, "eventually", `echo "$LATE_TOKEN"`, "secrets: [LATE_TOKEN]")

	if err := e.Secrets().Set("LATE_TOKEN", "value-goes-here"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.LoadFromDisk(ctx); err != nil {
		t.Fatal(err)
	}

	jobs, err := e.Jobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !jobs[0].Runnable() {
		t.Fatalf("job still misconfigured after the secret was set: %q", jobs[0].ConfigError)
	}
	if _, err := runJob(t, e, "eventually", engine.RunOptions{}); err != nil {
		t.Errorf("running the job: %v", err)
	}
}
