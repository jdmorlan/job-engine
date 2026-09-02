package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/jdmorlan/job-engine/internal/engine"
	"github.com/jdmorlan/job-engine/internal/jobdef"
	"github.com/jdmorlan/job-engine/internal/model"
	"github.com/jdmorlan/job-engine/internal/store"
)

// Reader is the read surface every observability command uses.
//
// Two things satisfy it: the daemon's HTTP client, and an engine opened in this
// process. Having one interface means each command is written once, rather than
// once per transport -- which is what keeps the two paths from drifting into
// showing subtly different answers to the same question.
//
// D15 wants everything to go through the daemon. That is what happens when one
// is running. The embedded path is D19's stage 0: a binary you downloaded, no
// daemon, and every command still works.
type Reader interface {
	Jobs(ctx context.Context) ([]store.Job, error)
	Job(ctx context.Context, slug string) (store.Job, error)
	Definition(ctx context.Context, slug string) (*jobdef.Definition, error)
	Runs(ctx context.Context, jobSlug string, limit int) ([]store.Run, error)
	Run(ctx context.Context, id int64) (store.Run, error)
	Logs(ctx context.Context, runID int64, attempt int) ([]store.LogLine, error)
	CurrentState(ctx context.Context, slug string) (*store.StateVersion, error)
	StateHistory(ctx context.Context, slug string, limit int) ([]store.StateVersion, error)
	Waiting(ctx context.Context) (engine.Waiting, error)
	Events(ctx context.Context, limit int) ([]model.Event, error)

	// Source describes where the answers came from, so a confusing result can
	// be traced to the right place.
	Source() string
}

// withReader runs fn against a daemon if one is listening, and against an
// engine opened here if not.
//
// The daemon is tried first and always wins, because it is the process holding
// the truth: it has the scheduler's in-memory state, and it is the only writer.
// Falling back the other way -- preferring local and using the daemon only on
// failure -- would mean the CLI sometimes read a database another process was
// actively writing, which is exactly the locking surprise D15 avoids.
func withReader(ctx context.Context, env *Env, fn func(context.Context, Reader) error) error {
	if client, err := Connect(env.Layout); err == nil {
		if reachable(ctx, client) {
			return fn(ctx, client)
		}
	}

	eng, err := engine.New(engine.Options{
		Layout:  env.Layout,
		Version: env.Version,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		return adviseLocked(err)
	}
	defer func() {
		if closeErr := eng.Close(context.WithoutCancel(ctx)); closeErr != nil {
			fmt.Fprintf(env.Stderr, "je: closing engine: %v\n", closeErr)
		}
	}()

	// With no daemon watching files, what is on disk is the only meaningful
	// answer to "what is this job?".
	if _, err := eng.LoadFromDisk(ctx); err != nil {
		return err
	}
	return fn(ctx, embedded{eng})
}

// reachable reports whether the daemon actually answers, as opposed to having
// left a runtime file behind when it died.
func reachable(ctx context.Context, c *Client) bool {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := c.Health(ctx)
	return err == nil
}

// embedded adapts an in-process engine to Reader.
//
// The methods that differ from the engine's own signatures do so because the
// HTTP API addresses jobs by slug -- a client has no job ids -- and one
// interface for both transports has to speak the transport-independent name.
type embedded struct{ eng *engine.Engine }

func (e embedded) Source() string { return "local" }

func (e embedded) Jobs(ctx context.Context) ([]store.Job, error) { return e.eng.Jobs(ctx) }

func (e embedded) Job(ctx context.Context, slug string) (store.Job, error) {
	return e.eng.Job(ctx, slug)
}

func (e embedded) Definition(ctx context.Context, slug string) (*jobdef.Definition, error) {
	def, _, err := e.eng.Definition(ctx, slug)
	return def, err
}

func (e embedded) Runs(ctx context.Context, jobSlug string, limit int) ([]store.Run, error) {
	var jobID int64
	if jobSlug != "" {
		job, err := e.eng.Job(ctx, jobSlug)
		if err != nil {
			return nil, err
		}
		jobID = job.ID
	}
	return e.eng.Runs(ctx, jobID, limit)
}

func (e embedded) Run(ctx context.Context, id int64) (store.Run, error) { return e.eng.Run(ctx, id) }

func (e embedded) Logs(ctx context.Context, runID int64, attempt int) ([]store.LogLine, error) {
	if attempt == 0 {
		run, err := e.eng.Run(ctx, runID)
		if err != nil {
			return nil, err
		}
		attempt = run.AttemptCount
	}
	return e.eng.Logs(ctx, runID, attempt)
}

func (e embedded) CurrentState(ctx context.Context, slug string) (*store.StateVersion, error) {
	job, err := e.eng.Job(ctx, slug)
	if err != nil {
		return nil, err
	}
	state, err := e.eng.CurrentState(ctx, job.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &state, nil
}

func (e embedded) StateHistory(ctx context.Context, slug string, limit int) ([]store.StateVersion, error) {
	job, err := e.eng.Job(ctx, slug)
	if err != nil {
		return nil, err
	}
	return e.eng.StateHistory(ctx, job.ID, limit)
}

func (e embedded) Waiting(ctx context.Context) (engine.Waiting, error) { return e.eng.Waiting(ctx) }

func (e embedded) Events(ctx context.Context, limit int) ([]model.Event, error) {
	return e.eng.RecentEvents(ctx, limit)
}
