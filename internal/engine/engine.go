// Package engine is the core of the job engine, as a library.
//
// D18 makes this a structural requirement rather than a stylistic one: Almanac
// is a second consumer, and the way it consumes the engine is by running the
// daemon and talking to its API. For that to stay true, the core must have no
// opinion about being a daemon or a CLI. Concretely, nothing in this package
// may call os.Exit, print to stdout or stderr, read flags, or install signal
// handlers. It takes a context, returns errors, and logs through an injected
// *slog.Logger.
//
// The daemon is a thin wrapper around this. The CLI is a client of the daemon.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/jdmorlan/job-engine/internal/ca"
	"github.com/jdmorlan/job-engine/internal/lockfile"
	"github.com/jdmorlan/job-engine/internal/model"
	"github.com/jdmorlan/job-engine/internal/paths"
	"github.com/jdmorlan/job-engine/internal/secrets"
	"github.com/jdmorlan/job-engine/internal/store"
)

// Options configures a new Engine. Only Layout is required.
type Options struct {
	Layout paths.Layout

	// Logger receives the engine's own diagnostics. Nil means discard, which
	// is what a test wants; the daemon passes a real one.
	Logger *slog.Logger

	// GitHubAPI overrides where the GitHub API is, for GitHub Enterprise and
	// for tests that serve a repository locally. Empty means api.github.com.
	GitHubAPI string

	// Version is reported by Health and gates worker registration: D20/C10
	// refuses version skew loudly rather than negotiating.
	Version string

	// Now exists so tests can control time. Nil means time.Now.
	Now func() time.Time

	// Concurrency caps how many runs a single worker executes at once (D8).
	// Zero means DefaultConcurrency. Under D20/C11 the cap is enforced by the
	// worker, since that is where processes are; the control plane bounds the
	// queue, not the machine.
	Concurrency int
}

// Engine is the control plane. It owns the database, decides what should run,
// and records what came back. It is the sole writer (D20/C1) and it never
// executes a job (D20/C11) -- that is a worker's only job.
type Engine struct {
	opts    Options
	log     *slog.Logger
	now     func() time.Time
	store   *store.Store
	secrets *secrets.Store

	// authority issues worker identities, and tokens are the one-time
	// credentials that let a machine ask for one (D25 step 5). Both are
	// created lazily: a deployment that never enrolls a worker never writes a
	// CA key, and nothing about the plaintext path needs either.
	authorityOnce sync.Once
	authority     *ca.Authority
	authorityErr  error
	tokens        *ca.Tokens
	lock          *lockfile.Lock
	broker        *logBroker

	// inflight tracks runs a worker currently holds (D20). See the type.
	inflightMu sync.Mutex
	inflight   map[int64]*inflight

	// routes is the compiled trigger table, keyed by event type (D17). It is
	// held in memory because every recorded event is offered to it and it
	// changes only on load, so the common case -- an event nothing listens for
	// -- costs one map lookup rather than a query.
	routesMu sync.RWMutex
	routes   map[string][]compiledRoute

	// githubBaseURL is where the GitHub API lives. Empty means the real one.
	//
	// Configurable because GitHub Enterprise answers on a different host, which
	// is a real deployment rather than a test affordance -- and it is what lets
	// a test serve a repository without reaching the internet. See
	// export_test.go.
	githubBaseURL string

	// scheduleReloads carries a request from Sync to a running scheduler, and
	// a channel to answer on. Unbuffered on purpose: a send that finds nobody
	// listening means no scheduler is running, which Sync needs to be able to
	// tell from one that is merely slow.
	scheduleReloads chan chan error

	// started records whether Start ran. A tool that opens the engine to read
	// (a migration check, a test) must not litter the timeline with an
	// engine.started/stopped pair.
	started   bool
	startedAt time.Time
	// downtime is how long the engine was stopped before this start, derived
	// from the last engine.stopped event. Zero on a first-ever start.
	downtime time.Duration
	// uncleanStop records that the previous run ended without writing
	// engine.stopped -- a crash, a SIGKILL, or a laptop that lost power.
	uncleanStop bool
}

// New opens the engine's databases and takes the data directory lock.
//
// Taking the lock here rather than in the daemon is deliberate. The lock exists
// to guarantee a single writer, and this type is the writer. Putting the
// invariant on the component that can violate it means an embedding consumer
// (D18) gets the protection without having to remember to ask for it.
func New(opts Options) (*Engine, error) {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	if err := opts.Layout.EnsureData(); err != nil {
		return nil, err
	}

	lock, holder, err := lockfile.Acquire(opts.Layout.Lock())
	if err != nil {
		if errors.Is(err, lockfile.ErrHeld) {
			// P1: an error a human can act on. "Already running" is not
			// actionable; a pid is.
			return nil, fmt.Errorf(
				"another engine is already using %s (pid %d). "+
					"Two engines on one database double-fire every schedule",
				opts.Layout.Data, holder)
		}
		return nil, err
	}

	st, err := store.Open(opts.Layout)
	if err != nil {
		lock.Release()
		return nil, err
	}

	return &Engine{
		opts:          opts,
		githubBaseURL: opts.GitHubAPI,
		log:           opts.Logger,
		now:           opts.Now,
		store:         st,
		secrets:       secrets.Open(opts.Layout.Data),
		tokens:        ca.NewTokens(),
		lock:          lock,
		broker:        newLogBroker(),

		scheduleReloads: make(chan chan error),
	}, nil
}

// Start records that the engine came up and works out how long it was down.
//
// D16 makes downtime a recorded fact rather than an unexplained hole in the
// timeline. This matters more here than it would on a server: macOS suspends
// the daemon during sleep (D15), so gaps are routine, and the difference
// between "the machine was asleep" and "something went wrong" is most of what
// makes a laptop-hosted scheduler trustworthy. It is also the input D9's
// catch-up policy needs to decide what to do about the windows we missed.
func (e *Engine) Start(ctx context.Context) error {
	e.startedAt = e.now()
	e.started = true

	lastStop, err := e.store.LastEventOfType(ctx, model.EventEngineStopped)
	lastStart, startErr := e.store.LastEventOfType(ctx, model.EventEngineStarted)

	switch {
	case err == nil:
		e.downtime = e.startedAt.Sub(lastStop.CreatedAt)
		// A start with no stop after it means we died without cleaning up.
		e.uncleanStop = startErr == nil && lastStart.ID > lastStop.ID
	case isNoRows(err):
		// First ever start, or the stop event has aged out of retention.
		// Neither is a problem; there is simply no gap to report.
		e.uncleanStop = startErr == nil
	default:
		return fmt.Errorf("reading last stop event: %w", err)
	}

	// The route table before the first event, so a chain that starts from
	// engine.started is wired by the time the engine says it started. Routes
	// live in the database, so this is the table the last load left behind --
	// the engine comes up wired exactly as it went down.
	if err := e.reloadRoutes(ctx); err != nil {
		return err
	}

	if _, _, err := e.publish(ctx, model.Event{
		Type:      model.EventEngineStarted,
		Source:    model.SourceEngine,
		CreatedAt: e.startedAt,
	}); err != nil {
		return fmt.Errorf("recording engine.started: %w", err)
	}

	// D5: anything still running or queued in the database is a run we were
	// killed in the middle of. It becomes `interrupted`, a distinct state from
	// `failed`, because the job did not fail -- we did.
	interrupted, err := e.store.InterruptRunning(ctx, e.startedAt)
	if err != nil {
		return fmt.Errorf("recovering in-flight runs: %w", err)
	}

	e.log.Info("engine started",
		"data_dir", e.opts.Layout.Data,
		"downtime", e.downtime.Round(time.Second),
		"unclean_stop", e.uncleanStop,
		"interrupted_runs", interrupted)
	return nil
}

// Close records engine.stopped, then releases the database and the lock.
//
// The context is separate from the one that cancelled the daemon: shutdown
// still needs to write, and writing with an already-cancelled context would
// leave exactly the hole in the timeline this event exists to prevent.
func (e *Engine) Close(ctx context.Context) error {
	if e.started {
		// Appended rather than published. Routing on the way down would queue
		// work for an engine that is closing its database two lines later, and
		// a run that exists only to be interrupted is worse than no run.
		if _, _, err := e.store.AppendEvent(ctx, model.Event{
			Type:      model.EventEngineStopped,
			Source:    model.SourceEngine,
			CreatedAt: e.now(),
		}); err != nil {
			// Log and keep going. Failing to record the stop is bad for the
			// timeline, but refusing to release the lock would be worse.
			e.log.Error("recording engine.stopped", "error", err)
		}
		e.log.Info("engine stopped")
	}

	err := e.store.Close()
	if lockErr := e.lock.Release(); err == nil {
		err = lockErr
	}
	return err
}

// Emit records an externally supplied event. This is D16's single ingress --
// the one command that turns every outside system into an event source without
// the engine containing a line of code about any of them.
//
// deduped reports that an event with this dedupe key already existed, in which
// case the returned event is the original.
func (e *Engine) Emit(ctx context.Context, ev model.Event) (out model.Event, deduped bool, err error) {
	if ev.Type == "" {
		return model.Event{}, false, errors.New("event type is required")
	}
	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = e.now()
	}
	return e.publish(ctx, ev)
}

// RecentEvents returns the newest events, most recent first.
func (e *Engine) RecentEvents(ctx context.Context, limit int) ([]model.Event, error) {
	if limit <= 0 || limit > 1000 {
		limit = 50
	}
	return e.store.RecentEvents(ctx, limit)
}

// Secrets exposes the secret store for the CLI's `je secret` commands.
//
// The store deliberately has no Get on the CLI path (D10): this returns the
// type, and the type offers naming and listing to callers while reserving value
// access for the engine building a job's environment.
func (e *Engine) Secrets() *secrets.Store { return e.secrets }

// Health is the answer to "is everything OK?", which P1 insists must be a
// query with an exit code rather than a vibe.
type Health struct {
	Version   string        `json:"version"`
	StartedAt time.Time     `json:"started_at"`
	Uptime    time.Duration `json:"uptime_ns"`
	DataDir   string        `json:"data_dir"`

	// LastDowntime is how long the engine was down before this start.
	LastDowntime time.Duration `json:"last_downtime_ns"`
	// UncleanStop reports that the previous run never wrote engine.stopped.
	UncleanStop bool `json:"unclean_stop"`

	// Workers is how many are online, and Labels which capabilities they
	// cover between them (D20/C8).
	//
	// On the health endpoint rather than behind `je workers` because zero is
	// not a detail: a control plane with no worker attached runs nothing at
	// all, and that has to be the first thing `je status` says rather than
	// something you find out by asking the right question.
	Workers int      `json:"workers"`
	Labels  []string `json:"labels,omitempty"`
}

// Health reports the control plane's current state.
//
// It takes a context because the worker count is a query. That is a change
// from the version that could answer entirely from memory, and it is the right
// trade: an uptime that cannot tell you nothing is running is not health.
func (e *Engine) Health(ctx context.Context) Health {
	h := Health{
		Version:      e.opts.Version,
		StartedAt:    e.startedAt,
		Uptime:       e.now().Sub(e.startedAt),
		DataDir:      e.opts.Layout.Data,
		LastDowntime: e.downtime,
		UncleanStop:  e.uncleanStop,
	}

	now := e.now()
	workers, err := e.store.Workers(ctx)
	if err != nil {
		// Health must not fail. An unknown worker count renders as zero, which
		// errs towards alarming rather than reassuring -- the right direction
		// for the one number that says whether anything can run.
		e.log.Error("counting workers for health", "error", err)
		return h
	}
	seen := map[string]bool{}
	for _, w := range workers {
		if !w.Online(now, LeaseTTL) {
			continue
		}
		h.Workers++
		for _, label := range w.Labels {
			if !seen[label] {
				seen[label] = true
				h.Labels = append(h.Labels, label)
			}
		}
	}
	sort.Strings(h.Labels)
	return h
}
