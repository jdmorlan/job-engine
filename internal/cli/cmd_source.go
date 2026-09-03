package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jdmorlan/job-engine/internal/api"
	"github.com/jdmorlan/job-engine/internal/engine"
	"github.com/jdmorlan/job-engine/internal/store"
)

func init() {
	register(&Command{
		Name:  "source",
		Args:  "[add <name> <path> | sync <name> | remove <name>]",
		Usage: "register and inspect the places definitions come from",
		Long: "Definitions and the code they run arrive from named sources, and a\n" +
			"source is a whole tree rather than a pile of YAML: the scripts a job\n" +
			"runs live beside it and arrive with it (D22).\n\n" +
			"Every engine has a built-in source called `local`, which is your jobs\n" +
			"directory. Its jobs keep bare names. Jobs from a registered source are\n" +
			"named for it -- weather/ingest -- so two repos may both contain a\n" +
			"sync.yaml without one shadowing the other. The short name still works\n" +
			"whenever it is unambiguous.\n\n" +
			"With no arguments, lists what is registered.",
		Run: runSource,
	})
}

func runSource(ctx context.Context, env *Env, args []string) error {
	fs := newFlagSet(commands["source"], env)
	subpath := fs.String("path", "", "the directory within the tree holding job files")
	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return listSources(ctx, env)
	}

	switch verb := rest[0]; verb {
	case "add":
		if len(rest) != 3 {
			return usagef("je source add <name> <path>")
		}
		return addSource(ctx, env, rest[1], rest[2], *subpath)
	case "sync":
		if len(rest) != 2 {
			return usagef("je source sync <name>")
		}
		return syncSource(ctx, env, rest[1])
	case "remove", "rm":
		if len(rest) != 2 {
			return usagef("je source remove <name>")
		}
		return removeSource(ctx, env, rest[1])
	default:
		return usagef("unknown subcommand %q; try add, sync or remove", verb)
	}
}

func listSources(ctx context.Context, env *Env) error {
	return withClient(ctx, env, func(ctx context.Context, rd *Client) error {
		sources, err := rd.Sources(ctx)
		if err != nil {
			return err
		}

		tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tKIND\tWHERE\tJOBS\tSYNCED")
		for _, s := range sources {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n",
				s.Name, s.Kind, sourceWhere(s), s.Jobs, sourceSynced(s))
		}
		if err := tw.Flush(); err != nil {
			return err
		}

		var broken []engine.SourceStatus
		for _, s := range sources {
			if s.LastError != "" {
				broken = append(broken, s)
			}
		}
		if len(broken) > 0 {
			// A source that stopped loading keeps serving its last good tree,
			// which is correct and is also exactly why it has to be said out
			// loud: everything looks fine and the repo has quietly stopped
			// updating.
			fmt.Fprintln(env.Stdout, "\nNOT LOADING  (still serving what they last loaded)")
			for _, s := range broken {
				fmt.Fprintf(env.Stdout, "  %s\n    %s\n", s.Name, s.LastError)
			}
			return errAttention
		}
		return nil
	})
}

func addSource(ctx context.Context, env *Env, name, path, subpath string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	// Checked here as well as on the control plane, because the message can be
	// better: this side knows whether the path exists *on the machine you
	// typed it on*, which is the more likely mistake once the control plane is
	// a container somewhere else.
	if _, err := os.Stat(abs); err != nil {
		return fmt.Errorf("%s: %w", abs, err)
	}

	return withClient(ctx, env, func(ctx context.Context, rd *Client) error {
		result, err := rd.AddSource(ctx, api.AddSourceRequest{
			Name:     name,
			Kind:     store.SourceKindDir,
			Location: abs,
			Subpath:  subpath,
		})
		if err != nil {
			return err
		}

		fmt.Fprintf(env.Stdout, "registered %s -> %s\n", name, abs)
		printLoaded(env, name, result)
		return nil
	})
}

func syncSource(ctx context.Context, env *Env, name string) error {
	return withClient(ctx, env, func(ctx context.Context, rd *Client) error {
		result, err := rd.SyncSource(ctx, name)
		if err != nil {
			return err
		}
		printLoaded(env, name, result)
		return nil
	})
}

func removeSource(ctx context.Context, env *Env, name string) error {
	return withClient(ctx, env, func(ctx context.Context, rd *Client) error {
		tombstoned, err := rd.RemoveSource(ctx, name)
		if err != nil {
			return err
		}
		fmt.Fprintf(env.Stdout, "unregistered %s\n", name)
		// Said explicitly, because "removed" reads as destructive and this is
		// not: the jobs stop being schedulable and every run they ever did is
		// still there (D19).
		fmt.Fprintf(env.Stdout,
			"  %d job(s) will no longer run; their runs, logs and cursors are kept\n",
			tombstoned)
		return nil
	})
}

func printLoaded(env *Env, name string, result engine.LoadResult) {
	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  jobs\t%d\n", result.Loaded)
	if result.Chains > 0 {
		fmt.Fprintf(tw, "  chains\t%d (%d route(s))\n", result.Chains, result.Routes)
	}
	if result.Removed > 0 {
		fmt.Fprintf(tw, "  removed\t%d whose files are gone (history kept)\n", result.Removed)
	}
	tw.Flush()

	if result.Loaded == 0 {
		// A registration that succeeds and provides nothing is the failure
		// this command is most likely to produce, and it looks like success.
		fmt.Fprintf(env.Stdout,
			"\n  no job files found -- job files are <source>/*.yaml, chains are <source>/chains/*.yaml\n"+
				"  if they live in a subdirectory: je source add %s <path> --path jobs\n", name)
	}
}

func sourceWhere(s engine.SourceStatus) string {
	if s.Kind == store.SourceKindDir {
		return collapseHome(s.Path)
	}
	if s.Ref != "" {
		return s.Location + "@" + s.Ref
	}
	return s.Location
}

func sourceSynced(s engine.SourceStatus) string {
	if s.LastError != "" {
		return "FAILED"
	}
	if s.SyncedAt == nil {
		return "-"
	}
	return roundDuration(time.Since(*s.SyncedAt)).String() + " ago"
}

// collapseHome shortens a path for a table, since these are usually long and
// usually under $HOME.
func collapseHome(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || !strings.HasPrefix(path, home+string(filepath.Separator)) {
		return path
	}
	return "~" + path[len(home):]
}
