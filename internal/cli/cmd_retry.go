package cli

import (
	"context"
	"fmt"
	"strconv"
)

func init() {
	register(&Command{
		Name:  "retry",
		Args:  "<run>",
		Usage: "add an attempt to an existing run and follow it",
		Long: "A retry is another go at the same intent: the same run, the same input\n" +
			"state, one more attempt -- attributed to you rather than to the engine (D7).\n" +
			"To start a new run with a fresh cursor read instead, use `je run <job>`.\n\n" +
			"A manual retry ignores the job's max_attempts. Typing this command is the\n" +
			"judgement the limit exists to protect.",
		Run: runRetry,
	})
}

func runRetry(ctx context.Context, env *Env, args []string) error {
	cmd := commands["retry"]
	fs := newFlagSet(cmd, env)
	quiet := fs.Bool("q", false, "do not stream the job's output")
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return usagef("expected exactly one run id, got %d", len(positional))
	}
	runID, err := strconv.ParseInt(positional[0], 10, 64)
	if err != nil {
		return usagef("%q is not a run id", positional[0])
	}

	return withClient(ctx, env, func(ctx context.Context, client *Client) error {
		retryCtx, cancel := withTimeout(ctx)
		defer cancel()

		run, err := client.RetryRun(retryCtx, runID, currentActor())
		if err != nil {
			return err
		}
		fmt.Fprintf(env.Stderr, "je: retrying run %d (attempt %d)\n",
			run.ID, run.AttemptCount+1)

		return followRun(ctx, env, client, run.ID, *quiet)
	})
}
