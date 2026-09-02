//go:build unix

// Package lockfile provides the exclusive, advisory, whole-data-directory lock
// the daemon holds for its lifetime.
//
// This is a correctness mechanism, not hygiene. Two daemons sharing one SQLite
// file each run their own schedule loop, so every job fires twice -- and the
// symptom (duplicate work, cursors leapfrogging) shows up far away from the
// cause. D18 moved this from theoretical to likely: if Almanac starts a daemon
// and `je install` has also started one, that is two.
//
// The lock is a flock, which the kernel releases when the process exits by any
// means, including SIGKILL and a panic. That property is the whole reason to
// use flock rather than a pidfile we would have to clean up ourselves: there is
// no such thing as a stale flock.
package lockfile

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// ErrHeld is returned when another process already holds the lock.
// Callers should surface Lock.HolderPID alongside it -- "something else has
// it" is not an actionable message, "pid 4821 has it" is (P1).
var ErrHeld = errors.New("lock is held by another process")

// Lock is an acquired flock. The zero value is not usable; call Acquire.
type Lock struct {
	file *os.File
}

// Acquire takes the lock without blocking.
//
// On failure with ErrHeld, pid is the process that holds it, or 0 if we could
// not determine it (the holder may not have written its pid yet).
func Acquire(path string) (*Lock, int, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, 0, fmt.Errorf("opening lock file: %w", err)
	}

	// LOCK_NB makes this fail immediately rather than blocking forever. A
	// daemon that silently waits for another daemon to exit is worse than one
	// that refuses to start and says why.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		pid := readPID(f)
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, pid, ErrHeld
		}
		return nil, pid, fmt.Errorf("locking %s: %w", path, err)
	}

	// We hold it. Record who we are, for the benefit of whoever fails next.
	if err := f.Truncate(0); err != nil {
		f.Close()
		return nil, 0, fmt.Errorf("truncating lock file: %w", err)
	}
	if _, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0); err != nil {
		f.Close()
		return nil, 0, fmt.Errorf("writing pid to lock file: %w", err)
	}
	return &Lock{file: f}, os.Getpid(), nil
}

// Release drops the lock. Closing the file is what releases the flock; the
// explicit Flock(LOCK_UN) is for clarity about the intent.
func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	err := l.file.Close()
	l.file = nil
	return err
}

func readPID(f *os.File) int {
	buf := make([]byte, 32)
	n, _ := f.ReadAt(buf, 0)
	pid, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil {
		return 0
	}
	return pid
}
