package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/jdmorlan/job-engine/internal/api"
	"github.com/jdmorlan/job-engine/internal/engine"
)

func init() {
	register(&Command{
		Name:  "retention",
		Args:  "sweep",
		Usage: "remove history past its keep period and return the space",
		Long: "This is the body of the `system/retention` job, not something you\n" +
			"normally type (P2). The engine's own housekeeping is an ordinary job:\n" +
			"a worker runs it on a schedule, it appears in `je runs`, it has logs,\n" +
			"and it can fail visibly -- which a loop hidden inside the control\n" +
			"plane could do none of.\n\n" +
			"It prints what it did, because that output is the job's log and the\n" +
			"only record anybody will read afterwards.",
		Run: runRetention,
	})
}

func runRetention(ctx context.Context, env *Env, args []string) error {
	fs := newFlagSet(commands["retention"], env)
	runs := fs.String("runs", "", "how long to keep run records (default 30d)")
	logs := fs.String("logs", "", "how long to keep captured output (default 30d)")
	events := fs.String("events", "", "how long to keep the timeline (default 30d)")
	maxRuns := fs.Int("max-runs", 0, "cap how many runs one sweep removes")
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 || positional[0] != "sweep" {
		return usagef("expected `je retention sweep`")
	}

	return withClient(ctx, env, func(ctx context.Context, client *Client) error {
		sweepCtx, cancel := withTimeout(ctx)
		defer cancel()

		result, err := client.Sweep(sweepCtx, api.SweepRequest{
			Runs: *runs, Logs: *logs, Events: *events, MaxRuns: *maxRuns,
		})
		if err != nil {
			return err
		}
		printSweep(env, result)
		return nil
	})
}

// printSweep is the job's log, and the only account of a deletion anybody will
// read afterwards. It says what went, what did not, and why not.
func printSweep(env *Env, s engine.Sweep) {
	fmt.Fprintf(env.Stdout, "keeping runs %s, logs %s, events %s\n",
		period(s.Policy.Runs), period(s.Policy.Logs), period(s.Policy.Events))

	if s.Space.Converted {
		// Said out loud because it happens once, costs a full rewrite of the
		// logs database, and is the reason every sweep before it reclaimed
		// nothing.
		fmt.Fprintln(env.Stdout,
			"converted the logs database so that freed space can be returned;\n"+
				"this happens once, on a deployment older than retention")
	}

	r := s.Removed
	if !r.Any() {
		fmt.Fprintln(env.Stdout, "nothing was past its keep period")
	} else {
		fmt.Fprintf(env.Stdout, "removed %d run(s), %d attempt(s), %d log line(s), %d event(s)\n",
			r.Runs, r.Attempts, r.LogLines, r.Events)
	}
	if r.RunsLeft > 0 {
		// A cap that stops quietly looks exactly like a sweep with nothing to
		// do, and the difference is a week of disk (P1).
		fmt.Fprintf(env.Stdout,
			"%d more run(s) are past the keep period and will go on the next sweep\n",
			r.RunsLeft)
	}
	if r.Pinned > 0 {
		fmt.Fprintf(env.Stdout,
			"%d run(s) are past the keep period but still referenced: a cursor was set "+
				"by them, or the timeline still describes them\n", r.Pinned)
	}
	fmt.Fprintf(env.Stdout, "reclaimed %s; logs database is now %s\n",
		humanBytes(s.Space.Reclaimed()), humanBytes(s.Space.BytesAfter))
}

// period renders a keep period the way the flags take it.
func period(d time.Duration) string {
	if d%engine.Day == 0 {
		return fmt.Sprintf("%dd", d/engine.Day)
	}
	return d.String()
}
