package gitsource_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/jdmorlan/job-engine/internal/gitsource"
)

// A tree the control plane serves must unpack through the same extractor a
// GitHub download does, wrapper and all -- otherwise the two paths drift and
// only one of them has the path-escape rules (D25).
func TestATreeRoundTripsThroughTheSameExtractor(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "weather.yaml"), "name: ingest\n")
	write(t, filepath.Join(src, "scripts", "ingest.sh"), "#!/bin/sh\necho hi\n")
	write(t, filepath.Join(src, "secrets.enc.yaml"), "GITHUB_TOKEN: ENC[...]\n")

	var buf bytes.Buffer
	if err := gitsource.Tar(src, "weather-a3f81c2", &buf); err != nil {
		t.Fatal(err)
	}

	dest := t.TempDir()
	got, err := gitsource.Extract(&buf, dest)
	if err != nil {
		t.Fatalf("a tree we wrote ourselves did not extract: %v", err)
	}
	if got.Files != 3 {
		t.Errorf("extracted %d files, want 3", got.Files)
	}

	// The wrapper is stripped, so the source root is the repository root --
	// scripts/ingest.sh, not weather-a3f81c2/scripts/ingest.sh.
	for _, rel := range []string{"weather.yaml", "scripts/ingest.sh", "secrets.enc.yaml"} {
		if _, err := os.Stat(filepath.Join(dest, filepath.FromSlash(rel))); err != nil {
			t.Errorf("%s did not survive the round trip: %v", rel, err)
		}
	}
}

// Executable bits matter: a job's command is usually a script in its own repo,
// and a mode lost in transit is a "permission denied" at run time.
func TestModesSurviveTheRoundTrip(t *testing.T) {
	src := t.TempDir()
	script := filepath.Join(src, "run.sh")
	write(t, script, "#!/bin/sh\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := gitsource.Tar(src, "root", &buf); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if _, err := gitsource.Extract(&buf, dest); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filepath.Join(dest, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o100 == 0 {
		t.Errorf("mode = %v, want the owner execute bit to survive", info.Mode())
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
