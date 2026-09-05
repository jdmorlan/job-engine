package cli

import (
	"context"
	"errors"
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
			"this machine. If there is not one, it offers to set one up: `je dev` is\n" +
			"you saying you want a development deployment, so being told to go and\n" +
			"type two more commands is a gap in this one rather than an instruction.\n" +
			"--yes skips the question.",
		Run: runDev,
	})
}

func runDev(ctx context.Context, env *Env, args []string) error {
	fs := newFlagSet(commands["dev"], env)
	var (
		dir   = fs.String("dir", ".", "the repository holding the job definition")
		quiet = fs.Bool("q", false, "do not stream the job's output")
		yes   = fs.Bool("yes", false, "set up a control plane and worker here without asking")
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

	client, err := connectOrStart(ctx, env, abs, *yes)
	if err != nil {
		return err
	}

	return runWithClient(ctx, client, func(ctx context.Context, client *Client) error {
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

// runWithClient is withClient's body once a client is in hand.
func runWithClient(ctx context.Context, c *Client, fn func(context.Context, *Client) error) error {
	return fn(ctx, c)
}

// connectOrStart finds the control plane, or offers to set one up.
//
// `je dev` is the one command that can reasonably do this. Everywhere else,
// "there is no control plane" is a fact about somebody's deployment and the
// right response is to say so. Here it is a fact about a machine somebody is
// doing development on, and they have already said what they want by typing
// `je dev` -- a control plane and a worker that can read the directory they are
// standing in. Asking them to go and type two more commands is the gap D26
// names: if the instruction starts with another command, that is a hole in
// this one.
//
// It asks first, and installs rather than spawning something untracked: a
// service is findable in `je control-plane status`, survives the terminal it
// was created from, and comes back out with `je control-plane remove`. A
// process this command backgrounded would be none of those.
func connectOrStart(ctx context.Context, env *Env, tree string, yes bool) (*Client, error) {
	client, err := connectOrAdvise(ctx, env)
	switch {
	case err == nil:
		return client, ensureWorkerAttached(ctx, env, client, yes)
	case !errors.Is(err, ErrNoControlPlane):
		// Something else is wrong -- no authority on this machine, a version
		// that cannot speak the transport -- and offering to install a second
		// control plane on top of it would be the confidently wrong answer.
		return nil, err
	}

	fmt.Fprintf(env.Stderr,
		"je: there is no control plane on this machine, and `je dev` needs one that\n"+
			"    can read %s.\n\n"+
			"    It would install a control plane and a worker here, as services:\n"+
			"      je control-plane install\n"+
			"      je worker join\n\n",
		tree)

	if !yes {
		if !interactive(env) {
			return nil, fmt.Errorf("no control plane, and no terminal here to ask in.\n" +
				"Run those two commands, or `je dev --yes`")
		}
		if !confirm(env, "set that up now?  [Y/n] ") {
			return nil, errReported
		}
	}

	// Native, not a container, and this is the one place that choice is forced
	// rather than defaulted. A control plane in a container cannot read the
	// directory you are editing -- which is the whole of what `je dev` does --
	// so a container here would install successfully and then fail every run.
	if err := installControlPlane(ctx, env, "", installMode{native: true}); err != nil {
		return nil, err
	}

	client, err = connectOrAdvise(ctx, env)
	if err != nil {
		return nil, err
	}
	return client, ensureWorkerAttached(ctx, env, client, true)
}

// ensureWorkerAttached is C8's failure, caught where it is cheap.
//
// A control plane with no worker accepts the run, queues it, and executes
// nothing -- and every view looks healthy while it does. `je dev` would sit
// streaming an empty log until somebody gave up.
func ensureWorkerAttached(ctx context.Context, env *Env, client *Client, yes bool) error {
	listCtx, cancel := withTimeout(ctx)
	defer cancel()

	workers, err := client.Workers(listCtx)
	if err != nil {
		return nil // not worth failing over; the run will report its own trouble
	}
	for _, w := range workers {
		if w.Online {
			return nil
		}
	}

	fmt.Fprintln(env.Stderr,
		"je: the control plane has no worker attached, so a run would queue and wait.")
	if !yes {
		if !interactive(env) {
			return fmt.Errorf("attach one:  je worker join")
		}
		if !confirm(env, "    attach one here?  [Y/n] ") {
			return errReported
		}
	}
	return joinWorker(ctx, env, workerJoin{
		name:   defaultWorkerName(),
		addr:   client.Addr(),
		labels: []string{store.DefaultLabel},
		mode:   installMode{native: true},
	})
}
