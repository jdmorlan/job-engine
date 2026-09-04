// Package shim materialises the helper a job can import, for the languages we
// ship one for (D21).
//
// D6 banned SDKs, and the cost it was avoiding was distribution: publishing to
// npm and PyPI means release processes, semver, registry accounts, and a
// compatibility matrix between engine versions and client versions. An embedded
// shim pays none of it -- and is better than a published SDK on the thing SDKs
// are worst at, because the shim that runs your job is materialised by the same
// binary that runs it. There is no version pair to be wrong.
//
// # Why this lives on the worker's side of D20
//
// D21 said "the engine carries the shim in its binary and materialises it at
// run time", which was written while the engine was one process. Materialising
// means writing to the filesystem the job will run on, and after C11 the
// control plane has no such filesystem: it never executes, and a worker
// receives a plain Dispatch value with no way back. So the worker is not the
// convenient home for this, it is the only possible one -- and it is already
// the half that knows about languages, because D28 put dependency preparation
// there.
//
// # The three rules, unchanged
//
// R1. The protocol is the floor; the shim is sugar. JE_STATE and the three
// output files remain the contract, and a language we ship no shim for
// participates exactly as fully as one we do.
//
// R2. The shim may never do anything the protocol cannot. The moment a helper
// exists that no other language can reach, "any language participates fully" is
// quietly false and the shim has become the real API. Convenience only, never
// capability: if a shim wants something new, the protocol grows first.
//
// R3. Declare the language; never detect it. Inferring TypeScript from
// ["npx", "tsx", "x.ts"] is a heuristic that is silently wrong on
// ["bash", "-c", "python foo.py"], and the whole point of `language:` is that
// this is a decision rather than a guess (P3).
package shim

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

//go:embed files
var files embed.FS

// Shim is one language's helper: where it goes in the tree, and what the job
// needs in its environment to reach it.
type Shim struct {
	// Language matches the job's `language:`, which is the same field D28's
	// dependency preparation keys off. One fact about the job, two
	// capabilities -- never two fields to keep in step.
	Language string

	// Files is the embedded directory holding them, which is not always the
	// language's own name: TypeScript and JavaScript share one set.
	Files string

	// Dir is where the files are written, relative to the source tree.
	//
	// In the tree rather than beside it, which is only reasonable because of
	// D27: the worker never runs from anybody's working copy. It runs from an
	// unpacked commit in its own cache, which already holds node_modules and
	// D28's marker file. There is nothing here to gitignore because there is
	// no repository to ignore it in.
	Dir string

	// Env is what makes the files importable, given the tree they landed in.
	// Empty for a language whose resolution needs no help.
	Env func(tree string) []string
}

// table is every shim we ship, and it is deliberately short.
//
// D21's sequencing: ship exactly one, and add a language when a real job needs
// one. Building a five-language shim framework before one job has run in one
// language is how the tail starts wagging the dog -- and a shim nobody uses
// rots quietly, which is worse than not having it.
var table = []Shim{{
	Language: "typescript", Files: "typescript",

	// node_modules, so that `import je from "je"` resolves from any depth in
	// the repository rather than from a relative path that depends on how deep
	// the importing file happens to be.
	//
	// This is the one directory a package manager also owns, so the shim is
	// re-checked on every run rather than once: an install that pruned it must
	// not be able to break the next job. Writing it after any install is what
	// makes that ordering safe.
	Dir: filepath.Join("node_modules", "je"),
}, {
	// Same files, same runtime. TypeScript and JavaScript are one ecosystem
	// with one module resolver, and a job that writes .mjs deserves the helper
	// as much as one that writes .ts -- splitting them would be a distinction
	// the thing being shipped does not have.
	Language: "javascript", Files: "typescript",

	// node_modules, so that `import je from "je"` resolves from any depth in
	// the repository rather than from a relative path that depends on how deep
	// the importing file happens to be.
	//
	// This is the one directory a package manager also owns, so the shim is
	// re-checked on every run rather than once: an install that pruned it must
	// not be able to break the next job. Writing it after any install is what
	// makes that ordering safe.
	Dir: filepath.Join("node_modules", "je"),
}}

// For returns the shim for a language, if we ship one.
func For(language string) (Shim, bool) {
	for _, s := range table {
		if s.Language == language {
			return s, true
		}
	}
	return Shim{}, false
}

// Install writes the shim into a source tree and returns the environment the
// job needs to reach it.
//
// A no-op for a language we ship no shim for, which is not a failure: R1 says
// such a job participates exactly as fully through the protocol itself.
//
// Called on every run rather than guarded by D28's marker file. The files are
// two of them and a few hundred bytes, and the cost of getting it wrong is a
// job that fails on an import after a package manager tidied a directory it
// considers its own.
func Install(language, tree string) ([]string, error) {
	s, ok := For(language)
	if !ok {
		return nil, nil
	}
	if tree == "" {
		return nil, fmt.Errorf(
			"a %s job needs a source tree to write its helpers into, and none was dispatched",
			language)
	}

	root := filepath.Join(tree, s.Dir)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("making room for the %s helpers: %w", language, err)
	}

	entries, err := fs.ReadDir(files, "files/"+s.Files)
	if err != nil {
		return nil, fmt.Errorf("reading the embedded %s shim: %w", language, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		body, err := files.ReadFile("files/" + s.Files + "/" + e.Name())
		if err != nil {
			return nil, err
		}
		dst := filepath.Join(root, e.Name())
		// Compared before writing, so a tree that is already correct is not
		// touched at all -- which keeps the mtimes a watching package manager
		// or bundler may be looking at stable.
		if current, err := os.ReadFile(dst); err == nil && string(current) == string(body) {
			continue
		}
		if err := os.WriteFile(dst, body, 0o644); err != nil {
			return nil, fmt.Errorf("writing %s: %w", dst, err)
		}
	}

	if s.Env == nil {
		return nil, nil
	}
	return s.Env(tree), nil
}
