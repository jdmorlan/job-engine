package cli

import (
	"context"
	"log/slog"
	"os"

	"github.com/jdmorlan/job-engine/internal/daemon"
)

func init() {
	register(&Command{
		Name:  "control-plane",
		Args:  "run",
		Usage: "the control plane: schedules, history, and the API",
		Long: "The control plane owns the database and is the only process that writes\n" +
			"to it. Every other command is a client of its API.\n\n" +
			"It never runs a job itself (D20/C11). That needs at least one worker,\n" +
			"which is what `je worker run` starts -- and a control plane with none\n" +
			"attached runs nothing at all, which `je status` says in its second line.\n\n" +
			"subcommands:\n" +
			"  run    run it in the foreground, in this terminal\n\n" +
			"To bring up a control plane and a worker together, use `je quickstart`.",
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
		return usagef("usage: je control-plane run")
	}

	switch positional[0] {
	case "run":
		if len(positional) != 1 {
			return usagef("unexpected argument %q", positional[1])
		}
		return runControlPlaneForeground(ctx, env, *addr, *verbose)
	default:
		return usagef("unknown subcommand %q; expected run", positional[0])
	}
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
