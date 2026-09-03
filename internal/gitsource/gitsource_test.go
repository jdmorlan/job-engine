package gitsource_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jdmorlan/job-engine/internal/gitsource"
)

func TestParseRepoAcceptsWhatPeopleType(t *testing.T) {
	for _, in := range []string{
		"you/weather-jobs",
		"github.com/you/weather-jobs",
		"https://github.com/you/weather-jobs",
		"https://github.com/you/weather-jobs.git",
		"git@github.com:you/weather-jobs.git",
	} {
		repo, err := gitsource.ParseRepo(in)
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if repo.String() != "you/weather-jobs" {
			t.Errorf("%s parsed as %s", in, repo)
		}
	}

	for _, in := range []string{"", "weather-jobs", "a/b/c", "/tmp/jobs"} {
		if _, err := gitsource.ParseRepo(in); err == nil {
			t.Errorf("%q parsed as a repository", in)
		}
	}
}

func TestLooksLikeRepoTellsAPathFromARepo(t *testing.T) {
	paths := []string{".", "./jobs", "/tmp/jobs", "~/code/jobs", "jobs"}
	repos := []string{"you/weather-jobs", "github.com/you/jobs", "git@github.com:you/jobs.git"}

	for _, p := range paths {
		if gitsource.LooksLikeRepo(p) {
			t.Errorf("%q was taken for a repository", p)
		}
	}
	for _, r := range repos {
		if !gitsource.LooksLikeRepo(r) {
			t.Errorf("%q was taken for a path", r)
		}
	}
}

// tarball builds a GitHub-shaped archive: everything under one wrapper
// directory named for the repo and commit.
func tarball(t *testing.T, entries []*tar.Header, bodies []string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for i, h := range entries {
		h.Name = "you-weather-jobs-a3f81c2/" + h.Name
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(bodies[i]))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(bodies[i])); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractUnpacksARepositoryWithoutItsWrapper(t *testing.T) {
	body := tarball(t,
		[]*tar.Header{
			{Name: "", Typeflag: tar.TypeDir, Mode: 0o755},
			{Name: "ingest.yaml", Typeflag: tar.TypeReg, Mode: 0o644},
			{Name: "scripts/ingest.sh", Typeflag: tar.TypeReg, Mode: 0o755},
		},
		[]string{"", "command: [\"echo\"]\n", "#!/bin/sh\n"},
	)

	dest := t.TempDir()
	out, err := gitsource.Extract(bytes.NewReader(body), dest)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if out.Files != 2 {
		t.Errorf("files = %d, want 2", out.Files)
	}

	// The repository's root, not owner-repo-sha/ -- a job's relative workdir
	// resolves against this, so the wrapper would put everything one level
	// deeper than the file says.
	if _, err := os.Stat(filepath.Join(dest, "ingest.yaml")); err != nil {
		t.Errorf("the wrapper directory was not stripped: %v", err)
	}

	info, err := os.Stat(filepath.Join(dest, "scripts", "ingest.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Error("the execute bit was not carried over, so the script will not run")
	}
}

// The oldest trick there is, and this is the only place in the project that
// unpacks an archive from outside it.
func TestExtractRefusesToWriteOutsideItsDirectory(t *testing.T) {
	cases := []struct {
		name  string
		entry string
	}{
		{"a parent traversal", "../escaped.txt"},
		{"a deep traversal", "scripts/../../../escaped.txt"},
		{"an absolute path", "/etc/escaped.txt"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := tarball(t,
				[]*tar.Header{{Name: tc.entry, Typeflag: tar.TypeReg, Mode: 0o644}},
				[]string{"owned\n"},
			)
			dest := filepath.Join(t.TempDir(), "tree")
			_, err := gitsource.Extract(bytes.NewReader(body), dest)
			if err == nil {
				t.Fatal("the archive was extracted")
			}
			if !strings.Contains(err.Error(), "escape") && !strings.Contains(err.Error(), "absolute") {
				t.Errorf("error = %q, want it to name the problem", err)
			}
		})
	}
}

func TestExtractLeavesLinksOutAndSaysHowMany(t *testing.T) {
	body := tarball(t,
		[]*tar.Header{
			{Name: "ingest.yaml", Typeflag: tar.TypeReg, Mode: 0o644},
			{Name: "evil", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd", Mode: 0o777},
		},
		[]string{"command: [\"echo\"]\n", ""},
	)

	dest := t.TempDir()
	out, err := gitsource.Extract(bytes.NewReader(body), dest)
	if err != nil {
		t.Fatal(err)
	}
	if out.SkippedLinks != 1 {
		t.Errorf("skipped links = %d, want 1", out.SkippedLinks)
	}
	if _, err := os.Lstat(filepath.Join(dest, "evil")); err == nil {
		t.Error("a symlink was written; a later entry can be written through one")
	}
}

func TestResolveRefAndTarballTalkToGitHub(t *testing.T) {
	var authSeen string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authSeen = r.Header.Get("Authorization")
		switch {
		case strings.HasSuffix(r.URL.Path, "/commits/main"):
			if r.Header.Get("Accept") != "application/vnd.github.sha" {
				t.Errorf("Accept = %q, want the sha media type", r.Header.Get("Accept"))
			}
			w.Write([]byte("a3f81c2ffffffffffffffffffffffffffffffffff"))
		case strings.Contains(r.URL.Path, "/tarball/"):
			w.Write(tarball(t,
				[]*tar.Header{{Name: "x.yaml", Typeflag: tar.TypeReg, Mode: 0o644}},
				[]string{"command: [\"echo\"]\n"}))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := gitsource.Client{BaseURL: server.URL, Token: "s3cret"}
	repo := gitsource.Repo{Owner: "you", Name: "weather-jobs"}

	sha, err := client.ResolveRef(context.Background(), repo, "")
	if err != nil {
		t.Fatal(err)
	}
	if sha != "a3f81c2ffffffffffffffffffffffffffffffffff" {
		t.Errorf("sha = %q", sha)
	}
	if authSeen != "Bearer s3cret" {
		t.Errorf("Authorization = %q, want the token to be sent", authSeen)
	}

	body, err := client.Tarball(context.Background(), repo, sha)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	if _, err := gitsource.Extract(body, t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

func TestGitHubErrorsSayWhatToDo(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer server.Close()
	repo := gitsource.Repo{Owner: "you", Name: "private-jobs"}

	// 404 is what GitHub returns for a private repository you cannot see, so
	// an unauthenticated miss has to mention both possibilities.
	_, err := gitsource.Client{BaseURL: server.URL}.ResolveRef(context.Background(), repo, "main")
	if err == nil || !strings.Contains(err.Error(), "je secret set GITHUB_TOKEN") {
		t.Errorf("unauthenticated 404 = %v, want it to mention a token", err)
	}

	_, err = gitsource.Client{BaseURL: server.URL, Token: "x"}.ResolveRef(context.Background(), repo, "main")
	if err == nil || !strings.Contains(err.Error(), "cannot see it") {
		t.Errorf("authenticated 404 = %v, want it to mention the token's access", err)
	}
}
