package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jdmorlan/job-engine/internal/executor"
	"github.com/jdmorlan/job-engine/internal/model"
	"github.com/jdmorlan/job-engine/internal/store"
)

// The control plane's half of D20.
//
// C11: the control plane never executes. It decides what should run, hands a
// worker everything needed to run it, and records what came back. Nothing in
// this file starts a process, and the executor package is imported only for the
// result type a worker reports.
//
// The seam is drawn where D14's contract already drew it. The cursor is read
// before the run exists and committed only after the attempt exits zero, so
// everything between those two points -- which is now a network round trip and
// a process on another machine -- can fail without moving it. That property is
// why this split is a relocation rather than a redesign.

// LeaseTTL is how long a claim is good for before the control plane decides the
// worker is gone (C5).
//
// Deliberately generous relative to the heartbeat interval: expiring a lease
// declares somebody's running job lost, and doing that because one heartbeat
// was slow would be worse than waiting. C6 is the reason this cannot be tuned
// to zero risk -- dead and partitioned are indistinguishable, so this number
// only chooses how long we wait before saying so.
const LeaseTTL = 90 * time.Second

// HeartbeatInterval is how often a worker should renew. Three fit inside a
// lease, so two may be lost without consequence.
const HeartbeatInterval = 30 * time.Second

// ErrLeaseLost is C7's fencing signal, returned to a worker whose claim was
// revoked while it was out of contact. The worker's duty on seeing it is to
// kill the process and discard the result rather than write into a run the
// control plane has already given up on.
var ErrLeaseLost = errors.New("this run's lease was revoked")

// ErrVersionSkew is C10: a worker whose version does not match refuses to
// register and is told why, which is cheaper than protocol negotiation and more
// honest than silent incompatibility.
var ErrVersionSkew = errors.New("worker version does not match the control plane")

// ErrLabelTaken is D20 open question 2, answered as "refuse".
//
// Two workers advertising one label is far more likely to be an accidental
// second laptop than a deliberate pair, and C1 gives no reason to want two.
// Refusing at registration makes the mistake visible immediately instead of at
// 3am when a job runs on the wrong machine.
var ErrLabelTaken = errors.New("another online worker already advertises this label")

// Dispatch is one attempt, everything a worker needs to run it, and nothing
// else.
//
// It is a plain value for the same reason executor.Spec is: the worker gets no
// access to the store, the engine, or the database (C1). In particular it gets
// resolved secret values because it must build the process environment, and no
// way to enumerate the store they came from -- D10's "only declared secrets"
// rule survives the network hop unchanged.
type Dispatch struct {
	RunID   int64  `json:"run_id"`
	Attempt int    `json:"attempt"`
	JobSlug string `json:"job_slug"`

	Command []string `json:"command"`
	Workdir string   `json:"workdir"`

	// Env is the complete environment minus the four values the worker can
	// only know locally: JOB_WORKDIR and the three output channel paths (D6).
	// The worker creates the scratch directory and appends them, which is what
	// keeps the job's contract byte-identical whether it runs beside the
	// control plane or on another machine.
	Env []string `json:"env"`

	Timeout time.Duration `json:"timeout"`
	Grace   time.Duration `json:"grace"`

	// Lease is how long this claim is good for without a heartbeat.
	Lease time.Duration `json:"lease"`
}

// Completion is a worker's report of how an attempt ended.
//
// The three channels arrive as bytes rather than as parsed values because the
// control plane validates them (D6's size caps, the JSON object constraint, the
// depth guard). A worker is not a trusted validator of the protocol; it is a
// place where a process ran.
type Completion struct {
	Result executor.Result `json:"result"`

	// ExecError means the worker could not run the command at all -- a missing
	// binary, an unreadable workdir. Distinct from a command that ran and
	// failed, which is a Result with a non-zero exit code.
	ExecError string `json:"exec_error,omitempty"`

	// Interrupted is D5: the worker was shutting down, so the job did not fail
	// -- we did. It has to travel on the wire because only the worker knows it.
	// Before D20 the engine read its own context here; now the process and the
	// context that killed it are on the other side of the seam, and inferring
	// "interrupted" from a non-zero exit on this side would relabel every
	// ordinary failure during a restart.
	Interrupted bool `json:"interrupted,omitempty"`

	StateOut []byte `json:"state_out,omitempty"`
	Output   []byte `json:"output,omitempty"`
	Events   []byte `json:"events,omitempty"`
}

// LogLine is one captured line on its way from a worker to storage.
type LogLine struct {
	Stream string    `json:"stream"`
	TS     time.Time `json:"ts"`
	Line   string    `json:"line"`
}

// RegisterWorker admits a worker to the data plane.
func (e *Engine) RegisterWorker(ctx context.Context, w store.Worker) (store.Worker, error) {
	if w.ID == "" || w.Name == "" {
		return store.Worker{}, errors.New("a worker needs an id and a name")
	}
	if len(w.Labels) == 0 {
		w.Labels = []string{store.DefaultLabel}
	}
	if len(w.Roles) == 0 {
		w.Roles = []string{store.RoleExecute}
	}
	if !sameVersion(w.Version, e.opts.Version) {
		return store.Worker{}, fmt.Errorf("%w: worker is %s, control plane is %s",
			ErrVersionSkew, w.Version, e.opts.Version)
	}

	now := e.now()
	existing, err := e.store.Workers(ctx)
	if err != nil {
		return store.Worker{}, err
	}
	for _, other := range existing {
		if other.ID == w.ID || !other.Online(now, LeaseTTL) {
			continue
		}
		for _, label := range w.Labels {
			for _, taken := range other.Labels {
				if label == taken {
					return store.Worker{}, fmt.Errorf("%w: %q is served by %s",
						ErrLabelTaken, label, other.Name)
				}
			}
		}
	}

	w.RegisteredAt, w.LastSeenAt = now, now
	saved, err := e.store.RegisterWorker(ctx, w)
	if err != nil {
		return store.Worker{}, err
	}
	e.log.Info("worker registered", "worker", w.Name, "labels", w.Labels)
	e.recordWorkerEvent(ctx, EventWorkerRegistered, w, "")
	return saved, nil
}

// Heartbeat renews a worker's lease and the leases on the runs it holds.
//
// The returned ids are runs the worker believes it holds but does not: C7's
// fencing list. Renewing the run leases here rather than in a separate call is
// what makes a partitioned worker lose its claim on exactly the runs whose
// leases lapsed, rather than on all of them or none.
func (e *Engine) Heartbeat(ctx context.Context, workerID string, holding []int64) ([]int64, error) {
	now := e.now()
	if err := e.store.TouchWorker(ctx, workerID, now); err != nil {
		return nil, err
	}

	var revoked []int64
	for _, runID := range holding {
		ok, err := e.store.RenewRunLease(ctx, runID, workerID, now, LeaseTTL)
		if err != nil {
			return nil, err
		}
		if !ok {
			revoked = append(revoked, runID)
		}
	}
	return revoked, nil
}

// Claim leases the next run this worker may take and returns its Dispatch.
//
// A nil Dispatch with a nil error means there is nothing for this worker, which
// is the ordinary case and not a failure.
func (e *Engine) Claim(ctx context.Context, workerID string) (*Dispatch, error) {
	worker, err := e.store.WorkerByID(ctx, workerID)
	if err != nil {
		return nil, err
	}
	now := e.now()
	if err := e.store.TouchWorker(ctx, workerID, now); err != nil {
		return nil, err
	}

	run, err := e.store.ClaimNextRunForWorker(ctx, workerID, worker.Labels, now, LeaseTTL)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	prepared, err := e.prepareClaimed(ctx, run)
	if err != nil {
		return nil, err
	}
	return e.dispatchFor(ctx, prepared, worker)
}

// dispatchFor turns a claimed run into the value a worker executes.
//
// This is the first half of what used to be execute(): the attempt row, the
// environment, and the resolved secrets. The second half -- promote and finish
// -- is Complete. Nothing between them touches a process.
func (e *Engine) dispatchFor(ctx context.Context, p Prepared, worker store.Worker) (*Dispatch, error) {
	startedAt := e.now()
	p.Run.StartedAt = &startedAt
	p.Run.Status = model.StatusRunning
	e.broker.Publish(p.Run.ID, StreamEvent{
		Kind: StreamStatus, Status: model.StatusRunning, TS: startedAt,
	})

	attempt, err := e.store.CreateAttempt(ctx, store.Attempt{
		RunID:             p.Run.ID,
		TriggeringEventID: &p.Cause.ID,
		Actor:             p.Actor,
		Executor:          string(p.Def.Runtime),
	})
	if err != nil {
		return nil, err
	}

	env, err := e.buildEnv(ctx, p.Job, p.Def, p.Run, attempt, p.Cause, p.StateIn)
	if err != nil {
		return nil, err
	}
	workdir, err := e.resolveWorkdir(p.Def.Workdir)
	if err != nil {
		return nil, err
	}

	// The same resolved values that went into the environment drive redaction,
	// so the two cannot drift (D10). Redaction stays on this side: logs are
	// stored here, and a worker bug must not be able to put a secret into the
	// permanent record.
	resolved, err := e.secrets.Resolve(p.Def.Secrets)
	if err != nil {
		return nil, fmt.Errorf("job %s: %w", p.Job.Slug, err)
	}
	e.rememberRedactor(p.Run.ID, newRedactor(resolved))

	e.log.Info("dispatched",
		"job", p.Job.Slug, "run", p.Run.ID, "attempt", attempt.Number, "worker", worker.Name)

	return &Dispatch{
		RunID:   p.Run.ID,
		Attempt: attempt.Number,
		JobSlug: p.Job.Slug,
		Command: p.Def.Command,
		Workdir: workdir,
		Env:     env,
		Timeout: p.Def.Timeout.D,
		Grace:   executor.DefaultGrace,
		Lease:   LeaseTTL,
	}, nil
}

// AppendLogs stores lines a worker captured and republishes them to watchers.
//
// Redaction happens here rather than on the worker so that what is written to
// the log database is redacted regardless of what the worker sent (D10).
func (e *Engine) AppendLogs(ctx context.Context, runID int64, attempt int, lines []LogLine) error {
	if len(lines) == 0 {
		return nil
	}
	red, seq := e.nextSeq(runID, len(lines))

	stored := make([]store.LogLine, 0, len(lines))
	published := make([]StreamEvent, 0, len(lines))
	for i, line := range lines {
		text := line.Line
		if red != nil {
			text = red.Replace(text)
		}
		n := seq + int64(i) + 1
		stored = append(stored, store.LogLine{
			RunID: runID, Attempt: attempt, Seq: n,
			Stream: line.Stream, TS: line.TS, Line: text,
		})
		published = append(published, StreamEvent{
			Kind: StreamLog, Seq: n, Attempt: attempt,
			Stream: line.Stream, TS: line.TS, Line: text,
		})
	}

	if err := e.store.AppendLogs(ctx, stored); err != nil {
		return err
	}
	// Published after storage, so a subscriber can never see a line the
	// control plane has not yet accepted.
	for _, ev := range published {
		e.broker.Publish(runID, ev)
	}
	return nil
}

// Complete records how an attempt ended.
//
// This is the second half of the old execute(), and the ordering inside it is
// unchanged, because the ordering is D14's contract: everything is parsed and
// validated before anything is committed, and the cursor moves only after a
// zero exit.
func (e *Engine) Complete(ctx context.Context, runID int64, workerID string, c Completion) error {
	run, err := e.store.RunByID(ctx, runID)
	if err != nil {
		return err
	}
	if run.WorkerID == nil || *run.WorkerID != workerID {
		// C7: the claim was revoked while this worker was out of contact. The
		// control plane has already moved on, and letting a late result write
		// state now is precisely what fencing exists to prevent.
		return fmt.Errorf("%w: run %d is no longer held by %s", ErrLeaseLost, runID, workerID)
	}
	if run.Status.Terminal() {
		return fmt.Errorf("%w: run %d already finished as %s", ErrLeaseLost, runID, run.Status)
	}

	prepared, err := e.prepareClaimed(ctx, run)
	if err != nil {
		return err
	}
	attempt, err := e.attemptNumber(ctx, runID, run.AttemptCount)
	if err != nil {
		return err
	}

	defer e.forgetRedactor(runID)

	result := &RunResult{
		Run: run, Attempt: attempt, Job: prepared.Job,
		StateIn: &prepared.StateIn, causeDepth: prepared.Cause.Depth,
		PrimaryCursor: prepared.Def.State.PrimaryCursor,
		TimedOut:      c.Result.TimedOut, Killed: c.Result.Killed,
	}

	if c.ExecError != "" {
		return e.finish(ctx, result, model.StatusFailed, c.Result, c.ExecError)
	}
	if !c.Result.Succeeded() {
		status := statusForCompletion(c)
		// D14, the whole point: failure, timeout or interruption and the cursor
		// does not move. The channel contents are simply discarded.
		return e.finish(ctx, result, status, c.Result, interruptMessage(status, c.Result))
	}

	if err := e.promote(ctx, result, prepared.Def, c); err != nil {
		return e.finish(ctx, result, model.StatusFailed, c.Result, err.Error())
	}
	return e.finish(ctx, result, model.StatusSucceeded, c.Result, "")
}

// attemptNumber loads the attempt a completion refers to.
func (e *Engine) attemptNumber(ctx context.Context, runID int64, number int) (store.Attempt, error) {
	attempts, err := e.store.AttemptsForRun(ctx, runID)
	if err != nil {
		return store.Attempt{}, err
	}
	for _, a := range attempts {
		if a.Number == number {
			return a, nil
		}
	}
	return store.Attempt{}, fmt.Errorf("run %d has no attempt %d", runID, number)
}

// ExpireLeases marks runs whose worker stopped heartbeating.
//
// C6's honest outcome: `lost` rather than `failed`, because we do not know that
// the job failed -- only that we stopped hearing about it. C8 requires that
// this be loud rather than a silent stall, so it is an event and a terminal
// status, not a run left running forever.
//
// `on_node_lost: retry` (at-least-once) is not implemented; every lost run
// takes the `fail` path, which is the safe default and the only one v0.6 ships.
func (e *Engine) ExpireLeases(ctx context.Context) (int, error) {
	now := e.now()
	expired, err := e.store.ExpiredLeases(ctx, now)
	if err != nil {
		return 0, err
	}

	for _, run := range expired {
		job, err := e.store.JobByID(ctx, run.JobID)
		if err != nil {
			return 0, err
		}
		msg := "the worker holding this run stopped responding; " +
			"it may have died, or it may still be running the job"
		if err := e.store.FinishRun(ctx, run.ID, model.StatusLost, now, nil, msg); err != nil {
			return 0, err
		}
		if run.AttemptCount > 0 {
			if attempt, err := e.attemptNumber(ctx, run.ID, run.AttemptCount); err == nil {
				if err := e.store.FinishAttempt(
					ctx, attempt.ID, model.StatusLost, now, nil, msg); err != nil {
					return 0, err
				}
			}
		}
		payload, _ := json.Marshal(map[string]any{
			"job": job.Slug, "run": run.ID, "worker": run.WorkerID,
		})
		if _, _, err := e.store.AppendEvent(ctx, model.Event{
			Type:          EventRunLost,
			Source:        model.SourceEngine,
			Payload:       payload,
			CausedByRunID: &run.ID,
			CreatedAt:     now,
		}); err != nil {
			return 0, err
		}
		e.broker.Publish(run.ID, StreamEvent{Kind: StreamDone, Status: model.StatusLost, TS: now})
		e.forgetRedactor(run.ID)
		e.log.Warn("run lost", "run", run.ID, "job", job.Slug)

		if run.WorkerID != nil {
			if err := e.store.MarkWorkerGone(ctx, *run.WorkerID, now); err != nil {
				return 0, err
			}
		}
	}
	return len(expired), nil
}

// Workers lists the data plane (C8).
func (e *Engine) Workers(ctx context.Context) ([]WorkerView, error) {
	workers, err := e.store.Workers(ctx)
	if err != nil {
		return nil, err
	}
	now := e.now()
	out := make([]WorkerView, 0, len(workers))
	for _, w := range workers {
		out = append(out, WorkerView{Worker: w, Online: w.Online(now, LeaseTTL)})
	}
	return out, nil
}

// WorkerView is a worker plus the liveness verdict, so a client does not have
// to know what the lease TTL is to render the only column anyone reads.
type WorkerView struct {
	store.Worker
	Online bool `json:"online"`
}

// inflight is what the control plane keeps in memory for a run a worker holds.
//
// Two things, both of which would otherwise cost a query per log batch: the
// redactor (a definition lookup and a secret resolve) and the sequence number
// (a MAX over the log table). Log lines arrive in the hot path, so neither
// belongs there.
//
// Losing this map costs correctness in exactly one bounded way: a control plane
// that restarts mid-run restarts the sequence at zero for that attempt. The run
// is already being finished as interrupted at that point (D5), so the lines in
// question belong to a run nobody will read as authoritative.
type inflight struct {
	redact *strings.Replacer
	seq    int64
}

func (e *Engine) rememberRedactor(runID int64, r *strings.Replacer) {
	e.inflightMu.Lock()
	defer e.inflightMu.Unlock()
	if e.inflight == nil {
		e.inflight = map[int64]*inflight{}
	}
	e.inflight[runID] = &inflight{redact: r}
}

// nextSeq reserves sequence numbers for a batch and returns the redactor.
func (e *Engine) nextSeq(runID int64, n int) (*strings.Replacer, int64) {
	e.inflightMu.Lock()
	defer e.inflightMu.Unlock()
	f := e.inflight[runID]
	if f == nil {
		f = &inflight{}
		if e.inflight == nil {
			e.inflight = map[int64]*inflight{}
		}
		e.inflight[runID] = f
	}
	start := f.seq
	f.seq += int64(n)
	return f.redact, start
}

func (e *Engine) forgetRedactor(runID int64) {
	e.inflightMu.Lock()
	defer e.inflightMu.Unlock()
	delete(e.inflight, runID)
}

// Event types for the data plane's own lifecycle (P2, C8).
const (
	EventWorkerRegistered = "worker.registered"
	EventWorkerLost       = "worker.lost"
	EventRunLost          = "run.lost"
)

func (e *Engine) recordWorkerEvent(ctx context.Context, kind string, w store.Worker, reason string) {
	payload, _ := json.Marshal(map[string]any{
		"worker": w.Name, "labels": w.Labels, "reason": reason,
	})
	if _, _, err := e.store.AppendEvent(ctx, model.Event{
		Type:      kind,
		Source:    model.SourceEngine,
		Payload:   payload,
		CreatedAt: e.now(),
	}); err != nil {
		e.log.Error("recording "+kind, "worker", w.Name, "error", err)
	}
}

// sameVersion compares versions ignoring a leading v.
func sameVersion(a, b string) bool {
	return strings.TrimPrefix(a, "v") == strings.TrimPrefix(b, "v")
}

// statusForCompletion decides which non-success terminal status a report means.
//
// The order matters: a timeout that arrived during a shutdown is still a
// timeout, because the job had already exhausted its own budget before we
// started stopping.
func statusForCompletion(c Completion) model.Status {
	switch {
	case c.Result.TimedOut:
		// D8 keeps this distinct from `failed` because they call for different
		// responses: one is a slow job, the other is a broken one.
		return model.StatusTimedOut
	case c.Interrupted:
		// D5: we were killed, not the job. `interrupted`, not `failed`.
		return model.StatusInterrupted
	default:
		return model.StatusFailed
	}
}
