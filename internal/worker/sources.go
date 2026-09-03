package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jdmorlan/job-engine/internal/engine"
	"github.com/jdmorlan/job-engine/internal/gitsource"
)

// sourceRoot decides where this job's code is on *this* machine, fetching it
// from the control plane if it is not here yet (D25).
//
// The order matters and is not an optimisation. A worker sharing the control
// plane's disk already has the tree at the dispatched path, and using it keeps
// the single-machine case byte-identical to what it was before workers could be
// remote -- no copy, no cache, nothing to go stale. Only a worker that cannot
// see that path fetches, which is exactly the case that used to fail.
func (w *Worker) sourceRoot(ctx context.Context, d engine.Dispatch) (string, error) {
	if d.SourceRoot != "" {
		if _, err := os.Stat(d.SourceRoot); err == nil {
			return d.SourceRoot, nil
		}
	}
	if d.SourceName == "" || d.SourceRevision == "" {
		// Nothing to fetch. A dir source has no revision to pin, so this stays
		// the honest refusal it always was rather than becoming a wrong guess.
		return d.SourceRoot, nil
	}
	return w.ensureTree(ctx, d.SourceName, d.SourceRevision)
}

// ensureTree returns the local path for one pinned revision, downloading it
// once.
//
// Addressed by commit, so a tree that is present is correct by construction and
// never needs revalidating: the only way its content could differ is if the
// commit differed, and then it would be a different directory.
func (w *Worker) ensureTree(ctx context.Context, name, revision string) (string, error) {
	dir := filepath.Join(w.opts.CacheDir, "sources", name, revision)
	if _, err := os.Stat(dir); err == nil {
		return dir, nil
	}

	w.log.Info("fetching source tree", "source", name, "revision", revision)

	body, err := w.client.SourceTree(ctx, name, revision)
	if err != nil {
		return "", fmt.Errorf("fetching %s at %s from the control plane: %w", name, revision, err)
	}
	defer body.Close()

	// Unpacked beside the destination and renamed, so an interrupted download
	// cannot leave a half-tree that the next run would treat as present and
	// execute out of.
	staging, err := os.MkdirTemp(filepath.Dir(dir), ".fetching-*")
	if err != nil {
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return "", err
		}
		if staging, err = os.MkdirTemp(filepath.Dir(dir), ".fetching-*"); err != nil {
			return "", err
		}
	}
	defer os.RemoveAll(staging)

	// The same extractor a GitHub download goes through, with the same
	// path-escape rules. The control plane is not a trusted source of archive
	// entries any more than GitHub is.
	if _, err := gitsource.Extract(body, staging); err != nil {
		return "", fmt.Errorf("unpacking %s at %s: %w", name, revision, err)
	}
	if err := os.Rename(staging, dir); err != nil {
		// Another slot won the race and put the same immutable revision there.
		if _, statErr := os.Stat(dir); statErr == nil {
			return dir, nil
		}
		return "", err
	}
	return dir, nil
}
