package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jdmorlan/job-engine/internal/jobdef"
	"github.com/jdmorlan/job-engine/internal/secretfile"
	"github.com/jdmorlan/job-engine/internal/store"
)

// LoadResult summarises one definition load.
type LoadResult struct {
	Loaded    int      `json:"loaded"`
	Removed   int64    `json:"removed"`
	Revision  string   `json:"revision,omitempty"`
	Source    string   `json:"source"`
	Misconfig []string `json:"misconfigured,omitempty"` // parsed but cannot run (D10)

	// Chains and Routes are the wiring half of the same load (D17). Reported
	// separately from jobs because "12 jobs, 0 routes" is the shape of a repo
	// whose chains directory was never copied, and that reads as fine
	// everywhere else.
	Chains int `json:"chains"`
	Routes int `json:"routes"`

	// Sources is how many registered places were read, and FailedSources names
	// the ones that would not load. Named rather than counted: "1 source
	// failed" sends somebody looking, and "weather failed" tells them where.
	// Ref is the branch, tag or commit a fetched source ended up tracking,
	// which is not always what was asked for: registering without one asks the
	// repository what its default branch is called.
	Ref string `json:"ref,omitempty"`

	Sources       int      `json:"sources,omitempty"`
	FailedSources []string `json:"failed_sources,omitempty"`

	// SchedulesApplied reports whether a running scheduler rebuilt its table
	// from these definitions.
	//
	// When it is false the definitions are in force but the clock has not
	// caught up, so a newly added schedule is loaded and not yet firing. That
	// is a normal, brief state and it is reported rather than hidden, because
	// "loaded but not scheduled" looks exactly like "loaded" from every other
	// view (P1).
	SchedulesApplied bool `json:"schedules_applied"`
}

// Load reads every definition from the source and projects it into the store.
//
// D19 requires this to be atomic: one unparseable file rejects the whole sync
// and the last good state keeps serving. Partial application would leave the
// engine running a configuration that exists in no commit and that no file
// describes, which is the state you cannot reason about at 2am.
func (e *Engine) Load(ctx context.Context, registered store.Source, src jobdef.Source) (LoadResult, error) {
	snap, err := src.Load(ctx)
	if err != nil {
		// Deliberately no partial write. The error names the file.
		return LoadResult{}, err
	}

	// The encrypted secrets travelling with these definitions, if any (D25).
	// Read for its *names* only: no key is involved and none could be, which is
	// the property that lets D10's load-time check keep working for secrets the
	// control plane cannot read.
	inRepo, secretsErr := e.repoSecretNames(src)

	result := LoadResult{Revision: snap.Revision, Source: src.Describe()}
	slugs := make([]string, 0, len(snap.Definitions))
	jobIDs := make(map[string]int64, len(snap.Definitions))

	for _, def := range snap.Definitions {
		hash, err := def.Hash()
		if err != nil {
			return LoadResult{}, err
		}
		snapshot, err := def.Snapshot()
		if err != nil {
			return LoadResult{}, err
		}

		configErr := def.ConfigError()
		if configErr == "" {
			// D10's pit of success. A job that declares a secret it cannot be
			// given is visibly misconfigured the moment its file is loaded,
			// rather than failing with a cryptic non-zero exit eight hours
			// later. The check lives here because only the engine can see the
			// secret store.
			missing, err := e.secrets.Missing(def.Secrets)
			if err != nil {
				return LoadResult{}, err
			}
			// A secret encrypted into this source satisfies the declaration
			// just as a locally set one does. The control plane cannot read the
			// value and does not need to: presence is the question at load
			// time, and presence is answerable without a key (D25).
			missing = without(missing, inRepo)

			switch {
			case secretsErr != "" && len(missing) > 0:
				// The file is there and unreadable, so "not set" would be a
				// guess. Say what is actually wrong.
				configErr = secretsErr
			case len(missing) > 0:
				configErr = fmt.Sprintf("secret not set: %s (set it with: je secret set %s)",
					strings.Join(missing, ", "), missing[0])
			}
		}
		// Identity carries the source (D22). For the built-in local source that
		// is the bare slug, so the common case reads the way it always did.
		qualified := registered.Qualify(def.Slug)

		job, err := e.store.UpsertJob(ctx, store.Job{
			Slug:           qualified,
			Source:         registered.Name,
			DefinitionHash: hash,
			Definition:     snapshot,
			FilePath:       def.FilePath(),
			Enabled:        def.Enabled,
			ConfigError:    configErr,
			Declared:       def.DeclaredLines(),
		})
		if err != nil {
			return LoadResult{}, err
		}

		// Keyed by the bare slug: a chain resolves job names within its own
		// source, so this map is that source's name space.
		jobIDs[def.Slug] = job.ID
		slugs = append(slugs, qualified)
		result.Loaded++
		if configErr != "" {
			result.Misconfig = append(result.Misconfig, qualified)
		}
	}

	// A job whose file has gone is disabled, never deleted (D19). Reverting a
	// commit must not erase the timeline. Scoped to this source (D22): a repo
	// that lost a file must not tombstone another repo's jobs.
	removed, err := e.store.DeleteJobsExceptInSource(ctx, registered.Name, slugs)
	if err != nil {
		return LoadResult{}, err
	}
	result.Removed = removed

	// Routes after jobs, because a route points at a job id. The order is not
	// an implementation detail: it is why a chain naming a job that does not
	// exist is a load error rather than a foreign key violation at 3am.
	if result.Chains, result.Routes, err = e.loadRoutes(ctx, registered, snap, jobIDs); err != nil {
		return LoadResult{}, err
	}

	e.log.Info("definitions loaded",
		"source", result.Source, "jobs", result.Loaded, "chains", result.Chains,
		"routes", result.Routes, "removed", result.Removed,
		"misconfigured", len(result.Misconfig))
	return result, nil
}

// loadRoutes projects the snapshot's chains into the routes table and rebuilds
// the in-memory trigger table.
//
// The name resolution here duplicates nothing: Snapshot.validate already
// refused a step naming a job that does not exist, so a missing id at this
// point would be a bug in this package rather than a mistake in a file, and it
// is reported as such.
func (e *Engine) loadRoutes(ctx context.Context, registered store.Source, snap jobdef.Snapshot, jobIDs map[string]int64) (chains, routes int, err error) {
	storedChains := make([]store.Chain, 0, len(snap.Chains))
	storedRoutes := make([]store.Route, 0, len(snap.Chains))

	for _, chain := range snap.Chains {
		storedChains = append(storedChains, store.Chain{
			Name:        registered.Qualify(chain.Name),
			Source:      registered.Name,
			Description: chain.Description,
			FilePath:    chain.FilePath(),
		})
		for i, step := range chain.Steps {
			targetID, ok := jobIDs[step.Run]
			if !ok {
				return 0, 0, fmt.Errorf("chain %s step %d targets unknown job %q",
					chain.Name, i+1, step.Run)
			}

			// The file names jobs bare; the events carry qualified names. The
			// rule is stored resolved, so a repo registered under two names
			// wires itself correctly both times without either copy of the
			// file mentioning a source (D22).
			match := step.On.Qualify(registered.Qualify)
			encoded, err := json.Marshal(match)
			if err != nil {
				return 0, 0, err
			}
			hash, err := jobdef.RouteHash(match, registered.Qualify(step.Run))
			if err != nil {
				return 0, 0, err
			}
			storedRoutes = append(storedRoutes, store.Route{
				TargetJobID: targetID,
				Match:       encoded,
				RouteHash:   hash,
				ChainName:   registered.Qualify(chain.Name),
				StepIndex:   i + 1,
				Authored:    store.AuthoredInChainFile,
				FilePath:    chain.FilePath(),
				Enabled:     true,
			})
		}
	}

	if err := e.store.ReplaceRoutes(ctx, registered.Name, storedChains, storedRoutes); err != nil {
		return 0, 0, err
	}
	if err := e.reloadRoutes(ctx); err != nil {
		return 0, 0, err
	}
	return len(storedChains), len(storedRoutes), nil
}

// loadDir loads a source from an unpacked tree on disk.
//
// Every source is a repository now, so `dir` is always somewhere in the cache:
// the tree fetched for a particular commit. It is not somewhere a person edits.
func (e *Engine) loadDir(ctx context.Context, registered store.Source, dir string) (LoadResult, error) {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		// Not an error: a fresh install has no jobs directory and must start.
		return LoadResult{Source: dir}, nil
	} else if err != nil {
		return LoadResult{}, fmt.Errorf("reading %s: %w", dir, err)
	}
	return e.Load(ctx, registered, jobdef.FSSource{
		FS:   os.DirFS(dir),
		Root: ".",
		Name: dir,
	})
}

// SourceDir is the unpacked tree a source currently reads from.
//
// Always in the cache, under the commit it came from. Empty until it has been
// fetched once, which is what a job dispatched from an unfetched source has to
// report rather than guess about.
func (e *Engine) SourceDir(src store.Source) string {
	if src.Kind == store.SourceKindSystem {
		// The engine's own jobs have no tree (P2). Their revision is the
		// binary's version rather than a commit, so joining it onto the cache
		// path would name a directory that has never existed and send the
		// worker looking for it.
		return ""
	}
	if src.Revision == "" {
		return ""
	}
	return filepath.Join(e.opts.Layout.SourceTree(src.Name, src.Revision), src.Subpath)
}

// Definition returns the parsed definition a job currently runs under.
//
// It reads the stored snapshot rather than the file on disk, so that what the
// engine reports is what the engine would execute -- if the file has since
// changed and not been reloaded, this shows the loaded version, not the edit.
func (e *Engine) Definition(ctx context.Context, slug string) (*jobdef.Definition, store.Job, error) {
	job, err := e.resolveJob(ctx, slug)
	if err != nil {
		return nil, store.Job{}, err
	}
	def, err := jobdef.FromSnapshot(job.Definition)
	if err != nil {
		return nil, store.Job{}, fmt.Errorf("job %s: %w", slug, err)
	}
	// The snapshot is the job; the line numbers are the file it came from, and
	// they are stored separately for that reason (P3).
	return def.WithDeclaredLines(job.Declared), job, nil
}

// repoSecretNames lists the secrets encrypted alongside these definitions.
//
// Returns names and, separately, a description of why it could not read them --
// rather than an error, because a malformed secrets file must not reject an
// otherwise valid sync. It makes the jobs that need those secrets
// misconfigured, which is visible and recoverable; rejecting the whole load
// would take down jobs that have nothing to do with it (D19's atomicity is
// about partial application, not about refusing everything).
func (e *Engine) repoSecretNames(src jobdef.Source) (map[string]bool, string) {
	reader, ok := src.(jobdef.SidecarReader)
	if !ok {
		return nil, ""
	}
	body, err := reader.ReadSidecar(secretfile.Name)
	if err != nil {
		return nil, fmt.Sprintf("%s could not be read: %v", secretfile.Name, err)
	}
	if len(body) == 0 {
		return nil, ""
	}
	file, err := secretfile.Parse(body)
	if err != nil {
		return nil, fmt.Sprintf("%s is not readable: %v", secretfile.Name, err)
	}
	names := make(map[string]bool, len(file.Names()))
	for _, n := range file.Names() {
		names[n] = true
	}
	return names, ""
}

// without removes from missing every name the repository supplies.
func without(missing []string, supplied map[string]bool) []string {
	if len(supplied) == 0 {
		return missing
	}
	out := missing[:0]
	for _, name := range missing {
		if !supplied[name] {
			out = append(out, name)
		}
	}
	return out
}
