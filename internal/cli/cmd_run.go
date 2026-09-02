package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os/user"
	"time"

	"github.com/jdmorlan/job-engine/internal/engine"
	"github.com/jdmorlan/job-engine/internal/model"
	"github.com/jdmorlan/job-engine/internal/store"
)

func init() {
	register(&Command{
		Name:  "run",
		Args:  "<job>",
		Usage: "run a job now and follow its output",
		Long: "Creates a new run, which is a new unit of intent with a fresh cursor read.\n" +
			"To add an attempt to an existing run instead, use `je retry <run>` (D7).\n\n" +
			"Works whether or not the daemon is running. With one, the run is queued\n" +
			"there and its output is streamed back; without one it executes here.\n" +
			"The exit code is the job's own either way, so this composes with your shell.",
		Run: runRun,
	})
}

func runRun(ctx context.Context, env *Env, args []string) error {
	cmd := commands["run"]
	fs := newFlagSet(cmd, env)
	quiet := fs.Bool("q", false, "do not stream the job's output")
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return usagef("expected exactly one job name, got %d", len(positional))
	}
	slug := positional[0]

	// The daemon owns the data directory whenever it is running, so this is
	// not a preference -- it is the only way to run a job without stopping the
	// scheduler. The two paths print the same thing.
	if client, err := Connect(env.Layout); err == nil && reachable(ctx, client) {
		return runViaDaemon(ctx, env, client, slug, *quiet)
	}
	return runInProcess(ctx, env, slug, *quiet)
}

// runViaDaemon queues the run with the daemon and follows it.
func runViaDaemon(ctx context.Context, env *Env, client *Client, slug string, quiet bool) error {
	triggerCtx, cancel := withTimeout(ctx)
	defer cancel()

	run, err := client.TriggerRun(triggerCtx, slug, currentActor())
	if err != nil {
		return err
	}

	// Streamed with the caller's context, not a timeout: a job may legitimately
	// run for its full hour, and the whole point of this path is watching it.
	streamErr := client.StreamRun(ctx, run.ID, func(ev engine.StreamEvent) {
		switch ev.Kind {
		case engine.StreamLog:
			if quiet {
				return
			}
			w := env.Stdout
			if ev.Stream == "stderr" {
				w = env.Stderr
			}
			fmt.Fprintln(w, ev.Line)
		case engine.StreamOverflow:
			// Honest rather than silent: the stored logs are complete, this
			// terminal just could not keep up.
			fmt.Fprintf(env.Stderr,
				"je: output was arriving faster than this terminal could take it; "+
					"see `je logs %d` for the complete log\n", run.ID)
		}
	})

	if streamErr != nil {
		if ctx.Err() != nil {
			// Ctrl-C detaches rather than cancelling. The run belongs to the
			// daemon now, and silently killing someone's half-finished job
			// because they stopped watching would be a bad surprise.
			fmt.Fprintf(env.Stderr,
				"\nje: stopped following. Run %d continues in the background: je logs %d\n",
				run.ID, run.ID)
			return err
		}
		return streamErr
	}

	detailCtx, cancelDetail := withTimeout(ctx)
	defer cancelDetail()

	detail, err := client.RunDetail(detailCtx, run.ID)
	if err != nil {
		return err
	}
	printRunDetail(env, detail)

	if detail.Run.Status != model.StatusSucceeded {
		return fmt.Errorf("run %d %s: %s", detail.Run.ID, detail.Run.Status, detail.Run.Error)
	}
	return nil
}

// runInProcess is D19 stage 0: no daemon, the CLI is the engine.
func runInProcess(ctx context.Context, env *Env, slug string, quiet bool) error {
	return withEngine(ctx, env, func(ctx context.Context, eng *engine.Engine) error {
		opts := engine.RunOptions{Actor: currentActor()}
		if !quiet {
			opts.Live = func(stream string, ts time.Time, line string) {
				w := env.Stdout
				if stream == "stderr" {
					w = env.Stderr
				}
				fmt.Fprintln(w, line)
			}
		}

		result, err := eng.RunJob(ctx, slug, opts)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("no job named %q in %s", slug, env.Layout.Jobs)
			}
			return err
		}

		// Rendered through the same function as the daemon path, so the two
		// cannot drift into showing different things about the same run.
		detail, err := eng.RunDetail(ctx, result.Run.ID)
		if err != nil {
			return err
		}
		printRunDetail(env, detail)

		if result.Run.Status != model.StatusSucceeded {
			return fmt.Errorf("run %d %s: %s", result.Run.ID, result.Run.Status, result.Run.Error)
		}
		return nil
	})
}

// printRunDetail renders what happened, after the job's own output.
//
// The cursor line is the part that exists nowhere else, and it is why D14
// insists the cursor's movement is part of the feature rather than a
// follow-up: seeing "since advanced" is what makes the handoff feel like a
// submission rather than a write into a void.
func printRunDetail(env *Env, d engine.RunDetail) {
	var duration time.Duration
	if d.Run.StartedAt != nil && d.Run.EndedAt != nil {
		duration = d.Run.EndedAt.Sub(*d.Run.StartedAt).Round(time.Millisecond)
	}

	mark := "x"
	if d.Run.Status == model.StatusSucceeded {
		mark = "ok"
	}
	fmt.Fprintf(env.Stdout, "\n%s  run %d %s in %s\n", mark, d.Run.ID, d.Run.Status, duration)

	if d.Run.Error != "" {
		fmt.Fprintf(env.Stdout, "    error   %s\n", d.Run.Error)
	}

	cursor := d.PrimaryCursor
	switch {
	case d.StateOut != nil && d.StateIn != nil:
		fmt.Fprintf(env.Stdout, "    cursor  %s  %s -> %s  (v%d)\n",
			cursor, d.StateIn.Summary(cursor), d.StateOut.Summary(cursor), d.StateOut.Version)
	case d.StateIn != nil && d.Run.Status == model.StatusSucceeded &&
		d.StateIn.SetByActor != store.ActorEngine:
		// Worth saying out loud for a job that uses a cursor: succeeding
		// without moving it means either the job is idle or it is quietly
		// broken, and silence cannot tell you which.
		//
		// Suppressed when the only version is the engine's seed. Every job
		// gets a seed so JE_STATE is never empty, but a job that has never
		// written a cursor of its own does not have one worth reporting, and
		// saying so on `je run demo-hello` is noise on the first command
		// anybody types.
		fmt.Fprintf(env.Stdout, "    cursor  %s unchanged at %s\n",
			cursor, d.StateIn.Summary(cursor))
	}

	for _, ev := range d.Emitted {
		if ev.Source == "job" {
			fmt.Fprintf(env.Stdout, "    emitted %s (event %d)\n", ev.Type, ev.ID)
		}
	}
	if len(d.Run.Output) > 0 {
		fmt.Fprintf(env.Stdout, "    output  %s\n", truncate(string(d.Run.Output), 120))
	}
	if len(d.Attempts) > 1 {
		fmt.Fprintf(env.Stdout, "    attempts %d\n", len(d.Attempts))
	}
}

// currentActor names the person responsible for a manual action (D7).
func currentActor() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "unknown"
}
