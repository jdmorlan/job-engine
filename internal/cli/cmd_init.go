package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
)

func init() {
	register(&Command{
		Name:  "init",
		Args:  "[directory]",
		Usage: "set up a new jobs repository",
		Long: "Creates the tree a source expects -- job files at the top, chains in\n" +
			"chains/, the code they run in scripts/ -- plus a README describing the\n" +
			"layout and a .gitignore, and initialises a git repository.\n\n" +
			"The repository is the point, not a nicety. A source is a whole tree\n" +
			"and not a pile of YAML (D22): the scripts a job runs live beside it\n" +
			"and travel to the worker with it, secrets are encrypted into it and\n" +
			"granted to named machines (D25), and a change to either is a diff\n" +
			"somebody reviews (D23). A directory of job files that exists only on\n" +
			"one laptop gets none of that.\n\n" +
			"--no-git skips it, for a tree you are going to put inside an existing\n" +
			"repository.\n\n" +
			"It registers nothing. Point the engine at it with\n" +
			"je source add <name> <directory>.",
		Run: runInit,
	})
}

func runInit(ctx context.Context, env *Env, args []string) error {
	fs := newFlagSet(commands["init"], env)
	noGit := fs.Bool("no-git", false, "do not initialise a git repository")
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

	versioned := ""
	if !*noGit {
		// Done rather than suggested. `git init` was in the next-steps list and
		// that was the wrong shape for it: everything this project wants a
		// source to be -- travelling to a worker, carrying encrypted secrets,
		// changing by reviewed diff -- assumes a repository, so a command
		// called `init` that leaves you without one has not finished.
		var err error
		if versioned, err = initRepository(abs, written); err != nil {
			fmt.Fprintf(env.Stderr, "\nthe files are written, but git did not run: %v\n", err)
		}
	}

	// Not a tabwriter: one of these lines carries a path, and a single long
	// path turns an aligned table into a scroll bar.
	fmt.Fprintf(env.Stdout, `
next
  cd %s
  je new <job> --script        writes into this repository
  je source add %s .   register it with the engine
`, dir, name)
	if versioned != "" {
		fmt.Fprintf(env.Stdout, "\n%s\n", versioned)
	}

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

// initRepository makes the tree a git repository and commits what was written.
//
// Committing as well as initialising, because an empty repository is not much
// better than none: the files this command just wrote are the ones a person
// would want as a first commit, and leaving them staged-or-not is a state
// somebody has to resolve before doing anything else.
//
// An existing repository is left entirely alone -- `je init` inside one is how
// you add a jobs tree to a repo you already have, and committing on somebody
// else's behalf there would be a surprise.
func initRepository(dir string, written []string) (string, error) {
	if root, err := gitRoot(dir); err == nil {
		return "already versioned: " + root, nil
	}
	if _, err := exec.LookPath("git"); err != nil {
		return "", fmt.Errorf("git is not installed")
	}
	if out, err := runGit(dir, "init", "--quiet"); err != nil {
		return "", fmt.Errorf("git init: %s", strings.TrimSpace(out))
	}
	if len(written) == 0 {
		return "initialised a git repository", nil
	}
	if out, err := runGit(dir, "add", "-A"); err != nil {
		return "", fmt.Errorf("git add: %s", strings.TrimSpace(out))
	}
	if out, err := runGit(dir, "commit", "--quiet", "-m", "Jobs repository"); err != nil {
		// A machine with no configured git identity cannot commit, which is
		// not this command failing -- the tree is there and versioned.
		return "initialised a git repository; nothing committed yet (" +
			strings.TrimSpace(lastLine(out)) + ")", nil
	}
	return "initialised a git repository and committed the tree.\n" +
		"Push it somewhere when you have a remote -- until then this disk is the only copy.", nil
}
