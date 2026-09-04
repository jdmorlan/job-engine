package shim_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jdmorlan/job-engine/internal/shim"
)

// R1: a language we ship no shim for participates exactly as fully through the
// protocol, so asking for one is a no-op rather than a failure.
func TestALanguageWithNoShimIsNotAFailure(t *testing.T) {
	env, err := shim.Install("go", t.TempDir())
	if err != nil {
		t.Fatalf("a language with no shim was an error: %v", err)
	}
	if env != nil {
		t.Errorf("env = %v, want nothing", env)
	}
	if _, err := shim.Install("", t.TempDir()); err != nil {
		t.Errorf("a job with no language was an error: %v", err)
	}
}

func TestTheShimIsWrittenWhereTheLanguageResolvesIt(t *testing.T) {
	tree := t.TempDir()
	if _, err := shim.Install("typescript", tree); err != nil {
		t.Fatal(err)
	}
	// node_modules, so `import je from "je"` resolves from any depth rather
	// than from a relative path that depends on where the importing file is.
	for _, name := range []string{"je.mjs", "package.json"} {
		if _, err := os.Stat(filepath.Join(tree, "node_modules", "je", name)); err != nil {
			t.Errorf("%s was not written: %v", name, err)
		}
	}
}

// The one directory a package manager also owns. An install that pruned the
// shim must not be able to break the next job, which is why this is re-checked
// every run rather than guarded by D28's marker file.
func TestTheShimIsRestoredIfSomethingRemovesIt(t *testing.T) {
	tree := t.TempDir()
	if _, err := shim.Install("typescript", tree); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(tree, "node_modules")); err != nil {
		t.Fatal(err)
	}
	if _, err := shim.Install("typescript", tree); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(tree, "node_modules", "je", "je.mjs")); err != nil {
		t.Errorf("the shim was not restored: %v", err)
	}
}

// R2, as far as a test can hold it: the shim reads the protocol's environment
// and writes the protocol's files, and names nothing else.
//
// It cannot prove the rule -- that takes reading the diff -- but it catches the
// specific way it gets broken, which is a helper that quietly reaches for
// something only one language can have.
// The types ship with the shim rather than being scaffolded into somebody's
// repository, so that a job's editor knows about `je` without there being a
// second copy to keep in step. This is the drift that will actually happen: a
// helper added to the implementation and not to the declaration.
func TestEveryHelperIsTyped(t *testing.T) {
	tree := t.TempDir()
	if _, err := shim.Install("typescript", tree); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(tree, "node_modules", "je")

	types, err := os.ReadFile(filepath.Join(dir, "je.d.ts"))
	if err != nil {
		t.Fatalf("no declarations shipped with the shim: %v", err)
	}
	impl, err := os.ReadFile(filepath.Join(dir, "je.mjs"))
	if err != nil {
		t.Fatal(err)
	}

	for _, member := range []string{"state", "lastSuccessAt", "event", "setState", "emit", "output"} {
		if !strings.Contains(string(impl), member) {
			t.Errorf("the shim no longer implements %q; is this test out of date?", member)
		}
		if !strings.Contains(string(types), member) {
			t.Errorf("%q is implemented but not declared, so an editor will not know about it", member)
		}
	}

	// TypeScript finds them through the package manifest, and which field it
	// reads depends on the project's moduleResolution -- so both are set, and
	// a package.json that names neither would typecheck as `any` while looking
	// like it worked.
	manifest, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	var pkg struct {
		Types   string `json:"types"`
		Exports map[string]struct {
			Types string `json:"types"`
		} `json:"exports"`
	}
	if err := json.Unmarshal(manifest, &pkg); err != nil {
		t.Fatal(err)
	}
	if pkg.Types == "" {
		t.Error(`package.json has no "types", so classic module resolution finds no declarations`)
	}
	if pkg.Exports["."].Types == "" {
		t.Error(`the "." export has no "types", so node16 and bundler resolution find none`)
	}
}

func TestTheShimTouchesOnlyTheProtocol(t *testing.T) {
	tree := t.TempDir()
	if _, err := shim.Install("typescript", tree); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(tree, "node_modules", "je", "je.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(body)

	// Every channel of D6, and the five things D21 says the shim exposes.
	for _, want := range []string{
		"JE_STATE", "JE_LAST_SUCCESS_AT", "EVENT_PAYLOAD",
		"JOB_STATE_OUT_FILE", "JOB_OUTPUT_FILE", "JOB_EVENTS_FILE",
		"setState", "emit", "output", "lastSuccessAt",
	} {
		if !strings.Contains(source, want) {
			t.Errorf("the shim does not mention %q", want)
		}
	}
	// Nothing that would make it more than sugar: no network, no engine
	// address, no way to reach anything the protocol does not offer.
	for _, forbidden := range []string{"fetch(", "http", "JE_ADDR", "node:net"} {
		if strings.Contains(source, forbidden) {
			t.Errorf("the shim reaches for %q, which the protocol cannot do (R2)", forbidden)
		}
	}
}
