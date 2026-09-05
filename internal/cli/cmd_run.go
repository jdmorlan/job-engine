package cli

import (
	"context"
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
			"The run is queued with the control plane, dispatched to a worker, and its\n" +
			"output streamed back here. The exit code is the job's own, so this composes\n" +
			"with your shell.",
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

	return withClient(ctx, env, func(ctx context.Context, client *Client) error {
		return runViaControlPlane(ctx, env, client, slug, *quiet)
	})
}

// runViaControlPlane queues the run and follows it.
//
// There is no second path. `je run` used to execute the job in this process
// when no control plane was reachable, which made the CLI a writer to the
// database and gave the system two executors with different behaviour for
// logs, timeouts and cancellation. D20/C11 removed that: every run goes to a
// worker, including this one.
func runViaControlPlane(ctx context.Context, env *Env, client *Client, slug string, quiet bool) error {
	triggerCtx, cancel := withTimeout(ctx)
	defer cancel()

	run, err := client.TriggerRun(triggerCtx, slug, currentActor())
	if err != nil {
		return err
	}
	return followRun(ctx, env, client, run.ID, quiet)
}

// followRun streams a run to the terminal and reports how it ended.
//
// Shared by `je run` and `je retry` because the half after the request is
// identical: both are watching one run, and a run that retries is still one
// run -- which is exactly why this loop does not stop when an attempt fails.
func followRun(ctx context.Context, env *Env, client *Client, runID int64, quiet bool) error {
	// Streamed with the caller's context, not a timeout: a job may legitimately
	// run for its full hour, and the whole point of this path is watching it.
	streamErr := client.StreamRun(ctx, runID, func(ev engine.StreamEvent) {
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
		case engine.StreamStatus:
			// The retry notice (D7). Without it the terminal simply goes quiet
			// for the length of the backoff, which reads as a hang.
			if ev.Detail != "" {
				fmt.Fprintf(env.Stderr, "je: %s\n", ev.Detail)
			}
		case engine.StreamOverflow:
			// Honest rather than silent: the stored logs are complete, this
			// terminal just could not keep up.
			fmt.Fprintf(env.Stderr,
				"je: output was arriving faster than this terminal could take it; "+
					"see `je logs %d` for the complete log\n", runID)
		}
	})

	if streamErr != nil {
		if ctx.Err() != nil {
			// Ctrl-C detaches rather than cancelling. The run belongs to the
			// control plane and its worker now, and silently killing someone's
			// half-finished job because they stopped watching would be a bad
			// surprise.
			fmt.Fprintf(env.Stderr, "\n%s %s\n",
				env.Style.Muted(fmt.Sprintf(
					"je: stopped following. Run %d continues in the background:", runID)),
				env.Style.Cmd(fmt.Sprintf("je logs %d", runID)))
			return nil
		}
		return streamErr
	}

	detailCtx, cancelDetail := withTimeout(ctx)
	defer cancelDetail()

	detail, err := client.RunDetail(detailCtx, runID)
	if err != nil {
		return err
	}
	printRunDetail(env, detail)

	if detail.Run.Status != model.StatusSucceeded {
		return fmt.Errorf("run %d %s: %s", detail.Run.ID, detail.Run.Status, detail.Run.Error)
	}
	return nil
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

	// The verdict line is the one thing somebody watching a run is waiting
	// for, and until now it looked like every other line the job printed. It
	// gets the only strong colour on the screen.
	st := env.Style
	mark, verdict := st.Bad("x"), st.Bad(string(d.Run.Status))
	if d.Run.Status == model.StatusSucceeded {
		mark, verdict = st.Good("ok"), st.Good(string(d.Run.Status))
	}
	fmt.Fprintf(env.Stdout, "\n%s  run %d %s in %s\n", mark, d.Run.ID, verdict, duration)

	if d.Run.Error != "" {
		fmt.Fprintf(env.Stdout, "    %s   %s\n", st.Header("error"), st.Bad(d.Run.Error))
	}

	cursor := d.PrimaryCursor
	switch {
	case d.StateOut != nil && d.StateIn != nil:
		fmt.Fprintf(env.Stdout, "    %s  %s  %s -> %s  %s\n", st.Header("cursor"),
			cursor, d.StateIn.Summary(cursor), d.StateOut.Summary(cursor),
			st.Muted(fmt.Sprintf("(v%d)", d.StateOut.Version)))
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
		fmt.Fprintf(env.Stdout, "    %s  %s %s\n", st.Header("cursor"), cursor,
			st.Muted("unchanged at "+d.StateIn.Summary(cursor)))
	}

	for _, ev := range d.Emitted {
		if ev.Source == "job" {
			fmt.Fprintf(env.Stdout, "    %s %s %s\n", st.Header("emitted"), ev.Type,
				st.Muted(fmt.Sprintf("(event %d)", ev.ID)))
		}
	}
	if len(d.Run.Output) > 0 {
		fmt.Fprintf(env.Stdout, "    %s  %s\n", st.Header("output"),
			truncate(string(d.Run.Output), 120))
	}
	// D7's question, answered here rather than in a separate view: "did a human
	// have to intervene, or did this just work?" is read off the attempt list.
	if len(d.Attempts) > 1 {
		fmt.Fprintln(env.Stdout, "    "+st.Header("attempts"))
		tw := env.table()
		for _, a := range d.Attempts {
			detail := a.Error
			if detail == "" && a.Status == model.StatusSucceeded {
				detail = "-"
			}
			fmt.Fprintf(tw, "      %d\t%s\t%s\t%s\t%s\n",
				a.Number, st.State(string(a.Status)), st.Muted(attemptCauseText(a)),
				runDuration(a.StartedAt, a.EndedAt), st.Muted(truncate(detail, 60)))
		}
		tw.Flush()
	}
}

// attemptCauseText says whose idea an attempt was.
//
// The first attempt is the run's own cause; anything after it is a retry, and
// the difference between the engine deciding and a person deciding is the
// distinction D7 exists to preserve.
func attemptCauseText(a store.Attempt) string {
	if a.Number == 1 {
		if a.Actor == "" {
			return "engine"
		}
		return a.Actor
	}
	if a.Actor == "" {
		return "automatic retry"
	}
	return a.Actor + " (retry)"
}

// currentActor names the person responsible for a manual action (D7).
func currentActor() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "unknown"
}
