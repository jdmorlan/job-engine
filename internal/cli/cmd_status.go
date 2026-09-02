package cli

import (
	"context"
	"errors"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/jdmorlan/job-engine/internal/service"
)

func init() {
	register(&Command{
		Name:  "status",
		Usage: "show whether the engine is running and healthy",
		Run:   runStatus,
	})
}

func runStatus(ctx context.Context, env *Env, args []string) error {
	cmd := commands["status"]
	fs := newFlagSet(cmd, env)
	if extra, err := parseArgs(fs, args); err != nil {
		return err
	} else if len(extra) > 0 {
		return usagef("unexpected argument %q", extra[0])
	}

	client, err := Connect(env.Layout)
	if err != nil {
		return adviseNoDaemon(err)
	}

	ctx, cancel := withTimeout(ctx)
	defer cancel()

	health, err := client.Health(ctx)
	if err != nil {
		return adviseNoDaemon(err)
	}

	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "engine\trunning (%s)\n", health.Version)
	fmt.Fprintf(tw, "uptime\t%s\n", roundDuration(health.Uptime))
	fmt.Fprintf(tw, "since\t%s\n", health.StartedAt.Local().Format(time.RFC1123))
	fmt.Fprintf(tw, "data dir\t%s\n", health.DataDir)
	fmt.Fprintf(tw, "jobs dir\t%s\n", health.JobsDir)

	// D16 records the engine's own downtime so a gap in a job's history can be
	// attributed to "the machine was asleep" rather than "something went
	// wrong". Distinguishing those two is most of what makes a laptop-hosted
	// scheduler trustworthy, so it is on the default status view, not hidden.
	if health.LastDowntime >= time.Second {
		fmt.Fprintf(tw, "last gap\t%s before this start\n", roundDuration(health.LastDowntime))
	}
	if health.UncleanStop {
		fmt.Fprintf(tw, "note\tprevious run ended without a clean shutdown\n")
	}

	// The daemon runs whatever binary started it, and `je upgrade` replaces a
	// file rather than a process. Without saying so, the next thing that
	// happens is somebody upgrading, seeing no change, and concluding the
	// upgrade failed. A visible mismatch beats a mystery (P1), and it is the
	// same reasoning behind D20 C10 refusing node version skew loudly.
	if state, err := serviceState(); err == nil && !state.Installed {
		// Not an error -- running `je serve` in a terminal is a legitimate
		// mode, and D19 calls it stage 0. But somebody who expects unattended
		// operation should find out now rather than after a reboot.
		fmt.Fprintf(tw, "autostart\tnot registered (je service install)\n")
	}
	if !sameVersion(health.Version, env.Version) {
		fmt.Fprintf(tw, "version\tdaemon is running %s, this binary is %s\n",
			health.Version, env.Version)
		fmt.Fprintf(tw, "\trestart the daemon to pick up %s\n", env.Version)
	}
	return tw.Flush()
}

// serviceState reports on the OS service registration, if this platform has
// one. Failures are the caller's cue to say nothing rather than to complain:
// an unsupported platform is not a problem with the engine.
func serviceState() (service.State, error) {
	mgr, err := service.New()
	if err != nil {
		return service.State{}, err
	}
	return mgr.Status()
}

// adviseNoDaemon turns a connection failure into an instruction.
//
// P1 in the small: the error a person sees at 2am should tell them what to do
// next, not just what went wrong.
func adviseNoDaemon(err error) error {
	if errors.Is(err, ErrNoDaemon) {
		return fmt.Errorf("%w\n\nStart one with:  je serve", err)
	}
	return err
}

// roundDuration renders a duration at a precision a human cares about:
// seconds when it is short, minutes once it is long enough not to matter.
func roundDuration(d time.Duration) time.Duration {
	switch {
	case d < time.Minute:
		return d.Round(time.Second)
	case d < time.Hour:
		return d.Round(time.Second)
	default:
		return d.Round(time.Minute)
	}
}
