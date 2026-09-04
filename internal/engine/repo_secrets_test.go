package engine_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/jdmorlan/job-engine/internal/engine"
	"github.com/jdmorlan/job-engine/internal/secretfile"
	"github.com/jdmorlan/job-engine/internal/store"
)

// D10's load-time check has to keep working for secrets the control plane
// cannot read. A secret encrypted into the source satisfies the declaration --
// on names alone, with no key anywhere near the engine (D25).
func TestARepoSecretSatisfiesADeclarationWithoutAKey(t *testing.T) {
	ctx := context.Background()
	e, _ := chainFixture(t, nil, nil)

	writeSecretsFile(t, treeDir(e), "WEATHER_API_KEY", "hunter2")
	writeJobFile(t, treeDir(e), "ingest", "command: [\"/bin/sh\", \"-c\", \"true\"]\nsecrets: [WEATHER_API_KEY]\n")

	if _, err := e.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	job := jobBySlug(t, e, qual("ingest"))
	if job.ConfigError != "" {
		t.Fatalf("a job whose secret is encrypted beside it was misconfigured: %s", job.ConfigError)
	}
}

// The other half: a declaration nothing supplies is still a load-time error,
// which is the whole of D10's pit of success.
func TestASecretNothingSuppliesIsStillMisconfigured(t *testing.T) {
	ctx := context.Background()
	e, _ := chainFixture(t, nil, nil)

	writeSecretsFile(t, treeDir(e), "WEATHER_API_KEY", "hunter2")
	writeJobFile(t, treeDir(e), "ingest", "command: [\"/bin/sh\", \"-c\", \"true\"]\nsecrets: [SOMETHING_ELSE]\n")

	if _, err := e.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	job := jobBySlug(t, e, qual("ingest"))
	if !strings.Contains(job.ConfigError, "SOMETHING_ELSE") {
		t.Errorf("ConfigError = %q, want it to name the secret nothing supplies", job.ConfigError)
	}
}

// A secrets file that cannot be parsed must not reject the sync. It makes the
// jobs that need it misconfigured -- visible and recoverable -- rather than
// taking down every job in the source alongside it.
func TestABrokenSecretsFileDoesNotRejectTheSync(t *testing.T) {
	ctx := context.Background()
	e, _ := chainFixture(t, nil, nil)

	if err := os.WriteFile(filepath.Join(treeDir(e), secretfile.Name),
		[]byte("this is not a secrets file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeJobFile(t, treeDir(e), "needs", "command: [\"/bin/sh\", \"-c\", \"true\"]\nsecrets: [WEATHER_API_KEY]\n")
	writeJobFile(t, treeDir(e), "fine", "command: [\"/bin/sh\", \"-c\", \"true\"]\n")

	if _, err := e.Sync(ctx); err != nil {
		t.Fatalf("a broken secrets file rejected the whole sync: %v", err)
	}

	if got := jobBySlug(t, e, qual("needs")).ConfigError; !strings.Contains(got, secretfile.Name) {
		t.Errorf("ConfigError = %q, want it to name the unreadable file rather than "+
			"claim the secret was never set", got)
	}
	if got := jobBySlug(t, e, qual("fine")).ConfigError; got != "" {
		t.Errorf("a job that needs no secrets was taken down too: %s", got)
	}
}

func writeSecretsFile(t *testing.T, dir string, name, value string) {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	f, err := secretfile.New([]string{id.Recipient().String()})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Set(id, name, value); err != nil {
		t.Fatal(err)
	}
	body, err := f.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, secretfile.Name), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeJobFile(t *testing.T, dir, slug, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, slug+".yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func jobBySlug(t *testing.T, e *engine.Engine, slug string) store.Job {
	t.Helper()
	jobs, err := e.Jobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if j.Slug == slug {
			return j
		}
	}
	t.Fatalf("no job %q was loaded; got %d jobs", slug, len(jobs))
	return store.Job{}
}

// The C11 half: the control plane dispatches the *name* of a secret it cannot
// read, and puts no value in the environment (D25).
func TestDispatchCarriesNamesForSecretsTheControlPlaneCannotRead(t *testing.T) {
	ctx := context.Background()
	e, _ := chainFixture(t, nil, nil)

	writeSecretsFile(t, treeDir(e), "WEATHER_API_KEY", "sk-live-value")
	writeJobFile(t, treeDir(e), "ingest",
		"command: [\"/bin/sh\", \"-c\", \"true\"]\nsecrets: [WEATHER_API_KEY]\n")
	if _, err := e.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := e.RegisterWorker(ctx, store.Worker{
		ID: "w1", Name: "w1", Labels: []string{store.DefaultLabel},
		Roles: []string{store.RoleExecute}, Version: e.Health(ctx).Version,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.TriggerRun(ctx, qual("ingest"), engine.RunOptions{}); err != nil {
		t.Fatal(err)
	}

	d, err := e.Claim(ctx, "w1")
	if err != nil || d == nil {
		t.Fatalf("claim: %v, dispatch %v", err, d)
	}

	if len(d.Secrets) != 1 || d.Secrets[0] != "WEATHER_API_KEY" {
		t.Errorf("Secrets = %v, want [WEATHER_API_KEY] for the worker to resolve", d.Secrets)
	}
	// The value must appear nowhere in what crosses the wire.
	for _, kv := range d.Env {
		if strings.Contains(kv, "sk-live-value") {
			t.Fatalf("the control plane put a secret it cannot read into the environment: %q", kv)
		}
		if strings.HasPrefix(kv, "WEATHER_API_KEY=") {
			t.Fatalf("the control plane injected WEATHER_API_KEY itself: %q", kv)
		}
	}
}
