package testsupport

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Package testsupport provides a fake GitHub that serves whatever is in a
// directory right now.
//
// Every source is a repository, so a test that wants definitions has to serve
// them the way a repository does. That is more machinery than writing files
// into a jobs directory was, and it buys the thing the old fixture could not
// test: the fetch, the tarball, the commit, and the cache keyed by it. A test
// that edits a file and calls Sync now exercises the same path production does,
// including resolving a new revision because the content changed.
type GitHub struct {
	// URL is where the fake API answers.
	URL string

	server *httptest.Server

	mu    sync.Mutex
	repos map[string]string // "owner/name" -> directory on disk
}

func NewGitHub() *GitHub {
	g := &GitHub{repos: map[string]string{}}
	g.server = httptest.NewServer(http.HandlerFunc(g.serve))
	g.URL = g.server.URL
	return g
}

// Close stops the server.
func (g *GitHub) Close() { g.server.Close() }

// add registers a repository backed by a directory, and returns the location a
// source would name it by.
func (g *GitHub) Add(repo, dir string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.repos[repo] = dir
	return repo
}

func (g *GitHub) dirFor(path string) (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for repo, dir := range g.repos {
		if strings.Contains(path, repo) {
			return dir, true
		}
	}
	return "", false
}

func (g *GitHub) serve(w http.ResponseWriter, r *http.Request) {
	dir, ok := g.dirFor(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch {
	case strings.Contains(r.URL.Path, "/commits/"):
		// Derived from the content, so editing a file genuinely produces a new
		// revision and a sync has something to notice.
		w.Write([]byte(treeSHA(dir)))
	case strings.Contains(r.URL.Path, "/tarball/"):
		writeTarball(w, dir, treeSHA(dir))
	default:
		w.Write([]byte(`{"default_branch":"main"}`))
	}
}

// treeSHA hashes the tree, so a revision means "this content" the way a commit
// does.
func treeSHA(dir string) string {
	h := sha1.New()
	var names []string
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		names = append(names, path)
		return nil
	})
	sort.Strings(names)
	for _, path := range names {
		rel, _ := filepath.Rel(dir, path)
		body, _ := os.ReadFile(path)
		fmt.Fprintf(h, "%s\x00%s\x00", rel, body)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func writeTarball(w http.ResponseWriter, dir, sha string) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	// GitHub wraps the tree in a single top-level directory, and the extractor
	// strips exactly one component -- so the fake has to wrap it too.
	prefix := "repo-" + sha[:7] + "/"
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		tw.WriteHeader(&tar.Header{
			Name:     prefix + filepath.ToSlash(rel),
			Typeflag: tar.TypeReg,
			Mode:     0o755,
			Size:     int64(len(body)),
		})
		tw.Write(body)
		return nil
	})
	tw.Close()
	gz.Close()
	w.Write(buf.Bytes())
}
