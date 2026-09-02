package cli

import (
	"context"
	"fmt"
	"text/tabwriter"
	"time"
)

func init() {
	register(&Command{
		Name:  "events",
		Usage: "list recent events, newest first",
		Long: "Events are the spine of the system: a schedule firing, a run finishing,\n" +
			"an outside system calling `je emit`, and the engine's own start and stop.\n" +
			"Everything that happens is one, so this is the rawest view of the timeline.",
		Run: runEvents,
	})
}

func runEvents(ctx context.Context, env *Env, args []string) error {
	cmd := commands["events"]
	fs := newFlagSet(cmd, env)
	limit := fs.Int("n", 20, "how many events to show")
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

	events, err := client.Events(ctx, *limit)
	if err != nil {
		return adviseNoDaemon(err)
	}
	if len(events) == 0 {
		fmt.Fprintln(env.Stdout, "no events yet")
		return nil
	}

	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tWHEN\tTYPE\tSOURCE\tPAYLOAD")
	for _, e := range events {
		payload := string(e.Payload)
		if payload == "" {
			payload = "-"
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n",
			e.ID, e.CreatedAt.Local().Format(time.DateTime), e.Type, e.Source, payload)
	}
	return tw.Flush()
}
