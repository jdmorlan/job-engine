package jobdef

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// Source supplies job definitions.
//
// D19 asks for exactly this one abstraction up front, and is specific about
// why: "definition loading is a pluggable source, not 'read this directory'...
// a small choice now and the difference between the GitOps version being a
// feature and being a rewrite." Local disk is source #1 and git is source #2,
// with neither special-cased.
//
// It is the one interface in the codebase defined ahead of its second
// implementation. Q1's rule against that was about the storage layer, where the
// abstraction would have encoded SQLite's assumptions; here the second
// implementation is already specified and the interface is three lines.
type Source interface {
	// Load returns everything the source currently holds. It is a whole-world
	// read rather than a diff, because D19 requires sync to be atomic: one
	// unparseable file rejects the whole load and the last good state keeps
	// serving, so partial application can never leave the engine in a state
	// that exists in no commit.
	Load(ctx context.Context) (Snapshot, error)

	// Describe names the source for error messages and `je status`.
	Describe() string
}

// Snapshot is one consistent read of a Source.
type Snapshot struct {
	Definitions []*Definition

	// Revision identifies the version of the whole set. Empty for a directory
	// on disk; a commit SHA for a git source, which is what lets D11 point
	// `je why` at the exact commit that defined the job that fired.
	Revision string
}

// FSSource loads definitions from a filesystem directory.
//
// It takes an fs.FS rather than a path so the same code serves a directory, a
// test's fstest.MapFS, and eventually a git worktree, without any of them being
// a special case.
type FSSource struct {
	FS   fs.FS
	Root string // subdirectory holding job files, e.g. "jobs"
	Name string // what to call this in messages
}

func (s FSSource) Describe() string {
	if s.Name != "" {
		return s.Name
	}
	return s.Root
}

// Load reads every .yaml file in the root directory, non-recursively.
//
// Non-recursive on purpose: nested directories would make a job's slug
// ambiguous (is it "etl/weather" or "weather"?), and the slug ends up in CLI
// arguments and event payloads where ambiguity is expensive. Chains get their
// own directory rather than a subdirectory of this one (D17).
func (s FSSource) Load(ctx context.Context) (Snapshot, error) {
	root := s.Root
	if root == "" {
		root = "."
	}

	entries, err := fs.ReadDir(s.FS, root)
	if err != nil {
		// A missing jobs directory is an empty set, not a failure. A fresh
		// install has no jobs and should still start.
		if _, statErr := fs.Stat(s.FS, root); statErr != nil {
			return Snapshot{}, nil
		}
		return Snapshot{}, fmt.Errorf("reading %s: %w", root, err)
	}

	var (
		snap  Snapshot
		seen  = map[string]string{}
		files []string
	)
	for _, e := range entries {
		if e.IsDir() || !isYAML(e.Name()) {
			continue
		}
		files = append(files, e.Name())
	}
	// Sorted so that a duplicate-slug error names the same pair every time,
	// and so load order is reproducible.
	sort.Strings(files)

	for _, name := range files {
		if err := ctx.Err(); err != nil {
			return Snapshot{}, err
		}
		full := path.Join(root, name)
		body, err := fs.ReadFile(s.FS, full)
		if err != nil {
			return Snapshot{}, fmt.Errorf("reading %s: %w", full, err)
		}

		slug := SlugFromPath(name)
		if prev, dup := seen[slug]; dup {
			// Possible because .yaml and .yml both parse to the same slug.
			return Snapshot{}, fmt.Errorf("%s and %s are both job %q", prev, full, slug)
		}
		seen[slug] = full

		def, err := Parse(full, slug, body)
		if err != nil {
			// D19: one bad file rejects the whole load. Loading the other nine
			// would leave the engine running a configuration that exists in no
			// commit and that no file describes.
			return Snapshot{}, err
		}
		snap.Definitions = append(snap.Definitions, def)
	}
	return snap, nil
}

func isYAML(name string) bool {
	return strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")
}
