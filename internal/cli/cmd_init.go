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
		".gitignore": "# The engine keeps its own state elsewhere; nothing here is generated.\n",
	}
	for _, sub := range []string{"chains", "scripts"} {
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
	fmt.Fprintf(tw, "  <name>.yaml\ta job. The file name is the job's name.\n")
	fmt.Fprintf(tw, "  chains/<name>.yaml\tone flow, wiring jobs to each other\n")
	fmt.Fprintf(tw, "  scripts/\tthe code your jobs run\n")
	tw.Flush()

	// Not a tabwriter: one of these lines carries a path, and a single long
	// path turns an aligned table into a scroll bar.
	// The steps that actually reach a running job. A source is a repository
	// the control plane fetches (D22), so pushing is not optional politeness --
	// it is how the engine ever sees any of this.
	fmt.Fprintf(env.Stdout, `
next
  cd %s
  je new <job> --language python     writes a job and its script here
  git init && git add -A && git commit -m "jobs"
  gh repo create %s --private --source=. --push

then point the engine at it:
  je source add %s <you>/%s
`, dir, name, name, name)

	if len(skipped) > 0 {
		fmt.Fprintf(env.Stderr, "\nleft alone (already present): %v\n", skipped)
	}
	return nil
}

func repoReadme(name string) string {
	return fmt.Sprintf(`# %s

Jobs for [je](https://github.com/jdmorlan/job-engine).

    <name>.yaml          a job; the file name is the job's name
    chains/<name>.yaml   one flow, wiring jobs to each other
    scripts/             the code the jobs run

Register this repository with an engine:

    je source add %s .

Job names from this source are prefixed with it -- `+"`%s/<job>`"+` -- so it can
sit alongside other repositories without their names colliding. The short name
works whenever it is unambiguous.

## Writing a job

    je new my-job --script

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

`+"`je new --script`"+` writes a script with all of it in place.
`, title(name), name, name)
}

// title is titleFromSlug for a repository name, which may not be a slug.
func title(name string) string {
	if name == "" || name == "." {
		return "jobs"
	}
	return titleFromSlug(name)
}
