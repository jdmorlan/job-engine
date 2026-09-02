package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/jdmorlan/job-engine/internal/engine"
)

// withEngine runs fn against an engine opened in this process.
//
// This is D19's stage 0 -- "je run weather-ingest, foreground, no daemon, exit
// code" -- and it exists only because D18 made the core a library. The daemon
// is one wrapper around that library and this is another.
//
// It does NOT call Start, so a one-shot command leaves no engine.started /
// engine.stopped pair in the timeline. Those events mean "the scheduler is
// up", and a foreground run is not that.
//
// The lock makes the two modes mutually exclusive: if a daemon is running it
// holds the data directory, and this fails with an error saying so. That is
// correct rather than unfortunate -- two writers would double-fire schedules --
// but it is a temporary shape. Once the API grows a runs endpoint, `je run`
// will route through the daemon when one exists and fall back to this when it
// does not, with identical output either way.
func withEngine(ctx context.Context, env *Env, fn func(context.Context, *engine.Engine) error) error {
	eng, err := engine.New(engine.Options{
		Layout:  env.Layout,
		Version: env.Version,
		// Engine diagnostics are not this command's output. A foreground run
		// prints the job's logs, not the engine's.
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		return adviseLocked(err)
	}
	defer func() {
		if closeErr := eng.Close(context.WithoutCancel(ctx)); closeErr != nil {
			fmt.Fprintf(env.Stderr, "je: closing engine: %v\n", closeErr)
		}
	}()

	// Definitions are read from disk on every invocation. In stage 0 there is
	// no daemon watching files, so "what is currently on disk" is the only
	// meaningful answer to "what is this job?".
	if _, err := eng.LoadFromDisk(ctx); err != nil {
		return err
	}
	return fn(ctx, eng)
}

// adviseLocked turns the data directory lock error into an instruction.
func adviseLocked(err error) error {
	if errors.Is(err, os.ErrPermission) {
		return err
	}
	return fmt.Errorf("%w\n\nA running daemon owns the data directory. "+
		"Stop it, or wait for `je run` to route through the daemon API", err)
}
