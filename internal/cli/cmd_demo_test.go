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

func demoEnv(t *testing.T) (*Env, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	out := &bytes.Buffer{}
	return &Env{
		Stdout: out,
		Stderr: &bytes.Buffer{},
		Stdin:  strings.NewReader(""),
		Layout: paths.Layout{Data: dir, Jobs: filepath.Join(dir, "jobs")},
	}, out
}

// TestDemoJobsAreValid is the test that matters most here. The examples are
// the first thing anybody runs, and an example that does not load is worse
// than no example at all -- it teaches that the tool is broken.
func TestDemoJobsAreValid(t *testing.T) {
	env, _ := demoEnv(t)
	if err := runDemo(context.Background(), env, nil); err != nil {
		t.Fatalf("je demo: %v", err)
	}

	src := jobdef.FSSource{FS: os.DirFS(env.Layout.Jobs), Root: "."}
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

		// Every example must be runnable as written, with nothing installed
		// and nothing configured.
		if got := def.ConfigError(); got != "" {
			t.Errorf("%s is not runnable out of the box: %s", def.Slug, got)
		}
		if len(def.Command) == 0 {
			t.Errorf("%s has no command", def.Slug)
		}
	}
	for slug, found := range want {
		if !found {
			t.Errorf("example %s was not written", slug)
		}
	}

	// The chain is loaded by the same source as the jobs, so this also pins
	// that its steps name jobs that exist and that it does not close a loop --
	// both of which are load errors, and both of which the examples would
	// otherwise be the first place anybody hit.
	if len(snap.Chains) != 1 || snap.Chains[0].Name != "demo-pipeline" {
		t.Fatalf("chains = %+v, want the demo-pipeline example", snap.Chains)
	}
	if len(snap.Chains[0].Steps) != 2 {
		t.Errorf("demo-pipeline has %d steps, want 2", len(snap.Chains[0].Steps))
	}
}

func TestDemoScriptsAreExecutableAndPresent(t *testing.T) {
	env, _ := demoEnv(t)
	if err := runDemo(context.Background(), env, nil); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"counter.sh", "flaky.sh", "ingest.sh"} {
		path := filepath.Join(env.Layout.Jobs, "scripts", name)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("%s was not written: %v", name, err)
		}
		if info.Mode().Perm()&0o100 == 0 {
			t.Errorf("%s is not executable, so reading and running it by hand does not work", name)
		}
	}
}

// TestDemoDoesNotClobber protects the thing that makes these files useful:
// once written they are yours, and editing one must not be undone by running
// the command again.
func TestDemoDoesNotClobber(t *testing.T) {
	env, out := demoEnv(t)
	if err := runDemo(context.Background(), env, nil); err != nil {
		t.Fatal(err)
	}

	edited := filepath.Join(env.Layout.Jobs, "demo-hello.yaml")
	mine := "command: [\"/bin/sh\", \"-c\", \"echo mine\"]\n"
	if err := os.WriteFile(edited, []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	if err := runDemo(context.Background(), env, nil); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(edited)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != mine {
		t.Error("running je demo twice overwrote an edited example")
	}
}

func TestDemoRemoveLeavesOtherFilesAlone(t *testing.T) {
	env, _ := demoEnv(t)
	if err := runDemo(context.Background(), env, nil); err != nil {
		t.Fatal(err)
	}

	// A job of the user's own, which removal must not touch.
	own := filepath.Join(env.Layout.Jobs, "my-job.yaml")
	if err := os.WriteFile(own, []byte("command: [\"true\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := runDemo(context.Background(), env, []string{"--remove"}); err != nil {
		t.Fatalf("je demo --remove: %v", err)
	}

	if _, err := os.Stat(own); err != nil {
		t.Errorf("--remove deleted a job it did not write: %v", err)
	}
	if _, err := os.Stat(filepath.Join(env.Layout.Jobs, "demo-hello.yaml")); !os.IsNotExist(err) {
		t.Error("--remove left an example behind")
	}
	if _, err := os.Stat(filepath.Join(env.Layout.Chains(), "demo-pipeline.yaml")); !os.IsNotExist(err) {
		t.Error("--remove left the example chain behind")
	}
}
