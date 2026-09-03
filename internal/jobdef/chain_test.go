package jobdef_test

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jdmorlan/job-engine/internal/jobdef"
)

func parseChain(t *testing.T, body string) *jobdef.Chain {
	t.Helper()
	c, err := jobdef.ParseChain("chains/daily.yaml", "daily", []byte(body))
	if err != nil {
		t.Fatalf("ParseChain: %v", err)
	}
	return c
}

func TestAChainIsItsStepsAndNothingElse(t *testing.T) {
	c := parseChain(t, `
description: extract then roll up
steps:
  - on: { event: run.succeeded, where: { job: extract } }
    run: rollup
`)
	if c.Name != "daily" {
		t.Errorf("name = %q, want the file name", c.Name)
	}
	if len(c.Steps) != 1 {
		t.Fatalf("steps = %d", len(c.Steps))
	}
	step := c.Steps[0]
	if step.Run != "rollup" || step.On.Event != "run.succeeded" {
		t.Fatalf("step = %+v", step)
	}
	if step.On.Where["job"] != "extract" {
		t.Errorf("where = %v", step.On.Where)
	}

	// The rule's identity is its match and its target, so moving it down the
	// file must not make it a different rule.
	hash, err := step.RouteHash()
	if err != nil {
		t.Fatal(err)
	}
	same := parseChain(t, `
steps:
  - on: { event: other.thing }
    run: rollup
  - on: { event: run.succeeded, where: { job: extract } }
    run: rollup
`)
	moved, err := same.Steps[1].RouteHash()
	if err != nil {
		t.Fatal(err)
	}
	if hash != moved {
		t.Error("moving a step down the file changed its route hash")
	}
}

func TestChainErrorsNameWhatToFix(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{{
		name: "a misspelled key is not silently ignored",
		body: "steps:\n  - on: { event: a.b }\n    runs: rollup\n",
		want: "field runs not found",
	}, {
		name: "fan-in says it is not built rather than not a field",
		body: "steps:\n  - on:\n      all_of:\n        - { event: a.b }\n        - { event: c.d }\n      within: 6h\n    run: rollup\n",
		want: "not implemented yet (D3)",
	}, {
		name: "a step with no target",
		body: "steps:\n  - on: { event: a.b }\n",
		want: "needs `run:`",
	}, {
		name: "a where value that is not a scalar",
		body: "steps:\n  - on: { event: a.b, where: { tags: [x, y] } }\n    run: rollup\n",
		want: "equality",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := jobdef.ParseChain("chains/daily.yaml", "daily", []byte(tc.body))
			if err == nil {
				t.Fatal("no error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// An unfinished chain file is not a broken one. Sync is atomic (D19), so
// refusing to load a file somebody is halfway through writing would take every
// job in the source down with it.
func TestAChainWithNoStepsLoadsAndWiresNothing(t *testing.T) {
	c := parseChain(t, "description: not written yet\n\nsteps: []\n")
	if len(c.Steps) != 0 {
		t.Fatalf("steps = %d", len(c.Steps))
	}
	bare := parseChain(t, "description: not written yet\n")
	if len(bare.Steps) != 0 {
		t.Fatalf("steps = %d", len(bare.Steps))
	}
}

func TestAMatchComparesFieldsForEquality(t *testing.T) {
	c := parseChain(t, `
steps:
  - on: { event: weather.ingested, where: { station: north, count: 41, dry: true } }
    run: rollup
`)
	m := c.Steps[0].On

	if !m.Matches("weather.ingested", []byte(`{"station":"north","count":41,"dry":true,"extra":"ignored"}`)) {
		t.Error("an event carrying every declared field did not match")
	}
	if m.Matches("weather.ingested", []byte(`{"station":"south","count":41,"dry":true}`)) {
		t.Error("a different station matched")
	}
	if m.Matches("weather.ingested", []byte(`{"station":"north","count":41}`)) {
		t.Error("a missing field matched")
	}
	if m.Matches("other.event", []byte(`{"station":"north","count":41,"dry":true}`)) {
		t.Error("a different event type matched")
	}

	// No `where` at all is "every event of this type", including one with no
	// payload -- otherwise `on: { event: engine.started }` could never fire.
	bare := parseChain(t, "steps:\n  - on: { event: engine.started }\n    run: rollup\n")
	if !bare.Steps[0].On.Matches("engine.started", nil) {
		t.Error("a pattern with no where did not match an event with no payload")
	}
}

// A chain is loaded with the jobs it names, so these are the checks only the
// whole source can make -- and both of them fail silently at runtime if they
// are not made here.
func TestASourceRefusesWiringItCannotHonour(t *testing.T) {
	jobFile := "command: [\"echo\", \"hi\"]\n"

	cases := []struct {
		name  string
		files fstest.MapFS
		want  string
	}{{
		name: "a step naming a job that does not exist",
		files: fstest.MapFS{
			"jobs/extract.yaml": &fstest.MapFile{Data: []byte(jobFile)},
			"jobs/chains/daily.yaml": &fstest.MapFile{Data: []byte(
				"steps:\n  - on: { event: run.succeeded, where: { job: extract } }\n    run: rollup\n")},
		},
		want: `no job named "rollup"`,
	}, {
		name: "a job wired back to itself",
		files: fstest.MapFS{
			"jobs/a.yaml": &fstest.MapFile{Data: []byte(jobFile)},
			"jobs/b.yaml": &fstest.MapFile{Data: []byte(jobFile)},
			"jobs/chains/loop.yaml": &fstest.MapFile{Data: []byte(
				"steps:\n" +
					"  - on: { event: run.succeeded, where: { job: a } }\n    run: b\n" +
					"  - on: { event: run.succeeded, where: { job: b } }\n    run: a\n")},
		},
		want: "closes a loop",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := jobdef.FSSource{FS: tc.files, Root: "jobs"}
			_, err := src.Load(context.Background())
			if err == nil {
				t.Fatal("the source loaded")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestASourceLoadsJobsAndChainsTogether(t *testing.T) {
	src := jobdef.FSSource{FS: fstest.MapFS{
		"jobs/extract.yaml": &fstest.MapFile{Data: []byte("command: [\"echo\", \"hi\"]\n")},
		"jobs/rollup.yaml":  &fstest.MapFile{Data: []byte("command: [\"echo\", \"hi\"]\n")},
		"jobs/chains/daily.yaml": &fstest.MapFile{Data: []byte(
			"steps:\n  - on: { event: run.succeeded, where: { job: extract } }\n    run: rollup\n")},
	}, Root: "jobs"}

	snap, err := src.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(snap.Definitions) != 2 {
		t.Errorf("jobs = %d, want 2 -- the chains directory is not a job", len(snap.Definitions))
	}
	if len(snap.Chains) != 1 || snap.Chains[0].Name != "daily" {
		t.Fatalf("chains = %+v", snap.Chains)
	}
}
