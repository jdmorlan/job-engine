package cli

import (
	"context"
	"fmt"
	"text/tabwriter"

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
	if extra, err := parseArgs(fs, args); err != nil {
		return err
	} else if len(extra) > 0 {
		return usagef("unexpected argument %q", extra[0])
	}

	return withEngine(ctx, env, func(ctx context.Context, eng *engine.Engine) error {
		jobs, err := eng.Jobs(ctx)
		if err != nil {
			return err
		}
		if len(jobs) == 0 {
			fmt.Fprintf(env.Stdout, "no jobs in %s\n", env.Layout.Jobs)
			return nil
		}

		tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "JOB\tSTATUS\tCOMMAND\tFILE")
		for _, j := range jobs {
			def, _, err := eng.Definition(ctx, j.Slug)
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
		return nil
	})
}

func jobStatus(j store.Job) string {
	switch {
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
