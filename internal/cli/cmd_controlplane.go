package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

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
			"It serves HTTPS from an authority it issues itself, and a client that\n" +
			"presents a certificate from that authority is that worker (D25). There\n" +
			"is no plaintext listener and no flag that brings one back.\n\n" +
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
			"For the whole engine in one command -- this, a worker and the web client,\n" +
			"each in the shape it should actually run in -- use `je up`.",
		Local: true,
		Run:   runControlPlane,
	})
}

func runControlPlane(ctx context.Context, env *Env, args []string) error {
	cmd := commands["control-plane"]
	fs := newFlagSet(cmd, env)
	addr := fs.String("addr", daemon.DefaultAddr, "address to listen on")
	verbose := fs.Bool("v", false, "log at debug level")
	tlsHosts := fs.String("tls-host", "", "comma-separated extra names this control plane is reached by")
	githubAPI := fs.String("github-api", "", "GitHub API base URL, for GitHub Enterprise")
	useDocker := fs.Bool("docker", false, "install as a container instead of a native service")
	native := fs.Bool("native", false, "install as a native service (launchd or systemd)")
	printOnly := fs.Bool("print", false, "print what would be done, and do nothing")
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
		return runControlPlaneForeground(ctx, env, *addr, *verbose, splitLabels(*tlsHosts), *githubAPI)
	case "install":
		return installControlPlane(ctx, env, *addr, installMode{
			docker: *useDocker, native: *native, printOnly: *printOnly,
		})
	case "status":
		return componentStatus(ctx, env, service.ControlPlane)
	case "remove":
		return removeComponent(ctx, env, service.ControlPlane)
	default:
		return usagef("unknown subcommand %q; expected run, install, status or remove",
			positional[0])
	}
}

// installControlPlane sets up a control plane and proves it answers.
func installControlPlane(ctx context.Context, env *Env, addr string, mode installMode) error {
	// C8, said at the only moment it is cheap to act on. A control plane with
	// no worker is registered, running, and executes nothing -- and every
	// other view will look healthy while it does so.
	next := "It will not run anything yet: a control plane never executes (D20).\n" +
		"Attach a worker:  je worker join"

	// Verification is "does it answer", not "did the supervisor accept a file".
	verify := func(ctx context.Context) error { return waitForControlPlane(ctx, env) }

	// A containerised control plane brings its own authority, and this machine
	// has to be holding that one before anything tries to verify.
	//
	// The container generates a CA inside its own volume. A machine that has
	// ever run a native control plane already has a different CA on disk, and
	// authorityPath will keep preferring it -- so every connection is checked
	// against a control plane that no longer exists. That failure is worse than
	// it sounds: the container is up and answering, and the error names an
	// authority the reader has never heard of.
	//
	// Copied here rather than on demand because this is the moment the answer
	// changes; doing it per-command would mean a `docker cp` on every
	// connection. Before `verify`, because `verify` is itself a connection.
	dockerVerify := func(ctx context.Context) error {
		if err := adoptContainerAuthorityWithin(ctx, env.Layout, 15*time.Second); err != nil {
			return fmt.Errorf("taking the authority out of the container: %w", err)
		}
		return verify(ctx)
	}

	kind, err := chooseMode(env, mode)
	if err != nil {
		return err
	}

	if kind == modeDocker {
		image, err := dockerImage(env, mode)
		if err != nil {
			return err
		}
		spec := dockerSpec{
			component: "control-plane",
			owner:     env.Layout.Data,
			image:     image,
			// 0.0.0.0 inside the container; the published port is what
			// decides who can reach it, and that stays on loopback.
			args:  []string{"--addr", "0.0.0.0:7620"},
			ports: []string{addr + ":7620"},
			volumes: []string{
				// A named volume, never a bind mount: SQLite over a macOS
				// bind mount goes through VirtioFS and has the same class of
				// silent locking pathology D19 warns about for NFS.
				"je-data:/var/lib/je",
			},
			// D9/D19: containers default to UTC and schedules mean local time
			// to a human. This is the "why did my 3am job run at 8pm" bug.
			env: []string{"TZ=" + localTimezone()},
			// Joined to the shared bridge so a containerised worker can reach
			// it by name, which is the only address that works from inside
			// another container.
			network: networkName,
		}
		if !mode.printOnly {
			if err := ensureNetwork(ctx); err != nil {
				return err
			}
		}
		// The published address, which is how this host reaches it. Inside the
		// container it binds 0.0.0.0, and that address is meaningless here.
		return runDockerInstall(ctx, env, spec, mode, dockerVerify, next, addr)
	}

	return installComponent(ctx, env, installPlan{
		component: service.ControlPlane,
		args:      []string{"--addr", addr},
		verify:    verify,
		nextStep:  next,
	})
}

func runControlPlaneForeground(ctx context.Context, env *Env, addr string, verbose bool, tlsHosts []string, githubAPI string) error {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	// Logs go to stderr so that stdout stays available for anything a command
	// is genuinely producing. In a container both are collected anyway, and
	// keeping them separate is what makes the collected stream readable.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	return daemon.Run(ctx, daemon.Config{
		Layout:    env.Layout,
		Addr:      addr,
		Version:   env.Version,
		Logger:    logger,
		TLSHosts:  tlsHosts,
		GitHubAPI: githubAPI,
	})
}
