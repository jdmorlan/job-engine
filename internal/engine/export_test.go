package engine

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
