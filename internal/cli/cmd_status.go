package cli

import (
	"context"
	"errors"
	"fmt"
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
	return tw.Flush()
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
