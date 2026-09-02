package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/jdmorlan/job-engine/internal/daemon"
	"github.com/jdmorlan/job-engine/internal/selfupdate"
	"github.com/jdmorlan/job-engine/internal/service"
)

func init() {
	register(&Command{
		Name:  "service",
		Args:  "install|uninstall|status|restart",
		Usage: "keep the daemon running across logins and crashes",
		Long: "Registers `je serve` with launchd or systemd as a per-user service, so it\n" +
			"starts at login and comes back if it dies. No sudo: it runs as you, which\n" +
			"is what lets jobs reach your files.\n\n" +
			"This is separate from installing the binary. `je upgrade` replaces the\n" +
			"file; `je service restart` makes the running daemon pick it up.",
		Run: runService,
	})
}

func runService(ctx context.Context, env *Env, args []string) error {
	cmd := commands["service"]
	fs := newFlagSet(cmd, env)
	addr := fs.String("addr", daemon.DefaultAddr, "address the daemon should listen on")
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return usagef("usage: je service install|uninstall|status|restart")
	}

	mgr, err := service.New()
	if err != nil {
		return err
	}

	switch positional[0] {
	case "install":
		return serviceInstall(env, mgr, *addr)
	case "uninstall":
		return serviceUninstall(env, mgr)
	case "status":
		return serviceStatus(env, mgr)
	case "restart":
		return serviceRestart(env, mgr)
	default:
		return usagef("unknown subcommand %q; expected install, uninstall, status or restart",
			positional[0])
	}
}

func serviceInstall(env *Env, mgr service.Manager, addr string) error {
	binary, err := selfupdate.CurrentBinary()
	if err != nil {
		return err
	}
	// A service manager will not search a PATH you assume, so anything but an
	// absolute path here produces a unit that fails at boot with no clue why.
	if !filepath.IsAbs(binary) {
		return fmt.Errorf("cannot register %s: the service needs an absolute path to je", binary)
	}
	if err := env.Layout.EnsureData(); err != nil {
		return err
	}

	logPath := filepath.Join(env.Layout.Data, "daemon.log")
	cfg := service.Config{
		Binary:  binary,
		DataDir: env.Layout.Data,
		Addr:    addr,
		LogPath: logPath,
		// Captured from the installing shell rather than left to the service
		// manager's minimal default. Explained at length on service.Config,
		// because it is the difference between jobs working and jobs failing
		// with "command not found" only when the daemon runs them.
		Path: os.Getenv("PATH"),
	}

	if err := mgr.Install(cfg); err != nil {
		return err
	}

	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "registered with\t%s\n", mgr.Name())
	fmt.Fprintf(tw, "unit\t%s\n", mgr.UnitPath())
	fmt.Fprintf(tw, "binary\t%s\n", binary)
	fmt.Fprintf(tw, "data dir\t%s\n", env.Layout.Data)
	fmt.Fprintf(tw, "listening on\t%s\n", addr)
	fmt.Fprintf(tw, "engine log\t%s\n", logPath)
	tw.Flush()

	fmt.Fprintf(env.Stdout, "\nIt is running now and will start again at login.\n"+
		"  je status     is it healthy\n"+
		"  je waiting    what it intends to do next\n")

	// Said plainly, because it is the surprise: the unit pins the binary's
	// path and its PATH as they are right now.
	fmt.Fprintf(env.Stdout, "\nThe service records this binary's location and your current PATH.\n"+
		"Run `je service install` again after moving je or changing PATH.\n")
	return nil
}

func serviceUninstall(env *Env, mgr service.Manager) error {
	if err := mgr.Uninstall(); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "removed the %s service (%s)\n", mgr.Name(), mgr.UnitPath())
	// Nothing was deleted but the registration. Worth saying, since "uninstall"
	// is a frightening word next to a database of run history.
	fmt.Fprintln(env.Stdout, "your jobs, run history and cursors are untouched")
	return nil
}

func serviceRestart(env *Env, mgr service.Manager) error {
	state, err := mgr.Status()
	if err != nil {
		return err
	}
	if !state.Installed {
		return fmt.Errorf("no %s service is registered; install it with: je service install", mgr.Name())
	}
	if err := mgr.Restart(); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "restarted the %s service\n", mgr.Name())
	return nil
}

func serviceStatus(env *Env, mgr service.Manager) error {
	state, err := mgr.Status()
	if err != nil {
		return err
	}

	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "manager\t%s\n", state.Manager)
	switch {
	case !state.Installed:
		fmt.Fprintf(tw, "registered\tno\n")
	case !state.Loaded:
		fmt.Fprintf(tw, "registered\tyes, but not loaded\n")
	default:
		fmt.Fprintf(tw, "registered\tyes\n")
	}
	if state.PID > 0 {
		fmt.Fprintf(tw, "running\tpid %d\n", state.PID)
	} else if state.Installed {
		fmt.Fprintf(tw, "running\tno\n")
	}
	fmt.Fprintf(tw, "unit\t%s\n", state.UnitPath)
	if state.Detail != "" {
		fmt.Fprintf(tw, "note\t%s\n", state.Detail)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	if !state.Installed {
		fmt.Fprintln(env.Stdout, "\nRegister it with: je service install")
	}
	return nil
}
