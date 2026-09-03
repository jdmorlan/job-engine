package engine

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/jdmorlan/job-engine/internal/jobdef"
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
func (e *Engine) Load(ctx context.Context, src jobdef.Source) (LoadResult, error) {
	snap, err := src.Load(ctx)
	if err != nil {
		// Deliberately no partial write. The error names the file.
		return LoadResult{}, err
	}

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
			if len(missing) > 0 {
				configErr = fmt.Sprintf("secret not set: %s (set it with: je secret set %s)",
					strings.Join(missing, ", "), missing[0])
			}
		}
		job, err := e.store.UpsertJob(ctx, store.Job{
			Slug:           def.Slug,
			DefinitionHash: hash,
			Definition:     snapshot,
			FilePath:       def.FilePath(),
			Enabled:        def.Enabled,
			ConfigError:    configErr,
		})
		if err != nil {
			return LoadResult{}, err
		}

		jobIDs[def.Slug] = job.ID
		slugs = append(slugs, def.Slug)
		result.Loaded++
		if configErr != "" {
			result.Misconfig = append(result.Misconfig, def.Slug)
		}
	}

	// A job whose file has gone is disabled, never deleted (D19). Reverting a
	// commit must not erase the timeline.
	removed, err := e.store.DeleteJobsExcept(ctx, slugs)
	if err != nil {
		return LoadResult{}, err
	}
	result.Removed = removed

	// Routes after jobs, because a route points at a job id. The order is not
	// an implementation detail: it is why a chain naming a job that does not
	// exist is a load error rather than a foreign key violation at 3am.
	if result.Chains, result.Routes, err = e.loadRoutes(ctx, snap, jobIDs); err != nil {
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
func (e *Engine) loadRoutes(ctx context.Context, snap jobdef.Snapshot, jobIDs map[string]int64) (chains, routes int, err error) {
	storedChains := make([]store.Chain, 0, len(snap.Chains))
	storedRoutes := make([]store.Route, 0, len(snap.Chains))

	for _, chain := range snap.Chains {
		storedChains = append(storedChains, store.Chain{
			Name:        chain.Name,
			Description: chain.Description,
			FilePath:    chain.FilePath(),
		})
		for i, step := range chain.Steps {
			targetID, ok := jobIDs[step.Run]
			if !ok {
				return 0, 0, fmt.Errorf("chain %s step %d targets unknown job %q",
					chain.Name, i+1, step.Run)
			}
			match, err := step.MatchJSON()
			if err != nil {
				return 0, 0, err
			}
			hash, err := step.RouteHash()
			if err != nil {
				return 0, 0, err
			}
			storedRoutes = append(storedRoutes, store.Route{
				TargetJobID: targetID,
				Match:       match,
				RouteHash:   hash,
				ChainName:   chain.Name,
				StepIndex:   i + 1,
				Source:      store.RouteSourceChainFile,
				FilePath:    chain.FilePath(),
				Enabled:     true,
			})
		}
	}

	if err := e.store.ReplaceRoutes(ctx, storedChains, storedRoutes); err != nil {
		return 0, 0, err
	}
	if err := e.reloadRoutes(ctx); err != nil {
		return 0, 0, err
	}
	return len(storedChains), len(storedRoutes), nil
}

// LoadFromDisk is the convenience path for the local source, which is source #1.
func (e *Engine) LoadFromDisk(ctx context.Context) (LoadResult, error) {
	dir := e.opts.Layout.Jobs
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		// Not an error: a fresh install has no jobs directory and must start.
		return LoadResult{Source: dir}, nil
	} else if err != nil {
		return LoadResult{}, fmt.Errorf("reading jobs directory: %w", err)
	}
	return e.Load(ctx, jobdef.FSSource{
		FS:   os.DirFS(dir),
		Root: ".",
		Name: dir,
	})
}

// Definition returns the parsed definition a job currently runs under.
//
// It reads the stored snapshot rather than the file on disk, so that what the
// engine reports is what the engine would execute -- if the file has since
// changed and not been reloaded, this shows the loaded version, not the edit.
func (e *Engine) Definition(ctx context.Context, slug string) (*jobdef.Definition, store.Job, error) {
	job, err := e.store.JobBySlug(ctx, slug)
	if err != nil {
		return nil, store.Job{}, err
	}
	def, err := jobdef.FromSnapshot(job.Definition)
	if err != nil {
		return nil, store.Job{}, fmt.Errorf("job %s: %w", slug, err)
	}
	return def, job, nil
}
