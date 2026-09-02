package cli

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"
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
		return adviseNoControlPlane(err)
	}

	ctx, cancel := withTimeout(ctx)
	defer cancel()

	health, err := client.Health(ctx)
	if err != nil {
		return adviseNoControlPlane(err)
	}

	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "control plane\trunning (%s)\n", health.Version)

	// C8, and the most important line here. A control plane with no worker
	// attached runs nothing at all: schedules fire into a queue that nobody
	// drains. That is exactly the kind of quiet nothing-happening P1 exists to
	// prevent, so it is stated on the default view and stated as a problem.
	if health.Workers == 0 {
		fmt.Fprintf(tw, "workers\tNONE -- nothing will run\n")
		fmt.Fprintf(tw, "\tstart one:  je worker\n")
	} else {
		fmt.Fprintf(tw, "workers\t%d online (%s)\n",
			health.Workers, strings.Join(health.Labels, ", "))
	}
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

	// The control plane runs whatever binary started it, and `je upgrade`
	// replaces a file rather than a process. Without saying so, the next thing
	// that happens is somebody upgrading, seeing no change, and concluding the
	// upgrade failed. A visible mismatch beats a mystery (P1), and it is the
	// same reasoning behind D20/C10 refusing worker version skew loudly.
	if !sameVersion(health.Version, env.Version) {
		fmt.Fprintf(tw, "version\tcontrol plane is running %s, this binary is %s\n",
			health.Version, env.Version)
		fmt.Fprintf(tw, "\trestart it to pick up %s\n", env.Version)
	}
	return tw.Flush()
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
