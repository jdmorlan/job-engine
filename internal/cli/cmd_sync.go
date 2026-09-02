package cli

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"
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

		tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintf(tw, "source\t%s\n", result.Source)
		fmt.Fprintf(tw, "loaded\t%d job(s)\n", result.Loaded)
		if result.Revision != "" {
			// D11/D19: with a git source this is the commit that defines what
			// now runs, which is what makes `je why` able to point at one.
			fmt.Fprintf(tw, "revision\t%s\n", result.Revision)
		}
		if result.Removed > 0 {
			// Said explicitly because it is the destructive-looking half, and
			// it is not destructive: history stays.
			fmt.Fprintf(tw, "removed\t%d job(s) whose files are gone (history kept)\n", result.Removed)
		}
		if !result.SchedulesApplied {
			// Brief and normal, but it is the window in which a new schedule
			// is loaded and not yet firing -- so it is stated rather than left
			// to look identical to a working sync.
			fmt.Fprintf(tw, "schedules\tnot rebuilt yet; the clock will catch up\n")
		}
		if err := tw.Flush(); err != nil {
			return err
		}

		if len(result.Misconfig) > 0 {
			// Last, because it is the part that needs a human.
			fmt.Fprintf(env.Stdout,
				"\n%d job(s) loaded but cannot run: %s\n  je jobs   for the reason\n",
				len(result.Misconfig), strings.Join(result.Misconfig, ", "))
			return errAttention
		}
		return nil
	})
}
