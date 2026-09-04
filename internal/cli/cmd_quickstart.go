package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/jdmorlan/job-engine/internal/daemon"
	"github.com/jdmorlan/job-engine/internal/jobdef"
	"github.com/jdmorlan/job-engine/internal/worker"
)

func init() {
	register(&Command{
		Name:  "quickstart",
		Usage: "run a control plane and a worker together, in this terminal",
		Long: "A whole job engine in one command, for trying it out.\n\n" +
			"D20 splits the system in two: a control plane that decides what should\n" +
			"run, and workers that run it. That is the right shape and it costs a\n" +
			"second thing to start, so this starts both.\n\n" +
			"It is not a third mode. The worker here talks to the control plane over\n" +
			"the same HTTP API a worker on another machine would use -- there is no\n" +
			"in-process shortcut, and nothing works here that would not work split\n" +
			"across two boxes -- including the transport, which is HTTPS with the\n" +
			"worker enrolling itself against the control plane's own authority. For anything unattended use `docker compose up -d`.",
		Local: true,
		Run:   runQuickstart,
	})
}

func runQuickstart(ctx context.Context, env *Env, args []string) error {
	cmd := commands["quickstart"]
	fs := newFlagSet(cmd, env)
	addr := fs.String("addr", daemon.DefaultAddr, "address for the control plane")
	labels := fs.String("labels", jobdef.DefaultRunsOn, "labels for the worker it starts")
	verbose := fs.Bool("v", false, "log at debug level")
	if extra, err := parseArgs(fs, args); err != nil {
		return err
	} else if len(extra) > 0 {
		return usagef("unexpected argument %q", extra[0])
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	ready := make(chan struct{})
	planeDone := make(chan error, 1)
	go func() {
		planeDone <- daemon.Run(ctx, daemon.Config{
			Layout:  env.Layout,
			Addr:    *addr,
			Version: env.Version,
			Logger:  logger,
			Ready:   ready,
		})
	}()

	// Wait for the listener before dialling. Racing it would mean the worker's
	// first few claims fail with connection refused, which looks like a broken
	// install on the very first command somebody runs.
	select {
	case <-ready:
	case err := <-planeDone:
		if err != nil {
			return err
		}
		return fmt.Errorf("the control plane stopped before it was ready")
	case <-ctx.Done():
		return <-planeDone
	}

	// Resolved from the runtime file rather than from --addr, so that a port of
	// 0 (which the OS picks) reaches the port it actually chose.
	info, err := daemon.ReadRuntime(env.Layout.Runtime())
	if err != nil {
		cancel()
		<-planeDone
		return err
	}

	// The local case, so the worker enrolls itself from the token the control
	// plane just wrote into the data directory they share -- no token to paste
	// and no step to explain (D25).
	//
	// Fatal now, where it used to be a warning. It could be a warning while a
	// worker without an identity still had a plaintext socket to fall back to;
	// with the flip there is nothing behind it, so carrying on would print
	// "one worker attached" and then fail every claim.
	if err := autoEnroll(ctx, env, dialable(info.Address), defaultWorkerName(), splitLabels(*labels), nil); err != nil {
		cancel()
		<-planeDone
		return fmt.Errorf("enrolling the worker this command starts: %w", err)
	}

	client, err := dialControlPlane(env, dialable(info.Address))
	if err != nil {
		cancel()
		<-planeDone
		return err
	}
	w, err := worker.New(worker.Options{
		Name:    defaultWorkerName(),
		Labels:  splitLabels(*labels),
		JobsDir: env.Layout.Jobs,
		// The same two paths `je worker run` passes. Leaving them to the
		// worker's own defaults put its age key under <data>/cache while
		// `je worker keygen` wrote <data>/identity, so a secret encrypted to
		// the key you had just created could not be read here (D25).
		CacheDir:     env.Layout.Data,
		IdentityFile: env.Layout.AgeIdentity(),
		Version:      env.Version,
		Client:       client,
		Logger:       logger,
	})
	if err != nil {
		cancel()
		<-planeDone
		return err
	}

	fmt.Fprintf(env.Stderr,
		"\nje: control plane on %s, one worker attached (%s)\n"+
			"    try:  je jobs        in another terminal\n"+
			"          je run <job>\n\n",
		info.Address, *labels)

	workerDone := make(chan error, 1)
	go func() { workerDone <- w.Run(ctx) }()

	// Either half stopping takes the other down. A control plane with no worker
	// runs nothing (C11) and a worker with no control plane has nothing to do,
	// so leaving one of them alive would only produce a system that looks up
	// and is not.
	select {
	case err := <-workerDone:
		cancel()
		<-planeDone
		return err
	case err := <-planeDone:
		cancel()
		<-workerDone
		return err
	}
}
