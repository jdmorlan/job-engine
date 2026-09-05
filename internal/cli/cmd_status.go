package cli

import (
	"context"
	"fmt"
	"strings"
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
		return adviseNoControlPlane(env, err)
	}

	ctx, cancel := withTimeout(ctx)
	defer cancel()

	health, err := client.Health(ctx)
	if err != nil {
		return adviseNoControlPlane(env, err)
	}

	st := env.Style
	tw := env.table()
	// The label column is scaffolding: you know what you asked for, you are
	// here for the values.
	label := func(s string) string { return st.Header(s) }
	fmt.Fprintf(tw, "%s\t%s %s\n", label("control plane"),
		st.Good("running"), st.Muted("("+health.Version+")"))

	// C8, and the most important line here. A control plane with no worker
	// attached runs nothing at all: schedules fire into a queue that nobody
	// drains. That is exactly the kind of quiet nothing-happening P1 exists to
	// prevent, so it is stated on the default view and stated as a problem.
	if health.Workers == 0 {
		fmt.Fprintf(tw, "%s\t%s\n", label("workers"), st.Bad("NONE -- nothing will run"))
		fmt.Fprintf(tw, "\tattach one:  %s   %s\n",
			st.Cmd("je worker join"), st.Muted("(registered, survives a reboot)"))
		fmt.Fprintf(tw, "\t             %s    %s\n",
			st.Cmd("je worker run"), st.Muted("(foreground, this terminal)"))
	} else {
		fmt.Fprintf(tw, "%s\t%s %s\n", label("workers"),
			st.Good(fmt.Sprintf("%d online", health.Workers)),
			st.Muted("("+strings.Join(health.Labels, ", ")+")"))
	}
	fmt.Fprintf(tw, "%s\t%s\n", label("uptime"), roundDuration(health.Uptime))
	fmt.Fprintf(tw, "%s\t%s\n", label("since"),
		st.Muted(health.StartedAt.Local().Format(time.RFC1123)))
	fmt.Fprintf(tw, "%s\t%s\n", label("data dir"), st.Muted(health.DataDir))

	// D16 records the engine's own downtime so a gap in a job's history can be
	// attributed to "the machine was asleep" rather than "something went
	// wrong". Distinguishing those two is most of what makes a laptop-hosted
	// scheduler trustworthy, so it is on the default status view, not hidden.
	if health.LastDowntime >= time.Second {
		fmt.Fprintf(tw, "%s\t%s\n", label("last gap"),
			st.Warn(fmt.Sprintf("%s before this start", roundDuration(health.LastDowntime))))
	}
	if health.UncleanStop {
		fmt.Fprintf(tw, "%s\t%s\n", label("note"),
			st.Warn("previous run ended without a clean shutdown"))
	}

	// The control plane runs whatever binary started it, and `je upgrade`
	// replaces a file rather than a process. Without saying so, the next thing
	// that happens is somebody upgrading, seeing no change, and concluding the
	// upgrade failed. A visible mismatch beats a mystery (P1), and it is the
	// same reasoning behind D20/C10 refusing worker version skew loudly.
	if !sameVersion(health.Version, env.Version) {
		fmt.Fprintf(tw, "%s\t%s\n", label("version"),
			st.Warn(fmt.Sprintf("control plane is running %s, this binary is %s",
				health.Version, env.Version)))
		fmt.Fprintf(tw, "\t%s\n", st.Muted("restart it to pick up "+env.Version))
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
