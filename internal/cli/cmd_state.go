package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/jdmorlan/job-engine/internal/engine"
	"github.com/jdmorlan/job-engine/internal/store"
)

func init() {
	register(&Command{
		Name:  "state",
		Args:  "get|history <job>",
		Usage: "show a job's cursor and how it has moved",
		Long: "D14 treats the cursor's movement over time as part of the feature rather\n" +
			"than a follow-up. \"The cursor stopped moving on Tuesday even though runs\n" +
			"kept succeeding\" is a bug class that is invisible in other job engines\n" +
			"and obvious here.",
		Run: runState,
	})
}

func runState(ctx context.Context, env *Env, args []string) error {
	cmd := commands["state"]
	fs := newFlagSet(cmd, env)
	limit := fs.Int("n", 20, "how many versions to show in history")
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 2 {
		return usagef("usage: je state get|history <job>")
	}
	sub, slug := positional[0], positional[1]

	return withEngine(ctx, env, func(ctx context.Context, eng *engine.Engine) error {
		def, job, err := eng.Definition(ctx, slug)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("no job named %q", slug)
			}
			return err
		}

		switch sub {
		case "get":
			current, err := eng.CurrentState(ctx, job.ID)
			if errors.Is(err, sql.ErrNoRows) {
				fmt.Fprintf(env.Stdout,
					"%s has no cursor yet; it will be seeded on its first run\n", slug)
				return nil
			} else if err != nil {
				return err
			}
			fmt.Fprintf(env.Stdout, "%s\n", current.Value)
			return nil

		case "history":
			versions, err := eng.StateHistory(ctx, job.ID, *limit)
			if err != nil {
				return err
			}
			if len(versions) == 0 {
				fmt.Fprintf(env.Stdout, "%s has no cursor yet\n", slug)
				return nil
			}
			tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
			for _, v := range versions {
				fmt.Fprintf(tw, "v%d\t%s\t%s\t%s -> %s\n",
					v.Version,
					v.CreatedAt.Local().Format(time.DateTime),
					stateAuthor(v),
					def.State.PrimaryCursor,
					v.Summary(def.State.PrimaryCursor))
			}
			return tw.Flush()

		default:
			return usagef("unknown subcommand %q; expected get or history", sub)
		}
	})
}

// stateAuthor says who moved the cursor. The distinction between the engine's
// seed, a run, and a person is the first thing you want when a cursor is
// somewhere surprising.
func stateAuthor(v store.StateVersion) string {
	switch {
	case v.SetByActor == store.ActorEngine:
		return "engine (seed)"
	case v.SetByActor != "":
		return v.SetByActor + " (manual)"
	case v.SetByRun != nil:
		return fmt.Sprintf("run %d", *v.SetByRun)
	default:
		return "unknown"
	}
}
