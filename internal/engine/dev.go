package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jdmorlan/job-engine/internal/jobdef"
	"github.com/jdmorlan/job-engine/internal/store"
)

// The working copy, as a source (D2).
//
// `je dev` points the control plane at a directory on its own machine and runs
// a job out of it. Everything after that is the ordinary path: the same
// dispatch, the same worker, the same environment, the same secrets decrypted
// from the same file, the same logs and events and cursor.
//
// That sameness is the entire design. The first answer to this problem was a
// harness in the CLI that ran the job itself, and it reimplemented four things
// the worker already did -- dependency preparation, the environment, the
// executor call, the log sink. One of them drifted before the release that
// shipped it was a day old. D20/C11 withdrew the daemonless path for exactly
// that reason, and a development harness is not exempt from it: a tool that
// tells you your job works has to be running your job the way the engine will.

// ErrNotLocal means the control plane cannot see the directory being offered,
// which on any split deployment it cannot.
var ErrNotLocal = errors.New("this control plane cannot read that directory")

// RegisterDev points the `dev` source at a directory and loads it.
//
// The whole directory, not one job: a source loads atomically (D19), chains
// resolve against the jobs beside them, and a repository with one unparseable
// file is a repository that does not load. Reproducing that here is the point
// -- the failure you get while writing should be the failure you would get on
// push, not a friendlier subset of it.
func (e *Engine) RegisterDev(ctx context.Context, dir string) (LoadResult, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return LoadResult{}, err
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		// The honest failure for a split deployment, named rather than
		// discovered three layers down as a job that cannot find its code.
		return LoadResult{}, fmt.Errorf(
			"%w: %s.\n"+
				"`je dev` runs the definitions in a directory, so the control plane has "+
				"to be on the machine holding them -- try `je up` here",
			ErrNotLocal, abs)
	}

	src := store.Source{
		Name:     store.DevSourceName,
		Kind:     store.SourceKindDev,
		Location: abs,
	}
	if err := e.store.UpsertSource(ctx, src); err != nil {
		return LoadResult{}, err
	}

	result, err := e.loadRegistered(ctx, src)
	if err != nil {
		_ = e.store.RecordSourceSync(ctx, src.Name, "", err.Error())
		return LoadResult{}, err
	}
	if err := e.store.RecordSourceSync(ctx, src.Name, "", ""); err != nil {
		return LoadResult{}, err
	}
	if err := e.reloadRoutes(ctx); err != nil {
		return LoadResult{}, err
	}
	e.requestScheduleReload(ctx)
	return result, nil
}

// devSource loads a working copy.
//
// No revision, because a working copy has none. What ran is still recorded --
// the run pins the definition hash like any other (D11) -- but the code it
// invoked is whatever was on the disk at the time, which is the honest answer
// for a tree nobody has committed and the reason this is not a source you can
// serve production from.
func (e *Engine) devSource(src store.Source) jobdef.FSSource {
	return jobdef.FSSource{FS: os.DirFS(src.Location), Root: ".", Name: src.Location}
}
