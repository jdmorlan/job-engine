package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jdmorlan/job-engine/internal/service"
)

// Shared machinery for `je control-plane install` and `je worker join`.
//
// The two are deliberately separate commands, because they are different acts.
// A control plane is *created*: it decides where the database lives, which is
// the one thing here you cannot lose. A worker is *joined*: it needs a control
// plane that already exists, and it is the Nth machine rather than the first.
// Collapsing them into one `je setup` would hide that asymmetry behind a flag.
//
// What they share is the obligation, and it is the reason this file exists at
// all: write something a human can read, prove it worked, and say what happens
// next. An install command that prints "installed" and leaves you to discover
// otherwise is the operator's version of the silent fallback we removed from
// the read path -- and every stale-guidance bug in this CLI so far would have
// been caught by a setup step that actually ran the thing afterwards.

// installPlan is what a component's registration needs.
type installPlan struct {
	component service.Component
	args      []string

	// verify runs after the service is up and reports whether it truly works.
	// Not whether the unit loaded -- whether the component is doing its job.
	verify func(ctx context.Context) error

	// nextStep is printed last: the thing the person still has to do. Empty
	// when they are done.
	nextStep string
}

// installComponent registers a component and proves it works.
func installComponent(ctx context.Context, env *Env, plan installPlan) error {
	mgr, err := service.New(plan.component)
	if err != nil {
		return err
	}

	binary, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding this binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(binary); err == nil {
		// A service manager will not follow a symlink you have since moved.
		binary = resolved
	}

	logPath := filepath.Join(env.Layout.Data, string(plan.component)+".log")
	cfg := service.Config{
		Component: plan.component,
		Binary:    binary,
		DataDir:   env.Layout.Data,
		Args:      plan.args,
		LogPath:   logPath,
		// The PATH gotcha, and it is the classic way this goes wrong: a
		// service manager starts processes with a minimal PATH, and D6 passes
		// PATH through to jobs. Without capturing the installing shell's, every
		// job calling `python3` or anything from Homebrew fails only under the
		// service and works by hand.
		Path: os.Getenv("PATH"),
	}

	if err := mgr.Install(cfg); err != nil {
		return err
	}

	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "registered\t%s with %s\n", plan.component, mgr.Name())
	fmt.Fprintf(tw, "unit\t%s\n", mgr.UnitPath())
	fmt.Fprintf(tw, "log\t%s\n", logPath)
	if err := tw.Flush(); err != nil {
		return err
	}

	// The part that makes this worth having. Registering a unit proves the
	// service manager accepted a file; it proves nothing about the engine.
	if plan.verify != nil {
		if err := plan.verify(ctx); err != nil {
			fmt.Fprintf(env.Stderr,
				"\nregistered, but it is not working yet:\n  %v\n\n"+
					"  %s   has what it said on the way up\n",
				err, logPath)
			return errAttention
		}
		fmt.Fprintln(env.Stdout, "\nverified: it is running and answering.")
	}

	if plan.nextStep != "" {
		fmt.Fprintf(env.Stdout, "\n%s\n", plan.nextStep)
	}
	return nil
}

// componentStatus renders what the OS thinks of one component.
func componentStatus(env *Env, c service.Component) error {
	mgr, err := service.New(c)
	if err != nil {
		return err
	}
	state, err := mgr.Status()
	if err != nil {
		return err
	}

	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	if !state.Installed {
		fmt.Fprintf(tw, "%s\tnot registered\n", c)
		if err := tw.Flush(); err != nil {
			return err
		}
		fmt.Fprintf(env.Stdout, "\nRegister it:  je %s %s\n", c, registerVerb(c))
		return nil
	}
	fmt.Fprintf(tw, "%s\tregistered with %s\n", c, mgr.Name())
	fmt.Fprintf(tw, "unit\t%s\n", state.UnitPath)
	if state.PID > 0 {
		fmt.Fprintf(tw, "running\tpid %d\n", state.PID)
	} else {
		// Registered but not running is the interesting failure, so it is
		// stated as one rather than left as an absent row.
		fmt.Fprintf(tw, "running\tno -- registered but not up\n")
	}
	if state.Detail != "" {
		fmt.Fprintf(tw, "detail\t%s\n", state.Detail)
	}
	return tw.Flush()
}

// removeComponent unregisters, and says exactly what it did and did not touch.
func removeComponent(env *Env, c service.Component) error {
	mgr, err := service.New(c)
	if err != nil {
		return err
	}
	state, err := mgr.Status()
	if err != nil {
		return err
	}
	if !state.Installed {
		fmt.Fprintf(env.Stdout, "%s is not registered; nothing to remove\n", c)
		return nil
	}
	if err := mgr.Uninstall(); err != nil {
		return err
	}

	// Naming what survives matters as much as doing the removal. D19's rule
	// that deleting a definition never deletes history applies to the operator
	// too: somebody removing a service should not have to wonder whether they
	// just destroyed their run history.
	fmt.Fprintf(env.Stdout,
		"removed the %s service (%s)\n\n"+
			"Left alone: your data directory, job definitions, run history and secrets.\n"+
			"            %s\n",
		c, state.UnitPath, env.Layout.Data)
	return nil
}

// registerVerb is the subcommand that registers a component, which differs
// because the acts differ: you create a control plane and join a worker.
func registerVerb(c service.Component) string {
	if c == service.Worker {
		return "join"
	}
	return "install"
}

// waitForControlPlane polls until the control plane answers, or gives up.
//
// Polling rather than sleeping a fixed time: a service manager starts a process
// asynchronously, and the honest answer to "is it up?" is to ask it.
func waitForControlPlane(ctx context.Context, env *Env) error {
	deadline := time.Now().Add(15 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		client, err := Connect(env.Layout)
		if err == nil {
			checkCtx, cancel := withTimeout(ctx)
			_, err = client.Health(checkCtx)
			cancel()
			if err == nil {
				return nil
			}
		}
		last = err

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	if last == nil {
		last = errors.New("it did not start answering")
	}
	return last
}

// joinWorker registers a worker attached to an existing control plane.
//
// The verification here is the good one, and it is only possible because the
// control plane is the sole writer (C1): a worker that has really joined shows
// up in `je workers` on the far side. An enrollment that cannot confirm it
// landed is not finished -- it is a file on disk and a hope.
func joinWorker(
	ctx context.Context, env *Env, name, addr string, labels []string, concurrency int,
) error {
	args := []string{"--addr", addr, "--name", name, "--labels", strings.Join(labels, ",")}
	if concurrency > 0 {
		args = append(args, "--concurrency", strconv.Itoa(concurrency))
	}

	return installComponent(ctx, env, installPlan{
		component: service.Worker,
		args:      args,
		verify: func(ctx context.Context) error {
			return waitForWorker(ctx, env, name)
		},
		nextStep: fmt.Sprintf(
			"It will take jobs whose runs_on is one of: %s\n"+
				"See what is attached:  je workers",
			strings.Join(labels, ", ")),
	})
}

// waitForWorker polls the control plane until this worker appears online.
func waitForWorker(ctx context.Context, env *Env, name string) error {
	deadline := time.Now().Add(20 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		client, err := Connect(env.Layout)
		if err == nil {
			listCtx, cancel := withTimeout(ctx)
			workers, listErr := client.Workers(listCtx)
			cancel()
			if listErr == nil {
				for _, w := range workers {
					if w.Name == name && w.Online {
						return nil
					}
				}
				// Reached the control plane and this worker is not there. Not
				// yet an error: registration is asynchronous.
				last = fmt.Errorf("the control plane is up but %q has not registered", name)
			} else {
				last = listErr
			}
		} else {
			last = err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	if last == nil {
		last = errors.New("it did not appear in the worker list")
	}
	return last
}
