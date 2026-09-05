package engine_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jdmorlan/job-engine/internal/engine"
	"github.com/jdmorlan/job-engine/internal/model"
)

// `je dev` is the authoring loop, and the whole design is that it is not a
// separate one: the definitions come from a working copy and everything after
// that is the ordinary path.
//
// The version this replaced was a harness in the CLI that ran the job itself.
// It reimplemented dependency preparation, the environment, the executor call
// and the log sink -- and its environment had already drifted from the real one
// before the release that shipped it was a day old. These tests are mostly
// about the sameness.

// devRepo writes a working copy and returns its path.
func devRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range files {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestADevJobRunsThroughTheOrdinaryPath(t *testing.T) {
	ctx := context.Background()
	e := newEngine(t, tempLayout(t), nil)
	defer e.Close(ctx)
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}

	dir := devRepo(t, map[string]string{
		"water-plants/job.yaml": "command: [\"/bin/sh\", \"zones.sh\"]\n",
		"water-plants/zones.sh": "echo watering 2 zones\n",
	})
	if _, err := e.RegisterDev(ctx, dir); err != nil {
		t.Fatalf("RegisterDev: %v", err)
	}

	// The same helper every other run test uses: enqueue, claim, execute
	// elsewhere, report back. If a dev run needed a different one, it would not
	// be evidence about what happens when you push.
	result, err := runJob(t, e, "dev/water-plants", engine.RunOptions{Actor: "tester"})
	if err != nil {
		t.Fatalf("running a dev job: %v", err)
	}
	if result.Run.Status != model.StatusSucceeded {
		t.Fatalf("run is %s: %s", result.Run.Status, result.Run.Error)
	}

	// Real logs, which is most of why this exists rather than a harness that
	// prints to the terminal and forgets.
	lines, err := e.Logs(ctx, result.Run.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || !strings.Contains(lines[0].Line, "watering 2 zones") {
		t.Errorf("logs = %+v", lines)
	}
}

// A job runs in its own folder, which is what lets its command name the files
// beside it -- and it has to be true here for the same reason it is true on a
// worker, since it is the same code deciding.
func TestADevJobRunsInItsOwnFolder(t *testing.T) {
	ctx := context.Background()
	e := newEngine(t, tempLayout(t), nil)
	defer e.Close(ctx)
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}

	dir := devRepo(t, map[string]string{
		"ingest/job.yaml":    "command: [\"/bin/sh\", \"-c\", \"cat fixture.txt\"]\n",
		"ingest/fixture.txt": "beside the job\n",
	})
	if _, err := e.RegisterDev(ctx, dir); err != nil {
		t.Fatal(err)
	}
	result, err := runJob(t, e, "dev/ingest", engine.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Run.Status != model.StatusSucceeded {
		t.Fatalf("run is %s: %s", result.Run.Status, result.Run.Error)
	}
}

// The loop: an edit takes effect on the next command, with no commit and no
// sync. Nothing is cached, because a working copy's whole nature is that it
// changes between runs.
func TestReRegisteringPicksUpAnEdit(t *testing.T) {
	ctx := context.Background()
	e := newEngine(t, tempLayout(t), nil)
	defer e.Close(ctx)
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}

	dir := devRepo(t, map[string]string{
		"tick/job.yaml": "command: [\"echo\", \"first\"]\n",
	})
	if _, err := e.RegisterDev(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tick", "job.yaml"),
		[]byte("command: [\"echo\", \"second\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RegisterDev(ctx, dir); err != nil {
		t.Fatal(err)
	}

	def, _, err := e.Definition(ctx, "dev/tick")
	if err != nil {
		t.Fatal(err)
	}
	if got := def.CommandLine(); !strings.Contains(got, "second") {
		t.Errorf("command = %q, want the edited one", got)
	}
}

// The limitation D27 named, stated rather than discovered: this needs a control
// plane that can read the directory, which is one on your own machine.
func TestRegisteringADirectoryTheControlPlaneCannotSeeIsRefused(t *testing.T) {
	ctx := context.Background()
	e := newEngine(t, tempLayout(t), nil)
	defer e.Close(ctx)
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	_, err := e.RegisterDev(ctx, filepath.Join(t.TempDir(), "nowhere"))
	if err == nil {
		t.Fatal("a directory that does not exist was accepted")
	}
	if !strings.Contains(err.Error(), "je up") {
		t.Errorf("the refusal does not name the fix: %v", err)
	}
}

// A whole source loads atomically (D19), and reproducing that is the point: the
// failure you get while writing should be the failure you would get on push.
func TestOneBadFileStopsTheWholeDirectoryLoading(t *testing.T) {
	ctx := context.Background()
	e := newEngine(t, tempLayout(t), nil)
	defer e.Close(ctx)
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}

	dir := devRepo(t, map[string]string{
		"good/job.yaml": "command: [\"true\"]\n",
		"bad/job.yaml":  "overlap: sometimes\ncommand: [\"true\"]\n",
	})
	if _, err := e.RegisterDev(ctx, dir); err == nil {
		t.Fatal("a directory with an unloadable job was accepted")
	}
}

// Its runs are its own. A job you are writing must not be able to move the
// cursor of the same job served from a repository, and the name is what keeps
// them apart.
func TestDevJobsAreNamedForTheDevSource(t *testing.T) {
	ctx := context.Background()
	e := newEngine(t, tempLayout(t), nil)
	defer e.Close(ctx)
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}

	dir := devRepo(t, map[string]string{"sync/job.yaml": "command: [\"true\"]\n"})
	if _, err := e.RegisterDev(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Definition(ctx, "dev/sync"); err != nil {
		t.Errorf("dev/sync did not load: %v", err)
	}
}
