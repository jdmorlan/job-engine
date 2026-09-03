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

// componentStatus renders how one component is set up here.
//
// It looks for both shapes, because reporting "not registered" while a
// container of that name is running would be a confident lie -- and the whole
// argument for these commands is that a deployment should be legible.
func componentStatus(ctx context.Context, env *Env, c service.Component) error {
	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)

	if containerExists(ctx, string(c)) {
		fmt.Fprintf(tw, "%s\trunning as a container\n", c)
		fmt.Fprintf(tw, "container\t%s\n", containerName(string(c)))
		if endpoint, err := ReadEndpoint(env.Layout.Endpoint()); err == nil && endpoint.Address != "" {
			fmt.Fprintf(tw, "reachable at\t%s\n", endpoint.Address)
		}
		fmt.Fprintf(tw, "logs\tdocker logs -f %s\n", containerName(string(c)))
		return tw.Flush()
	}

	mgr, err := service.New(c)
	if err != nil {
		return err
	}
	state, err := mgr.Status()
	if err != nil {
		return err
	}

	if !state.Installed {
		fmt.Fprintf(tw, "%s\tnot set up here\n", c)
		if err := tw.Flush(); err != nil {
			return err
		}
		fmt.Fprintf(env.Stdout, "\nSet it up:  je %s %s\n", c, registerVerb(c))
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

// removeComponent tears down a component however it was set up.
//
// It checks both shapes rather than asking which you used, because somebody
// removing something does not necessarily remember how they installed it --
// and "nothing to remove" while a container is still running would be a lie.
func removeComponent(ctx context.Context, env *Env, c service.Component) error {
	var removed []string

	if stopped, err := stopContainer(ctx, string(c)); err != nil {
		return err
	} else if stopped {
		removed = append(removed, "container "+containerName(string(c)))
	}

	// A recorded endpoint outliving the thing it points at would make every
	// later command fail against an address with nothing behind it, which
	// reads as "broken" rather than "not set up".
	if c == service.ControlPlane {
		if err := RemoveEndpoint(env.Layout.Endpoint()); err != nil {
			return err
		}
	}

	mgr, err := service.New(c)
	if err == nil {
		state, err := mgr.Status()
		if err != nil {
			return err
		}
		if state.Installed {
			if err := mgr.Uninstall(); err != nil {
				return err
			}
			removed = append(removed, "service "+state.UnitPath)
		}
	}

	if len(removed) == 0 {
		fmt.Fprintf(env.Stdout, "%s is not set up here; nothing to remove\n", c)
		return nil
	}

	// Naming what survives matters as much as doing the removal. D19's rule
	// that deleting a definition never deletes history applies to the operator
	// too: somebody removing a service should not have to wonder whether they
	// just destroyed their run history.
	// Naming what survives matters as much as doing the removal. D19's rule
	// that deleting a definition never deletes history applies to the operator
	// too: somebody removing a service should not have to wonder whether they
	// just destroyed their run history.
	fmt.Fprintf(env.Stdout,
		"removed %s: %s\n\n"+
			"Left alone: your data directory, job definitions, run history and secrets.\n"+
			"            %s\n",
		c, strings.Join(removed, ", "), env.Layout.Data)
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
type workerJoin struct {
	name        string
	addr        string
	labels      []string
	concurrency int
	mode        installMode
}

func joinWorker(ctx context.Context, env *Env, j workerJoin) error {
	args := []string{"--addr", j.addr, "--name", j.name,
		"--labels", strings.Join(j.labels, ",")}
	if j.concurrency > 0 {
		args = append(args, "--concurrency", strconv.Itoa(j.concurrency))
	}

	verify := func(ctx context.Context) error { return waitForWorker(ctx, env, j.name) }
	next := fmt.Sprintf(
		"It will take jobs whose runs_on is one of: %s\n"+
			"See what is attached:  je workers",
		strings.Join(j.labels, ", "))

	kind, err := chooseMode(env, j.mode)
	if err != nil {
		return err
	}

	if kind == modeDocker {
		image, err := dockerImage(env, j.mode)
		if err != nil {
			return err
		}
		// A container worker can only run what is in its image, and this one
		// is FROM scratch. That is right for the system worker, whose jobs are
		// the engine's own, and wrong for yours -- a worker that runs Python
		// jobs needs a Python image with /je in it. Said here rather than
		// discovered later as "command not found".
		fmt.Fprintln(env.Stderr,
			"note: this image contains only je, so a container worker can run only\n"+
				"      jobs whose commands are self-contained. Use --native for jobs\n"+
				"      that need tools from this machine.")

		// Rewritten for the container's view of the network, which is not the
		// host's. See workerTarget.
		target, network := workerTarget(ctx, j.addr)
		containerArgs := append([]string{}, args...)
		for i := range containerArgs {
			if containerArgs[i] == j.addr {
				containerArgs[i] = target
			}
		}
		if network != "" && !j.mode.printOnly {
			if err := ensureNetwork(ctx); err != nil {
				return err
			}
		}

		spec := dockerSpec{
			component: "worker",
			image:     image,
			args:      containerArgs,
			// No volume: a worker holds nothing durable (C2). Losing this
			// container costs its in-flight runs and nothing else.
			env:     []string{"TZ=" + localTimezone()},
			network: network,
		}
		// A worker records nothing: it holds no state (C2), and clients talk to
		// the control plane, never to it.
		return runDockerInstall(ctx, env, spec, j.mode, verify, next, "")
	}

	return installComponent(ctx, env, installPlan{
		component: service.Worker,
		args:      args,
		verify:    verify,
		nextStep:  next,
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

// installMode is how the caller asked for a component to be kept alive.
type installMode struct {
	docker    bool
	native    bool
	printOnly bool
}

type modeKind int

const (
	modeNative modeKind = iota
	modeDocker
)

// chooseMode decides container-versus-native, and says which it chose.
//
// It picks a default rather than asking, because an install command that
// interrogates you is worse than one that acts and explains. But it never
// chooses silently: the whole argument for removing the CLI's daemonless
// fallback was that a decision you cannot see is worse than one you disagree
// with, and that applies at least as much to something that changes your
// machine.
func chooseMode(env *Env, mode installMode) (modeKind, error) {
	switch {
	case mode.docker && mode.native:
		return 0, usagef("--docker and --native contradict each other")
	case mode.native:
		return modeNative, nil
	case mode.docker:
		if err := dockerAvailable(); err != nil {
			return 0, fmt.Errorf("%w\n\nUse --native to register it with the "+
				"system service manager instead", err)
		}
		return modeDocker, nil
	}

	// Unasked. Native is the default on a machine with a service manager: it
	// needs nothing installed, it runs as you -- which is what lets a worker
	// reach your files -- and it does not require Docker Desktop to be running
	// for your scheduler to work.
	if err := dockerAvailable(); err == nil {
		fmt.Fprintln(env.Stderr,
			"using the system service manager; --docker to use a container instead")
	}
	return modeNative, nil
}

// runDockerInstall starts a component as a container and verifies it.
//
// endpoint, when set, is where clients on this host will reach it. It is
// recorded before verification, because verification itself goes through the
// same lookup -- a control plane we cannot find is one we cannot check.
func runDockerInstall(
	ctx context.Context, env *Env, spec dockerSpec, mode installMode,
	verify func(context.Context) error, nextStep string, endpoint string,
) error {
	// --print exists so this is auditable rather than magic. Everything the
	// CLI does here you could have typed, and being able to read it is what
	// makes a generated deployment better than a documented one rather than
	// merely faster.
	if mode.printOnly {
		fmt.Fprintln(env.Stdout, spec.String())
		return nil
	}

	if err := spec.start(ctx); err != nil {
		return err
	}

	if endpoint != "" {
		if err := WriteEndpoint(env.Layout.Endpoint(), Endpoint{
			Address: endpoint, Kind: "docker",
		}); err != nil {
			return err
		}
	}

	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "started\t%s as a container\n", spec.component)
	fmt.Fprintf(tw, "container\t%s\n", containerName(spec.component))
	fmt.Fprintf(tw, "image\t%s\n", spec.image)
	if endpoint != "" {
		fmt.Fprintf(tw, "reachable at\t%s\n", endpoint)
	}
	fmt.Fprintf(tw, "logs\tdocker logs -f %s\n", containerName(spec.component))
	if err := tw.Flush(); err != nil {
		return err
	}

	if verify != nil {
		if err := verify(ctx); err != nil {
			fmt.Fprintf(env.Stderr,
				"\nstarted, but it is not working yet:\n  %v\n\n"+
					"  docker logs %s   has what it said on the way up\n",
				err, containerName(spec.component))
			return errAttention
		}
		fmt.Fprintln(env.Stdout, "\nverified: it is running and answering.")
	}
	if nextStep != "" {
		fmt.Fprintf(env.Stdout, "\n%s\n", nextStep)
	}
	return nil
}

// localTimezone is what the host calls its timezone, for a container that
// would otherwise default to UTC.
func localTimezone() string {
	if tz := os.Getenv("TZ"); tz != "" {
		return tz
	}
	// The zoneinfo symlink is how macOS and most Linux distributions record
	// it, and reading it is more reliable than parsing `date`.
	if link, err := os.Readlink("/etc/localtime"); err == nil {
		if i := strings.Index(link, "zoneinfo/"); i >= 0 {
			return link[i+len("zoneinfo/"):]
		}
	}
	return "UTC"
}

// dockerImage resolves the image to run, or explains why it cannot.
//
// In --print mode a placeholder is fine and an error is not: printing is meant
// to be readable before anything is possible, including on a dev build.
func dockerImage(env *Env, mode installMode) (string, error) {
	if mode.printOnly {
		return printableImage(env.Version), nil
	}
	image := imageRef(env.Version)
	if image == "" {
		return "", fmt.Errorf(
			"this is a %s build, which has no published image.\n"+
				"Install a release (see the README) or use --native", env.Version)
	}
	return image, nil
}
