package cli

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
)

//go:embed demo
var demoFiles embed.FS

func init() {
	register(&Command{
		Name:  "demo",
		Usage: "write four example jobs to get a feel for the engine",
		Long: "Writes ordinary job files into your jobs directory -- nothing built into\n" +
			"the binary, nothing special-cased. Read them, change them, break them,\n" +
			"and delete them with --remove when you are done.\n\n" +
			"The examples are files rather than something the engine carries because\n" +
			"a job IS a file. An example that appeared from nowhere would teach the\n" +
			"wrong model on the first thing you touched.",
		Run: runDemo,
	})
}

// demoJobs describes what each example is for, so the command can explain
// itself rather than just listing filenames.
var demoJobs = []struct {
	slug string
	what string
	why  string
}{
	{"demo-hello", "prints a line and exits", "the smallest job that exists"},
	{"demo-counter", "keeps a cursor and advances it", "what other schedulers leave to you"},
	{"demo-flaky", "fails about one run in three", "watch the cursor NOT move"},
	{"demo-tick", "runs every minute", "gives the daemon something to do"},
}

func runDemo(ctx context.Context, env *Env, args []string) error {
	cmd := commands["demo"]
	flags := newFlagSet(cmd, env)
	remove := flags.Bool("remove", false, "delete the example jobs again")
	force := flags.Bool("force", false, "overwrite example files that already exist")
	if extra, err := parseArgs(flags, args); err != nil {
		return err
	} else if len(extra) > 0 {
		return usagef("unexpected argument %q", extra[0])
	}

	if *remove {
		return removeDemo(env)
	}
	return writeDemo(env, *force)
}

func writeDemo(env *Env, force bool) error {
	if err := os.MkdirAll(env.Layout.Jobs, 0o755); err != nil {
		return fmt.Errorf("creating jobs directory: %w", err)
	}

	written, skipped, err := copyDemoTree(env.Layout.Jobs, force)
	if err != nil {
		return err
	}

	if len(written) == 0 && len(skipped) > 0 {
		fmt.Fprintf(env.Stdout,
			"the examples are already in %s\n  overwrite them with: je demo --force\n",
			env.Layout.Jobs)
		return nil
	}

	fmt.Fprintf(env.Stdout, "wrote %d files to %s\n\n", len(written), env.Layout.Jobs)

	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	for _, j := range demoJobs {
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", j.slug, j.what, j.why)
	}
	tw.Flush()

	// The tour matters as much as the files. Somebody who just ran this does
	// not yet know which command shows the thing the examples were written to
	// show, and making them go looking is how onboarding fails.
	fmt.Fprint(env.Stdout, `
Try this, in order:

  je run demo-hello              the loop: a command, its output, an exit code
  je run demo-counter            then run it again, and watch the cursor move
  je run demo-flaky              run it a few times; some fail, some do not
  je state history demo-flaky    the cursor moved only on the runs that worked
  je runs                        what happened, and when
  je waiting                     what has not happened yet

Then start the daemon and leave it alone for a couple of minutes:

  je serve &
  je waiting
  je runs demo-tick

Remove all of it with: je demo --remove
`)

	if len(skipped) > 0 {
		fmt.Fprintf(env.Stderr, "\nleft alone (already present): %s\n", strings.Join(skipped, ", "))
	}
	return nil
}

// copyDemoTree writes the embedded files out, preserving the scripts/
// subdirectory so the examples look like a real job repo (D22): definitions
// and the code they run, side by side.
func copyDemoTree(dest string, force bool) (written, skipped []string, err error) {
	err = fs.WalkDir(demoFiles, "demo", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel("demo", path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dest, rel)

		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}

		if _, statErr := os.Stat(target); statErr == nil && !force {
			// Never clobber something the user may have edited. The whole
			// point of these being files is that they are yours once written.
			skipped = append(skipped, rel)
			return nil
		}

		body, err := demoFiles.ReadFile(path)
		if err != nil {
			return err
		}
		// Scripts need the execute bit even though the job invokes them
		// through /bin/sh, so that reading and running them by hand works too.
		mode := os.FileMode(0o644)
		if strings.HasSuffix(rel, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(target, body, mode); err != nil {
			return err
		}
		written = append(written, rel)
		return nil
	})
	return written, skipped, err
}

func removeDemo(env *Env) error {
	var removed int
	err := fs.WalkDir(demoFiles, "demo", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel("demo", path)
		if err != nil {
			return err
		}
		// Only files the demo itself wrote. Anything else in the jobs
		// directory is the user's and is none of our business.
		if err := os.Remove(filepath.Join(env.Layout.Jobs, rel)); err == nil {
			removed++
		} else if !os.IsNotExist(err) {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}

	// The scripts directory goes only if the demo emptied it.
	_ = os.Remove(filepath.Join(env.Layout.Jobs, "scripts"))

	fmt.Fprintf(env.Stdout, "removed %d example files from %s\n", removed, env.Layout.Jobs)
	fmt.Fprintln(env.Stdout, "their run history and cursors are still there; `je runs` still works")
	return nil
}
