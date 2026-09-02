package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"text/tabwriter"
	"time"
)

func init() {
	register(&Command{
		Name:  "runs",
		Args:  "[job]",
		Usage: "list recent runs, newest first",
		Run:   runRuns,
	})
	register(&Command{
		Name:  "logs",
		Args:  "<run>",
		Usage: "show the captured output of a run",
		Long: "Lines are stored individually and timestamped at the moment the job\n" +
			"produced them, not the moment the engine got around to storing them (D6).",
		Run: runLogs,
	})
}

func runRuns(ctx context.Context, env *Env, args []string) error {
	cmd := commands["runs"]
	fs := newFlagSet(cmd, env)
	limit := fs.Int("n", 20, "how many runs to show")
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(positional) > 1 {
		return usagef("expected at most one job name, got %d", len(positional))
	}

	return withReader(ctx, env, func(ctx context.Context, rd Reader) error {
		var jobSlug string
		if len(positional) == 1 {
			jobSlug = positional[0]
		}

		runs, err := rd.Runs(ctx, jobSlug, *limit)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("no job named %q", jobSlug)
			}
			return err
		}
		if len(runs) == 0 {
			fmt.Fprintln(env.Stdout, "no runs yet")
			return nil
		}

		// Resolve job names once rather than per row.
		names, err := jobNames(ctx, rd)
		if err != nil {
			return err
		}

		tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "RUN\tJOB\tSTATUS\tSTARTED\tDURATION\tATTEMPTS")
		for _, r := range runs {
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%d\n",
				r.ID, names[r.JobID], r.Status,
				formatOptionalTime(r.StartedAt), runDuration(r.StartedAt, r.EndedAt),
				r.AttemptCount)
		}
		return tw.Flush()
	})
}

func runLogs(ctx context.Context, env *Env, args []string) error {
	cmd := commands["logs"]
	fs := newFlagSet(cmd, env)
	attempt := fs.Int("attempt", 0, "which attempt to show (default: the last one)")
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return usagef("expected exactly one run id, got %d", len(positional))
	}
	runID, err := strconv.ParseInt(positional[0], 10, 64)
	if err != nil {
		return usagef("%q is not a run id", positional[0])
	}

	return withReader(ctx, env, func(ctx context.Context, rd Reader) error {
		lines, err := rd.Logs(ctx, runID, *attempt)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("no run %d", runID)
			}
			return err
		}
		if len(lines) == 0 {
			fmt.Fprintf(env.Stdout, "run %d produced no output\n", runID)
			return nil
		}

		for _, l := range lines {
			w := env.Stdout
			if l.Stream == "stderr" {
				w = env.Stderr
			}
			fmt.Fprintf(w, "%s  %s\n", l.TS.Local().Format("15:04:05.000"), l.Line)
		}
		return nil
	})
}

func formatOptionalTime(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.Local().Format(time.DateTime)
}

func runDuration(start, end *time.Time) string {
	if start == nil || end == nil {
		return "-"
	}
	return end.Sub(*start).Round(time.Millisecond).String()
}
