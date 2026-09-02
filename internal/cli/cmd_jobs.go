package cli

import (
	"context"
	"fmt"
	"text/tabwriter"

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
	all := fs.Bool("all", false, "include jobs whose definition file has been removed")
	if extra, err := parseArgs(fs, args); err != nil {
		return err
	} else if len(extra) > 0 {
		return usagef("unexpected argument %q", extra[0])
	}

	return withReader(ctx, env, func(ctx context.Context, rd Reader) error {
		loaded, err := rd.Jobs(ctx)
		if err != nil {
			return err
		}

		// Tombstoned jobs keep their history but are not part of "what is
		// loaded". Listing them by default would mean a jobs directory you
		// have tidied up reads as a wall of broken jobs -- the same reason P2
		// keeps system jobs out of the default view.
		var jobs []store.Job
		var hidden int
		for _, j := range loaded {
			if j.Removed() && !*all {
				hidden++
				continue
			}
			jobs = append(jobs, j)
		}

		if len(jobs) == 0 {
			fmt.Fprintf(env.Stdout, "no jobs in %s\n", env.Layout.Jobs)
			return nil
		}

		tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "JOB\tSTATUS\tCOMMAND\tFILE")
		for _, j := range jobs {
			def, err := rd.Definition(ctx, j.Slug)
			command := ""
			if err == nil {
				command = truncate(shellJoin(def.Command), 40)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", j.Slug, jobStatus(j), command, j.FilePath)
		}
		if err := tw.Flush(); err != nil {
			return err
		}

		// The reason, not just the state. A table cell saying "broken" that
		// does not say why is a prompt to go reading files (P1).
		for _, j := range jobs {
			if reason := brokenReason(j); reason != "" {
				fmt.Fprintf(env.Stdout, "\n%s: %s\n", j.Slug, reason)
			}
		}
		if hidden > 0 {
			fmt.Fprintf(env.Stdout,
				"\n%d removed job(s) hidden; their history is intact. je jobs --all\n", hidden)
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

func shellJoin(argv []string) string {
	out := ""
	for i, a := range argv {
		if i > 0 {
			out += " "
		}
		out += a
	}
	return out
}
