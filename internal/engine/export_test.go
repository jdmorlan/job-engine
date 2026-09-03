package engine

import (
	"context"
	"time"

	"github.com/jdmorlan/job-engine/internal/store"
)

// Abandon releases the engine's resources the way process death would: the
// store is closed and the lock is dropped, but engine.stopped is never
// written. That is the state a crash, a SIGKILL, or a lost battery leaves
// behind, and Start must be able to recognise it.
//
// It lives in a _test.go file so that "abandon without recording a stop" is
// not something the real API offers. There is no legitimate caller.
func Abandon(e *Engine) error {
	if err := e.store.Close(); err != nil {
		return err
	}
	return e.lock.Release()
}

// SetLastWindowForTest rewinds a schedule's position on its grid, which is how
// tests simulate the machine having been asleep. There is no legitimate
// non-test caller: moving a schedule backwards outside a test is how you fire
// a thousand runs by accident.
func SetLastWindowForTest(e *Engine, jobID int64, index int, window time.Time) error {
	return e.store.SetLastWindow(context.Background(), jobID, index, window)
}

// SetGitHubBaseURLForTest points fetches at a stub server.
//
// Here rather than on Options, because the real API's address is not something
// this engine is configured with -- it is where GitHub is. A knob for it in the
// product would be a knob nobody should turn.
func SetGitHubBaseURLForTest(e *Engine, url string) { e.githubBaseURL = url }

// StaleWorkerForTest rewrites a registered worker's version, which is the one
// state the real API cannot produce: RegisterWorker refuses skew, so a worker
// only ever arrives at the right version. What this simulates is the control
// plane being upgraded underneath a worker that is already registered and
// still polling -- C10's actual failure mode, and the reason Claim has to
// check as well as Register (D24).
func StaleWorkerForTest(e *Engine, w store.Worker, version string) error {
	w.Version = version
	_, err := e.store.RegisterWorker(context.Background(), w)
	return err
}
