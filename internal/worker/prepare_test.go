package worker

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A TypeScript job's dependencies are installed from its own lockfile, and the
// binaries that install produced are on PATH afterwards.
//
// This runs a real `pnpm install` against a real registry. That is slow and it
// is the point: the thing worth testing is whether an ecosystem's own frozen
// install works when driven this way, and a fake installer would test only that
// the code calls what it was told to call.
func TestATypeScriptTreeIsPreparedFromItsLockfile(t *testing.T) {
	if _, err := exec.LookPath("pnpm"); err != nil {
		t.Skip("pnpm is not installed")
	}

	tree := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(tree, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("package.json", `{
  "name": "jobs", "private": true,
  "dependencies": { "ms": "2.1.3" }
}`)
	// Resolved once, here, so the test exercises --frozen-lockfile rather than
	// a resolution -- which is what a job's tree would arrive with.
	lock := exec.Command("pnpm", "install", "--lockfile-only")
	lock.Dir = tree
	if out, err := lock.CombinedOutput(); err != nil {
		t.Skipf("could not resolve a lockfile (no network?): %s", out)
	}

	w := &Worker{
		opts:     Options{Name: "test"},
		prepared: newPrepared(),
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	binDir, err := w.prepare(context.Background(), "typescript", tree)
	if err != nil {
		t.Fatalf("preparing: %v", err)
	}
	if want := filepath.Join(tree, "node_modules", ".bin"); binDir != want {
		t.Errorf("bin dir = %q, want %q", binDir, want)
	}
	if _, err := os.Stat(filepath.Join(tree, "node_modules", "ms")); err != nil {
		t.Fatalf("the declared dependency is not installed: %v", err)
	}

	// Preparing again is free: the marker records the install, so a second job
	// from the same tree does not reinstall.
	marker := filepath.Join(tree, prepareMarker+"-typescript")
	before, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("no marker was written: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(tree, "node_modules", "ms")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.prepare(context.Background(), "typescript", tree); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(tree, "node_modules", "ms")); err == nil {
		t.Error("a prepared tree was reinstalled; the marker did not short-circuit")
	}

	// A changed lockfile changes the key, so the next prepare does install.
	write("pnpm-lock.yaml", strings.TrimSpace(string(before))+"\n# changed\n")
	if _, err := w.prepare(context.Background(), "typescript", tree); err == nil {
		// The install will fail because the lockfile is now nonsense, which is
		// itself the proof that the key noticed: a marker that ignored the
		// lockfile would have short-circuited and returned nil.
		t.Error("a changed lockfile did not invalidate the prepared marker")
	}
}

// A language nothing on this machine can prepare fails before the job starts,
// and says which worker and what to run.
func TestAMissingToolchainIsNamedNotGuessed(t *testing.T) {
	w := &Worker{
		opts:     Options{Name: "buildbox"},
		prepared: newPrepared(),
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "pyproject.toml"), []byte("[project]\nname='x'\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := exec.LookPath("uv"); err == nil {
		t.Skip("uv is installed, so this machine cannot exercise the missing case")
	}
	_, err := w.prepare(context.Background(), "python", tree)
	if err == nil {
		t.Fatal("a job needing a toolchain this worker lacks was prepared anyway")
	}
	for _, want := range []string{"python", "uv", "buildbox", "je worker runtime install"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%s", want, err)
		}
	}
}

// A tree with no manifest has no dependencies, which is an ordinary job: a
// single Python file, a script with nothing to install.
//
// D28 made this a hard error, and that was right while `language:` meant only
// "install my dependencies". D21's shim keys off the same field, so it now
// means "this job is written in X" -- and demanding a package.json whose only
// purpose is to satisfy the check would be the tail wagging the dog.
func TestATreeWithNoManifestHasNothingToInstall(t *testing.T) {
	if _, err := exec.LookPath("pnpm"); err != nil {
		t.Skip("pnpm is not installed")
	}
	w := &Worker{
		opts:     Options{Name: "test"},
		prepared: newPrepared(),
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	binDir, err := w.prepare(context.Background(), "typescript", t.TempDir())
	if err != nil {
		t.Fatalf("a tree with nothing to install was refused: %v", err)
	}
	if binDir != "" {
		t.Errorf("binDir = %q for a tree with no dependencies", binDir)
	}
}

// The protection D28 wanted, kept where it applies. A tree with a manifest is a
// project, and a project that cannot be installed from its lockfile is still an
// error -- a lockfile somebody forgot to commit is exactly the case that error
// was written for, and that tree has a manifest.
func TestAProjectThatCannotBeInstalledIsStillAnError(t *testing.T) {
	if _, err := exec.LookPath("pnpm"); err != nil {
		t.Skip("pnpm is not installed")
	}
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "package.json"),
		[]byte(`{"name":"x","version":"0.0.0","dependencies":{"left-pad":"1.3.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	w := &Worker{
		opts:     Options{Name: "test"},
		prepared: newPrepared(),
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if _, err := w.prepare(context.Background(), "typescript", tree); err == nil {
		t.Error("a project with a manifest and no lockfile was prepared anyway")
	}
}
