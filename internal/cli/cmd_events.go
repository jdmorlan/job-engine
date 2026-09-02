package cli

import (
	"context"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/jdmorlan/job-engine/internal/engine"
	"github.com/jdmorlan/job-engine/internal/model"
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

	// Reads go through the embedded engine in stage 0, the same as `je jobs`,
	// `je runs` and `je state`. See the note on withEngine: once the API has
	// endpoints for these, every read routes through the daemon when one is
	// running and falls back to embedded when it is not.
	return withEngine(ctx, env, func(ctx context.Context, eng *engine.Engine) error {
		events, err := eng.RecentEvents(ctx, *limit)
		if err != nil {
			return err
		}
		if len(events) == 0 {
			fmt.Fprintln(env.Stdout, "no events yet")
			return nil
		}

		tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tWHEN\tTYPE\tSOURCE\tCAUSE\tPAYLOAD")
		for _, e := range events {
			payload := string(e.Payload)
			if payload == "" {
				payload = "-"
			}
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n",
				e.ID, e.CreatedAt.Local().Format(time.DateTime), e.Type, e.Source,
				causeOf(e), truncate(payload, 40))
		}
		return tw.Flush()
	})
}

// causeOf renders what an event came from, which is the first hop of the
// causation chain `je why` will later print in full (D12).
func causeOf(e model.Event) string {
	switch {
	case e.CausedByRunID != nil:
		return fmt.Sprintf("run %d", *e.CausedByRunID)
	case e.CausedByEventID != nil:
		return fmt.Sprintf("event %d", *e.CausedByEventID)
	default:
		return "-"
	}
}
