package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	// Route, when set, is the rule that matched that event (D17). It is
	// snapshotted onto the run with its hash, so "why did this run?" can name
	// the chain and step that started it even after the file has changed (D11).
	Route *store.Route
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

// ErrOverlapSkipped means the job's overlap policy declined to start a run.
// It is a normal outcome recorded as an event, not a failure, but a caller
// that asked for a run needs to know it did not get one.
var ErrOverlapSkipped = errors.New("the previous run has not finished (overlap: skip)")

// TriggerRun queues a run and returns immediately.
//
// `je run` posts here, gets a run id, and follows the live stream while a
// worker executes it. It returns rather than blocking because the caller is not
// the executor and never was -- tying a job's lifetime to an HTTP request would
// mean a dropped connection could end somebody's half-finished job.
func (e *Engine) TriggerRun(ctx context.Context, slug string, opts RunOptions) (store.Run, error) {
	prepared, err := e.Enqueue(ctx, slug, opts)
	if err != nil {
		return store.Run{}, err
	}
	if prepared == nil {
		return store.Run{}, ErrOverlapSkipped
	}
	return prepared.Run, nil
}

// Prepared is a run that exists in the database and is waiting for a worker.
//
// It is the value that crosses D20's seam: everything decided by the control
// plane before any process exists, which is deliberately everything that is
// easy to get subtly different -- which event caused the run, which cursor
// version it starts from, and what happens when the job is already running.
type Prepared struct {
	Run     store.Run
	Job     store.Job
	Def     *jobdef.Definition
	StateIn store.StateVersion
	Cause   model.Event

	// Actor is the person responsible, carried onto the attempt row so the
	// history can distinguish a human intervening from an automatic retry (D7).
	Actor string
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
			if err := e.recordSkipped(ctx, job, opts.TriggeringEvent,
				"the previous run has not finished (overlap: skip)"); err != nil {
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

	newRun := store.Run{
		JobID:             job.ID,
		DefinitionHash:    job.DefinitionHash,
		TriggeringEventID: &cause.ID,
		StateVersionIn:    &stateIn.Version,
		Overlap:           string(def.Overlap),
		// C3: snapshotted here for the same reason the overlap policy is. A
		// definition reloaded between enqueue and dispatch must not move a run
		// that is already waiting.
		RunsOn: def.RunsOn,
	}
	if opts.Route != nil {
		newRun.TriggeringRouteID = &opts.Route.ID
		newRun.RouteHash = opts.Route.RouteHash
	}

	// Which commit's code this run will execute (D22). Snapshotted at enqueue
	// for the same reason the definition and the overlap policy are: a source
	// that re-syncs between enqueue and dispatch must not change what this
	// already-queued run is a record of.
	if revision, err := e.sourceRevision(ctx, job); err != nil {
		return nil, err
	} else {
		newRun.SourceRevision = revision
	}

	run, err := e.store.CreateRun(ctx, newRun)
	if err != nil {
		return nil, err
	}

	return &Prepared{
		Run: run, Job: job, Def: def, StateIn: stateIn, Cause: cause, Actor: opts.Actor,
	}, nil
}

// recordSkipped notes that the overlap policy declined to start a run.
//
// D9 and P1 both demand this: a job that quietly does not run is the single
// most confusing thing a scheduler can do, so the decision is an event with a
// reason rather than an absence.
func (e *Engine) recordSkipped(ctx context.Context, job store.Job, cause *model.Event, reason string) error {
	payload, _ := json.Marshal(map[string]string{"job": job.Slug, "reason": reason})
	skipped := model.Event{
		Type:      EventRunSkipped,
		Source:    model.SourceEngine,
		Payload:   payload,
		CreatedAt: e.now(),
	}
	// A skip inside a chain inherits the causation of the event that asked for
	// the run, so the depth guard keeps counting and `je events` still shows
	// what the decision was about.
	if cause != nil {
		skipped.CausedByEventID = &cause.ID
		skipped.Depth = cause.Depth + 1
	}
	_, _, err := e.publish(ctx, skipped)
	return err
}

// runCause establishes the single event that caused this run (D7).
func (e *Engine) runCause(ctx context.Context, job store.Job, opts RunOptions, at time.Time) (model.Event, error) {
	if opts.TriggeringEvent != nil {
		return *opts.TriggeringEvent, nil
	}
	payload, _ := json.Marshal(map[string]string{"job": job.Slug})
	event, _, err := e.publish(ctx, model.Event{
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
// promote validates the three output channels and commits them.
//
// Order matters. Everything is parsed and validated before anything is written,
// so a malformed events file cannot leave a committed cursor behind. That is
// the atomicity the four separate channels make possible -- and one of the
// reasons the withdrawn single-channel design was worse.
//
// The channels arrive as bytes from a worker rather than as files on this
// machine, and the validation stayed here on purpose: the caps and the object
// constraint are the protocol (D6), and a worker is a place where a process
// ran, not a trusted enforcer of the contract.
func (e *Engine) promote(ctx context.Context, result *RunResult, def *jobdef.Definition, c Completion) error {
	stateOut, err := validateJSONObject(c.StateOut, jobdef.MaxStateBytes, "state")
	if err != nil {
		return err
	}
	output, err := validateJSONObject(c.Output, 1<<20, "output")
	if err != nil {
		return err
	}
	events, err := parseEvents(c.Events)
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
		stored, _, err := e.publish(ctx, ev)
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
	if _, _, err := e.publish(ctx, model.Event{
		Type:          eventType,
		Source:        model.SourceEngine,
		Payload:       payload,
		CausedByRunID: &result.Run.ID,
		Depth:         result.depthForEmitted(),
		CreatedAt:     at,
	}); err != nil {
		return fmt.Errorf("recording %s: %w", eventType, err)
	}

	// Published last, after everything is durable, so a client that stops
	// reading on `done` cannot miss a line that was still in flight.
	e.broker.Publish(result.Run.ID, StreamEvent{Kind: StreamDone, Status: status, TS: at})

	result.Run.Status = status
	result.Run.EndedAt = &at
	result.Run.Error = errMsg
	result.Attempt.Status = status
	result.Attempt.ExitCode = exec.ExitCode
	return nil
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

// validateJSONObject checks one of the single-value output channels (D6).
//
// Empty means "no change", unambiguously -- which is why not writing the file
// is a supported outcome rather than an error (D14).
//
// The caps are enforced on this side of D20's seam. A worker reads the file and
// sends what it found; whether that is acceptable is the control plane's
// decision, because it is the control plane's contract.
func validateJSONObject(body []byte, max int, what string) (json.RawMessage, error) {
	if len(body) > max {
		return nil, fmt.Errorf("job wrote %d bytes of %s, over the %d byte limit",
			len(body), what, max)
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

// parseEvents reads the events channel, one JSON object per line (D17).
func parseEvents(body []byte) ([]model.Event, error) {
	if len(body) == 0 {
		return nil, nil
	}
	var out []model.Event
	sc := bufio.NewScanner(bytes.NewReader(body))
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
