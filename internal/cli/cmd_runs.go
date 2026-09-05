package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
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

	return withClient(ctx, env, func(ctx context.Context, rd *Client) error {
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
			// "No runs yet" is a lie about a job whose runs retention has
			// removed, and it is the worst instance of the confusion D13's
			// counting exists to prevent: an empty list is exactly what a job
			// that has never run looks like.
			if removed := removedRuns(ctx, rd, jobSlug); removed > 0 {
				fmt.Fprintf(env.Stdout,
					"no runs within the keep period; %d older run(s) have been "+
						"removed by retention. "+env.Style.Cmd("je explain system/retention")+"\n", removed)
				return nil
			}
			fmt.Fprintln(env.Stdout, "no runs yet")
			return nil
		}

		// Resolve job names once rather than per row.
		names, err := jobNames(ctx, rd)
		if err != nil {
			return err
		}

		st := env.Style
		tw := env.table()
		fmt.Fprintln(tw, st.Header("RUN\tJOB\tSTATUS\tSTARTED\tDURATION\tATTEMPTS"))
		for _, r := range runs {
			// Only a retried run has an attempt count worth reading; on every
			// other row the 1 is a column of ones, so it is dimmed rather than
			// dropped -- it still answers the question, it just stops
			// competing with the answer.
			attempts := fmt.Sprintf("%d", r.AttemptCount)
			if r.AttemptCount > 1 {
				attempts = st.Warn(attempts)
			} else {
				attempts = st.Muted(attempts)
			}
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n",
				r.ID, names[r.JobID], st.State(string(r.Status)),
				st.Muted(formatOptionalTime(r.StartedAt)), runDuration(r.StartedAt, r.EndedAt),
				attempts)
		}
		if err := tw.Flush(); err != nil {
			return err
		}

		// The floor under the list (D13). Deletion erases its own evidence:
		// without this line, a year-old job that retention has trimmed to
		// thirty days is indistinguishable from a job that started last month,
		// and nothing else in the system can tell you which you are looking
		// at.
		return printRetentionFloor(ctx, env, rd, jobSlug)
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

	return withClient(ctx, env, func(ctx context.Context, rd *Client) error {
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
			// The timestamp is the least interesting thing on a log line and
			// the leftmost, which is a bad combination: dimmed, it becomes a
			// margin instead of a column the eye has to cross.
			fmt.Fprintf(w, "%s  %s\n",
				env.Style.Muted(l.TS.Local().Format("15:04:05.000")), l.Line)
		}
		return nil
	})
}

// printRetentionFloor says how much history retention has removed.
//
// Scoped to the job being listed when there is one, and totalled across every
// job otherwise, because the question it answers -- "is this all of it?" -- is
// asked of whatever list is on the screen.
func printRetentionFloor(ctx context.Context, env *Env, rd *Client, jobSlug string) error {
	removed := removedRuns(ctx, rd, jobSlug)
	if removed == 0 {
		return nil
	}
	fmt.Fprintf(env.Stdout, "%s\n",
		env.Style.Muted(fmt.Sprintf("\n%d older run(s) have been removed by retention. ", removed))+
			env.Style.Cmd("je explain system/retention"))
	return nil
}

// removedRuns totals what retention has taken, for one job or for all of them.
//
// A failure to read it is not worth failing the command over: the list it
// annotates is correct either way, and this is a footnote to it.
func removedRuns(ctx context.Context, rd *Client, jobSlug string) int64 {
	jobs, err := rd.Jobs(ctx)
	if err != nil {
		return 0
	}
	var removed int64
	for _, j := range jobs {
		if jobSlug != "" && j.Slug != jobSlug {
			continue
		}
		removed += j.RunsRemoved
	}
	return removed
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
