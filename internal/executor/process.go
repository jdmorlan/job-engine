//go:build unix

package executor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Process runs a job as a child process. It is D1's default executor: zero
// dependencies and a one-second feedback loop, with no isolation -- documented,
// not mitigated.
type Process struct{}

func (Process) Name() string { return "process" }

// DefaultGrace is how long a job gets to shut down after SIGTERM.
const DefaultGrace = 10 * time.Second

// maxLineBytes caps a single captured line. A job that emits a 500MB line
// without a newline should not be able to exhaust the engine's memory; it gets
// truncated and told so.
const maxLineBytes = 1 << 20

// Run executes the command, streaming its output, and enforces the timeout.
func (p Process) Run(ctx context.Context, spec Spec) (Result, error) {
	if len(spec.Command) == 0 {
		return Result{}, errors.New("no command to run")
	}
	grace := spec.Grace
	if grace <= 0 {
		grace = DefaultGrace
	}

	// Two separate contexts, and the distinction matters. runCtx carries the
	// job's timeout; ctx is the engine shutting down. Both must stop the
	// process, but only the first is a `timed_out` verdict (D8).
	runCtx := ctx
	var cancelTimeout context.CancelFunc
	if spec.Timeout > 0 {
		runCtx, cancelTimeout = context.WithTimeout(ctx, spec.Timeout)
		defer cancelTimeout()
	}

	cmd := exec.Command(spec.Command[0], spec.Command[1:]...)
	cmd.Dir = spec.Workdir
	cmd.Env = spec.Env

	// Resolve the program against the PATH this job was given, not the one
	// this process happens to have.
	//
	// os/exec resolves Command[0] when the command is built, using the
	// *parent's* PATH -- setting cmd.Env afterwards does not change what it
	// found. So a job whose dependencies put `tsx` in its own node_modules/.bin
	// fails with "executable file not found" while the binary is sitting right
	// there (D28). Anything already a path is left alone, and a program that is
	// genuinely absent still fails, with the same message it would have.
	if resolved, err := lookPathIn(spec.Command[0], spec.Env); err == nil {
		cmd.Path = resolved
		// exec.Command records the failed lookup in cmd.Err, and Start returns
		// that error even when Path has since been set. Clearing it is the
		// other half of the fix, and without it the resolution above is silent
		// and useless.
		cmd.Err = nil
	}

	// Put the child in its own process group so that killing it kills its
	// children too. Without this, `npx tsx script.ts` leaves the actual node
	// process running after a timeout -- the shape of bug where a job "times
	// out" every hour and quietly keeps a dozen orphans alive.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return Result{}, err
	}

	result := Result{StartedAt: time.Now()}
	if err := cmd.Start(); err != nil {
		// The command could not be started at all. That is an engine-visible
		// problem with the definition, not a job failure.
		return Result{}, fmt.Errorf("starting %s: %w", spec.Command[0], err)
	}

	// Capture both pipes concurrently. They must be drained even if we are
	// about to kill the process: a child blocked writing to a full pipe will
	// not respond to SIGTERM.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); scan(stdout, StreamStdout, spec.Output) }()
	go func() { defer wg.Done(); scan(stderr, StreamStderr, spec.Output) }()

	// Signal escalation runs alongside the wait. SIGTERM, then a grace period,
	// then SIGKILL to the whole process group (D6).
	waitDone := make(chan struct{})
	go func() {
		select {
		case <-waitDone:
			return
		case <-runCtx.Done():
		}
		signalGroup(cmd, syscall.SIGTERM)
		select {
		case <-waitDone:
		case <-time.After(grace):
			signalGroup(cmd, syscall.SIGKILL)
			result.Killed = true
		}
	}()

	// Wait for the pipes to close before reaping, so no output is lost.
	wg.Wait()
	waitErr := cmd.Wait()
	close(waitDone)

	result.EndedAt = time.Now()
	result.TimedOut = spec.Timeout > 0 && errors.Is(runCtx.Err(), context.DeadlineExceeded)

	if code, ok := exitCode(waitErr); ok {
		result.ExitCode = &code
	} else if waitErr != nil && !result.TimedOut && ctx.Err() == nil {
		return result, fmt.Errorf("waiting for %s: %w", spec.Command[0], waitErr)
	}
	return result, nil
}

// signalGroup sends a signal to the child's whole process group.
//
// The negative pid is the Unix convention for "this process group". It is
// wrapped in a nil check because the process may have exited between the
// select firing and this call.
func signalGroup(cmd *exec.Cmd, sig syscall.Signal) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, sig)
}

// exitCode extracts a process's exit status from Wait's error.
func exitCode(err error) (int, bool) {
	if err == nil {
		return 0, true
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), true
	}
	return 0, false
}

// scan reads lines from a pipe and hands each to the writer with the time it
// was observed.
//
// The timestamp is taken here, at read time, rather than at write time, because
// the difference between "the job printed this at 04:03" and "the engine got
// around to storing it at 04:07" is exactly the kind of gap that makes a
// timeline untrustworthy.
func scan(r io.Reader, stream Stream, out LineWriter) {
	if out == nil {
		_, _ = io.Copy(io.Discard, r) // still drain, or the child blocks
		return
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	for sc.Scan() {
		out.WriteLine(stream, time.Now(), sc.Text())
	}
	if err := sc.Err(); err != nil {
		// A line over the cap, or a broken pipe. Report it as job output rather
		// than swallowing it -- the author needs to know their output was cut.
		out.WriteLine(StreamStderr, time.Now(),
			fmt.Sprintf("je: output capture stopped: %v", err))
	}
}

// lookPathIn finds a program using a PATH taken from the environment the job
// will actually run with.
func lookPathIn(program string, env []string) (string, error) {
	if strings.ContainsRune(program, os.PathSeparator) {
		return "", errors.New("already a path")
	}
	var path string
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v // last one wins, as it does for a process
		}
	}
	if path == "" {
		return "", errors.New("no PATH in the job's environment")
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, program)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() &&
			info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", exec.ErrNotFound
}
