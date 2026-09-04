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

// A job declaring a language whose project files are not there is a definition
// problem, and says so rather than running the command and failing oddly.
func TestAMissingManifestIsADefinitionProblem(t *testing.T) {
	if _, err := exec.LookPath("pnpm"); err != nil {
		t.Skip("pnpm is not installed")
	}
	w := &Worker{
		opts:     Options{Name: "test"},
		prepared: newPrepared(),
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	_, err := w.prepare(context.Background(), "typescript", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "package.json") {
		t.Errorf("error = %v, want it to name the manifest that is missing", err)
	}
}
