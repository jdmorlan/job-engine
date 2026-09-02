package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jdmorlan/job-engine/internal/executor"
	"github.com/jdmorlan/job-engine/internal/jobdef"
	"github.com/jdmorlan/job-engine/internal/model"
	"github.com/jdmorlan/job-engine/internal/store"
)

// Event types the engine emits around a run. Chain files match on these (D17).
const (
	EventRunRequested = "run.requested"
	EventRunSucceeded = "run.succeeded"
	EventRunFailed    = "run.failed"

	// EventRunSkipped records the overlap policy declining to start a run, and
	// EventScheduleMissed records windows a gap swallowed. Both exist because
	// D9 insists gaps are explained rather than silent.
	EventRunSkipped     = "run.skipped"
	EventScheduleFired  = "schedule.fired"
	EventScheduleMissed = "schedule.missed"

	// EventScheduleStarted records the engine anchoring a schedule it has
	// never seen before, so "why did this not backfill?" has an answer.
	EventScheduleStarted = "schedule.started"
)

// maxDepth is D3's loop guard: an event caused by a run caused by an event, ten
// deep, is a cycle we refuse to continue rather than a workflow.
const maxDepth = 10

// RunOptions configures a single invocation.
type RunOptions struct {
	// Actor is the person responsible, when there is one. D7 uses it to
	// distinguish a human intervening from an automatic retry.
	Actor string

	// TriggeringEvent, when set, is the event that caused this run. A manual
	// `je run` leaves it nil and the engine emits run.requested instead, so
	// every run has exactly one cause.
	TriggeringEvent *model.Event

	// Live receives log lines as they are produced, for foreground `je run`.
	// Storage happens regardless; this is only for watching it happen.
	Live func(stream string, ts time.Time, line string)
}

// RunResult is everything that happened, for the CLI to render.
type RunResult struct {
	Run     store.Run
	Attempt store.Attempt
	Job     store.Job

	StateIn  *store.StateVersion
	StateOut *store.StateVersion
	Output   json.RawMessage
	Emitted  []model.Event

	TimedOut bool
	Killed   bool

	// PrimaryCursor is the key the definition nominated for display, carried
	// here so a caller can render the cursor without re-reading the snapshot.
	PrimaryCursor string

	// causeDepth is the depth of the event that caused this run. Anything the
	// run emits is one deeper, which is how D3's loop guard counts.
	causeDepth int
}

// RunJob enqueues a job and runs it immediately, bypassing the queue.
//
// This is the foreground path -- `je run` at a terminal, D19 stage 0 -- where
// waiting behind the concurrency cap would be surprising: you asked for it now,
// and there is no scheduler competing for slots.
func (e *Engine) RunJob(ctx context.Context, slug string, opts RunOptions) (*RunResult, error) {
	prepared, err := e.Enqueue(ctx, slug, opts)
	if err != nil {
		return nil, err
	}
	if prepared == nil {
		return nil, fmt.Errorf("job %s is already running (overlap: skip)", slug)
	}
	return e.execute(ctx, *prepared, opts)
}

// Prepared is a run that exists in the database and is waiting to execute.
type Prepared struct {
	Run     store.Run
	Job     store.Job
	Def     *jobdef.Definition
	StateIn store.StateVersion
	Cause   model.Event
}

// Enqueue validates a job, records its cause, reads its cursor, and creates a
// queued run. It does not execute anything.
//
// A nil Prepared with a nil error means the overlap policy declined to start
// this run -- a normal outcome, recorded as an event, not a failure.
//
// The split between this and execute exists so the scheduler and the
// foreground path share one implementation of everything that is easy to get
// subtly different: which event caused the run, which cursor version it starts
// from, and what happens when the job is already running.
func (e *Engine) Enqueue(ctx context.Context, slug string, opts RunOptions) (*Prepared, error) {
	def, job, err := e.Definition(ctx, slug)
	if err != nil {
		return nil, err
	}
	if !job.Runnable() {
		return nil, unrunnable(job)
	}

	// D8's overlap policy. Checked here rather than at execution time so that
	// a skipped run never occupies a queue slot, and so the reason is recorded
	// at the moment the decision is made.
	if def.Overlap == jobdef.OverlapSkip {
		active, err := e.store.JobHasActiveRun(ctx, job.ID)
		if err != nil {
			return nil, err
		}
		if active {
			if err := e.recordSkipped(ctx, job, "the previous run has not finished (overlap: skip)"); err != nil {
				return nil, err
			}
			return nil, nil
		}
	}

	// One timestamp for the whole run. D14 requires every attempt to see the
	// same input state, so a re-read clock between attempts would open a gap.
	startedAt := e.now()

	cause, err := e.runCause(ctx, job, opts, startedAt)
	if err != nil {
		return nil, err
	}

	// Read (or seed) the cursor before the run exists, so the run can record
	// which version it started from.
	stateIn, err := e.stateForRun(ctx, job, def, startedAt)
	if err != nil {
		return nil, err
	}

	run, err := e.store.CreateRun(ctx, store.Run{
		JobID:             job.ID,
		DefinitionHash:    job.DefinitionHash,
		TriggeringEventID: &cause.ID,
		StateVersionIn:    &stateIn.Version,
		Overlap:           string(def.Overlap),
	})
	if err != nil {
		return nil, err
	}

	return &Prepared{Run: run, Job: job, Def: def, StateIn: stateIn, Cause: cause}, nil
}

// execute runs a prepared run to completion.
//
// The ordering here is the whole of D14's contract and is worth reading as a
// unit: the cursor was read before the run existed, and it commits only after
// the attempt exits zero. Everything between those two points can fail without
// moving it, which is the bug class this engine exists to eliminate.
func (e *Engine) execute(ctx context.Context, p Prepared, opts RunOptions) (*RunResult, error) {
	startedAt := e.now()
	if err := e.store.StartRun(ctx, p.Run.ID, startedAt); err != nil {
		return nil, err
	}
	// Keep the in-memory copy in step with the row. StartRun writes the
	// database; without this the returned Run still says it never started and
	// every duration renders as zero.
	p.Run.StartedAt = &startedAt
	p.Run.Status = model.StatusRunning

	attempt, err := e.store.CreateAttempt(ctx, store.Attempt{
		RunID:             p.Run.ID,
		TriggeringEventID: &p.Cause.ID,
		Actor:             opts.Actor,
		Executor:          string(p.Def.Runtime),
	})
	if err != nil {
		return nil, err
	}

	result := &RunResult{
		Run: p.Run, Attempt: attempt, Job: p.Job,
		StateIn: &p.StateIn, causeDepth: p.Cause.Depth,
		PrimaryCursor: p.Def.State.PrimaryCursor,
	}

	// The scratch directory holds the three output channels. It is per-attempt
	// and removed afterwards, which is what makes them a handoff rather than
	// storage -- the durable copy is the one the engine promotes.
	scratch, err := os.MkdirTemp("", fmt.Sprintf("je-%s-%d-%d-", p.Job.Slug, p.Run.ID, attempt.Number))
	if err != nil {
		return nil, fmt.Errorf("creating scratch directory: %w", err)
	}
	defer os.RemoveAll(scratch)

	channels := newChannels(scratch)
	env, err := e.buildEnv(ctx, p.Job, p.Def, p.Run, attempt, p.Cause, p.StateIn, channels)
	if err != nil {
		return nil, err
	}

	workdir, err := expandHome(p.Def.Workdir)
	if err != nil {
		return nil, err
	}

	// The same resolved values that went into the environment drive redaction,
	// so the two cannot drift (D10).
	resolved, err := e.secrets.Resolve(p.Def.Secrets)
	if err != nil {
		return nil, fmt.Errorf("job %s: %w", p.Job.Slug, err)
	}
	sink := newLogSink(e, p.Run.ID, attempt.Number, opts.Live, newRedactor(resolved))

	execResult, execErr := executor.Process{}.Run(ctx, executor.Spec{
		Command: p.Def.Command,
		Workdir: workdir,
		Env:     env,
		Timeout: p.Def.Timeout.D,
		Output:  sink,
	})
	sink.Close()

	result.TimedOut = execResult.TimedOut
	result.Killed = execResult.Killed

	// Bookkeeping after the process exits must not inherit the cancellation
	// that stopped it. Shutting the engine down is precisely when recording
	// what happened matters most, and writing with a dead context would leave
	// the hole in the timeline that `interrupted` exists to fill (D5, D16).
	//
	// The original ctx is still consulted below to decide *which* terminal
	// status this is -- cancellation is the fact, it just must not prevent us
	// from writing the fact down.
	finishCtx, cancelFinish := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancelFinish()

	// An executor error means we could not run the command at all -- a missing
	// binary, an unreadable workdir. That is distinct from a command that ran
	// and failed, and the run records it as such.
	if execErr != nil {
		return result, e.finish(finishCtx, result, model.StatusFailed, execResult, execErr.Error())
	}

	status := statusFor(execResult, ctx)
	if !execResult.Succeeded() {
		// D14, the whole point: failure, timeout, crash or interrupt and the
		// cursor does not move. The channel files are simply discarded.
		return result, e.finish(finishCtx, result, status, execResult, interruptMessage(status, execResult))
	}

	// Success. Promote the three channels, in an order chosen so that a
	// malformed file fails the run before anything is committed.
	if err := e.promote(finishCtx, result, p.Def, channels); err != nil {
		return result, e.finish(finishCtx, result, model.StatusFailed, execResult, err.Error())
	}
	return result, e.finish(finishCtx, result, model.StatusSucceeded, execResult, "")
}

// recordSkipped notes that the overlap policy declined to start a run.
//
// D9 and P1 both demand this: a job that quietly does not run is the single
// most confusing thing a scheduler can do, so the decision is an event with a
// reason rather than an absence.
func (e *Engine) recordSkipped(ctx context.Context, job store.Job, reason string) error {
	payload, _ := json.Marshal(map[string]string{"job": job.Slug, "reason": reason})
	_, _, err := e.store.AppendEvent(ctx, model.Event{
		Type:      EventRunSkipped,
		Source:    model.SourceEngine,
		Payload:   payload,
		CreatedAt: e.now(),
	})
	return err
}

// runCause establishes the single event that caused this run (D7).
func (e *Engine) runCause(ctx context.Context, job store.Job, opts RunOptions, at time.Time) (model.Event, error) {
	if opts.TriggeringEvent != nil {
		return *opts.TriggeringEvent, nil
	}
	payload, _ := json.Marshal(map[string]string{"job": job.Slug})
	event, _, err := e.store.AppendEvent(ctx, model.Event{
		Type:      EventRunRequested,
		Source:    model.SourceCLI,
		Payload:   payload,
		Actor:     opts.Actor,
		CreatedAt: at,
	})
	if err != nil {
		return model.Event{}, fmt.Errorf("recording run cause: %w", err)
	}
	return event, nil
}

// stateForRun returns the cursor this run starts from, seeding it on a first
// run (D14).
//
// The seed is stored as version 1 rather than computed on the fly, so that
// `je state history` shows where the cursor came from. Magic that appears in
// the history is behaviour; magic that does not is the thing you cannot explain
// at 2am (P1).
func (e *Engine) stateForRun(ctx context.Context, job store.Job, def *jobdef.Definition, at time.Time) (store.StateVersion, error) {
	current, err := e.store.CurrentState(ctx, job.ID)
	switch {
	case err == nil:
		return current, nil
	case !isNoRows(err):
		return store.StateVersion{}, err
	}

	// No cursor yet. Seed it with this run's start time, never with the epoch:
	// the recovery from "I wanted history" is `je state set` backwards, and
	// there is no recovery from having already hammered the source API.
	//
	// This is a seed, not a maintained value. Nothing advances it but the job.
	seed, err := json.Marshal(map[string]string{
		def.State.PrimaryCursor: at.UTC().Format(time.RFC3339),
	})
	if err != nil {
		return store.StateVersion{}, err
	}
	return e.store.CommitState(ctx, store.StateVersion{
		JobID:      job.ID,
		Value:      seed,
		SetByActor: store.ActorEngine,
	})
}

// channels holds the three output file paths of D6's protocol.
type channels struct {
	stateOut string
	output   string
	events   string
}

func newChannels(dir string) channels {
	return channels{
		stateOut: filepath.Join(dir, "state.json"),
		output:   filepath.Join(dir, "output.json"),
		events:   filepath.Join(dir, "events.jsonl"),
	}
}

// promote reads the three output channels and commits them.
//
// Order matters. Everything is parsed and validated before anything is written,
// so a malformed events file cannot leave a committed cursor behind. That is
// the atomicity the four separate channels make possible -- and one of the
// reasons the withdrawn single-channel design was worse.
func (e *Engine) promote(ctx context.Context, result *RunResult, def *jobdef.Definition, ch channels) error {
	stateOut, err := readJSONObject(ch.stateOut, jobdef.MaxStateBytes, "state")
	if err != nil {
		return err
	}
	output, err := readJSONObject(ch.output, 1<<20, "output")
	if err != nil {
		return err
	}
	events, err := readEvents(ch.events)
	if err != nil {
		return err
	}

	// Everything parsed. Now commit.
	if stateOut != nil {
		committed, err := e.store.CommitState(ctx, store.StateVersion{
			JobID:    result.Job.ID,
			Value:    stateOut,
			SetByRun: &result.Run.ID,
		})
		if err != nil {
			return err
		}
		result.StateOut = &committed
	}
	result.Output = output

	for _, ev := range events {
		ev.Source = model.SourceJob
		ev.CausedByRunID = &result.Run.ID
		ev.Depth = result.depthForEmitted()
		if ev.Depth > maxDepth {
			return fmt.Errorf("refusing to emit %q: causation depth %d exceeds %d, which is a cycle",
				ev.Type, ev.Depth, maxDepth)
		}
		stored, _, err := e.store.AppendEvent(ctx, ev)
		if err != nil {
			return fmt.Errorf("emitting %q: %w", ev.Type, err)
		}
		result.Emitted = append(result.Emitted, stored)
	}
	return nil
}

// depthForEmitted is one deeper than the event that caused this run.
func (r *RunResult) depthForEmitted() int { return r.causeDepth + 1 }

// finish writes the terminal state of the run and its attempt, and emits the
// completion event that chains match on (D17).
func (e *Engine) finish(ctx context.Context, result *RunResult, status model.Status, exec executor.Result, errMsg string) error {
	at := e.now()

	if err := e.store.FinishAttempt(ctx, result.Attempt.ID, status, at, exec.ExitCode, errMsg); err != nil {
		return err
	}
	if err := e.store.FinishRun(ctx, result.Run.ID, status, at, result.Output, errMsg); err != nil {
		return err
	}

	eventType := EventRunFailed
	if status == model.StatusSucceeded {
		eventType = EventRunSucceeded
	}
	payload, _ := json.Marshal(map[string]any{
		"job":    result.Job.Slug,
		"run":    result.Run.ID,
		"status": string(status),
	})
	if _, _, err := e.store.AppendEvent(ctx, model.Event{
		Type:          eventType,
		Source:        model.SourceEngine,
		Payload:       payload,
		CausedByRunID: &result.Run.ID,
		Depth:         result.depthForEmitted(),
		CreatedAt:     at,
	}); err != nil {
		return fmt.Errorf("recording %s: %w", eventType, err)
	}

	result.Run.Status = status
	result.Run.EndedAt = &at
	result.Run.Error = errMsg
	result.Attempt.Status = status
	result.Attempt.ExitCode = exec.ExitCode
	return nil
}

func statusFor(exec executor.Result, ctx context.Context) model.Status {
	switch {
	case exec.TimedOut:
		// D8 keeps this distinct from `failed` because they call for different
		// responses: one is a slow job, the other is a broken one.
		return model.StatusTimedOut
	case ctx.Err() != nil:
		// D5: we were killed, not the job. `interrupted`, not `failed`.
		return model.StatusInterrupted
	default:
		return model.StatusFailed
	}
}

// interruptMessage explains a non-success outcome in the terms of its status.
func interruptMessage(status model.Status, exec executor.Result) string {
	if status == model.StatusInterrupted {
		return "the engine stopped while this run was in flight"
	}
	return execFailureMessage(exec)
}

func execFailureMessage(exec executor.Result) string {
	switch {
	case exec.TimedOut && exec.Killed:
		return "timed out, then did not exit within the grace period and was killed"
	case exec.TimedOut:
		return "timed out"
	case exec.ExitCode != nil:
		return fmt.Sprintf("exited %d", *exec.ExitCode)
	default:
		return "did not produce an exit code"
	}
}

func unrunnable(job store.Job) error {
	switch {
	case job.LoadError != "":
		return fmt.Errorf("job %s did not load: %s", job.Slug, job.LoadError)
	case job.ConfigError != "":
		return fmt.Errorf("job %s is misconfigured: %s", job.Slug, job.ConfigError)
	default:
		return fmt.Errorf("job %s is disabled", job.Slug)
	}
}

// readJSONObject reads one of the single-value output channels.
//
// A missing file means "no change", unambiguously -- which is why not writing
// it is a supported outcome rather than an error (D14).
func readJSONObject(path string, max int, what string) (json.RawMessage, error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if info.Size() > int64(max) {
		return nil, fmt.Errorf("job wrote %d bytes of %s, over the %d byte limit",
			info.Size(), what, max)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil, nil
	}
	if !json.Valid(body) {
		return nil, fmt.Errorf("job wrote invalid JSON to its %s channel", what)
	}
	if body[0] != '{' {
		// Constrained to an object so that state has named keys, which is what
		// makes primary_cursor and the history view meaningful.
		return nil, fmt.Errorf("job %s must be a JSON object, got %c", what, body[0])
	}
	return json.RawMessage(body), nil
}

// readEvents parses the JSONL events channel (D17).
func readEvents(path string) ([]model.Event, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []model.Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for line := 1; sc.Scan(); line++ {
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		var ev struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(raw, &ev); err != nil {
			// Name the line. "Your events file is bad" is not actionable; "line
			// 7 is bad" is (P1).
			return nil, fmt.Errorf("events channel line %d: %w", line, err)
		}
		if ev.Type == "" {
			return nil, fmt.Errorf("events channel line %d: missing \"type\"", line)
		}
		out = append(out, model.Event{Type: ev.Type, Payload: ev.Payload})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading events channel: %w", err)
	}
	return out, nil
}

// expandHome resolves a leading ~ in a configured path.
//
// Job files say `workdir: ~/code/almanac`, and a process's Dir is not run
// through a shell, so nothing else would expand it. Without this the job fails
// with "no such directory: ~/code/almanac", which reads like a typo.
func expandHome(path string) (string, error) {
	if path == "" || !strings.HasPrefix(path, "~") {
		return path, nil
	}
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return "", fmt.Errorf("cannot expand %q; only ~/ is supported", path)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~")), nil
}
