package cli

import (
	"context"
	"fmt"
	"text/tabwriter"
	"time"
)

func init() {
	register(&Command{
		Name:  "waiting",
		Usage: "show what the engine intends to do but has not done yet",
		Long: "The negative space: what is scheduled, what is queued behind the\n" +
			"concurrency cap, and what is blocked and will never resolve itself.\n\n" +
			"Most job engines can show you what ran. Very few can show you what\n" +
			"didn't, and what it is waiting for. Exits 3 if something is blocked,\n" +
			"so it is usable in a script.",
		Run: runWaiting,
	})
}

func runWaiting(ctx context.Context, env *Env, args []string) error {
	cmd := commands["waiting"]
	fs := newFlagSet(cmd, env)
	if extra, err := parseArgs(fs, args); err != nil {
		return err
	} else if len(extra) > 0 {
		return usagef("unexpected argument %q", extra[0])
	}

	return withReader(ctx, env, func(ctx context.Context, rd Reader) error {
		w, err := rd.Waiting(ctx)
		if err != nil {
			return err
		}

		names, err := jobNames(ctx, rd)
		if err != nil {
			return err
		}

		if len(w.Running) > 0 {
			fmt.Fprintln(env.Stdout, "RUNNING")
			tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
			for _, r := range w.Running {
				fmt.Fprintf(tw, "  %s\trun %d\tstarted %s\n",
					names[r.JobID], r.ID, sinceText(r.StartedAt))
			}
			tw.Flush()
			fmt.Fprintln(env.Stdout)
		}

		if len(w.Queued) > 0 {
			fmt.Fprintln(env.Stdout, "QUEUED  (behind the concurrency cap)")
			tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
			for _, r := range w.Queued {
				fmt.Fprintf(tw, "  %s\trun %d\tqueued %s\n",
					names[r.JobID], r.ID, sinceText(&r.QueuedAt))
			}
			tw.Flush()
			fmt.Fprintln(env.Stdout)
		}

		if len(w.Scheduled) > 0 {
			fmt.Fprintln(env.Stdout, "SCHEDULED")
			tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
			for _, s := range w.Scheduled {
				fmt.Fprintf(tw, "  %s\t%s\tnext %s\t(in %s)\n",
					s.Job, s.Schedule,
					s.Next.Local().Format(time.DateTime),
					untilText(s.Next))
			}
			tw.Flush()
			fmt.Fprintln(env.Stdout)
		}

		if len(w.Blocked) > 0 {
			// Last, because it is the part that needs a human. Putting it at
			// the bottom means it is what you are looking at when the command
			// finishes.
			fmt.Fprintln(env.Stdout, "BLOCKED  (these will not run until you fix them)")
			for _, j := range w.Blocked {
				fmt.Fprintf(env.Stdout, "  %s\n    %s\n", j.Slug, brokenReason(j))
			}
			fmt.Fprintln(env.Stdout)
		}

		if len(w.Running)+len(w.Queued)+len(w.Scheduled)+len(w.Blocked) == 0 {
			fmt.Fprintln(env.Stdout, "nothing scheduled, queued, or running")
			return nil
		}

		if w.NeedsAttention() {
			return errAttention
		}
		return nil
	})
}

func jobNames(ctx context.Context, rd Reader) (map[int64]string, error) {
	jobs, err := rd.Jobs(ctx)
	if err != nil {
		return nil, err
	}
	names := make(map[int64]string, len(jobs))
	for _, j := range jobs {
		names[j.ID] = j.Slug
	}
	return names, nil
}

func sinceText(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return roundDuration(time.Since(*t)).String() + " ago"
}

func untilText(t time.Time) string {
	d := time.Until(t)
	if d < 0 {
		return "now"
	}
	return roundDuration(d).String()
}
