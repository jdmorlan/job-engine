package engine

import (
	"context"
	"fmt"

	"github.com/jdmorlan/job-engine/internal/store"
	"github.com/jdmorlan/job-engine/internal/system"
)

// The engine's own work, loaded as an ordinary source (P2).
//
// Everything here exists so that retention -- and whatever follows it -- is a
// job rather than a loop hidden inside the control plane. The alternative was
// available and cheaper: a ticker beside the lease sweeper, deleting rows and
// publishing an event. It was rejected because the engine's own housekeeping is
// exactly the work you most want to be able to see fail, and a job already has
// every part of that -- a run, an attempt, logs, a status, a place in
// `je waiting`, a `je explain` -- while a ticker has none of them and would
// need each one built again privately.

// loadSystemSource registers the built-in source and loads its definitions.
//
// Run at every start rather than once, because the definitions are compiled
// into the binary: an upgrade changes them, and there is no sync anybody could
// think to run. The revision is the engine's version, so the change is visible
// as a revision change like any other source's (D11).
func (e *Engine) loadSystemSource(ctx context.Context) error {
	src := store.Source{
		Name:     system.Name,
		Kind:     store.SourceKindSystem,
		Revision: e.opts.Version,
	}
	if err := e.store.UpsertSource(ctx, src); err != nil {
		return err
	}

	result, err := e.loadRegistered(ctx, src)
	if err != nil {
		// Recorded and returned. A control plane whose own retention job will
		// not parse has a bug in this repository, not a mistake in somebody's
		// file, and it should not start quietly missing it.
		_ = e.store.RecordSourceSync(ctx, src.Name, src.Revision, err.Error())
		return fmt.Errorf("loading the engine's own jobs: %w", err)
	}
	if err := e.store.RecordSourceSync(ctx, src.Name, src.Revision, ""); err != nil {
		return err
	}
	e.log.Debug("system jobs loaded", "jobs", result.Loaded, "version", e.opts.Version)
	return nil
}

// IsSystem reports whether a job name belongs to the engine's own source.
//
// P2's second rider: system jobs are filtered from the default views, or the
// "is everything OK?" screen becomes mostly housekeeping. Visible on request,
// never in your face.
func IsSystem(qualified string) bool {
	source, _ := store.SourceOfName(qualified)
	return source == system.Name
}
