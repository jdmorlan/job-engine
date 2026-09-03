package cli

import (
	"context"
	"log/slog"
	"os"

	"github.com/jdmorlan/job-engine/internal/daemon"
	"github.com/jdmorlan/job-engine/internal/service"
)

func init() {
	register(&Command{
		Name:  "control-plane",
		Args:  "run|install|status|remove",
		Usage: "the control plane: schedules, history, and the API",
		Long: "The control plane owns the database and is the only process that writes\n" +
			"to it. Every other command is a client of its API.\n\n" +
			"It never runs a job itself (D20/C11). That needs at least one worker,\n" +
			"which is what `je worker run` starts -- and a control plane with none\n" +
			"attached runs nothing at all, which `je status` says in its second line.\n\n" +
			"subcommands:\n" +
			"  run       run it in the foreground, in this terminal\n" +
			"  install   register it with launchd or systemd, so it survives a reboot\n" +
			"  status    is it registered, and is it up\n" +
			"  remove    unregister it; your data and history are left alone\n\n" +
			"`install` sets up only the control plane. A worker is a separate act on\n" +
			"a possibly different machine, so it is `je worker join` -- and until you\n" +
			"run one, nothing executes.\n\n" +
			"To try both together without registering anything, use `je quickstart`.",
		Run: runControlPlane,
	})
}

func runControlPlane(ctx context.Context, env *Env, args []string) error {
	cmd := commands["control-plane"]
	fs := newFlagSet(cmd, env)
	addr := fs.String("addr", daemon.DefaultAddr, "address to listen on")
	verbose := fs.Bool("v", false, "log at debug level")
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 {
		return usagef("usage: je control-plane run|install|status|remove")
	}

	if len(positional) != 1 {
		return usagef("unexpected argument %q", positional[1])
	}

	switch positional[0] {
	case "run":
		return runControlPlaneForeground(ctx, env, *addr, *verbose)
	case "install":
		return installControlPlane(ctx, env, *addr)
	case "status":
		return componentStatus(env, service.ControlPlane)
	case "remove":
		return removeComponent(env, service.ControlPlane)
	default:
		return usagef("unknown subcommand %q; expected run, install, status or remove",
			positional[0])
	}
}

// installControlPlane registers the control plane and proves it answers.
func installControlPlane(ctx context.Context, env *Env, addr string) error {
	return installComponent(ctx, env, installPlan{
		component: service.ControlPlane,
		args:      []string{"--addr", addr},

		// Verification is "does it answer", not "did launchd accept a file".
		verify: func(ctx context.Context) error {
			return waitForControlPlane(ctx, env)
		},

		// C8, said at the only moment it is cheap to act on. A control plane
		// with no worker is registered, running, and executes nothing -- and
		// every other view will look healthy while it does so.
		nextStep: "It will not run anything yet: a control plane never executes (D20).\n" +
			"Attach a worker:  je worker join",
	})
}

func runControlPlaneForeground(ctx context.Context, env *Env, addr string, verbose bool) error {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	// Logs go to stderr so that stdout stays available for anything a command
	// is genuinely producing. In a container both are collected anyway, and
	// keeping them separate is what makes the collected stream readable.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	return daemon.Run(ctx, daemon.Config{
		Layout:  env.Layout,
		Addr:    addr,
		Version: env.Version,
		Logger:  logger,
	})
}
