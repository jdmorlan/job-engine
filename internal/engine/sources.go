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
	"github.com/jdmorlan/job-engine/internal/secretfile"
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

	// Secrets are the names encrypted alongside this source's definitions, and
	// Recipients is how many keys can read them (D25).
	//
	// Names and a count, never values: this is the keyless half of the secrets
	// surface, and the control plane could not report more if it wanted to. It
	// is also the half worth having in a deployed instance, where "which
	// secrets exist and who can read them" is exactly what an audit asks.
	Secrets    []string `json:"secrets,omitempty"`
	Recipients []string `json:"recipients,omitempty"`

	// SecretsError is why the file above could not be read, when it is present
	// and unreadable. Distinct from having no secrets at all.
	SecretsError string `json:"secrets_error,omitempty"`
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
		status := SourceStatus{Source: src, Path: e.SourceDir(src)}
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
		status.Secrets, status.Recipients, status.SecretsError = e.sourceSecrets(src)
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
	switch src.Kind {
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
	// Confirms it exists, so removing a name nobody registered says so rather
	// than reporting that it tombstoned nothing.
	src, err := e.store.SourceByName(ctx, name)
	if err != nil {
		return 0, err
	}
	if src.Kind == store.SourceKindSystem {
		// Refused rather than allowed and then quietly undone. The engine
		// registers this source at every start, so removing it would report a
		// success that the next restart reverses -- and a command whose effect
		// expires when you reboot is worse than one that says no (P1).
		return 0, fmt.Errorf(
			"%s is the engine's own work and cannot be unregistered.\n"+
				"The engine registers it at every start, so this would be undone by the "+
				"next restart -- and a command whose effect expires when you reboot is "+
				"worse than one that says no.\n"+
				"There is no way to turn off a system job yet; see `je jobs --all` for "+
				"what they are.", name)
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
	if job.Source == "" {
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

// SourceTreeDir is where a pinned revision of a source is unpacked, if this
// control plane actually has it.
//
// Only registered sources with a real revision can be served: a `dir` source has
// no commit and is inherently local (D22's scratch loop), so asking for one is a
// mistake worth naming rather than a tree worth inventing (D25).
func (e *Engine) SourceTreeDir(ctx context.Context, name, revision string) (string, error) {
	src, err := e.store.SourceByName(ctx, name)
	if err != nil {
		return "", fmt.Errorf("no source named %q", name)
	}
	if src.Kind != store.SourceKindGitHub {
		return "", fmt.Errorf(
			"source %q is a %s, which has no revision to serve -- its files are on the control plane's own disk",
			name, src.Kind)
	}
	// The source *root*, which is the subpath when there is one -- not the
	// whole repository. A worker resolves a job's workdir against whatever it
	// receives, so serving the repository root would run every job from one
	// directory above where its code actually is (D22/D25).
	dir := filepath.Join(e.opts.Layout.SourceTree(name, revision), src.Subpath)
	if _, err := os.Stat(dir); err != nil {
		return "", fmt.Errorf("this control plane does not have %s at revision %s", name, revision)
	}
	return dir, nil
}

// sourceSecrets reads the names and recipients of a source's encrypted secrets.
//
// Read here rather than stored, for the same reason the job and chain counts
// above are counted rather than stored: a cached copy of what a file says is a
// thing that can disagree with the file. The files are small and this is a
// read-time view.
func (e *Engine) sourceSecrets(src store.Source) (names, recipients []string, problem string) {
	dir := e.SourceDir(src)
	if dir == "" {
		return nil, nil, ""
	}
	// SourceDir has already joined Subpath -- joining it again looked correct
	// and pointed at <path>/<path>/ for any source that used one.
	body, err := os.ReadFile(filepath.Join(dir, secretfile.Name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, ""
	}
	if err != nil {
		return nil, nil, fmt.Sprintf("%s could not be read: %v", secretfile.Name, err)
	}
	file, err := secretfile.Parse(body)
	if err != nil {
		return nil, nil, fmt.Sprintf("%s is not readable: %v", secretfile.Name, err)
	}
	return file.Names(), file.Recipients, ""
}
