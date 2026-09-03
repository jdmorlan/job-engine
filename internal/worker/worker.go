// Package worker is the data plane: the half of D20 that runs processes.
//
// It holds no durable state (C2). Everything it knows came from a Dispatch it
// was handed, and everything it learns goes back over the API. A restarting
// worker loses nothing but its in-flight runs, which is the constraint that
// keeps this from becoming a distributed system -- there is no worker-side
// database, no sync protocol, and nothing to reconcile on reconnect.
//
// It dials out and never listens (C4). No inbound ports, no service discovery,
// no registry, and it works from a laptop on cellular behind CGNAT.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jdmorlan/job-engine/internal/engine"
	"github.com/jdmorlan/job-engine/internal/executor"
	"github.com/jdmorlan/job-engine/internal/store"
)

// Options configures a worker.
type Options struct {
	// Name is what a human calls this machine. It appears in `je workers`.
	Name string

	// Labels are the capabilities this worker advertises (C3). A job's
	// runs_on must match one of them for its runs to arrive here.
	Labels []string

	// Concurrency caps how many jobs run here at once (D8). Under C11 the cap
	// belongs to the worker, because this is where processes are.
	Concurrency int

	// Version must match the control plane's (C10).
	Version string

	// JobsDir is where this machine keeps job definitions and the code beside
	// them. It is what an unset `workdir` resolves to (D20: paths are resolved
	// by whoever will use them).
	JobsDir string

	Client *Client
	Logger *slog.Logger
}

// pollInterval is how often an idle worker asks for work.
//
// A plain poll rather than a push. C4 requires the worker to dial out, and a
// held-open long poll would be the better shape for latency -- but it is also
// the shape that hides a half-dead connection until something needs to happen.
// Asking repeatedly makes liveness a property of the ordinary path rather than
// of an error path nobody exercises.
const pollInterval = time.Second

// Worker pulls dispatches from the control plane and executes them.
type Worker struct {
	opts   Options
	log    *slog.Logger
	client *Client

	id string

	mu      sync.Mutex
	holding map[int64]context.CancelFunc
}

// New returns a worker. It does not contact the control plane.
func New(opts Options) (*Worker, error) {
	if opts.Name == "" {
		return nil, errors.New("a worker needs a name")
	}
	if opts.Client == nil {
		return nil, errors.New("a worker needs a client")
	}
	if len(opts.Labels) == 0 {
		opts.Labels = []string{store.DefaultLabel}
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = engine.DefaultConcurrency
	}
	if opts.JobsDir == "" {
		return nil, errors.New("a worker needs a jobs directory to resolve workdirs against")
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return &Worker{
		opts:    opts,
		log:     opts.Logger,
		client:  opts.Client,
		holding: map[int64]context.CancelFunc{},
	}, nil
}

// Run registers and then serves until the context is cancelled.
func (w *Worker) Run(ctx context.Context) error {
	id, err := w.register(ctx)
	if err != nil {
		return err
	}
	w.id = id
	w.log.Info("worker registered",
		"name", w.opts.Name, "labels", w.opts.Labels, "concurrency", w.opts.Concurrency)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.heartbeatLoop(ctx)
	}()

	// One puller per concurrency slot. A slot that is executing does not ask
	// for more work, which is what makes the cap a cap rather than a hint.
	wg.Add(w.opts.Concurrency)
	for i := range w.opts.Concurrency {
		go func() {
			defer wg.Done()
			w.pullLoop(ctx, i)
		}()
	}

	wg.Wait()
	return nil
}

// register identifies this worker to the control plane.
//
// A stable id derived from the name means a restarted worker rejoins its own
// registration rather than accumulating rows. C2 makes that safe: there is
// nothing to reconcile, because the worker held nothing worth keeping.
func (w *Worker) register(ctx context.Context) (string, error) {
	saved, err := w.client.Register(ctx, store.Worker{
		ID:      workerID(w.opts.Name),
		Name:    w.opts.Name,
		Labels:  w.opts.Labels,
		Roles:   []string{store.RoleExecute},
		Version: w.opts.Version,
	})
	if err != nil {
		return "", err
	}
	return saved.ID, nil
}

// heartbeatLoop renews the lease and acts on C7's fencing list.
func (w *Worker) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(engine.HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		revoked, err := w.client.Heartbeat(ctx, w.id, w.held())
		if err != nil {
			// A failed heartbeat is not fatal here. The control plane decides
			// liveness unilaterally (C5), so the worst this costs is a lease
			// that expires while we keep working -- which C7 then resolves on
			// the next successful contact, by telling us to stop.
			w.log.Warn("heartbeat failed", "error", err)
			continue
		}
		for _, runID := range revoked {
			// C7: our claim was revoked while we were out of contact. Kill the
			// process and discard the result rather than writing into a run
			// the control plane has already given up on.
			w.log.Warn("lease revoked, abandoning run", "run", runID)
			w.abandon(runID)
		}
	}
}

func (w *Worker) pullLoop(ctx context.Context, slot int) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		dispatch, err := w.client.Claim(ctx, w.id)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			w.log.Error("claiming work", "slot", slot, "error", err)
			continue
		}
		if dispatch == nil {
			continue
		}
		w.execute(ctx, *dispatch)
	}
}

// execute runs one dispatch to completion and reports back.
//
// This is the half of the old engine.execute() that touches a process, and it
// is the only place in the system that starts one (C11).
func (w *Worker) execute(ctx context.Context, d engine.Dispatch) {
	runCtx, cancel := context.WithCancel(ctx)
	w.hold(d.RunID, cancel)
	defer func() {
		w.release(d.RunID)
		cancel()
	}()

	// The scratch directory holds the three output channels. It is per-attempt
	// and removed afterwards, which is what makes them a handoff rather than
	// storage -- the durable copy is the one the control plane promotes.
	//
	// It lives here, on the machine where the job runs, so D6 is unchanged by
	// the network: the job writes to local files exactly as it always did and
	// never learns that the engine is somewhere else.
	// The slug carries its source (D22), and a source-qualified name has a
	// slash in it -- which MkdirTemp rejects as a path separator rather than
	// treating as a name.
	scratch, err := os.MkdirTemp("", fmt.Sprintf("je-%s-%d-%d-",
		strings.ReplaceAll(d.JobSlug, "/", "-"), d.RunID, d.Attempt))
	if err != nil {
		w.report(ctx, d, engine.Completion{
			ExecError: fmt.Sprintf("creating scratch directory: %v", err),
		})
		return
	}
	defer os.RemoveAll(scratch)

	ch := newChannels(scratch)
	env := append([]string{}, d.Env...)
	env = append(env,
		"JOB_WORKDIR="+scratch,
		"JOB_STATE_OUT_FILE="+ch.stateOut,
		"JOB_OUTPUT_FILE="+ch.output,
		"JOB_EVENTS_FILE="+ch.events,
	)

	workdir, err := w.resolveWorkdir(d.Workdir, d.SourceRoot)
	if err != nil {
		w.report(ctx, d, engine.Completion{ExecError: err.Error()})
		return
	}

	sink := newLogShipper(w.client, d.RunID, d.Attempt, w.log)
	result, execErr := executor.Process{}.Run(runCtx, executor.Spec{
		Command: d.Command,
		Workdir: workdir,
		Env:     env,
		Timeout: d.Timeout,
		Grace:   d.Grace,
		Output:  sink,
	})
	sink.Close()

	if runCtx.Err() != nil && ctx.Err() == nil {
		// Cancelled by C7's fencing rather than by shutdown. The control plane
		// has already recorded this run as lost; reporting a result now is
		// exactly what fencing exists to prevent.
		w.log.Warn("discarding result for a revoked run", "run", d.RunID)
		return
	}

	completion := engine.Completion{
		Result: result,
		// D5: distinguish "we were shutting down" from "the job failed". Only
		// this side can tell, because only this side holds the context that
		// killed the process.
		Interrupted: ctx.Err() != nil,
	}
	if execErr != nil {
		completion.ExecError = execErr.Error()
	} else if result.Succeeded() {
		// Only read on success. A failed attempt's channels are discarded
		// (D14), and reading them would be work whose result is thrown away.
		completion.StateOut = readChannel(ch.stateOut)
		completion.Output = readChannel(ch.output)
		completion.Events = readChannel(ch.events)
	}

	// Reported with the parent context, not runCtx: shutting down is precisely
	// when recording what happened matters most (D5, D16).
	w.report(ctx, d, completion)
}

// report sends a completion, tolerating a control plane that is briefly away.
func (w *Worker) report(ctx context.Context, d engine.Dispatch, c engine.Completion) {
	reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	if err := w.client.Complete(reportCtx, d.RunID, w.id, c); err != nil {
		// Nothing to retry into: the run belongs to the control plane, and if
		// it will not accept the result the lease will expire and the run
		// becomes `lost` (C6). Saying so here is the only useful action.
		w.log.Error("reporting completion", "run", d.RunID, "error", err)
	}
}

func (w *Worker) hold(runID int64, cancel context.CancelFunc) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.holding[runID] = cancel
}

func (w *Worker) release(runID int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.holding, runID)
}

func (w *Worker) held() []int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]int64, 0, len(w.holding))
	for id := range w.holding {
		out = append(out, id)
	}
	return out
}

func (w *Worker) abandon(runID int64) {
	w.mu.Lock()
	cancel := w.holding[runID]
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

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

// readChannel reads one output channel, or nothing if the job did not write it.
//
// A missing file means "no change", unambiguously, which is why not writing it
// is a supported outcome rather than an error (D14). Size and shape are the
// control plane's to judge (D6), so this reads generously and lets the far side
// refuse -- with one bound, so that a job which writes a gigabyte cannot make
// the worker the thing that falls over.
func readChannel(path string) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	body := make([]byte, maxChannelBytes+1)
	n, _ := f.Read(body)
	return body[:n]
}

// maxChannelBytes bounds what a worker will carry. It is deliberately larger
// than any of D6's real caps, so that an oversized channel is refused by the
// control plane with the protocol's own error message rather than truncated
// into a confusing one here.
const maxChannelBytes = 4 << 20

// workerID derives a stable id from a name.
func workerID(name string) string { return "worker-" + name }

// resolveWorkdir decides where a command runs, on this machine.
//
// D20 moved this out of the control plane, which is the only place it can
// correctly live once the two can be different machines: an unset workdir means
// "where this worker keeps job definitions", and only the worker knows where
// that is. A control plane in a container resolving it would send its own
// container path to a laptop.
//
// The consequence worth stating: a job whose command references files next to
// its definition needs those files on the worker. Splitting the engine in two
// splits the definition from the code it runs, and closing that gap properly is
// D22's job (sources), not something to paper over with a path.
func (w *Worker) resolveWorkdir(declared, sourceRoot string) (string, error) {
	expanded, err := expandHome(declared)
	if err != nil {
		return "", err
	}

	// A job from a registered source runs in that source's tree, because its
	// code arrived with its definition and lives beside it there (D22). Only
	// the built-in local source falls back to this worker's jobs directory.
	base := w.opts.JobsDir
	if sourceRoot != "" {
		if _, err := os.Stat(sourceRoot); err != nil {
			// Said rather than papered over. Falling back to this worker's own
			// jobs directory would run the command somewhere its files are
			// not, and "command not found" three layers down is a much worse
			// version of this sentence.
			return "", fmt.Errorf(
				"this job's code is in %s, which this worker cannot see: %w",
				sourceRoot, err)
		}
		base = sourceRoot
	}

	switch {
	case expanded == "":
		return base, nil
	case filepath.IsAbs(expanded):
		return expanded, nil
	default:
		return filepath.Join(base, expanded), nil
	}
}

// expandHome resolves a leading ~ in a configured path.
//
// Job files say `workdir: ~/code/almanac`, and a process's Dir is not run
// through a shell, so nothing else would expand it. Without this the job fails
// with "no such directory: ~/code/almanac", which reads like a typo.
//
// It expands against the *worker's* home, which is the right one: this is the
// user the job will run as.
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
