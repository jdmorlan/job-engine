package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jdmorlan/job-engine/internal/gitsource"
	"github.com/jdmorlan/job-engine/internal/jobdef"
	"github.com/jdmorlan/job-engine/internal/model"
	"github.com/jdmorlan/job-engine/internal/store"
)

// EventSourceSynced records one source being re-read (D22).
//
// Separate from definitions.synced because the interesting fact is different:
// that event says what the engine now runs, this one says where it came from
// and which revision. Code fetched from a moving ref changing what runs tonight
// is precisely the thing that must never be silent.
const EventSourceSynced = "source.synced"

// SourceStatus is a registered source and what it currently provides.
type SourceStatus struct {
	store.Source

	// Jobs and Chains are what this source currently contributes. Counted at
	// read time rather than stored, so they cannot drift from the tables.
	Jobs   int `json:"jobs"`
	Chains int `json:"chains"`

	// Path is where a directory source actually reads from, with the built-in
	// local source's "wherever the jobs directory is" resolved.
	Path string `json:"path,omitempty"`
}

// Sources lists what is registered, with what each provides.
func (e *Engine) Sources(ctx context.Context) ([]SourceStatus, error) {
	sources, err := e.store.Sources(ctx)
	if err != nil {
		return nil, err
	}

	jobs, err := e.store.ListJobs(ctx)
	if err != nil {
		return nil, err
	}
	chains, err := e.store.ListChains(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]SourceStatus, 0, len(sources))
	for _, src := range sources {
		status := SourceStatus{Source: src}
		if src.Kind == store.SourceKindDir {
			status.Path = e.SourceDir(src)
		}
		for _, j := range jobs {
			if j.Source == src.Name && !j.Removed() {
				status.Jobs++
			}
		}
		for _, c := range chains {
			if c.Source == src.Name {
				status.Chains++
			}
		}
		out = append(out, status)
	}
	return out, nil
}

// AddSource registers a place definitions come from, and loads it.
//
// Loading immediately is the point: a registration that succeeds and then turns
// out to name a directory with no jobs in it, or one that will not parse, is a
// registration that looks fine and does nothing. The failure belongs at the
// moment you typed it.
func (e *Engine) AddSource(ctx context.Context, src store.Source) (LoadResult, error) {
	if src.Name == "" {
		return LoadResult{}, errors.New("a source needs a name")
	}
	if !jobdef.ValidName(src.Name) {
		return LoadResult{}, fmt.Errorf(
			"source name %q must be lowercase letters, digits and dashes -- it "+
				"prefixes every job name from this source", src.Name)
	}
	if src.Name == store.LocalSource {
		return LoadResult{}, fmt.Errorf("%s is the built-in source and cannot be re-registered",
			store.LocalSource)
	}

	switch src.Kind {
	case store.SourceKindDir:
		abs, err := filepath.Abs(src.Location)
		if err != nil {
			return LoadResult{}, err
		}
		src.Location = abs
		// Checked here rather than at the next sync. The control plane is the
		// thing that has to be able to read this directory, and it may not be
		// on the machine you typed the path on -- which is exactly the mistake
		// worth catching at registration time.
		if _, err := os.Stat(abs); err != nil {
			return LoadResult{}, fmt.Errorf(
				"the control plane cannot read %s: %w", abs, err)
		}
	case store.SourceKindGitHub:
		repo, err := gitsource.ParseRepo(src.Location)
		if err != nil {
			return LoadResult{}, err
		}
		src.Location = repo.String()
		if src.Ref == "" {
			// Asked rather than assumed: "main" is a convention and not a
			// rule. Resolved once, here, and stored -- so from now on the
			// source tracks a named branch rather than re-deciding what its
			// default is on every sync.
			client, err := e.gitClient(src)
			if err != nil {
				return LoadResult{}, err
			}
			branch, err := client.DefaultBranch(ctx, repo)
			if err != nil {
				return LoadResult{}, err
			}
			src.Ref = branch
		}
	default:
		return LoadResult{}, fmt.Errorf("unknown source kind %q", src.Kind)
	}

	// Whether this name has ever loaded anything decides what a failure below
	// means, so it is checked before the registration is written.
	_, existing := e.store.SourceByName(ctx, src.Name)
	isNew := errors.Is(existing, sql.ErrNoRows)

	if err := e.store.UpsertSource(ctx, src); err != nil {
		return LoadResult{}, err
	}

	result, err := e.loadRegistered(ctx, src)
	if err != nil {
		if isNew {
			// A first registration that cannot load has nothing to keep
			// serving, so leaving it behind would put a permanently broken row
			// in `je source` for what is almost always a typo in the argument.
			if removeErr := e.store.DeleteSource(ctx, src.Name); removeErr != nil {
				e.log.Error("removing a source that never loaded",
					"source", src.Name, "error", removeErr)
			}
			return LoadResult{}, err
		}
		// Re-registering an existing source and failing is different: it has a
		// last good tree, it is still serving it, and discarding the
		// registration over one bad file would be the destructive answer.
		if recordErr := e.store.RecordSourceSync(ctx, src.Name, "", err.Error()); recordErr != nil {
			e.log.Error("recording a failed source sync", "source", src.Name, "error", recordErr)
		}
		return LoadResult{}, err
	}
	if err := e.store.RecordSourceSync(ctx, src.Name, result.Revision, ""); err != nil {
		return LoadResult{}, err
	}

	e.recordSourceSynced(ctx, src, "", result)
	e.requestScheduleReload(ctx)
	return result, nil
}

// SyncSource re-reads one registered source.
func (e *Engine) SyncSource(ctx context.Context, name string) (LoadResult, error) {
	src, err := e.store.SourceByName(ctx, name)
	if err != nil {
		return LoadResult{}, err
	}
	was := src.Revision

	result, err := e.loadRegistered(ctx, src)
	if err != nil {
		if recordErr := e.store.RecordSourceSync(ctx, src.Name, was, err.Error()); recordErr != nil {
			e.log.Error("recording a failed source sync", "source", src.Name, "error", recordErr)
		}
		return LoadResult{}, err
	}
	if err := e.store.RecordSourceSync(ctx, src.Name, result.Revision, ""); err != nil {
		return LoadResult{}, err
	}

	e.recordSourceSynced(ctx, src, was, result)
	result.SchedulesApplied = e.requestScheduleReload(ctx)
	return result, nil
}

// RemoveSource unregisters a source and tombstones what it provided.
//
// Tombstones, never deletes: D19's rule that removing a definition file must not
// remove history applies at least as strongly to removing a whole repo. The runs
// happened, and `je runs` has to keep saying so.
func (e *Engine) RemoveSource(ctx context.Context, name string) (int64, error) {
	src, err := e.store.SourceByName(ctx, name)
	if err != nil {
		return 0, err
	}
	if src.Builtin() {
		return 0, fmt.Errorf("%s is built in and cannot be removed", store.LocalSource)
	}

	// Order matters: tombstone while the rows still name the source, then drop
	// the registration.
	removed, err := e.store.DeleteJobsExceptInSource(ctx, name, nil)
	if err != nil {
		return 0, err
	}
	if err := e.store.ReplaceRoutes(ctx, name, nil, nil); err != nil {
		return 0, err
	}
	if err := e.store.DeleteSource(ctx, name); err != nil {
		return 0, err
	}
	if err := e.reloadRoutes(ctx); err != nil {
		return 0, err
	}
	e.requestScheduleReload(ctx)
	return removed, nil
}

func (e *Engine) recordSourceSynced(ctx context.Context, src store.Source, was string, result LoadResult) {
	payload := map[string]any{
		"source": src.Name,
		"kind":   src.Kind,
		"jobs":   result.Loaded,
		"chains": result.Chains,
	}
	if result.Revision != "" {
		payload["to"] = result.Revision
	}
	if was != "" && was != result.Revision {
		// Both revisions, which is the whole reason this event exists: code
		// changing under a running engine has to be a row somebody can find.
		payload["from"] = was
	}
	body, _ := json.Marshal(payload)

	if _, _, err := e.publish(ctx, model.Event{
		Type:      EventSourceSynced,
		Source:    model.SourceEngine,
		Payload:   body,
		CreatedAt: e.now(),
	}); err != nil {
		e.log.Error("recording "+EventSourceSynced, "source", src.Name, "error", err)
	}
}

// SourceOfJob reports which registered source a job name belongs to, for
// messages that need to name it.
func SourceOfJob(qualified string) string {
	source, _ := store.SourceOfName(qualified)
	return source
}

// sourceRevision is the commit a job's code currently comes from, or empty for
// a job that is just a file on disk.
func (e *Engine) sourceRevision(ctx context.Context, job store.Job) (string, error) {
	if job.Source == "" || job.Source == store.LocalSource {
		return "", nil
	}
	src, err := e.store.SourceByName(ctx, job.Source)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// The source was unregistered while a job of its was still
			// runnable. Not a reason to refuse the run: the job row is the
			// authority on what runs, and an absent revision is honest.
			return "", nil
		}
		return "", err
	}
	return src.Revision, nil
}
