package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jdmorlan/job-engine/internal/model"
	"github.com/jdmorlan/job-engine/internal/store"
)

// EventDefinitionsSynced records a definition reload (P1, D11).
//
// "Why did this job change at 3am?" needs an answer in the timeline, not in a
// log file somebody has to still have. The payload carries the counts and the
// revision, so the answer is on the same screen as the runs it explains.
const EventDefinitionsSynced = "definitions.synced"

// syncTimeout bounds how long Sync waits for the scheduler to pick up the new
// definitions before answering anyway.
//
// Answering anyway is deliberate: the definitions are already committed at that
// point, so a slow scheduler is a reason to say "loaded, schedules catching up"
// rather than to fail a sync that in fact succeeded.
const syncTimeout = 5 * time.Second

// Sync reloads definitions from the source and rebuilds the schedule table.
//
// This is the endpoint the split made necessary. When the engine ran on your
// filesystem, "restart it" was an acceptable answer to "I edited a job file" --
// you were already in that terminal. Now the control plane is a container and
// possibly on another machine, and restarting it to pick up a one-line YAML
// change means dropping every in-flight run for an edit that touches none of
// them.
//
// It is atomic for the same reason Load is (D19): one unparseable file rejects
// the whole sync and the last good state keeps serving, because a configuration
// that exists in no file is the one you cannot reason about at 2am.
func (e *Engine) Sync(ctx context.Context) (LoadResult, error) {
	sources, err := e.store.Sources(ctx)
	if err != nil {
		return LoadResult{}, err
	}

	// Atomicity is per source (D22), which is a change worth being explicit
	// about: it used to be global. A weather repo that will not parse keeps its
	// own last good tree serving, and the home jobs -- which are fine, and
	// which the author may not even have touched -- keep loading. Making one
	// broken file anywhere stop every source loading anywhere would be a
	// stricter rule that protects nothing.
	var (
		total    LoadResult
		failed   []string
		firstErr error
	)
	for _, src := range sources {
		result, err := e.loadRegistered(ctx, src)
		if err != nil {
			failed = append(failed, src.Name)
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", src.Name, err)
			}
			if recordErr := e.store.RecordSourceSync(ctx, src.Name, src.Revision, err.Error()); recordErr != nil {
				e.log.Error("recording a failed source sync", "source", src.Name, "error", recordErr)
			}
			e.recordSync(ctx, LoadResult{Source: src.Name}, err)
			continue
		}
		if err := e.store.RecordSourceSync(ctx, src.Name, result.Revision, ""); err != nil {
			e.log.Error("recording a source sync", "source", src.Name, "error", err)
		}

		total.Loaded += result.Loaded
		total.Removed += result.Removed
		total.Chains += result.Chains
		total.Routes += result.Routes
		total.Misconfig = append(total.Misconfig, result.Misconfig...)
		total.Source = result.Source
		total.Revision = result.Revision
	}
	total.Sources = len(sources)
	total.FailedSources = failed

	// Definitions are committed; the schedule table is not. Until the scheduler
	// rebuilds it, a newly added `every: 15m` is loaded but silently not
	// firing, which is exactly the class of quiet nothing-happening P1 exists
	// to eliminate.
	total.SchedulesApplied = e.requestScheduleReload(ctx)

	if firstErr != nil {
		// Reported even though other sources loaded. The caller asked for the
		// world to be reloaded and it was not, and returning success with a
		// count somewhere in the payload is how that gets missed.
		e.recordSync(ctx, total, firstErr)
		return total, firstErr
	}
	e.recordSync(ctx, total, nil)
	return total, nil
}

// loadRegistered loads whichever kind of source this is.
func (e *Engine) loadRegistered(ctx context.Context, src store.Source) (LoadResult, error) {
	switch src.Kind {
	case store.SourceKindGitHub:
		result, err := e.loadGitHub(ctx, src)
		if err == nil && result.Revision != src.Revision {
			e.pruneTrees(src.Name, result.Revision, src.Revision)
		}
		return result, err
	default:
		return LoadResult{}, fmt.Errorf("unknown source kind %q", src.Kind)
	}
}

// requestScheduleReload asks a running scheduler to rebuild its table and waits
// for it, reporting whether it actually happened.
//
// False is not an error. An engine with no scheduler running -- a test, a
// one-shot tool -- has no table to rebuild, and a scheduler that is busy will
// rebuild on its own soon enough. Both are worth reporting and neither is worth
// failing over.
func (e *Engine) requestScheduleReload(ctx context.Context) bool {
	reply := make(chan error, 1)

	select {
	case e.scheduleReloads <- reply:
	default:
		// Nothing listening: no scheduler is running.
		return false
	}

	timeout := time.NewTimer(syncTimeout)
	defer timeout.Stop()

	select {
	case err := <-reply:
		if err != nil {
			e.log.Error("rebuilding the schedule table after sync", "error", err)
			return false
		}
		return true
	case <-timeout.C:
		return false
	case <-ctx.Done():
		return false
	}
}

func (e *Engine) recordSync(ctx context.Context, result LoadResult, syncErr error) {
	payload := map[string]any{
		"source":  result.Source,
		"jobs":    result.Loaded,
		"removed": result.Removed,
	}
	if len(result.Misconfig) > 0 {
		payload["misconfigured"] = result.Misconfig
	}
	if result.Revision != "" {
		payload["revision"] = result.Revision
	}
	if syncErr != nil {
		payload["error"] = syncErr.Error()
	}
	body, _ := json.Marshal(payload)

	if _, _, err := e.publish(ctx, model.Event{
		Type:      EventDefinitionsSynced,
		Source:    model.SourceEngine,
		Payload:   body,
		CreatedAt: e.now(),
	}); err != nil {
		e.log.Error("recording "+EventDefinitionsSynced, "error", err)
	}
}

// ErrSyncNotOwned is returned when the definition source is not this engine's
// to reload on demand.
//
// It has no callers yet and exists as the shape D19's git source needs: once
// the repo is the source of truth, a push-style sync must be refused with a
// sentence saying which mode is in force, rather than half-working.
var ErrSyncNotOwned = errors.New("definitions come from a source this engine does not reload on request")
