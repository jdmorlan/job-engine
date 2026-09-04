package worker

import (
	"context"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jdmorlan/job-engine/internal/engine"
	"github.com/jdmorlan/job-engine/internal/gitsource"
)

const testRevision = "a3f81c2ffffffffffffffffffffffffffffffffff"

// The case that used to be impossible: a job whose code is in a repository,
// running on a worker that does not share the control plane's disk.
//
// Before D25 this failed with "this job's code is in X, which this worker cannot
// see", which meant D20's remote workers and D22's git sources did not compose.
func TestAWorkerFetchesCodeItCannotSee(t *testing.T) {
	w, served := workerAgainstStub(t)

	root, err := w.sourceRoot(context.Background(), engine.Dispatch{
		// A path on the control plane that does not exist here, which is
		// exactly what a worker on another machine receives.
		SourceRoot:     "/var/lib/je/sources/weather/" + testRevision,
		SourceName:     "weather",
		SourceRevision: testRevision,
	})
	if err != nil {
		t.Fatalf("fetching a tree this worker cannot see: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(root, "scripts", "ingest.sh"))
	if err != nil {
		t.Fatalf("the job's code did not arrive: %v", err)
	}
	if !strings.Contains(string(body), "ingesting") {
		t.Errorf("script = %q, want the repository's own content", body)
	}
	if *served != 1 {
		t.Errorf("served %d times, want 1", *served)
	}
}

// A tree addressed by commit cannot change, so a second run must not re-download
// it -- and two slots claiming at once must not race each other into a
// half-written directory.
func TestAFetchedTreeIsDownloadedOnce(t *testing.T) {
	w, served := workerAgainstStub(t)
	ctx := context.Background()
	d := engine.Dispatch{
		SourceRoot: "/nowhere", SourceName: "weather", SourceRevision: testRevision,
	}

	first, err := w.sourceRoot(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	second, err := w.sourceRoot(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("two fetches gave different paths: %s and %s", first, second)
	}
	if *served != 1 {
		t.Errorf("served %d times, want the second to come from cache", *served)
	}
}

// A worker sharing the control plane's disk must keep using the path it was
// given. Copying it into a cache would be slower, would double the storage, and
// would let the two drift.
func TestAVisibleSourceRootIsUsedDirectly(t *testing.T) {
	w, served := workerAgainstStub(t)

	onDisk := t.TempDir()
	root, err := w.sourceRoot(context.Background(), engine.Dispatch{
		SourceRoot: onDisk, SourceName: "weather", SourceRevision: testRevision,
	})
	if err != nil {
		t.Fatal(err)
	}
	if root != onDisk {
		t.Errorf("root = %s, want the dispatched path %s", root, onDisk)
	}
	if *served != 0 {
		t.Error("a worker that could already see the tree fetched it anyway")
	}
}

// A dir source has no commit to pin, so there is nothing to fetch and the honest
// refusal has to survive rather than becoming a wrong guess (D22/D25).
func TestADirSourceStillReportsWhatItCannotSee(t *testing.T) {
	w, _ := workerAgainstStub(t)

	root, err := w.sourceRoot(context.Background(), engine.Dispatch{
		SourceRoot: "/var/lib/je/jobs", // no name, no revision
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if root != "/var/lib/je/jobs" {
		t.Errorf("root = %s, want the unreachable path passed through so "+
			"resolveWorkdir can say it cannot be seen", root)
	}
}

// workerAgainstStub returns a worker pointed at a control plane that serves one
// tree, and a counter of how many times it was asked for it.
func workerAgainstStub(t *testing.T) (*Worker, *int) {
	t.Helper()

	tree := t.TempDir()
	mustWrite(t, filepath.Join(tree, "ingest.yaml"), "command: [\"/bin/sh\", \"scripts/ingest.sh\"]\n")
	mustWrite(t, filepath.Join(tree, "scripts", "ingest.sh"), "#!/bin/sh\necho ingesting\n")

	served := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := "/v1/sources/weather/tree/" + testRevision
		if r.URL.Path != want {
			t.Errorf("requested %q, want %q", r.URL.Path, want)
			http.NotFound(w, r)
			return
		}
		served++
		if err := gitsource.Tar(tree, "weather-"+testRevision, w); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)

	// A TLS server with its own certificate written out for the client to
	// verify against, because that is the only kind of control plane there is
	// (D25) -- and the transport is exactly what a fake would get wrong.
	caPath := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE", Bytes: server.Certificate().Raw,
	}), 0o644); err != nil {
		t.Fatal(err)
	}

	client, err := DialCA(strings.TrimPrefix(server.URL, "https://"), caPath)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := New(Options{
		Name:     "test",
		Version:  "test",
		CacheDir: t.TempDir(),
		Client:   client,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return worker, &served
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
