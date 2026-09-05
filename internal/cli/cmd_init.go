package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"
)

func init() {
	register(&Command{
		Name:  "init",
		Args:  "[directory]",
		Usage: "set up a new jobs repository",
		Long: "Creates the tree a source expects -- job files at the top, chains in\n" +
			"chains/, the code they run in scripts/ -- plus a README describing the\n" +
			"layout and a .gitignore.\n\n" +
			"A source is a whole tree and not a pile of YAML (D22): the scripts a\n" +
			"job runs live beside it and travel with it, which is what makes a repo\n" +
			"portable to another machine unmodified.\n\n" +
			"It writes files and registers nothing, and it runs no git -- this is a\n" +
			"tree in a repository of yours, not one the engine manages. Point the\n" +
			"engine at it with je source add <name> <directory>.",
		Run: runInit,
	})
}

func runInit(ctx context.Context, env *Env, args []string) error {
	fs := newFlagSet(commands["init"], env)
	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}

	dir := "."
	if len(rest) == 1 {
		dir = rest[0]
	} else if len(rest) > 1 {
		return usagef("give one directory, or none for the current one")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}

	name := filepath.Base(abs)
	files := map[string]string{
		"README.md":  repoReadme(name),
		".gitignore": gitignore(),
	}
	// chains/ only. A job's code lives in the job's own folder now, so a
	// shared scripts/ directory is the layout this replaced -- and an empty
	// one sitting in a fresh repository is an invitation to use it.
	for _, sub := range []string{"chains"} {
		if err := os.MkdirAll(filepath.Join(abs, sub), 0o755); err != nil {
			return err
		}
	}

	var written, skipped []string
	for rel, body := range files {
		path := filepath.Join(abs, rel)
		if _, err := os.Stat(path); err == nil {
			// Never clobber. `je init` in an existing repo should be safe to
			// run, since that is exactly when somebody reaches for it.
			skipped = append(skipped, rel)
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return err
		}
		written = append(written, rel)
	}

	fmt.Fprintf(env.Stdout, "jobs repository at %s\n\n", abs)
	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  <name>/job.yaml\ta job. The folder's name is the job's name.\n")
	fmt.Fprintf(tw, "  <name>/\tand its code, beside it\n")
	fmt.Fprintf(tw, "  chains/<name>.yaml\tone flow, wiring jobs to each other\n")
	tw.Flush()

	// Not a tabwriter: one of these lines carries a path, and a single long
	// path turns an aligned table into a scroll bar.
	// The steps that actually reach a running job. A source is a repository
	// the control plane fetches (D22), so pushing is not optional politeness --
	// it is how the engine ever sees any of this.
	// `cd .` is not a step. Printed only when init was given a directory
	// other than the one you are standing in, which is the only time it is
	// something to do rather than noise in a list of instructions.
	step := fmt.Sprintf("  cd %s\n", dir)
	if here, err := os.Getwd(); err == nil && here == abs {
		step = ""
	}
	fmt.Fprintf(env.Stdout, `
next
%s  je new <job> --language python     writes a job and its script here
  je dev <job>                       run it from here, before pushing anything
  git init && git add -A && git commit -m "jobs"
  gh repo create %s --private --source=. --push

then point the engine at it:
  je source add %s <you>/%s
`, step, name, name, name)

	if len(skipped) > 0 {
		fmt.Fprintf(env.Stderr, "\nleft alone (already present): %v\n", skipped)
	}
	return nil
}

func repoReadme(name string) string {
	return fmt.Sprintf(`# %s

Jobs for [je](https://github.com/jdmorlan/job-engine).

    <name>/job.yaml      a job; the folder's name is the job's name
    <name>/...           its code and whatever else it needs, beside it
    chains/<name>.yaml   one flow, wiring jobs to each other

A job is a folder, so it is one thing to read, move or delete. A single
`+"`<name>.yaml`"+` at the top level is also a job, for one that has nothing to
keep beside it.

Register this repository with an engine:

    je source add %s .

Job names from this source are prefixed with it -- `+"`%s/<job>`"+` -- so it can
sit alongside other repositories without their names colliding. The short name
works whenever it is unambiguous.

## Writing a job

    je new my-job --language typescript
    je dev my-job

Job files hold only what you decided; everything else is a default. To see the
full picture, including the values this file does not set and where each one
came from:

    je explain my-job

## The contract

There is no SDK. The engine talks to a job through the environment and three
files, which is the same in every language:

    JE_STATE             your cursor, as JSON; seeded on the first run
    JOB_STATE_OUT_FILE   write the new cursor here; committed only on exit 0
    JOB_OUTPUT_FILE      structured output
    JOB_EVENTS_FILE      append JSONL to emit events other jobs can react to

`+"`je new --language <lang>`"+` writes a script with all of it in place. For
TypeScript and JavaScript there is also a helper to import, `+"`je`"+`, which the
worker writes beside your dependencies. It is sugar over exactly
those files and can never do more than they can.

`+"`je dev <job>`"+` runs a job straight from this directory, before it is pushed
anywhere -- a real run, with real logs and events.
`, title(name), name, name)
}

// title is titleFromSlug for a repository name, which may not be a slug.
func title(name string) string {
	if name == "" || name == "." {
		return "jobs"
	}
	return titleFromSlug(name)
}

// gitignore covers what running a job here leaves behind.
//
// It used to say "nothing here is generated", which stopped being true when the
// worker started preparing trees: a run installs your dependencies and
// writes the helpers your job imports (D28, D21), both into the tree it is
// standing in. That is the same thing a worker does to its own cache copy, and
// it is the right place for it -- but in your working copy it is somebody's
// first `git add -A`, and node_modules is not a thing to commit.
func gitignore() string {
	return `# What running a job here leaves behind.
#
# ` + "`je dev`" + ` prepares this tree the way a worker would: your dependencies
# installed from your lockfile, and the helpers your job imports written where
# your language resolves them. Both belong to the machine, not to the repository.
node_modules/
.venv/
__pycache__/
.je-prepared-*
`
}
