package engine_test

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

	"github.com/jdmorlan/job-engine/internal/engine"
	"github.com/jdmorlan/job-engine/internal/model"
	"github.com/jdmorlan/job-engine/internal/store"
)

// repo writes a jobs repository outside the engine's data directory, the way a
// git checkout of somebody's job repo looks.
func repo(t *testing.T, jobs map[string]string, chains map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "chains"), 0o755); err != nil {
		t.Fatal(err)
	}
	for slug, script := range jobs {
		write(t, filepath.Join(dir, slug+".yaml"),
			"command: [\"/bin/sh\", \"-c\", "+quote(script)+"]\n")
	}
	for name, body := range chains {
		write(t, filepath.Join(dir, "chains", name+".yaml"), body)
	}
	return dir
}

// addSource registers a second repository, served from a directory by the same
// fake GitHub the fixture uses.
func addSource(t *testing.T, e *engine.Engine, name, dir string) engine.LoadResult {
	t.Helper()
	hubFor(e).Add("you/"+name, dir)
	result, err := e.AddSource(context.Background(), store.Source{
		Name: name, Kind: store.SourceKindGitHub, Location: "you/" + name,
	})
	if err != nil {
		t.Fatalf("registering %s: %v", name, err)
	}
	return result
}

func TestAJobFromASourceIsNamedForIt(t *testing.T) {
	ctx := context.Background()
	e, _ := chainFixture(t, map[string]string{"scratch": `echo local`}, nil)
	addSource(t, e, "weather", repo(t, map[string]string{"ingest": `echo remote`}, nil))

	jobs, err := e.Jobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, j := range jobs {
		names[j.Slug] = j.Source
	}
	// Every job carries its source, because every job comes from one. There is
	// no built-in source qualifying to a bare slug any more: a definition lives
	// in a repository somebody registered, and the name it was registered under
	// is worth carrying.
	if names[qual("scratch")] != testSource {
		t.Errorf("fixture job is named %v, want %s", names, qual("scratch"))
	}
	if names["weather/ingest"] != "weather" {
		t.Errorf("source job is named %v, want weather/ingest", names)
	}
}

func TestAShortNameResolvesUntilItIsAmbiguous(t *testing.T) {
	ctx := context.Background()
	e, _ := chainFixture(t, map[string]string{"scratch": `echo local`}, nil)
	addSource(t, e, "weather", repo(t, map[string]string{"ingest": `echo weather`}, nil))

	// One source has it: the short form is unambiguous and resolves.
	job, err := e.Job(ctx, "ingest")
	if err != nil {
		t.Fatalf("resolving a unique short name: %v", err)
	}
	if job.Slug != "weather/ingest" {
		t.Errorf("resolved to %q", job.Slug)
	}

	// Two do: refusing is the point. Picking one would mean `je run ingest`
	// quietly running the wrong repository's job.
	addSource(t, e, "home", repo(t, map[string]string{"ingest": `echo home`}, nil))
	_, err = e.Job(ctx, "ingest")
	if err == nil {
		t.Fatal("an ambiguous short name resolved to something")
	}
	for _, want := range []string{"ambiguous", "home/ingest", "weather/ingest"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}

	// And naming the source always works.
	if _, err := e.Job(ctx, "home/ingest"); err != nil {
		t.Errorf("a qualified name did not resolve: %v", err)
	}
}

func TestABrokenSourceDoesNotTakeTheOthersDown(t *testing.T) {
	ctx := context.Background()
	e, _ := chainFixture(t, map[string]string{"scratch": `echo local`}, nil)

	good := repo(t, map[string]string{"fine": `echo fine`}, nil)
	addSource(t, e, "good", good)

	bad := repo(t, map[string]string{"also-fine": `echo ok`}, nil)
	addSource(t, e, "bad", bad)
	write(t, filepath.Join(bad, "broken.yaml"), "command: [\"echo\"]\nnonsense: true\n")

	// Atomicity is per source (D22). The broken repo keeps serving its last
	// good tree; the others must not notice.
	if _, err := e.Sync(ctx); err == nil {
		t.Fatal("a source that will not parse synced without complaint")
	}

	jobs, err := e.Jobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	live := map[string]bool{}
	for _, j := range jobs {
		if !j.Removed() {
			live[j.Slug] = true
		}
	}
	for _, want := range []string{qual("scratch"), "good/fine", "bad/also-fine"} {
		if !live[want] {
			t.Errorf("%s stopped loading because a different source is broken", want)
		}
	}

	// And the failure is visible where somebody would look for it, rather than
	// only in the error of whoever happened to run the sync.
	sources, err := e.Sources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sources {
		switch s.Name {
		case "bad":
			if s.LastError == "" {
				t.Error("the broken source does not report an error")
			}
		default:
			if s.LastError != "" {
				t.Errorf("%s reports an error it did not cause: %s", s.Name, s.LastError)
			}
		}
	}
}

func TestAChainWiresTheJobsInItsOwnSource(t *testing.T) {
	ctx := context.Background()
	e, _ := chainFixture(t, nil, nil)

	// Two registrations of files that name jobs bare. Each has to wire itself,
	// which is what makes a repository portable: the same file, registered
	// twice, wires two independent flows and neither mentions a source.
	chainBody := `
steps:
  - on: { event: run.succeeded, where: { job: extract } }
    run: rollup
`
	jobs := map[string]string{"extract": `echo extracting`, "rollup": `echo rolling`}
	addSource(t, e, "weather", repo(t, jobs, map[string]string{"daily": chainBody}))
	addSource(t, e, "home", repo(t, jobs, map[string]string{"daily": chainBody}))

	if _, err := runJob(t, e, "weather/extract", engine.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if executed := drainQueue(t, e); executed != 1 {
		t.Fatalf("ran %d jobs, want only weather's rollup", executed)
	}

	weather, err := e.Chain(ctx, "weather/daily")
	if err != nil {
		t.Fatal(err)
	}
	if weather.State != engine.ChainComplete {
		t.Fatalf("weather/daily = %q, want complete", weather.State)
	}
	if weather.Steps[0].Job != "weather/rollup" {
		t.Errorf("weather's chain targets %q", weather.Steps[0].Job)
	}

	home, err := e.Chain(ctx, "home/daily")
	if err != nil {
		t.Fatal(err)
	}
	if home.State != engine.ChainNeverRun {
		t.Fatalf("home/daily = %q, want never run -- weather's job succeeded, not home's", home.State)
	}
}

func TestRemovingASourceKeepsItsHistory(t *testing.T) {
	ctx := context.Background()
	e, _ := chainFixture(t, nil, nil)
	addSource(t, e, "weather", repo(t, map[string]string{"ingest": `echo ingesting`}, nil))

	if _, err := runJob(t, e, "weather/ingest", engine.RunOptions{}); err != nil {
		t.Fatal(err)
	}

	tombstoned, err := e.RemoveSource(ctx, "weather")
	if err != nil {
		t.Fatal(err)
	}
	if tombstoned != 1 {
		t.Errorf("tombstoned %d jobs, want 1", tombstoned)
	}

	// D19's rule, applied to a whole repository: the runs happened, and
	// `je runs` has to keep saying so.
	runs, err := e.Runs(ctx, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want the one that happened before the source went", len(runs))
	}
	if _, err := e.Enqueue(ctx, "weather/ingest", engine.RunOptions{}); err == nil {
		t.Error("a job from an unregistered source still runs")
	}
}

// A source is a repository. Anything else is refused at registration, which is
// the moment somebody can still do something about it.
//
// The directory kind used to be accepted here, and it never travelled: a job
// whose code sat on the control plane's disk could only run on a worker sharing
// that disk, so it was broken the moment there were two machines (D22/D25).
func TestASourceMustBeARepository(t *testing.T) {
	ctx := context.Background()
	e, _ := chainFixture(t, map[string]string{"scratch": `echo local`}, nil)

	if _, err := e.AddSource(ctx, store.Source{
		Name: "onefile", Kind: "dir", Location: t.TempDir(),
	}); err == nil {
		t.Error("a directory was registered as a source")
	}
}

func githubStub(t *testing.T, sha string, files map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/repos/you/jobs"):
			// Asked once at registration, so a repository whose default branch
			// is not called main still works.
			w.Write([]byte(`{"default_branch":"trunk"}`))
		case strings.Contains(r.URL.Path, "/commits/"):
			if !strings.HasSuffix(r.URL.Path, "/commits/trunk") {
				t.Errorf("resolved %q, want the repository's own default branch", r.URL.Path)
			}
			w.Write([]byte(sha))
		case strings.Contains(r.URL.Path, "/tarball/"):
			var buf bytes.Buffer
			gz := gzip.NewWriter(&buf)
			tw := tar.NewWriter(gz)
			for name, body := range files {
				header := &tar.Header{
					Name:     "you-jobs-" + sha[:7] + "/" + name,
					Typeflag: tar.TypeReg,
					Mode:     0o755,
					Size:     int64(len(body)),
				}
				if err := tw.WriteHeader(header); err != nil {
					t.Error(err)
					return
				}
				if _, err := tw.Write([]byte(body)); err != nil {
					t.Error(err)
					return
				}
			}
			tw.Close()
			gz.Close()
			w.Write(buf.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestAPinnedTreeIsServableAndAnythingElseIsRefused(t *testing.T) {
	ctx := context.Background()
	e, _ := chainFixture(t, nil, nil)

	const sha = "a3f81c2ffffffffffffffffffffffffffffffffff"
	server := githubStub(t, sha, map[string]string{
		"ingest.yaml": "command: [\"/bin/sh\", \"-c\", \"true\"]\n",
	})
	defer server.Close()
	engine.SetGitHubBaseURLForTest(e, server.URL)

	if _, err := e.AddSource(ctx, store.Source{
		Name: "weather", Kind: store.SourceKindGitHub, Location: "you/jobs",
	}); err != nil {
		t.Fatal(err)
	}

	dir, err := e.SourceTreeDir(ctx, "weather", sha)
	if err != nil {
		t.Fatalf("a fetched revision was not servable: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ingest.yaml")); err != nil {
		t.Errorf("the served directory is not the tree: %v", err)
	}

	// A revision this control plane never fetched. Saying so beats serving an
	// empty archive that fails as "command not found" on the other machine.
	if _, err := e.SourceTreeDir(ctx, "weather", "0000000000000000000000000000000000000000"); err == nil {
		t.Error("a revision that was never fetched was served anyway")
	}
}

func TestAFetchedSourceRunsItsOwnCodeAndRecordsTheCommit(t *testing.T) {
	ctx := context.Background()
	e, layout := chainFixture(t, nil, nil)

	const sha = "a3f81c2ffffffffffffffffffffffffffffffffff"
	server := githubStub(t, sha, map[string]string{
		"ingest.yaml":       "command: [\"/bin/sh\", \"scripts/ingest.sh\"]\n",
		"scripts/ingest.sh": "#!/bin/sh\necho ingesting from the repository\n",
	})
	defer server.Close()
	engine.SetGitHubBaseURLForTest(e, server.URL)

	if _, err := e.AddSource(ctx, store.Source{
		Name: "weather", Kind: store.SourceKindGitHub, Location: "you/jobs",
	}); err != nil {
		t.Fatalf("registering a fetched source: %v", err)
	}

	// The tree is unpacked under the commit it came from, so a tree that has
	// been fetched once never needs fetching again.
	tree := layout.SourceTree("weather", sha)
	if _, err := os.Stat(filepath.Join(tree, "scripts", "ingest.sh")); err != nil {
		t.Fatalf("the repository was not unpacked where the cache expects it: %v", err)
	}

	// The job runs its own repository's script, which is the whole point of a
	// source being a tree rather than a pile of YAML.
	out, err := runJob(t, e, "weather/ingest", engine.RunOptions{})
	if err != nil {
		t.Fatalf("running a job from a fetched source: %v", err)
	}
	if out.Run.Status != model.StatusSucceeded {
		t.Fatalf("status = %s: %s", out.Run.Status, out.Run.Error)
	}
	// Without this, "what ran?" is unanswerable for a job whose code came from
	// a moving branch (D11, D22).
	if out.Run.SourceRevision != sha {
		t.Errorf("run recorded revision %q, want the commit it ran from", out.Run.SourceRevision)
	}

	sources, err := e.Sources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sources {
		if s.Name == "weather" && s.Revision != sha {
			t.Errorf("source revision = %q", s.Revision)
		}
	}
}
