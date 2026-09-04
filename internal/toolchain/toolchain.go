// Package toolchain knows how to prepare a source tree so a job written in a
// particular language can run (D28).
//
// It is a table rather than a module per language, because every ecosystem
// converged on the same three facts: a manifest that declares dependencies, a
// lockfile that pins them, and one command that materialises them
// deterministically. Adding a language is a row here; the mechanism that uses a
// row lives once, in the worker.
//
// What it deliberately does not do is decide how a job runs. The job's own
// `command:` still runs the job's own file -- this only makes the dependencies
// present and puts the right binaries on PATH first, so D6's "the filesystem is
// the contract, there is nothing to import" survives intact.
package toolchain

import (
	"fmt"
	"sort"
	"strings"
)

// Toolchain is everything the worker needs to know about one language.
type Toolchain struct {
	// Name is what a job declares in `language:`.
	Name string

	// Tool is the executable that must be on PATH for this language to be
	// preparable. One per language, and one static binary to install.
	Tool string

	// Manifest is the file whose presence means "this tree is a project of
	// this kind". Its absence is a definition error worth naming, not a
	// reason to guess.
	Manifest string

	// Lockfile pins the dependency graph, and is therefore the cache key: two
	// trees with the same lockfile need the same install, whatever commit they
	// came from. Empty means the manifest is its own lock.
	Lockfile string

	// Install materialises dependencies, run in the tree. Every one of these
	// is the ecosystem's own "install exactly what the lockfile says" mode --
	// never the one that is free to resolve something newer, because a job
	// that quietly changed its dependencies between two runs of the same
	// commit would break D11's "a run says what it ran under".
	Install []string

	// Version reports the tool's own version, which goes into the cache key:
	// the same lockfile installed under two runtimes can produce different
	// native modules, and a key that ignores this poisons the entry for every
	// other machine.
	Version []string

	// BinDir is prepended to PATH after installing, relative to the tree. It
	// is what makes `command: ["tsx", "job.ts"]` work without the job knowing
	// where its dependencies landed.
	BinDir string
}

// table is every language the worker can prepare.
//
// The ordering property worth preserving: the first three tools each bootstrap
// their own language runtime from something already in the repository -- uv
// from requires-python, pnpm from use-node-version, go from the toolchain
// directive -- so the version of the language is a property of the code rather
// than of the machine. A row that does not have that property is still allowed;
// it just costs a prerequisite, which is worth noticing when adding one.
var table = []Toolchain{
	{
		Name: "typescript", Tool: "pnpm",
		Manifest: "package.json", Lockfile: "pnpm-lock.yaml",
		Install: []string{"pnpm", "install", "--frozen-lockfile"},
		Version: []string{"pnpm", "--version"},
		BinDir:  "node_modules/.bin",
	},
	{
		Name: "javascript", Tool: "pnpm",
		Manifest: "package.json", Lockfile: "pnpm-lock.yaml",
		Install: []string{"pnpm", "install", "--frozen-lockfile"},
		Version: []string{"pnpm", "--version"},
		BinDir:  "node_modules/.bin",
	},
	{
		Name: "python", Tool: "uv",
		Manifest: "pyproject.toml", Lockfile: "uv.lock",
		Install: []string{"uv", "sync", "--frozen"},
		Version: []string{"uv", "--version"},
		BinDir:  ".venv/bin",
	},
	{
		Name: "go", Tool: "go",
		Manifest: "go.mod", Lockfile: "go.sum",
		Install: []string{"go", "mod", "download"},
		Version: []string{"go", "version"},
		// Nothing to prepend: the go tool resolves its own module cache, and a
		// job runs `go run ./...` rather than a binary that install produced.
		BinDir: "",
	},
}

// Lookup finds a language by name.
func Lookup(name string) (Toolchain, bool) {
	for _, t := range table {
		if strings.EqualFold(t.Name, name) {
			return t, true
		}
	}
	return Toolchain{}, false
}

// Names lists every language the engine knows how to prepare, for error
// messages that say what the alternatives are.
func Names() []string {
	out := make([]string, 0, len(table))
	for _, t := range table {
		out = append(out, t.Name)
	}
	sort.Strings(out)
	return out
}

// Unknown is the error a definition gets for a language with no row.
//
// A load-time error rather than a run-time one: D10's rule is that a job which
// cannot run should say so in `je jobs` rather than at 3am, and "no such
// language" is knowable from the file alone.
func Unknown(name string) error {
	return fmt.Errorf("language: %s is not one this engine can prepare (%s)",
		name, strings.Join(Names(), ", "))
}

// ProjectFiles is every manifest and lockfile this table knows about.
//
// A source tree holds a project as well as its definitions, so `pnpm-lock.yaml`
// sits beside `ingest.yaml` -- and "every .yaml here is a job" would parse the
// lockfile as a broken definition and fail the whole sync (D19 makes one
// unparseable file fail everything). The loader skips these, and it gets the
// list from here so that adding a language teaches the loader about its files
// without anybody remembering to.
func ProjectFiles() []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range table {
		for _, name := range []string{t.Manifest, t.Lockfile} {
			if name != "" && !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	sort.Strings(out)
	return out
}
