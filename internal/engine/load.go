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
		if _, err := e.store.UpsertJob(ctx, store.Job{
			Slug:           def.Slug,
			DefinitionHash: hash,
			Definition:     snapshot,
			FilePath:       def.FilePath(),
			Enabled:        def.Enabled,
			ConfigError:    configErr,
		}); err != nil {
			return LoadResult{}, err
		}

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

	e.log.Info("definitions loaded",
		"source", result.Source, "jobs", result.Loaded,
		"removed", result.Removed, "misconfigured", len(result.Misconfig))
	return result, nil
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
