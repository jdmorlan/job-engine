package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Recoverable means committed AND pushed, and every weaker state has to fail.
//
// The states matter because something deletes a directory on the strength of
// this answer. A check that stopped at "is it a git repository" would call an
// unpushed repository safe, which is the most expensive way to be wrong: it
// looks careful and it loses the work anyway.
func TestDefinitionsAreOnlyRecoverableWhenPushed(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}

	dir := t.TempDir()
	jobs := filepath.Join(dir, "jobs")
	if err := os.MkdirAll(jobs, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(jobs, name),
			[]byte("command: [\"/bin/sh\", \"-c\", \"true\"]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git := func(args ...string) {
		t.Helper()
		if out, err := runGit(jobs, args...); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}

	// An empty directory has nothing to lose, so it is not an obstacle.
	if got := definitionsRecoverable(jobs); !got.recoverable {
		t.Errorf("an empty directory was treated as unrecoverable: %s", got.why)
	}

	write("a.yaml")
	if got := definitionsRecoverable(jobs); got.recoverable {
		t.Error("a plain directory of job files was called recoverable")
	}

	git("init", "--quiet")
	git("config", "user.email", "test@example.com")
	git("config", "user.name", "Test")
	if got := definitionsRecoverable(jobs); got.recoverable {
		t.Error("a repository with uncommitted files was called recoverable")
	}

	git("add", "-A")
	git("commit", "--quiet", "-m", "jobs")
	if got := definitionsRecoverable(jobs); got.recoverable {
		t.Error("a committed repository with no remote was called recoverable")
	}

	remote := filepath.Join(dir, "remote.git")
	if out, err := exec.Command("git", "init", "--quiet", "--bare", remote).CombinedOutput(); err != nil {
		t.Fatalf("creating the remote: %s", out)
	}
	git("remote", "add", "origin", remote)
	if got := definitionsRecoverable(jobs); got.recoverable {
		t.Error("a repository with a remote it has not pushed to was called recoverable")
	}

	git("push", "--quiet", "--set-upstream", "origin", "HEAD")
	if got := definitionsRecoverable(jobs); !got.recoverable {
		t.Errorf("a pushed repository was not recoverable: %s", got.why)
	}

	// And one commit past the remote makes it unrecoverable again, which is
	// the case a "does it have a remote" check would miss.
	write("b.yaml")
	git("add", "-A")
	git("commit", "--quiet", "-m", "more")
	if got := definitionsRecoverable(jobs); got.recoverable {
		t.Error("a repository with unpushed commits was called recoverable")
	}
}
