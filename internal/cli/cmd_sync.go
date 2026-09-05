package cli

import (
	"context"
	"fmt"
	"strings"
)

func init() {
	register(&Command{
		Name:  "sync",
		Usage: "reload job definitions on the control plane",
		Long: "Re-reads the definition source and rebuilds the schedule table.\n\n" +
			"The whole sync succeeds or none of it does: one file that will not\n" +
			"parse is rejected and the definitions already in force keep serving,\n" +
			"rather than leaving the engine running a configuration that exists in\n" +
			"no file (D19).\n\n" +
			"A job file that has been deleted stops being scheduled and keeps its\n" +
			"history. Reverting a commit must not erase the timeline.",
		Run: runSync,
	})
}

func runSync(ctx context.Context, env *Env, args []string) error {
	cmd := commands["sync"]
	fs := newFlagSet(cmd, env)
	if extra, err := parseArgs(fs, args); err != nil {
		return err
	} else if len(extra) > 0 {
		return usagef("unexpected argument %q", extra[0])
	}

	return withClient(ctx, env, func(ctx context.Context, c *Client) error {
		syncCtx, cancel := withTimeout(ctx)
		defer cancel()

		result, err := c.Sync(syncCtx)
		if err != nil {
			return err
		}

		st := env.Style
		tw := env.table()
		fmt.Fprintf(tw, "%s\t%s\n", st.Header("source"), result.Source)
		fmt.Fprintf(tw, "%s\t%s\n", st.Header("loaded"),
			st.Good(fmt.Sprintf("%d job(s)", result.Loaded)))
		if result.Chains > 0 {
			// Stated only when there are any, but stated: "7 jobs" reads
			// identically whether the chains directory came across or not, and
			// a repo whose wiring did not arrive still runs every job it has.
			fmt.Fprintf(tw, "\t%s\n",
				st.Muted(fmt.Sprintf("%d chain(s), %d route(s)", result.Chains, result.Routes)))
		}
		if result.Revision != "" {
			// D11/D19: with a git source this is the commit that defines what
			// now runs, which is what makes `je why` able to point at one.
			fmt.Fprintf(tw, "%s\t%s\n", st.Header("revision"), st.Muted(result.Revision))
		}
		if result.Removed > 0 {
			// Said explicitly because it is the destructive-looking half, and
			// it is not destructive: history stays.
			fmt.Fprintf(tw, "%s\t%d job(s) whose files are gone %s\n", st.Header("removed"),
				result.Removed, st.Muted("(history kept)"))
		}
		if !result.SchedulesApplied {
			// Brief and normal, but it is the window in which a new schedule
			// is loaded and not yet firing -- so it is stated rather than left
			// to look identical to a working sync.
			fmt.Fprintf(tw, "%s\t%s\n", st.Header("schedules"),
				st.Muted("not rebuilt yet; the clock will catch up"))
		}
		if err := tw.Flush(); err != nil {
			return err
		}

		if len(result.Misconfig) > 0 {
			// Last, because it is the part that needs a human.
			fmt.Fprintf(env.Stdout, "\n%s %s\n  %s   %s\n",
				st.Bad(fmt.Sprintf("%d job(s) loaded but cannot run:", len(result.Misconfig))),
				strings.Join(result.Misconfig, ", "),
				st.Cmd("je jobs"), st.Muted("for the reason"))
			return errAttention
		}
		return nil
	})
}
