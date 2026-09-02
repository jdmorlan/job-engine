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
	"time"

	"github.com/jdmorlan/job-engine/internal/lockfile"
	"github.com/jdmorlan/job-engine/internal/model"
	"github.com/jdmorlan/job-engine/internal/paths"
	"github.com/jdmorlan/job-engine/internal/store"
)

// Options configures a new Engine. Only Layout is required.
type Options struct {
	Layout paths.Layout

	// Logger receives the engine's own diagnostics. Nil means discard, which
	// is what a test wants; the daemon passes a real one.
	Logger *slog.Logger

	// Version is reported by Health, and will later gate node registration
	// (D20 C10 refuses version skew loudly rather than negotiating).
	Version string

	// Now exists so tests can control time. Nil means time.Now.
	Now func() time.Time
}

// Engine owns the database and, once the scheduler exists, the run loop.
// It is the sole writer (D20 C1).
type Engine struct {
	opts  Options
	log   *slog.Logger
	now   func() time.Time
	store *store.Store
	lock  *lockfile.Lock

	// started records whether Start ran. A one-shot `je run` with no daemon
	// (D19 stage 0) opens the engine without claiming to be one, and must not
	// litter the timeline with an engine.started/stopped pair per command.
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
		opts:  opts,
		log:   opts.Logger,
		now:   opts.Now,
		store: st,
		lock:  lock,
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

	if _, _, err := e.store.AppendEvent(ctx, model.Event{
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
	return e.store.AppendEvent(ctx, ev)
}

// RecentEvents returns the newest events, most recent first.
func (e *Engine) RecentEvents(ctx context.Context, limit int) ([]model.Event, error) {
	if limit <= 0 || limit > 1000 {
		limit = 50
	}
	return e.store.RecentEvents(ctx, limit)
}

// Health is the answer to "is everything OK?", which P1 insists must be a
// query with an exit code rather than a vibe.
type Health struct {
	Version   string        `json:"version"`
	StartedAt time.Time     `json:"started_at"`
	Uptime    time.Duration `json:"uptime_ns"`
	DataDir   string        `json:"data_dir"`
	JobsDir   string        `json:"jobs_dir"`

	// LastDowntime is how long the engine was down before this start.
	LastDowntime time.Duration `json:"last_downtime_ns"`
	// UncleanStop reports that the previous run never wrote engine.stopped.
	UncleanStop bool `json:"unclean_stop"`
}

// Health reports the engine's current state.
func (e *Engine) Health() Health {
	return Health{
		Version:      e.opts.Version,
		StartedAt:    e.startedAt,
		Uptime:       e.now().Sub(e.startedAt),
		DataDir:      e.opts.Layout.Data,
		JobsDir:      e.opts.Layout.Jobs,
		LastDowntime: e.downtime,
		UncleanStop:  e.uncleanStop,
	}
}
