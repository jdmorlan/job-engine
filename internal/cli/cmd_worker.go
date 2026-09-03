package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/jdmorlan/job-engine/internal/daemon"
	"github.com/jdmorlan/job-engine/internal/jobdef"
	"github.com/jdmorlan/job-engine/internal/worker"
)

func init() {
	register(&Command{
		Name:  "worker",
		Args:  "run",
		Usage: "a worker: the thing that actually executes jobs",
		Long: "A worker executes jobs. The control plane never does (D20/C11), so a\n" +
			"deployment with no worker runs nothing at all -- `je status` says so.\n\n" +
			"Workers advertise capability labels, and a job's `runs_on` picks one.\n" +
			"A worker on your Mac advertising `macos` is how a job that has to talk\n" +
			"to Shortcuts reaches the machine that can.\n\n" +
			"It holds no state and opens no ports: it dials the control plane and\n" +
			"keeps asking for work, which is why it works from a laptop behind NAT.\n\n" +
			"subcommands:\n" +
			"  run    run it in the foreground, in this terminal\n\n" +
			"`je workers` lists the ones already attached.",
		Run: runWorker,
	})
	register(&Command{
		Name:  "workers",
		Usage: "list the workers attached to the control plane",
		Run:   runWorkers,
	})
}

func runWorker(ctx context.Context, env *Env, args []string) error {
	cmd := commands["worker"]
	fs := newFlagSet(cmd, env)
	name := fs.String("name", defaultWorkerName(), "what to call this worker")
	labels := fs.String("labels", jobdef.DefaultRunsOn, "comma-separated capability labels")
	addr := fs.String("addr", "", "control plane address (default: the one this data dir records)")
	concurrency := fs.Int("concurrency", 0, "how many jobs to run at once")
	verbose := fs.Bool("v", false, "log at debug level")
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 {
		return usagef("usage: je worker run")
	}
	switch positional[0] {
	case "run":
		if len(positional) != 1 {
			return usagef("unexpected argument %q", positional[1])
		}
	default:
		return usagef("unknown subcommand %q; expected run", positional[0])
	}

	target := *addr
	if target == "" {
		// Same resolution the CLI uses, so a worker on this machine finds the
		// control plane without being told twice.
		resolved, err := controlPlaneAddr(env)
		if err != nil {
			return adviseNoControlPlane(err)
		}
		target = resolved
	}

	client, err := worker.Dial(target)
	if err != nil {
		return err
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	w, err := worker.New(worker.Options{
		Name:        *name,
		Labels:      splitLabels(*labels),
		Concurrency: *concurrency,
		Version:     env.Version,
		Client:      client,
		Logger:      logger,
	})
	if err != nil {
		return err
	}
	logger.Info("connecting", "control_plane", client.Addr())
	return w.Run(ctx)
}

func runWorkers(ctx context.Context, env *Env, args []string) error {
	cmd := commands["workers"]
	fs := newFlagSet(cmd, env)
	if extra, err := parseArgs(fs, args); err != nil {
		return err
	} else if len(extra) > 0 {
		return usagef("unexpected argument %q", extra[0])
	}

	return withClient(ctx, env, func(ctx context.Context, c *Client) error {
		listCtx, cancel := withTimeout(ctx)
		defer cancel()

		workers, err := c.Workers(listCtx)
		if err != nil {
			return err
		}
		if len(workers) == 0 {
			// C8: this is the single most important diagnostic in the system.
			// A control plane with no worker runs nothing, and the sentence
			// saying so is worth more than an empty table.
			fmt.Fprintln(env.Stdout,
				"no workers attached -- nothing will run.\n\n"+
					"Start one:  je worker run\n"+
					"            docker compose up -d   (unattended)")
			return nil
		}

		tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tROLES\tLABELS\tSESSION")
		for _, w := range workers {
			session := "offline"
			if w.Online {
				session = "online " + humanAge(w.LastSeenAt)
			} else if w.GoneAt != nil {
				session = "offline since " + humanAge(*w.GoneAt)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
				w.Name, strings.Join(w.Roles, ", "), strings.Join(w.Labels, ", "), session)
		}
		return tw.Flush()
	})
}

// controlPlaneAddr resolves where the control plane is listening.
func controlPlaneAddr(env *Env) (string, error) {
	if addr := os.Getenv("JE_ADDR"); addr != "" {
		return addr, nil
	}
	info, err := daemon.ReadRuntime(env.Layout.Runtime())
	if err != nil {
		return "", err
	}
	return info.Address, nil
}

// defaultWorkerName is the machine's name, which is what a person calls it.
func defaultWorkerName() string {
	if host, err := os.Hostname(); err == nil && host != "" {
		return host
	}
	return "worker"
}

func splitLabels(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
