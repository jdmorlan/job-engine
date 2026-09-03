package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"text/tabwriter"
)

func init() {
	register(&Command{
		Name:  "explain",
		Args:  "<job>",
		Usage: "show every effective value of a job, and where it came from",
		Long: "P3 splits a job in two: the file holds what you decided, and this\n" +
			"shows the truth. Every value the engine will actually use, marked\n" +
			"either with the line you wrote it on or as a default.\n\n" +
			"That split is what lets a job file stay short without becoming\n" +
			"mysterious -- a file that omits the defaults is only honest if\n" +
			"something can show them to you.\n\n" +
			"It also answers \"what starts this job?\", which the file cannot: a\n" +
			"schedule is written here, and a chain step pointing at this job is\n" +
			"written somewhere else entirely.",
		Run: runExplain,
	})
}

func runExplain(ctx context.Context, env *Env, args []string) error {
	fs := newFlagSet(commands["explain"], env)
	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usagef("give exactly one job name")
	}

	return withClient(ctx, env, func(ctx context.Context, rd *Client) error {
		x, err := rd.Explain(ctx, rest[0])
		if err != nil {
			return err
		}

		fmt.Fprintf(env.Stdout, "%s", x.Slug)
		if x.DisplayName != "" {
			fmt.Fprintf(env.Stdout, "  %s", x.DisplayName)
		}
		fmt.Fprintln(env.Stdout)
		if x.Description != "" {
			fmt.Fprintf(env.Stdout, "%s\n", x.Description)
		}
		fmt.Fprintf(env.Stdout, "%s\n\n", x.FilePath)

		file := filepath.Base(x.FilePath)
		tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
		for _, f := range x.Fields {
			// The provenance column is the entire point of this view, so it is
			// never blank: every value is either something you wrote at a line
			// we can name, or a default.
			origin := "(default)"
			if f.Declared() {
				origin = fmt.Sprintf("(%s:%d)", file, f.Line)
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", f.Field, f.Value, origin)
		}
		for _, s := range x.Secrets {
			status := "NOT SET -- this job will not run"
			if s.Set {
				status = "set"
			}
			fmt.Fprintf(tw, "  secret %s\t%s\n", s.Name, status)
		}
		if err := tw.Flush(); err != nil {
			return err
		}

		fmt.Fprintln(env.Stdout)
		if len(x.Triggers) == 0 {
			// Not a footnote. A job nothing starts is a job that only runs when
			// you type its name, and that is worth saying out loud rather than
			// leaving as an empty section somebody has to notice.
			fmt.Fprintln(env.Stdout, "nothing starts this job automatically")
			fmt.Fprintf(env.Stdout, "  je run %s   to run it now\n", x.Slug)
		} else {
			fmt.Fprintln(env.Stdout, "starts when")
			tw = tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
			for _, t := range x.Triggers {
				switch t.Kind {
				case "schedule":
					fmt.Fprintf(tw, "  %s\tcatch_up: %s\t%s\n",
						t.Schedule, t.CatchUp, filepath.Base(t.File))
				default:
					fmt.Fprintf(tw, "  %s\tchain %s step %d\t%s\n",
						t.Match, t.Chain, t.Step, filepath.Base(t.File))
				}
			}
			if err := tw.Flush(); err != nil {
				return err
			}
		}

		if x.Problem != "" {
			// Last, because it is what you should still be looking at.
			fmt.Fprintf(env.Stdout, "\nthis job cannot run: %s\n", x.Problem)
			return errAttention
		}
		return nil
	})
}
