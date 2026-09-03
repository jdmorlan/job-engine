package cli

import (
	"context"
	"fmt"
	"text/tabwriter"

	"github.com/jdmorlan/job-engine/internal/store"

	"github.com/jdmorlan/job-engine/internal/api"
)

// DemoRepo and DemoPath are where the examples live (D22).
//
// A directory in this repository, registered as a *subpath* -- not a repository
// of its own. Three things fall out of that, and the third is the reason:
//
//   - The examples ship with the engine, so they cannot reference something the
//     binary does not have. Registered at the binary's own version tag, a v0.4
//     `je` gets the v0.4 examples.
//   - They stay under this repository's CI, which keeps proving they parse.
//     Moving them out would have quietly dropped that.
//   - It exercises `--path`. One repository holding `python-jobs/` and
//     `typescript-jobs/`, registered as two sources, is a real thing to want --
//     and until now nothing used it. Having the first command anybody runs
//     depend on it is the cheapest possible test.
//
// A repository rather than files copied onto disk, because copying exercised
// the scratch loop instead of the thing the engine is for. The cost is the
// network, which is the same network the binary was downloaded over.
const (
	DemoRepo = "github.com/jdmorlan/job-engine"
	DemoPath = "demo"
)

// DemoSource is what the registered source is called, and therefore the prefix
// on every example job's name: demo/demo-hello.
const DemoSource = "demo"

func init() {
	register(&Command{
		Name:  "demo",
		Usage: "register a handful of example jobs and a chain, to get a feel for the engine",
		Long: "Registers " + DemoRepo + " as a source named \"" + DemoSource + "\".\n\n" +
			"An ordinary repository of ordinary job files -- nothing built into the\n" +
			"binary, nothing special-cased. Read them, fork them, break them, and\n" +
			"unregister them with --remove when you are done.\n\n" +
			"A repository rather than files copied onto your disk, because that is\n" +
			"what a jobs repository actually is (D22): the examples arrive the way\n" +
			"your own jobs would, pinned to a commit and named for their source.\n" +
			"An example that appeared from nowhere would teach the wrong model on\n" +
			"the first thing you touched.\n\n" +
			"This one needs the network -- the same network the binary came over.",
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
	{"demo-tick", "runs every minute", "gives the scheduler something to do"},
	{"demo-ingest", "emits an event when it finishes", "the start of a chain"},
	{"demo-report", "runs because that event happened", "no clock, no polling"},
	{"demo-archive", "runs after the report succeeds", "and not at all if it doesn't"},
}

// demoChains is listed separately because a chain is a different noun: it runs
// nothing itself, it says what runs after what.
var demoChains = []struct {
	name string
	what string
}{
	{"demo-pipeline", "wires the three above into one named flow"},
}

func runDemo(ctx context.Context, env *Env, args []string) error {
	cmd := commands["demo"]
	flags := newFlagSet(cmd, env)
	remove := flags.Bool("remove", false, "unregister the examples again")
	if extra, err := parseArgs(flags, args); err != nil {
		return err
	} else if len(extra) > 0 {
		return usagef("unexpected argument %q", extra[0])
	}

	return withClient(ctx, env, func(ctx context.Context, c *Client) error {
		if *remove {
			return removeDemo(ctx, env, c)
		}
		return addDemo(ctx, env, c)
	})
}

func addDemo(ctx context.Context, env *Env, c *Client) error {
	addCtx, cancel := withTimeout(ctx)
	defer cancel()

	// Pinned to the tag of the binary doing the registering, so the examples
	// always match the engine. A dev build has no tag and tracks the default
	// branch instead -- the same fallback `je web start` makes for an image
	// that was never published.
	result, err := c.AddSource(addCtx, api.AddSourceRequest{
		Name:     DemoSource,
		Kind:     store.SourceKindGitHub,
		Location: DemoRepo,
		Subpath:  DemoPath,
		Ref:      demoRef(env.Version),
	})
	if err != nil {
		return fmt.Errorf("registering the examples: %w", err)
	}

	fmt.Fprintf(env.Stdout, "registered %s/%s as %q", DemoRepo, DemoPath, DemoSource)
	if result.Revision != "" {
		fmt.Fprintf(env.Stdout, " at %s", shortRevision(result.Revision))
	}
	fmt.Fprintf(env.Stdout, "\n\n")

	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	for _, j := range demoJobs {
		fmt.Fprintf(tw, "  %s/%s\t%s\t%s\n", DemoSource, j.slug, j.what, j.why)
	}
	tw.Flush()

	// Chains under their own heading rather than in the same table. A chain
	// runs nothing itself, and a list that mixes the two would suggest it is
	// just another job.
	fmt.Fprintln(env.Stdout, "\nand one chain, which runs nothing itself:")
	tw = tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	for _, ch := range demoChains {
		fmt.Fprintf(tw, "  %s/%s\t%s\n", DemoSource, ch.name, ch.what)
	}
	tw.Flush()

	printDemoTour(env)
	return nil
}

func removeDemo(ctx context.Context, env *Env, c *Client) error {
	rmCtx, cancel := withTimeout(ctx)
	defer cancel()

	tombstoned, err := c.RemoveSource(rmCtx, DemoSource)
	if err != nil {
		return fmt.Errorf("unregistering the examples: %w", err)
	}
	// Tombstoned, not deleted: the runs they produced keep a readable name
	// (D22). Saying so beats implying the history went with them.
	fmt.Fprintf(env.Stdout,
		"unregistered %q; %d job(s) will no longer run.\n"+
			"Their history is still there -- `je runs` keeps naming them.\n",
		DemoSource, tombstoned)
	return nil
}

func shortRevision(rev string) string {
	if len(rev) > 8 {
		return rev[:8]
	}
	return rev
}

// printDemoTour is the half that matters as much as the files. Somebody who
// just ran this does not yet know which command shows the thing the examples
// were written to show, and making them go looking is how onboarding fails.
//
// Starting the engine comes first. Under D20 the control plane does not
// execute, so every command below needs something running -- and a tour whose
// first step fails with "no control plane" would teach the wrong first lesson
// about a tool whose whole claim is that it explains itself.
//
// The names are qualified now (demo/demo-hello), because the examples arrive
// from a registered source. That is D22 working, and the tour should show it
// rather than hide it behind an unqualified name that happens to resolve.
func printDemoTour(env *Env) {
	fmt.Fprint(env.Stdout, `
First, start the engine. It is two components -- a control plane that decides
what runs, and a worker that runs it -- so this starts both, in this terminal:

  je quickstart

Then, in another terminal, try this in order:

  je run demo/demo-hello         the loop: a command, its output, an exit code
  je run demo/demo-counter       then run it again, and watch the cursor move
  je run demo/demo-flaky         run it a few times; some fail, some do not
  je state history demo/demo-flaky   the cursor moved only on the runs that worked
  je runs                        what happened, and when
  je waiting                     what has not happened yet
  je workers                     what is attached, and what it can run
  je source                      where these came from, and at which commit

Leave it alone for a couple of minutes, then:

  je runs demo/demo-tick         the scheduler fired it without you

Then the part other job engines do not have:

  je run demo/demo-ingest        one command, and watch what follows it
  je chains                      the flows, and how the last pass went
  je chain demo-pipeline         every step, and how long the whole thing took
  je events                      the same story as raw events, with causation

Remove all of it with: je demo --remove
`)
}

// demoRef pins the examples to this binary's release, or tracks the default
// branch when there is no release to pin to.
//
// Empty rather than "main": D22 asks the repository what its own default branch
// is rather than assuming, and that answer should not be second-guessed here.
func demoRef(version string) string {
	if version == "" || version == "dev" {
		return ""
	}
	return version
}
