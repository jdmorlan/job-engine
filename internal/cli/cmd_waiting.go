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

	return withClient(ctx, env, func(ctx context.Context, rd *Client) error {
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
			fmt.Fprintln(env.Stdout, "QUEUED  (waiting for a worker slot)")
			tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
			for _, r := range w.Queued {
				fmt.Fprintf(tw, "  %s\trun %d\tqueued %s\n",
					names[r.JobID], r.ID, sinceText(&r.QueuedAt))
			}
			tw.Flush()
			fmt.Fprintln(env.Stdout)
		}

		if len(w.Retrying) > 0 {
			fmt.Fprintln(env.Stdout, "RETRYING  (failed, waiting to try again)")
			tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
			for _, r := range w.Retrying {
				next := "now"
				if r.NextAttemptAt != nil {
					next = fmt.Sprintf("in %s", untilText(*r.NextAttemptAt))
				}
				fmt.Fprintf(tw, "  %s\trun %d\tattempt %d failed\tnext attempt %s\n",
					names[r.JobID], r.ID, r.AttemptCount, next)
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

		if len(w.Unservable) > 0 {
			// C8: queued work nothing can take. Above BLOCKED because it is
			// less obviously broken -- everything about it looks like ordinary
			// queueing, which is exactly why it needs to be said out loud.
			fmt.Fprintln(env.Stdout,
				"WAITING FOR A WORKER  (queued for a label nothing is serving)")
			for _, u := range w.Unservable {
				fmt.Fprintf(env.Stdout, "  runs_on: %s\n    %d run(s), jobs: %s\n"+
					"    start one:  je worker run --labels %s\n",
					u.Label, len(u.Runs), strings.Join(u.Jobs, ", "), u.Label)
			}
			// The command above is the local case. A label like `macos` usually
			// means a machine that is not this one, and that machine needs an
			// identity before it can talk to anything (D25) -- so the line that
			// mints one belongs here rather than in a page somebody has to know
			// to go and read.
			fmt.Fprintln(env.Stdout,
				"  On another machine, enroll it first:  je enroll <name> --labels <label>")
			fmt.Fprintln(env.Stdout)
		}

		if len(w.Triggers) > 0 {
			// D3 calls this view the feature, and it is: the mechanism is a
			// few hundred lines, and answering "why hasn't the rollup run?"
			// without reading logs is what makes it worth having.
			fmt.Fprintln(env.Stdout,
				"WAITING TO FAN IN  (some conditions met, the rest not yet)")
			tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "  TRIGGER\tWAITING ON\tSATISFIED\tEXPIRES")
			for _, t := range w.Triggers {
				satisfied := make([]string, 0, len(t.Satisfied))
				for _, sc := range t.Satisfied {
					satisfied = append(satisfied,
						fmt.Sprintf("%s (%s)", sc.Condition, sc.At.Local().Format("15:04")))
				}
				fmt.Fprintf(tw, "  %s/step %d -> %s\t%s\t%s\t%s\n",
					t.Chain, t.Step, t.Job,
					strings.Join(t.Waiting, ", "),
					strings.Join(satisfied, "; "),
					humanUntil(t.Expires))
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			fmt.Fprintln(env.Stdout,
				"\n  After EXPIRES the events already seen fall out of the window,\n"+
					"  and the trigger starts again from whatever arrives next.")
			fmt.Fprintln(env.Stdout)
		}

		if len(w.UnservedRuntimes) > 0 {
			// A label matched and a worker would have taken this, and it would
			// have failed on arrival for want of a toolchain. Distinct from the
			// label case above and worth its own heading: the fix is on a
			// machine that already exists rather than a machine that does not.
			fmt.Fprintln(env.Stdout,
				"WAITING FOR A RUNTIME  (queued for a language no worker can prepare)")
			for _, u := range w.UnservedRuntimes {
				fmt.Fprintf(env.Stdout, "  language: %s\n    %d run(s), jobs: %s\n"+
					"    on a worker that should run these:  je worker runtime install %s\n",
					u.Language, len(u.Runs), strings.Join(u.Jobs, ", "), u.Language)
			}
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

		if len(w.Running)+len(w.Queued)+len(w.Retrying)+len(w.Scheduled)+len(w.Blocked) == 0 {
			fmt.Fprintln(env.Stdout, "nothing scheduled, queued, or running")
			return nil
		}

		if w.NeedsAttention() {
			return errAttention
		}
		return nil
	})
}

func jobNames(ctx context.Context, rd *Client) (map[int64]string, error) {
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
