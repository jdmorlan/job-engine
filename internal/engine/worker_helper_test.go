package engine_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jdmorlan/job-engine/internal/engine"
	"github.com/jdmorlan/job-engine/internal/executor"
	"github.com/jdmorlan/job-engine/internal/model"
	"github.com/jdmorlan/job-engine/internal/store"
)

// outcome is what a run produced, assembled from the control plane after a
// worker reported it.
//
// It is the shape the old in-process RunResult had, kept so that these tests
// still read as statements about jobs rather than about transport. What changed
// underneath is the important part: every one of them now goes through D20's
// real seam -- enqueue, claim, execute elsewhere, report back -- instead of
// through a path that no longer exists in the product.
type outcome struct {
	Run      store.Run
	Job      store.Job
	StateIn  *store.StateVersion
	StateOut *store.StateVersion
	Emitted  []model.Event
	Output   json.RawMessage
	TimedOut bool
	Killed   bool
}

// runJob queues a job and executes it the way a worker would.
//
// This is deliberately a faithful re-implementation of internal/worker rather
// than a shortcut through the engine: it creates the scratch directory, adds
// the four environment values only a worker can know, runs the process, and
// reports bytes back. A helper that called some private engine method instead
// would test a path the product does not have.
//
// The HTTP hop is the one thing it leaves out, and that is covered separately
// by the worker package's own round-trip test.
func runJob(t *testing.T, e *engine.Engine, slug string, opts engine.RunOptions) (outcome, error) {
	t.Helper()
	return runJobWith(t, e, slug, opts, context.Background())
}

// runJobWith is runJob with a controllable context for the execution itself,
// so a test can simulate a worker being shut down mid-run.
func runJobWith(
	t *testing.T, e *engine.Engine, slug string, opts engine.RunOptions, execCtx context.Context,
) (outcome, error) {
	t.Helper()
	ctx := context.Background()

	workerID := ensureWorker(t, e)

	run, err := e.TriggerRun(ctx, slug, opts)
	if err != nil {
		return outcome{}, err
	}

	dispatch, err := e.Claim(ctx, workerID)
	if err != nil {
		return outcome{}, err
	}
	if dispatch == nil {
		t.Fatalf("nothing was dispatched for run %d; the worker's labels do not match", run.ID)
	}

	completion := executeDispatch(t, e, *dispatch, execCtx)
	if err := e.Complete(ctx, dispatch.RunID, workerID, completion); err != nil {
		return outcome{}, err
	}

	detail, err := e.RunDetail(ctx, dispatch.RunID)
	if err != nil {
		return outcome{}, err
	}
	job, err := e.Job(ctx, detail.JobSlug)
	if err != nil {
		return outcome{}, err
	}
	return outcome{
		Run:      detail.Run,
		Job:      job,
		StateIn:  detail.StateIn,
		StateOut: detail.StateOut,
		// Only what the job itself emitted. RunDetail carries everything caused
		// by the run, which includes the engine's own run.succeeded -- correct
		// for the timeline, wrong for a test asking what the job produced.
		Emitted:  jobEmitted(detail.Emitted),
		Output:   detail.Run.Output,
		TimedOut: detail.Run.Status == model.StatusTimedOut,
		Killed:   completion.Result.Killed,
	}, nil
}

// runJobInterrupted runs a job and cancels the worker mid-flight, which is how
// a shutdown looks from the control plane's side (D5).
func runJobInterrupted(t *testing.T, e *engine.Engine, slug string, after time.Duration) (outcome, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(after)
		cancel()
	}()
	defer cancel()
	return runJobWith(t, e, slug, engine.RunOptions{}, ctx)
}

// executeDispatch is what a worker does with a Dispatch.
func executeDispatch(t *testing.T, e *engine.Engine, d engine.Dispatch, ctx context.Context) engine.Completion {
	t.Helper()

	scratch := t.TempDir()
	stateOut := filepath.Join(scratch, "state.json")
	output := filepath.Join(scratch, "output.json")
	events := filepath.Join(scratch, "events.jsonl")

	env := append([]string{}, d.Env...)
	env = append(env,
		"JOB_WORKDIR="+scratch,
		"JOB_STATE_OUT_FILE="+stateOut,
		"JOB_OUTPUT_FILE="+output,
		"JOB_EVENTS_FILE="+events,
	)

	sink := &collectingSink{}
	result, execErr := executor.Process{}.Run(ctx, executor.Spec{
		Command: d.Command,
		Workdir: d.Workdir,
		Env:     env,
		Timeout: d.Timeout,
		Grace:   d.Grace,
		Output:  sink,
	})

	if len(sink.lines) > 0 {
		if err := e.AppendLogs(ctx, d.RunID, d.Attempt, sink.lines); err != nil {
			t.Fatalf("appending logs: %v", err)
		}
	}

	completion := engine.Completion{Result: result, Interrupted: ctx.Err() != nil}
	if execErr != nil {
		completion.ExecError = execErr.Error()
		return completion
	}
	if result.Succeeded() {
		completion.StateOut = readIfPresent(stateOut)
		completion.Output = readIfPresent(output)
		completion.Events = readIfPresent(events)
	}
	return completion
}

func readIfPresent(path string) []byte {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return body
}

type collectingSink struct{ lines []engine.LogLine }

func (s *collectingSink) WriteLine(stream executor.Stream, ts time.Time, line string) {
	s.lines = append(s.lines, engine.LogLine{Stream: string(stream), TS: ts, Line: line})
}

// ensureWorker registers one worker per engine, advertising the default label.
func ensureWorker(t *testing.T, e *engine.Engine) string {
	t.Helper()
	saved, err := e.RegisterWorker(context.Background(), store.Worker{
		ID:     "test-worker",
		Name:   "test-worker",
		Labels: []string{store.DefaultLabel},
		Roles:  []string{store.RoleExecute},
		// C10 is enforced, so the helper has to satisfy it like any worker.
		Version: e.Health(context.Background()).Version,
	})
	if err != nil {
		t.Fatalf("registering the test worker: %v", err)
	}
	return saved.ID
}

// pumpWorker claims and executes whatever the scheduler queues, until ctx ends.
//
// It is the same loop internal/worker runs, minus the HTTP hop and the
// heartbeat: claim, execute, report. Tests that exercise the scheduler need one
// attached for the same reason a real deployment does.
func pumpWorker(t *testing.T, e *engine.Engine, ctx context.Context) {
	t.Helper()
	workerID := ensureWorker(t, e)

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}

		dispatch, err := e.Claim(context.Background(), workerID)
		if err != nil || dispatch == nil {
			continue
		}
		completion := executeDispatch(t, e, *dispatch, ctx)
		// Reported with a live context: the run finished, and recording that
		// is what the shutdown path most needs to get right (D5).
		_ = e.Complete(context.Background(), dispatch.RunID, workerID, completion)
	}
}

func jobEmitted(events []model.Event) []model.Event {
	var out []model.Event
	for _, ev := range events {
		if ev.Source == model.SourceJob {
			out = append(out, ev)
		}
	}
	return out
}

// executorSuccess builds the result a worker reports for a clean exit.
func executorSuccess(exit *int) executor.Result {
	return executor.Result{ExitCode: exit}
}
