package jobdef

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/jdmorlan/job-engine/internal/secretfile"
	"github.com/jdmorlan/job-engine/internal/toolchain"
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

	// Chains are the flows wiring those jobs together (D17). They arrive in
	// the same snapshot as the jobs deliberately: a chain names jobs, so
	// resolving those names needs both halves of one consistent read, and
	// loading them separately would open a window where a chain points at a
	// job the engine has not seen yet.
	Chains []*Chain

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

// SidecarReader is a source that can hand back one file sitting beside the
// definitions, by name.
//
// Optional rather than part of Source, because it is not something every source
// must be able to do and the interface above is deliberately three lines. The
// engine asks for it and copes when a source cannot answer -- which is what lets
// the encrypted secrets file (D25) travel with definitions without jobdef
// needing to know that secrets exist.
type SidecarReader interface {
	ReadSidecar(name string) ([]byte, error)
}

// ReadSidecar reads a file from this source's root. Missing is not an error:
// the overwhelmingly common source has no secrets file, and treating its
// absence as a failure would make the ordinary case the exceptional one.
func (s FSSource) ReadSidecar(name string) ([]byte, error) {
	body, err := fs.ReadFile(s.FS, path.Join(s.Root, name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return body, err
}

func (s FSSource) Describe() string {
	if s.Name != "" {
		return s.Name
	}
	return s.Root
}

// chainsDir is where chain files live, relative to the jobs root.
//
// A sibling directory rather than a subdirectory of jobs: a chain is a
// different noun with a different name space, and a file under jobs/ that is
// not a job would make the slug rule ("the file name is the job") a lie.
const chainsDir = "chains"

// Load reads a source's definitions: every .yaml file at the top level, and
// every directory holding a job.yaml.
//
// The two forms are one rule -- a job is named by the thing that contains it --
// and both are exactly one level deep. That bound is the whole reason nesting
// was refused before: arbitrary depth makes a slug ambiguous (is it
// "etl/weather" or "weather"?), and the slug ends up in CLI arguments and event
// payloads where ambiguity is expensive. A directory whose name is the job's
// name has the same property a file's name had.
//
// The folder form exists because a job is not only its definition. Its code, its
// fixtures and whatever else it needs belong beside it rather than in a shared
// scripts/ directory that grows a file per job and pairs with nothing -- so a
// job is a folder you can read, move or delete as one thing.
//
// Chains keep their own directory rather than being a subdirectory of this one
// (D17).
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
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
			continue
		}
		if isYAML(e.Name()) {
			files = append(files, e.Name())
		}
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

	if err := s.loadJobDirs(ctx, root, dirs, seen, &snap); err != nil {
		return Snapshot{}, err
	}

	if err := s.loadChains(ctx, path.Join(root, chainsDir), &snap); err != nil {
		return Snapshot{}, err
	}
	if err := snap.validate(); err != nil {
		return Snapshot{}, err
	}
	return snap, nil
}

// jobFile is what makes a directory a job.
//
// One name rather than "any single yaml in there", so that a folder holding a
// definition and a config file it reads is not an ambiguity to resolve. The
// directory names the job; this names the definition.
const jobFile = "job"

// loadJobDirs reads every <root>/<name>/job.yaml.
//
// A directory with no job file is simply not a job -- scripts/, node_modules/,
// anything somebody keeps beside their work -- which is why there is no list of
// directories to ignore. chains/ is excluded by the same rule and not by name.
func (s FSSource) loadJobDirs(
	ctx context.Context, root string, dirs []string, seen map[string]string, snap *Snapshot,
) error {
	sort.Strings(dirs)

	for _, dir := range dirs {
		if err := ctx.Err(); err != nil {
			return err
		}

		full, body, ok, err := s.readJobFile(path.Join(root, dir))
		if err != nil {
			return err
		}
		if !ok {
			continue
		}

		// The directoryname is the slug, so it has to be one. Said rather than
		// skipped: a folder holding a job.yaml is a job somebody wrote, and
		// quietly not loading it is the failure P1 rules out.
		if !ValidName(dir) {
			return fmt.Errorf(
				"%s is a job, so its directory name must be lowercase letters, "+
					"digits and dashes", full)
		}
		if prev, dup := seen[dir]; dup {
			return fmt.Errorf("%s and %s are both job %q", prev, full, dir)
		}
		seen[dir] = full

		def, err := Parse(full, dir, body)
		if err != nil {
			return err
		}
		// A job in a folder runs in that folder, unless it said otherwise.
		// Its command names files next to it, which is the point of the
		// layout, and resolving them against the repository root would make
		// every command start with the job's own name.
		if def.Workdir == "" {
			def.Workdir = dir
		}
		snap.Definitions = append(snap.Definitions, def)
	}
	return nil
}

// readJobFile finds the definition inside a job directory, accepting either
// extension the way the flat form does.
func (s FSSource) readJobFile(dir string) (full string, body []byte, ok bool, err error) {
	for _, ext := range []string{".yaml", ".yml"} {
		full = path.Join(dir, jobFile+ext)
		body, err = fs.ReadFile(s.FS, full)
		switch {
		case err == nil:
			return full, body, true, nil
		case errors.Is(err, fs.ErrNotExist):
			continue
		default:
			return "", nil, false, fmt.Errorf("reading %s: %w", full, err)
		}
	}
	return "", nil, false, nil
}

// loadChains reads <root>/chains/*.yaml. A missing directory is an empty set:
// most repositories have jobs before they have flows.
func (s FSSource) loadChains(ctx context.Context, dir string, snap *Snapshot) error {
	entries, err := fs.ReadDir(s.FS, dir)
	if err != nil {
		if _, statErr := fs.Stat(s.FS, dir); statErr != nil {
			return nil
		}
		return fmt.Errorf("reading %s: %w", dir, err)
	}

	var files []string
	for _, e := range entries {
		if !e.IsDir() && isYAML(e.Name()) {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)

	seen := map[string]string{}
	for _, name := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		full := path.Join(dir, name)
		body, err := fs.ReadFile(s.FS, full)
		if err != nil {
			return fmt.Errorf("reading %s: %w", full, err)
		}

		chainName := ChainNameFromPath(name)
		if prev, dup := seen[chainName]; dup {
			return fmt.Errorf("%s and %s are both chain %q", prev, full, chainName)
		}
		seen[chainName] = full

		chain, err := ParseChain(full, chainName, body)
		if err != nil {
			// D19 again: one bad file rejects the whole load. A half-applied
			// chain is wiring that exists in no commit.
			return err
		}
		snap.Chains = append(snap.Chains, chain)
	}
	return nil
}

// validate checks the things only the whole set can answer.
//
// Both checks here are load-time on purpose. A chain step naming a job that
// does not exist, or wiring a job back to itself, produces no error at all at
// runtime -- it produces silence, or a loop, which are the two failure modes
// this project exists to make impossible to reach quietly.
func (s Snapshot) validate() error {
	jobs := make(map[string]bool, len(s.Definitions))
	for _, def := range s.Definitions {
		jobs[def.Slug] = true
	}
	for _, chain := range s.Chains {
		for i, step := range chain.Steps {
			if !jobs[step.Run] {
				return fmt.Errorf("%s: step %d: no job named %q -- "+
					"a chain names jobs in its own source, and there is no %s.yaml",
					chain.FilePath(), i+1, step.Run, step.Run)
			}
		}
	}
	return checkCycles(s.Chains)
}

// Sidecars are files that live beside definitions and are not definitions.
//
// They have to be named, because "every .yaml in this directory is a job" is
// otherwise true and the encrypted secrets file (D25) would be parsed as a
// broken job -- rejecting the whole sync, since D19 makes one unparseable file
// fail everything.
var sidecars = func() map[string]bool {
	out := map[string]bool{secretfile.Name: true}
	// A source tree is also a project: its manifest and lockfile live beside
	// the definitions, and `pnpm-lock.yaml` is a .yaml that is emphatically not
	// a job (D28).
	for _, name := range toolchain.ProjectFiles() {
		out[name] = true
	}
	return out
}()

func isYAML(name string) bool {
	if sidecars[name] {
		return false
	}
	return strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")
}
