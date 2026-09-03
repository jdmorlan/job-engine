package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jdmorlan/job-engine/internal/gitsource"
	"github.com/jdmorlan/job-engine/internal/jobdef"
	"github.com/jdmorlan/job-engine/internal/store"
)

// loadGitHub resolves a ref, fetches the tree if it is not already here, and
// loads it.
//
// The resolve always happens; the download usually does not. A source tracking
// a branch therefore costs one small API call per sync and nothing else until
// somebody actually pushes, and the recorded revision is exact either way.
func (e *Engine) loadGitHub(ctx context.Context, src store.Source) (LoadResult, error) {
	repo, err := gitsource.ParseRepo(src.Location)
	if err != nil {
		return LoadResult{}, err
	}

	client, err := e.gitClient(src)
	if err != nil {
		return LoadResult{}, err
	}

	sha, err := client.ResolveRef(ctx, repo, src.Ref)
	if err != nil {
		return LoadResult{}, err
	}

	tree := e.opts.Layout.SourceTree(src.Name, sha)
	if _, err := os.Stat(tree); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return LoadResult{}, err
		}
		if err := e.fetchTree(ctx, client, repo, sha, tree); err != nil {
			return LoadResult{}, err
		}
	}

	root := tree
	if src.Subpath != "" {
		root = filepath.Join(tree, src.Subpath)
		if _, err := os.Stat(root); err != nil {
			return LoadResult{}, fmt.Errorf(
				"%s has no %s at %s -- --path names a directory inside the repository",
				repo, src.Subpath, sha[:7])
		}
	}

	result, err := e.Load(ctx, src, jobdef.FSSource{
		FS:   os.DirFS(root),
		Root: ".",
		Name: fmt.Sprintf("%s@%s", repo, sha[:7]),
	})
	if err != nil {
		return LoadResult{}, err
	}
	// The revision is the point of fetching by commit: it is what a run
	// records, and what makes "what code ran?" answerable for a job whose
	// source is a moving branch (D11, D22).
	result.Revision = sha
	result.Ref = src.Ref
	return result, nil
}

// fetchTree downloads and unpacks one commit, atomically.
//
// Unpacked beside the destination and then renamed, so an interrupted fetch
// cannot leave a half-extracted tree that looks complete. The cache is keyed by
// commit, and a directory that exists is taken as authoritative -- so "exists"
// has to mean "finished".
func (e *Engine) fetchTree(ctx context.Context, client gitsource.Client, repo gitsource.Repo, sha, tree string) error {
	if err := os.MkdirAll(filepath.Dir(tree), 0o700); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(filepath.Dir(tree), ".fetch-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging) // no-op once renamed

	body, err := client.Tarball(ctx, repo, sha)
	if err != nil {
		return err
	}
	defer body.Close()

	extracted, err := gitsource.Extract(body, staging)
	if err != nil {
		return fmt.Errorf("unpacking %s@%s: %w", repo, sha[:7], err)
	}
	if err := os.Rename(staging, tree); err != nil {
		return err
	}

	e.log.Info("fetched", "repo", repo.String(), "revision", sha[:7],
		"files", extracted.Files, "bytes", extracted.Bytes,
		"skipped_links", extracted.SkippedLinks)
	if extracted.SkippedLinks > 0 {
		// Said rather than counted quietly: a repository whose scripts are
		// symlinks arrives subtly incomplete, and the failure would otherwise
		// surface much later as a missing file at run time.
		e.log.Warn("symlinks were not unpacked",
			"repo", repo.String(), "count", extracted.SkippedLinks)
	}
	return nil
}

// gitClient builds an authenticated client if the source names a secret.
//
// The token is read here and handed to the fetcher, and never stored anywhere
// else: the source row holds the *name* of a secret (D10), so a registration is
// safe to print, back up, and put in `je source`.
func (e *Engine) gitClient(src store.Source) (gitsource.Client, error) {
	client := gitsource.Client{BaseURL: e.githubBaseURL}
	if src.TokenSecret == "" {
		return client, nil
	}

	resolved, err := e.secrets.Resolve([]string{src.TokenSecret})
	if err != nil {
		return gitsource.Client{}, fmt.Errorf(
			"source %s needs the secret %s: %w -- set it with: je secret set %s",
			src.Name, src.TokenSecret, err, src.TokenSecret)
	}
	client.Token = resolved[src.TokenSecret]
	return client, nil
}

// pruneTrees removes cached trees other than the current and previous
// revisions.
//
// The previous one is kept deliberately: a job from this source may be running
// right now, with its working directory inside that tree, and deleting the
// ground from under a running job to save a few megabytes is a bad trade.
func (e *Engine) pruneTrees(source, keep, alsoKeep string) {
	dir := filepath.Dir(e.opts.Layout.SourceTree(source, keep))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == keep || entry.Name() == alsoKeep {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			e.log.Warn("removing an old source tree", "source", source, "error", err)
		}
	}
}
