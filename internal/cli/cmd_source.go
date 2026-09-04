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
	"github.com/jdmorlan/job-engine/internal/gitsource"
	"github.com/jdmorlan/job-engine/internal/store"
)

func init() {
	register(&Command{
		Name:  "source",
		Args:  "[add [<name>] <owner/repo | path> | sync <name> | remove <name>]",
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
	ref := fs.String("ref", "", "branch, tag or commit to track (default: the repository's own default branch)")
	token := fs.String("token", "", "name of the secret holding a GitHub token, for a private repo")
	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return listSources(ctx, env)
	}

	switch verb := rest[0]; verb {
	case "add":
		// The name is optional: `je source add you/weather-jobs` should just
		// work, and the repository already has a name.
		var name, location string
		switch len(rest) {
		case 2:
			location = rest[1]
			name = defaultSourceName(location)
		case 3:
			name, location = rest[1], rest[2]
		default:
			return usagef("je source add [<name>] <owner/repo | path>")
		}
		return addSource(ctx, env, sourceSpec{
			name: name, location: location,
			subpath: *subpath, ref: *ref, token: *token,
		})
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
		fmt.Fprintln(tw, "NAME\tKIND\tWHERE\tREVISION\tJOBS\tSYNCED")
		for _, s := range sources {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n",
				s.Name, s.Kind, sourceWhere(s), sourceRevision(s), s.Jobs, sourceSynced(s))
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

type sourceSpec struct {
	name     string
	location string
	subpath  string
	ref      string
	token    string
}

// defaultSourceName is the repository or directory's own name, so registering
// something does not require inventing a second name for it.
//
// Lowercased and cleaned, because a source name prefixes every job name from
// it and job names are slugs -- and half of GitHub is called Hello-World.
func defaultSourceName(location string) string {
	if gitsource.LooksLikeRepo(location) {
		if repo, err := gitsource.ParseRepo(location); err == nil {
			return slugify(repo.Name)
		}
	}
	abs, err := filepath.Abs(location)
	if err != nil {
		return slugify(filepath.Base(location))
	}
	return slugify(filepath.Base(abs))
}

// slugify makes a derived name usable. It is deliberately only applied to
// names we derived: a name somebody typed is validated rather than corrected,
// since silently registering something under a different name than they asked
// for is worse than saying no.
func slugify(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func addSource(ctx context.Context, env *Env, spec sourceSpec) error {
	req := api.AddSourceRequest{
		Name:        spec.name,
		Subpath:     spec.subpath,
		Ref:         spec.ref,
		TokenSecret: spec.token,
	}

	// Repositories only. A directory source used to be accepted here and it
	// never travelled: a job whose code sat on the control plane's disk could
	// only run on a worker sharing that disk, so it was already broken the
	// moment there were two machines (D22/D25).
	switch {
	case gitsource.LooksLikeRepo(spec.location):
		repo, err := gitsource.ParseRepo(spec.location)
		if err != nil {
			return err
		}
		req.Kind = store.SourceKindGitHub
		req.Location = repo.String()
	default:
		return fmt.Errorf(
			"%s is neither a directory here nor a GitHub repository -- "+
				"write a repository as owner/repo", spec.location)
	}

	if req.Kind == store.SourceKindGitHub && req.TokenSecret == "" {
		fmt.Fprintf(env.Stdout, "resolving %s...\n", req.Location)
	}

	return withClient(ctx, env, func(ctx context.Context, rd *Client) error {
		result, err := rd.AddSource(ctx, req)
		if err != nil {
			return err
		}

		fmt.Fprintf(env.Stdout, "registered %s -> %s\n", req.Name, req.Location)
		if result.Revision != "" {
			fmt.Fprintf(env.Stdout, "  resolved %s -> %s\n",
				sourceRef(result, req.Ref), shortSHA(result.Revision))
		}
		printLoaded(env, req.Name, result)
		return nil
	})
}

// sourceRef is the ref that was actually tracked, which is not necessarily the
// one that was typed: registering without --ref asks the repository what its
// default branch is called.
func sourceRef(result engine.LoadResult, asked string) string {
	if result.Ref != "" {
		return result.Ref
	}
	if asked != "" {
		return asked
	}
	return "the default branch"
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func syncSource(ctx context.Context, env *Env, name string) error {
	return withClient(ctx, env, func(ctx context.Context, rd *Client) error {
		before, err := revisionOf(ctx, rd, name)
		if err != nil {
			return err
		}

		result, err := rd.SyncSource(ctx, name)
		if err != nil {
			return err
		}

		// The revision, and whether it moved, is the thing worth reporting: a
		// sync that changed nothing and a sync that pulled in somebody else's
		// commit look identical from a job count.
		switch {
		case result.Revision == "":
		case result.Revision == before:
			fmt.Fprintf(env.Stdout, "%s  %s (unchanged)\n", name, shortSHA(result.Revision))
		default:
			fmt.Fprintf(env.Stdout, "%s  %s -> %s\n",
				name, shortSHA(before), shortSHA(result.Revision))
		}
		printLoaded(env, name, result)
		return nil
	})
}

func revisionOf(ctx context.Context, rd *Client, name string) (string, error) {
	sources, err := rd.Sources(ctx)
	if err != nil {
		return "", err
	}
	for _, s := range sources {
		if s.Name == name {
			return s.Revision, nil
		}
	}
	return "", nil
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
	if s.Ref != "" {
		return s.Location + "@" + s.Ref
	}
	return s.Location
}

// sourceRevision is the commit a fetched source is currently serving. It is a
// column of its own because it is the answer to "what code is running?", which
// is not something the repository name can tell you.
func sourceRevision(s engine.SourceStatus) string {
	if s.Revision == "" {
		return "-"
	}
	return shortSHA(s.Revision)
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
