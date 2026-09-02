package jobdef_test

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jdmorlan/job-engine/internal/jobdef"
)

func parse(t *testing.T, body string) *jobdef.Definition {
	t.Helper()
	def, err := jobdef.Parse("jobs/x.yaml", "x", []byte(body))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return def
}

func TestDefaultsAreAppliedButNotDeclared(t *testing.T) {
	// P3: the file holds only what the author decided. Everything else is a
	// default, and `je explain` must be able to tell them apart.
	def := parse(t, `
command: ["echo", "hi"]
timeout: 30m
`)

	if def.Timeout.D != 30*time.Minute {
		t.Errorf("timeout = %s, want 30m", def.Timeout)
	}
	if def.Overlap != jobdef.DefaultOverlap {
		t.Errorf("overlap = %q, want the default %q", def.Overlap, jobdef.DefaultOverlap)
	}
	if def.State.PrimaryCursor != jobdef.DefaultPrimaryCursor {
		t.Errorf("primary_cursor = %q, want the default", def.State.PrimaryCursor)
	}

	if line, ok := def.DeclaredAt("timeout"); !ok {
		t.Error("timeout was written in the file but is not marked declared")
	} else if line != 3 {
		t.Errorf("timeout declared at line %d, want 3", line)
	}
	if _, ok := def.DeclaredAt("overlap"); ok {
		t.Error("overlap is a default but is marked as declared")
	}
}

func TestUnknownFieldIsRejected(t *testing.T) {
	// A misspelled key that silently does nothing is the worst failure mode a
	// config format has: the file looks right and the setting is ignored.
	_, err := jobdef.Parse("jobs/x.yaml", "x", []byte(`
command: ["echo"]
timeoutt: 30m
`))
	if err == nil {
		t.Fatal("unknown field accepted")
	}
	if !strings.Contains(err.Error(), "timeoutt") {
		t.Errorf("error does not name the offending field: %v", err)
	}
}

func TestValidationErrors(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"no command", "name: X\n", "command is required"},
		{"bad overlap", "command: [\"x\"]\noverlap: sometimes\n", "overlap must be"},
		{"bad duration", "command: [\"x\"]\ntimeout: soon\n", "not a duration"},
		{"schedule with neither", "command: [\"x\"]\non:\n  - catch_up: once\n", "either every or cron"},
		{"schedule with both", "command: [\"x\"]\non:\n  - every: 5m\n    cron: \"* * * * *\"\n", "not both"},
		{"unknown timezone", "command: [\"x\"]\non:\n  - every: 5m\n    timezone: Mars/Olympus\n", "unknown timezone"},
		{"empty file", "", "empty"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := jobdef.Parse("jobs/x.yaml", "x", []byte(tc.body))
			if err == nil {
				t.Fatalf("accepted invalid definition")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestHashIsStableAndSensitive(t *testing.T) {
	body := "command: [\"echo\", \"hi\"]\ntimeout: 30m\n"

	a := parse(t, body)
	b := parse(t, body)
	hashA, err := a.Hash()
	if err != nil {
		t.Fatal(err)
	}
	hashB, _ := b.Hash()
	if hashA != hashB {
		t.Errorf("same definition hashed differently: %s vs %s", hashA, hashB)
	}

	// D11 wants a new version whenever the effective definition changes, so
	// the run detail can show what actually executed.
	c := parse(t, "command: [\"echo\", \"hi\"]\ntimeout: 31m\n")
	hashC, _ := c.Hash()
	if hashA == hashC {
		t.Error("changing the timeout did not change the hash")
	}

	// Comments and formatting are not part of the definition's identity.
	d := parse(t, "# a comment\ncommand:   [\"echo\", \"hi\"]\n\ntimeout: 30m\n")
	hashD, _ := d.Hash()
	if hashA != hashD {
		t.Error("reformatting the file changed the hash")
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	// D11 is only real if the stored snapshot reconstructs the definition that
	// ran, long after the file has changed.
	def := parse(t, "name: X\ncommand: [\"echo\", \"hi\"]\ntimeout: 45m\nstate:\n  primary_cursor: watermark\n")
	body, err := def.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	back, err := jobdef.FromSnapshot(body)
	if err != nil {
		t.Fatal(err)
	}
	if back.Timeout.D != 45*time.Minute {
		t.Errorf("timeout survived as %s, want 45m", back.Timeout)
	}
	if back.State.PrimaryCursor != "watermark" {
		t.Errorf("primary_cursor survived as %q", back.State.PrimaryCursor)
	}
	hashBefore, _ := def.Hash()
	hashAfter, _ := back.Hash()
	if hashBefore != hashAfter {
		t.Error("a round-tripped definition hashes differently")
	}
}

func TestFSSourceLoadsDirectory(t *testing.T) {
	src := jobdef.FSSource{
		FS: fstest.MapFS{
			"jobs/alpha.yaml": {Data: []byte(`command: ["echo", "a"]`)},
			"jobs/beta.yml":   {Data: []byte(`command: ["echo", "b"]`)},
			"jobs/README.md":  {Data: []byte("not a job")},
		},
		Root: "jobs",
	}
	snap, err := src.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(snap.Definitions) != 2 {
		t.Fatalf("loaded %d definitions, want 2", len(snap.Definitions))
	}
	// Sorted, so errors and output are reproducible.
	if snap.Definitions[0].Slug != "alpha" || snap.Definitions[1].Slug != "beta" {
		t.Errorf("slugs = %s, %s", snap.Definitions[0].Slug, snap.Definitions[1].Slug)
	}
}

func TestFSSourceRejectsWholeLoadOnOneBadFile(t *testing.T) {
	// D19: partial application would leave the engine running a configuration
	// that exists in no commit and that no file describes.
	src := jobdef.FSSource{
		FS: fstest.MapFS{
			"jobs/good.yaml": {Data: []byte(`command: ["echo", "a"]`)},
			"jobs/bad.yaml":  {Data: []byte("command: [unclosed")},
		},
		Root: "jobs",
	}
	if _, err := src.Load(context.Background()); err == nil {
		t.Fatal("a malformed file did not reject the load")
	}
}

func TestFSSourceMissingDirectoryIsEmptyNotAnError(t *testing.T) {
	src := jobdef.FSSource{FS: fstest.MapFS{}, Root: "jobs"}
	snap, err := src.Load(context.Background())
	if err != nil {
		t.Fatalf("a fresh install with no jobs directory must still start: %v", err)
	}
	if len(snap.Definitions) != 0 {
		t.Errorf("got %d definitions from nothing", len(snap.Definitions))
	}
}
