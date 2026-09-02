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
)

func init() {
	register(&Command{
		Name:  "run",
		Args:  "<job>",
		Usage: "run a job now, in the foreground",
		Long: "Creates a new run, which is a new unit of intent with a fresh cursor read.\n" +
			"To add an attempt to an existing run instead, use `je retry <run>` (D7).\n\n" +
			"The exit code is the job's own, so this composes with the rest of your shell.",
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

	return withEngine(ctx, env, func(ctx context.Context, eng *engine.Engine) error {
		opts := engine.RunOptions{Actor: currentActor()}
		if !*quiet {
			// Streamed as the job produces it, so a foreground run has the
			// one-second feedback loop D1 chose the process executor for.
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

		printRunSummary(env, eng, ctx, result)

		// The job's verdict becomes ours. A script wrapping `je run` should not
		// have to parse output to find out whether the work happened.
		if result.Run.Status != model.StatusSucceeded {
			return fmt.Errorf("run %d %s: %s", result.Run.ID, result.Run.Status, result.Run.Error)
		}
		return nil
	})
}

// printRunSummary renders what happened after the job's own output.
//
// The cursor line is the part that exists nowhere else, and it is why D14
// insists the cursor's movement is part of the feature rather than a
// follow-up: seeing "since advanced" is what makes the handoff feel like a
// submission rather than a write into a void.
func printRunSummary(env *Env, eng *engine.Engine, ctx context.Context, r *engine.RunResult) {
	var duration time.Duration
	if r.Run.StartedAt != nil && r.Run.EndedAt != nil {
		duration = r.Run.EndedAt.Sub(*r.Run.StartedAt).Round(time.Millisecond)
	}

	mark := "x"
	if r.Run.Status == model.StatusSucceeded {
		mark = "ok"
	}
	fmt.Fprintf(env.Stdout, "\n%s  run %d %s in %s\n", mark, r.Run.ID, r.Run.Status, duration)

	if r.Run.Error != "" {
		fmt.Fprintf(env.Stdout, "    error   %s\n", r.Run.Error)
	}

	switch {
	case r.StateOut != nil:
		fmt.Fprintf(env.Stdout, "    cursor  %s  %s -> %s  (v%d)\n",
			r.PrimaryCursor, r.StateIn.Summary(r.PrimaryCursor),
			r.StateOut.Summary(r.PrimaryCursor), r.StateOut.Version)
	case r.StateIn != nil && r.Run.Status == model.StatusSucceeded:
		// Worth saying out loud. A job that succeeds without moving its cursor
		// is either stateless or quietly broken, and silence cannot tell you
		// which.
		fmt.Fprintf(env.Stdout, "    cursor  %s unchanged at %s\n",
			r.PrimaryCursor, r.StateIn.Summary(r.PrimaryCursor))
	}

	if len(r.Emitted) > 0 {
		for _, ev := range r.Emitted {
			fmt.Fprintf(env.Stdout, "    emitted %s (event %d)\n", ev.Type, ev.ID)
		}
	}
	if len(r.Output) > 0 {
		fmt.Fprintf(env.Stdout, "    output  %s\n", truncate(string(r.Output), 120))
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

// currentActor names the person responsible for a manual action (D7).
func currentActor() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "unknown"
}
