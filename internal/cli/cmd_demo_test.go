package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jdmorlan/job-engine/internal/jobdef"
	"github.com/jdmorlan/job-engine/internal/paths"
)

// demoDir is the examples as they sit in this repository, which is the whole
// reason they live here rather than in a repository of their own: keeping them
// under this CI keeps proving they parse.
const demoDir = "../../demo"

// TestDemoJobsAreValid is the test that matters most here. The examples are the
// first thing anybody runs, and an example that does not load is worse than no
// example at all -- it teaches that the tool is broken.
//
// It reads the directory directly now. `je demo` registers a source over the
// network, which a unit test must not do; the files are right here, and what
// needs proving is that they are valid, not that HTTP works.
func TestDemoJobsAreValid(t *testing.T) {
	src := jobdef.FSSource{FS: os.DirFS(demoDir), Root: "."}
	snap, err := src.Load(context.Background())
	if err != nil {
		t.Fatalf("the example jobs do not load: %v", err)
	}

	want := map[string]bool{
		"demo-hello": false, "demo-counter": false,
		"demo-flaky": false, "demo-tick": false,
		"demo-ingest": false, "demo-report": false, "demo-archive": false,
	}
	for _, def := range snap.Definitions {
		if _, ok := want[def.Slug]; !ok {
			t.Errorf("unexpected example job %q", def.Slug)
			continue
		}
		want[def.Slug] = true

		// Every example must be runnable as written, with nothing installed and
		// nothing configured.
		if got := def.ConfigError(); got != "" {
			t.Errorf("%s is not runnable out of the box: %s", def.Slug, got)
		}
		if len(def.Command) == 0 {
			t.Errorf("%s has no command", def.Slug)
		}
	}
	for slug, found := range want {
		if !found {
			t.Errorf("example %s is missing", slug)
		}
	}

	// The chain is loaded by the same source as the jobs, so this also pins
	// that its steps name jobs that exist and that it does not close a loop --
	// both load errors, and both of which the examples would otherwise be the
	// first place anybody hit.
	if len(snap.Chains) != 1 || snap.Chains[0].Name != "demo-pipeline" {
		t.Fatalf("chains = %+v, want the demo-pipeline example", snap.Chains)
	}
	if len(snap.Chains[0].Steps) != 2 {
		t.Errorf("demo-pipeline has %d steps, want 2", len(snap.Chains[0].Steps))
	}
}

func TestDemoScriptsAreExecutableAndPresent(t *testing.T) {
	for _, name := range []string{"counter.sh", "flaky.sh", "ingest.sh"} {
		path := filepath.Join(demoDir, "scripts", name)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("%s is missing: %v", name, err)
		}
		// Committed executable, because the tarball preserves the mode and a
		// script that arrives without it fails as "permission denied" on
		// somebody else's machine.
		if info.Mode().Perm()&0o100 == 0 {
			t.Errorf("%s is not executable, so it will not run once fetched", name)
		}
	}
}

// The examples must not need a secret. A first run that asks for credentials is
// not a demo, and D10 would correctly refuse to schedule the job.
func TestDemoNeedsNoSecrets(t *testing.T) {
	src := jobdef.FSSource{FS: os.DirFS(demoDir), Root: "."}
	snap, err := src.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, def := range snap.Definitions {
		if len(def.Secrets) > 0 {
			t.Errorf("%s declares %v; the examples must run with nothing configured",
				def.Slug, def.Secrets)
		}
	}
}

func TestTheDemoIsARegisteredSubpathNotEmbeddedFiles(t *testing.T) {
	if !strings.HasPrefix(DemoRepo, "github.com/") {
		t.Errorf("DemoRepo = %q, want a repository the engine can fetch", DemoRepo)
	}
	if DemoPath == "" {
		t.Error("the demo is registered as a subpath; that is what exercises --path")
	}
	if DemoSource == "" {
		t.Fatal("the demo source needs a name; it is the prefix on every example job")
	}
}

// A release pins the examples to its own tag, so a v0.4 binary cannot be handed
// examples written for v0.6. A dev build has no tag and tracks the default
// branch, which D22 asks the repository for rather than assuming is "main".
func TestTheDemoPinsToTheBinarysVersion(t *testing.T) {
	if got := demoRef("v0.4.2"); got != "v0.4.2" {
		t.Errorf("demoRef(v0.4.2) = %q, want the examples pinned to that release", got)
	}
	for _, version := range []string{"dev", ""} {
		if got := demoRef(version); got != "" {
			t.Errorf("demoRef(%q) = %q, want the default branch asked for rather than assumed",
				version, got)
		}
	}
}

// The tour has to name jobs the way they will actually be named once they
// arrive from a source -- qualified (D22). An unqualified name that happens to
// resolve today teaches the wrong thing and breaks as soon as a second source
// has a job with the same slug.
func TestTheTourUsesQualifiedNames(t *testing.T) {
	env, out := demoEnv(t)
	printDemoTour(env)

	tour := out.String()
	for _, want := range []string{
		"je run " + DemoSource + "/demo-hello",
		"je run " + DemoSource + "/demo-ingest",
		"je source",
		"je quickstart", // nothing below it works until the engine is running
	} {
		if !strings.Contains(tour, want) {
			t.Errorf("the tour does not mention %q", want)
		}
	}
	if strings.Contains(tour, "je run demo-hello\n") {
		t.Error("the tour still uses an unqualified job name")
	}
}

// demoEnv is shared with the other CLI tests, which is why it outlived the
// demo's own file-copying tests.
func demoEnv(t *testing.T) (*Env, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	// `je new` writes into the directory you are standing in, because that is
	// where a jobs repository is. The engine owns no directory to write into.
	t.Chdir(t.TempDir())
	out := &bytes.Buffer{}
	return &Env{
		Stdout: out,
		Stderr: &bytes.Buffer{},
		Stdin:  strings.NewReader(""),
		Layout: paths.Layout{Data: dir},
	}, out
}
