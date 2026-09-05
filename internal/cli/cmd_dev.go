package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jdmorlan/job-engine/internal/engine"
	"github.com/jdmorlan/job-engine/internal/store"
)

func init() {
	register(&Command{
		Name:  "dev",
		Args:  "<job>",
		Usage: "run a job from the directory you are in, before it is pushed anywhere",
		Long: "For writing jobs. It points the control plane on this machine at this\n" +
			"directory and runs a job out of it -- so an edit takes effect on the next\n" +
			"command, with no commit, no push and no sync.\n\n" +
			"Everything after that is the real thing: the same dispatch, the same\n" +
			"worker, the same environment, the same secrets from your secrets.enc.yaml,\n" +
			"the same logs, events and cursor. It is not a simulation of a run, and\n" +
			"there is no second executor that could tell you something the engine\n" +
			"would not.\n\n" +
			"So these are real runs, and you can watch them: `je runs`, `je logs`,\n" +
			"`je why`, `je chain`. They are named dev/<job>, which keeps their history\n" +
			"and their cursor separate from the same job served from a repository.\n\n" +
			"It needs a control plane that can read this directory, which means one on\n" +
			"this machine -- `je quickstart`.",
		Run: runDev,
	})
}

func runDev(ctx context.Context, env *Env, args []string) error {
	fs := newFlagSet(commands["dev"], env)
	var (
		dir   = fs.String("dir", ".", "the repository holding the job definition")
		quiet = fs.Bool("q", false, "do not stream the job's output")
	)
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return usagef("expected exactly one job name, got %d", len(positional))
	}
	name := positional[0]

	abs, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	if _, err := os.Stat(abs); err != nil {
		return fmt.Errorf("no directory at %s", abs)
	}

	return withClient(ctx, env, func(ctx context.Context, client *Client) error {
		registerCtx, cancel := withTimeout(ctx)
		defer cancel()

		// Re-registered on every run, which is what makes this a loop: the
		// definition the engine holds is the file on disk as of this command,
		// and an edit needs nothing else to take effect.
		result, err := client.RegisterDev(registerCtx, abs)
		if err != nil {
			return err
		}
		fmt.Fprintf(env.Stderr, "je: %s -- %s\n", abs, loadSummary(result))
		if result.Loaded == 0 {
			// Said here rather than left to the trigger, which would report on
			// whatever this name meant the last time the directory had jobs in
			// it -- a stale answer about a different folder entirely.
			return fmt.Errorf(
				"there are no job definitions in %s.\n"+
					"`je dev` reads them from the directory you are in: a job is "+
					"<name>/job.yaml, or a <name>.yaml at the top level", abs)
		}

		// Qualified here rather than by the caller, so that what you type is
		// the name in the file and what runs is unambiguously the dev copy.
		slug := store.DevSourceName + "/" + name
		if strings.Contains(name, "/") {
			return usagef(
				"%q names a source; `je dev` runs what is in this directory, so give "+
					"just the job's name", name)
		}
		return followDevRun(ctx, env, client, slug, *quiet)
	})
}

// followDevRun triggers the run and follows it, exactly as `je run` does.
//
// It is the same function underneath. The two commands differ in where the
// definition came from and in nothing else, which is the property that makes
// `je dev` worth trusting: if it behaved differently in any way, it would stop
// being evidence about what happens when you push.
func followDevRun(ctx context.Context, env *Env, client *Client, slug string, quiet bool) error {
	triggerCtx, cancel := withTimeout(ctx)
	defer cancel()

	run, err := client.TriggerRun(triggerCtx, slug, currentActor())
	if err != nil {
		return err
	}
	return followRun(ctx, env, client, run.ID, quiet)
}

// loadSummary says what the directory turned out to hold.
func loadSummary(r engine.LoadResult) string {
	parts := []string{fmt.Sprintf("%d job(s)", r.Loaded)}
	if r.Chains > 0 {
		parts = append(parts, fmt.Sprintf("%d chain(s)", r.Chains))
	}
	if len(r.Misconfig) > 0 {
		// Named rather than counted, because a job that parsed and cannot run
		// is the one you are about to wonder about (D10).
		parts = append(parts, fmt.Sprintf("misconfigured: %s", strings.Join(r.Misconfig, ", ")))
	}
	return strings.Join(parts, ", ")
}
