package cli

import (
	"context"
	"fmt"
	"path/filepath"
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

		st := env.Style
		fmt.Fprintf(env.Stdout, "%s", st.Title(x.Slug))
		if x.DisplayName != "" {
			fmt.Fprintf(env.Stdout, "  %s", x.DisplayName)
		}
		fmt.Fprintln(env.Stdout)
		if x.Description != "" {
			fmt.Fprintf(env.Stdout, "%s\n", x.Description)
		}
		fmt.Fprintf(env.Stdout, "%s\n\n", st.Muted(x.FilePath))

		file := filepath.Base(x.FilePath)
		tw := env.table()
		for _, f := range x.Fields {
			// The provenance column is the entire point of this view, so it is
			// never blank: every value is either something you wrote at a line
			// we can name, or a default.
			origin := "(default)"
			if f.Declared() {
				origin = fmt.Sprintf("(%s:%d)", file, f.Line)
			}
			// A value you set and a value you inherited are different kinds of
			// fact, and this view exists to tell them apart. Dimming the
			// inherited ones makes what the file actually says legible at a
			// glance, which is the question that brought you here.
			value, origin := f.Value, st.Muted(origin)
			if !f.Declared() {
				value = st.Muted(value)
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", st.Header(f.Field), value, origin)
		}
		for _, s := range x.Secrets {
			status := st.Bad("NOT SET -- this job will not run")
			if s.Set {
				status = st.Good("set")
			}
			fmt.Fprintf(tw, "  %s\t%s\n", st.Header("secret "+s.Name), status)
		}
		if err := tw.Flush(); err != nil {
			return err
		}

		fmt.Fprintln(env.Stdout)
		if len(x.Triggers) == 0 {
			// Not a footnote. A job nothing starts is a job that only runs when
			// you type its name, and that is worth saying out loud rather than
			// leaving as an empty section somebody has to notice.
			env.section("nothing starts this job automatically", "")
			fmt.Fprintf(env.Stdout, "  %s   %s\n",
				st.Cmd("je run "+x.Slug), st.Muted("to run it now"))
		} else {
			env.section("starts when", "")
			tw = env.table()
			for _, t := range x.Triggers {
				switch t.Kind {
				case "schedule":
					fmt.Fprintf(tw, "  %s\t%s\t%s\n", t.Schedule,
						st.Muted("catch_up: "+t.CatchUp), st.Muted(filepath.Base(t.File)))
				default:
					fmt.Fprintf(tw, "  %s\t%s\t%s\n", t.Match,
						st.Muted(fmt.Sprintf("chain %s step %d", t.Chain, t.Step)),
						st.Muted(filepath.Base(t.File)))
				}
			}
			if err := tw.Flush(); err != nil {
				return err
			}
		}

		if x.Problem != "" {
			// Last, because it is what you should still be looking at.
			fmt.Fprintf(env.Stdout, "\n%s %s\n", st.Bad("this job cannot run:"), x.Problem)
			return errAttention
		}
		return nil
	})
}
