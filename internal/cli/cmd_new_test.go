package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jdmorlan/job-engine/internal/jobdef"
)

// loadJobsDir is the check that matters for everything this command writes: the
// engine loads a whole source atomically (D19), so a generated file that does
// not parse does not just fail itself -- it stops every other job loading too.
func loadJobsDir(t *testing.T, env *Env) jobdef.Snapshot {
	t.Helper()
	// Whatever directory `je new` wrote into, which is the one it was run in.
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	snap, err := jobdef.FSSource{FS: os.DirFS(dir), Root: "."}.Load(context.Background())
	if err != nil {
		t.Fatalf("what je new wrote does not load: %v", err)
	}
	return snap
}

func TestNewWritesAJobThatLoadsAndRuns(t *testing.T) {
	env, out := demoEnv(t)
	if err := runNew(context.Background(), env, []string{"weather-ingest"}); err != nil {
		t.Fatalf("je new: %v", err)
	}

	snap := loadJobsDir(t, env)
	if len(snap.Definitions) != 1 {
		t.Fatalf("definitions = %d", len(snap.Definitions))
	}
	def := snap.Definitions[0]
	if def.Slug != "weather-ingest" {
		t.Errorf("slug = %q", def.Slug)
	}
	if def.DisplayName != "Weather Ingest" {
		t.Errorf("name = %q, want a title worth keeping", def.DisplayName)
	}
	// A placeholder command rather than a TODO: the point is that `je run`
	// works the moment the file exists.
	if len(def.Command) == 0 {
		t.Error("the generated job has no command, so it will not load")
	}
	if got := def.ConfigError(); got != "" {
		t.Errorf("the generated job cannot run as written: %s", got)
	}

	// P3: the file holds only what was asked for. Anything else in here is a
	// default restated, which is what job files exist not to be.
	body, err := os.ReadFile(filepath.Join(cwd(t), "weather-ingest.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{"timeout:", "overlap:", "runtime:", "on_interrupt:", "state:"} {
		if strings.Contains(string(body), unwanted) {
			t.Errorf("the template restates the default %s:\n%s", unwanted, body)
		}
	}
	if !strings.Contains(out.String(), "je explain weather-ingest") {
		t.Error("nothing pointed at je explain, which is where the defaults are")
	}
}

func TestNewScriptWritesAWorkingProtocolExample(t *testing.T) {
	env, _ := demoEnv(t)
	if err := runNew(context.Background(), env, []string{"nightly", "--script"}); err != nil {
		t.Fatal(err)
	}

	snap := loadJobsDir(t, env)
	def := snap.Definitions[0]
	if got := def.CommandLine(); !strings.Contains(got, "scripts/nightly.sh") {
		t.Errorf("command = %q, want it pointing at the script that was written", got)
	}

	path := filepath.Join(cwd(t), "scripts", "nightly.sh")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the script was not written: %v", err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Error("the script is not executable, so running it by hand does not work")
	}

	// The template is the documentation for D6, so every channel has to be in
	// it -- a job author has nothing else to read.
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, channel := range []string{
		"JE_STATE", "JOB_STATE_OUT_FILE", "JOB_OUTPUT_FILE", "JOB_EVENTS_FILE",
		"RUN_ID", "TRIGGERED_BY",
	} {
		if !strings.Contains(string(body), channel) {
			t.Errorf("the script template never mentions %s", channel)
		}
	}
}

func TestNewChainWritesAFileThatDoesNotBreakTheSync(t *testing.T) {
	env, _ := demoEnv(t)
	if err := runNew(context.Background(), env, []string{"daily", "--chain"}); err != nil {
		t.Fatal(err)
	}

	// The whole risk of writing a chain file for somebody: it has no steps
	// yet, sync is atomic, and a rejected chain file takes every job in the
	// source down with it.
	snap := loadJobsDir(t, env)
	if len(snap.Chains) != 1 || snap.Chains[0].Name != "daily" {
		t.Fatalf("chains = %+v", snap.Chains)
	}
	if len(snap.Chains[0].Steps) != 0 {
		t.Errorf("the generated chain wires something: %+v", snap.Chains[0].Steps)
	}
}

func TestNewRefusesToClobberOrToWriteSomethingBroken(t *testing.T) {
	env, _ := demoEnv(t)
	if err := runNew(context.Background(), env, []string{"taken"}); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"an existing job", []string{"taken"}, "already exists"},
		{"a name that is not a slug", []string{"Weather Ingest"}, "lowercase"},
		{"two schedules", []string{"x", "--every", "15m", "--cron", "0 3 * * *"}, "pick one"},
		{"a cron that does not parse", []string{"x", "--cron", "not a cron"}, "--cron"},
		{"a duration that does not parse", []string{"x", "--every", "fortnightly"}, "--every"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runNew(context.Background(), env, tc.args)
			if err == nil {
				t.Fatal("no error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
			// A rejected invocation must leave nothing behind, or the second
			// attempt fails with "already exists" about a file that was never
			// usable.
			if _, statErr := os.Stat(filepath.Join(cwd(t), "x.yaml")); statErr == nil {
				t.Error("a rejected je new left a file behind")
			}
		})
	}
}

// cwd is where `je new` wrote, which is where the test is standing.
func cwd(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
