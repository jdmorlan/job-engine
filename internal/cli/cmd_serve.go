package cli

import (
	"context"
	"log/slog"
	"os"

	"github.com/jdmorlan/job-engine/internal/daemon"
)

func init() {
	register(&Command{
		Name:  "serve",
		Usage: "run the control plane in the foreground",
		Long: "The control plane owns the database and is the only process that writes\n" +
			"to it. Every other command is a client of its API.\n\n" +
			"It never runs a job itself (D20/C11) -- that needs at least one worker,\n" +
			"which is what `je worker` starts. `docker compose up` brings up both.",
		Run: runServe,
	})
}

func runServe(ctx context.Context, env *Env, args []string) error {
	cmd := commands["serve"]
	fs := newFlagSet(cmd, env)
	addr := fs.String("addr", daemon.DefaultAddr, "address to listen on")
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
	// Logs go to stderr so that stdout stays available for anything a command
	// is genuinely producing. In a container both are collected anyway, and
	// keeping them separate is what makes the collected stream readable.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	return daemon.Run(ctx, daemon.Config{
		Layout:  env.Layout,
		Addr:    *addr,
		Version: env.Version,
		Logger:  logger,
	})
}
