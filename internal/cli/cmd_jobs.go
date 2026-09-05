package cli

import (
	"context"
	"fmt"

	"github.com/jdmorlan/job-engine/internal/engine"
	"github.com/jdmorlan/job-engine/internal/store"
)

func init() {
	register(&Command{
		Name:  "jobs",
		Usage: "list loaded job definitions",
		Long: "Reads the jobs directory and shows what the engine would run.\n" +
			"A job that parsed but cannot run is listed and visibly broken,\n" +
			"rather than silently absent (D10).",
		Run: runJobs,
	})
}

func runJobs(ctx context.Context, env *Env, args []string) error {
	cmd := commands["jobs"]
	fs := newFlagSet(cmd, env)
	all := fs.Bool("all", false, "include removed jobs, and the engine's own")
	if extra, err := parseArgs(fs, args); err != nil {
		return err
	} else if len(extra) > 0 {
		return usagef("unexpected argument %q", extra[0])
	}

	return withClient(ctx, env, func(ctx context.Context, rd *Client) error {
		loaded, err := rd.Jobs(ctx)
		if err != nil {
			return err
		}

		// Tombstoned jobs keep their history but are not part of "what is
		// loaded". Listing them by default would mean a jobs directory you
		// have tidied up reads as a wall of broken jobs -- the same reason P2
		// keeps system jobs out of the default view.
		var jobs []store.Job
		var hidden, housekeeping int
		for _, j := range loaded {
			switch {
			case j.Removed() && !*all:
				hidden++
			case engine.IsSystem(j.Slug) && !*all:
				// P2's second rider, in the view it was written about: the
				// engine's own jobs are real jobs with real runs, and putting
				// them on the "is everything OK?" screen would make that
				// screen mostly housekeeping. Visible on request.
				housekeeping++
			default:
				jobs = append(jobs, j)
			}
		}

		if len(jobs) == 0 {
			st := env.Style
			tw := env.table()
			fmt.Fprint(env.Stdout, "no jobs loaded.\n\n"+
				"Definitions live in a repository and reach the engine as a source:\n")
			fmt.Fprintf(tw, "  %s\t\n", st.Cmd("je source add <name> <owner/repo>"))
			fmt.Fprintf(tw, "  %s\t%s\n", st.Cmd("je demo"),
				st.Muted("the examples, from this project's repo"))
			return tw.Flush()
		}

		st := env.Style
		tw := env.table()
		fmt.Fprintln(tw, st.Header("JOB\tSTATUS\tCOMMAND\tFILE"))
		for _, j := range jobs {
			def, err := rd.Definition(ctx, j.Slug)
			command := ""
			if err == nil {
				command = truncate(def.CommandLine(), 40)
			}
			// The file path is here so you know where to go and edit; it is
			// not what you are scanning the list for.
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
				j.Slug, st.State(jobStatus(j)), command, st.Muted(j.FilePath))
		}
		if err := tw.Flush(); err != nil {
			return err
		}

		// The reason, not just the state. A table cell saying "broken" that
		// does not say why is a prompt to go reading files (P1).
		for _, j := range jobs {
			if reason := brokenReason(j); reason != "" {
				fmt.Fprintf(env.Stdout, "\n%s: %s\n", st.Bad(j.Slug), reason)
			}
		}
		if hidden > 0 {
			fmt.Fprintf(env.Stdout, "\n%s%s\n",
				st.Muted(fmt.Sprintf("%d removed job(s) hidden; their history is intact. ", hidden)),
				st.Cmd("je jobs --all"))
		}
		if housekeeping > 0 {
			fmt.Fprintf(env.Stdout, "\n%s%s\n",
				st.Muted(fmt.Sprintf("%d job(s) the engine runs for itself, hidden. ", housekeeping)),
				st.Cmd("je jobs --all"))
		}
		return nil
	})
}

func jobStatus(j store.Job) string {
	switch {
	case j.Removed():
		return "removed"
	case j.LoadError != "":
		return "load error"
	case j.ConfigError != "":
		return "misconfigured"
	case !j.Enabled:
		return "disabled"
	default:
		return "ok"
	}
}

func brokenReason(j store.Job) string {
	if j.Removed() {
		// Not broken. Deliberately deleted, and its history is still here.
		return ""
	}
	if j.LoadError != "" {
		return j.LoadError
	}
	return j.ConfigError
}
