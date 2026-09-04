package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jdmorlan/job-engine/internal/toolchain"
)

// Preparing a source tree so a job's language has what it needs (D28).
//
// The shape is: find the manifest, install exactly what the lockfile says, and
// put the resulting binaries on PATH. The job's own command then runs its own
// file, which is the whole point -- nothing here decides how a job executes.
//
// Installation happens in the tree rather than in a separate cache directory,
// which is a departure from what D28 described and a better answer than what it
// described. `pnpm` and `uv` both want to own a project directory, and both
// already keep a global content-addressed store, so the cross-commit sharing
// D28 wanted from a `cache/deps/<key>` directory is a thing the package manager
// does better than we would: a second commit with the same lockfile hardlinks
// from that store rather than downloading anything. Reimplementing it here
// would have been a worse copy of something already installed on the machine.
//
// What remains ours is knowing whether a given tree is already prepared, which
// is the marker file below.

// prepareMarker records which install a tree has already had.
const prepareMarker = ".je-prepared"

// prepared serialises preparation per tree.
//
// Two jobs from the same source can be dispatched at once, and two `pnpm
// install` processes in one directory is a corrupted `node_modules` rather than
// a slow one. Keyed by tree, so unrelated sources still prepare in parallel.
type prepared struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func newPrepared() *prepared { return &prepared{locks: map[string]*sync.Mutex{}} }

func (p *prepared) lockFor(tree string) *sync.Mutex {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.locks[tree] == nil {
		p.locks[tree] = &sync.Mutex{}
	}
	return p.locks[tree]
}

// prepare makes a tree's dependencies present and returns the directory to
// prepend to PATH.
//
// Returns ("", nil) for a job that declares no language, which is most of them.
func (w *Worker) prepare(ctx context.Context, language, tree string) (string, error) {
	if language == "" {
		return "", nil
	}
	tc, ok := toolchain.Lookup(language)
	if !ok {
		return "", toolchain.Unknown(language)
	}
	if tree == "" {
		return "", fmt.Errorf(
			"this job declares language: %s, which needs the source tree its "+
				"dependencies are declared in, and none was dispatched", language)
	}

	if _, err := os.Stat(filepath.Join(tree, tc.Manifest)); err != nil {
		return "", fmt.Errorf(
			"this job declares language: %s, so its source needs a %s and there "+
				"is none beside its definition.\n"+
				"Dependencies are declared in the repository the job came from, "+
				"and installed from its lockfile (D28).",
			language, tc.Manifest)
	}

	version, err := w.toolVersion(ctx, tc)
	if err != nil {
		return "", err
	}

	unlock := w.prepared.lockFor(tree)
	unlock.Lock()
	defer unlock.Unlock()

	key, err := prepareKey(tc, tree, version)
	if err != nil {
		return "", err
	}
	marker := filepath.Join(tree, prepareMarker+"-"+tc.Name)
	if done, _ := os.ReadFile(marker); strings.TrimSpace(string(done)) == key {
		return binDir(tc, tree), nil
	}

	w.log.Info("preparing dependencies", "language", tc.Name, "tree", tree)
	if err := w.install(ctx, tc, tree); err != nil {
		return "", err
	}
	// Written only after the install succeeded, so a killed or failing install
	// is retried rather than remembered as done.
	if err := os.WriteFile(marker, []byte(key+"\n"), 0o644); err != nil {
		return "", err
	}
	return binDir(tc, tree), nil
}

func binDir(tc toolchain.Toolchain, tree string) string {
	if tc.BinDir == "" {
		return ""
	}
	return filepath.Join(tree, tc.BinDir)
}

// toolVersion asks the tool what it is, which doubles as the check that it is
// installed at all.
func (w *Worker) toolVersion(ctx context.Context, tc toolchain.Toolchain) (string, error) {
	if _, err := w.lookPath(tc.Tool); err != nil {
		return "", fmt.Errorf(
			"this job is written in %s, and %s is not installed on this worker "+
				"(%s).\n"+
				"Install it here:  je worker runtime install %s\n"+
				"Or run the job on a worker that has it -- `je workers` says which do.",
			tc.Name, tc.Tool, w.opts.Name, tc.Name)
	}
	tool, err := w.lookPath(tc.Version[0])
	if err != nil {
		return "", err
	}
	out, err := exec.CommandContext(ctx, tool, tc.Version[1:]...).Output()
	if err != nil {
		return "", fmt.Errorf("asking %s its version: %w", tc.Tool, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// prepareKey identifies an install: this language, this tool version, this
// lockfile.
//
// The tool version is in the key deliberately. The same lockfile installed
// under two versions of a runtime can produce different native modules, because
// anything with a compiled addon builds against the ABI -- so a key that
// ignored it would let the first machine to install poison the answer for
// every other one.
func prepareKey(tc toolchain.Toolchain, tree, version string) (string, error) {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00", tc.Name, version)

	lock := tc.Lockfile
	if lock == "" {
		lock = tc.Manifest
	}
	body, err := os.ReadFile(filepath.Join(tree, lock))
	if err != nil {
		// No lockfile is not fatal, but it is worth being loud about in the
		// key: without one the install is not reproducible, so every distinct
		// manifest gets its own entry and nothing is shared.
		body, err = os.ReadFile(filepath.Join(tree, tc.Manifest))
		if err != nil {
			return "", err
		}
	}
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// install runs the ecosystem's own frozen-install command in the tree.
func (w *Worker) install(ctx context.Context, tc toolchain.Toolchain, tree string) error {
	ctx, cancel := context.WithTimeout(ctx, installTimeout)
	defer cancel()

	tool, err := w.lookPath(tc.Install[0])
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, tool, tc.Install[1:]...)
	cmd.Dir = tree
	// The installer itself may shell out to the toolchain it belongs to (pnpm
	// invoking node), so it needs the same PATH the job will get.
	cmd.Env = append(os.Environ(), "PATH="+w.pathWithToolchains())
	out, err := cmd.CombinedOutput()
	if err != nil {
		// The installer's own output, because it is the thing that knows what
		// went wrong -- a lockfile out of date with its manifest, a registry
		// that cannot be reached, a version that does not exist.
		return fmt.Errorf("installing %s dependencies with %s: %w\n%s",
			tc.Name, tc.Tool, err, strings.TrimSpace(lastLines(string(out), 20)))
	}
	return nil
}

// installTimeout bounds a cold install. Generous, because a first install on a
// slow network genuinely takes minutes and failing it would be worse than
// waiting; bounded, because a hung installer must not hold a job forever.
const installTimeout = 10 * time.Minute

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// pathWithToolchains puts this worker's installed tools first.
func (w *Worker) pathWithToolchains() string {
	if w.opts.ToolchainBin == "" {
		return os.Getenv("PATH")
	}
	return w.opts.ToolchainBin + string(os.PathListSeparator) + os.Getenv("PATH")
}
