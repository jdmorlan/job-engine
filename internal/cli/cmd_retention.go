package cli

import (
	"context"
	"fmt"
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

		result, err := client.Sweep(sweepCtx)
		if err != nil {
			return err
		}

		space := result.Space
		if space.Converted {
			// Said out loud because it happens once, costs a full rewrite of
			// the logs database, and is the reason every sweep before it
			// reclaimed nothing.
			fmt.Fprintln(env.Stdout,
				"converted the logs database so that freed space can be returned;\n"+
					"this happens once, on a deployment older than retention")
		}
		fmt.Fprintf(env.Stdout, "reclaimed %s across %d page(s); logs database is now %s\n",
			humanBytes(space.Reclaimed()), space.PagesFreed, humanBytes(space.BytesAfter))
		return nil
	})
}
