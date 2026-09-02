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
		Usage: "run the engine daemon in the foreground",
		Long: "The daemon owns the database and is the only process that writes to it.\n" +
			"Every other command is a client of its API.\n\n" +
			"Stage 0 of D19's progression is running this in a terminal and watching it.\n" +
			"`je install` will later register it with launchd so it starts at login.",
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
	// is genuinely producing. Under launchd both are redirected to a file,
	// and keeping them separate there is what makes the file readable.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	return daemon.Run(ctx, daemon.Config{
		Layout:  env.Layout,
		Addr:    *addr,
		Version: env.Version,
		Logger:  logger,
	})
}
