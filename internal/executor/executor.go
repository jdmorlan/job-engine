// Package executor runs a job's command.
//
// D1 puts two implementations behind one interface -- `process` (default, zero
// dependencies, one-second feedback loop) and `container` -- with an identical
// job protocol either way, so promoting a job from one to the other is a
// one-line change to its definition.
//
// Only the process executor exists today. The interface is here anyway because
// D1 agreed the shape and the second implementation is specified, not imagined.
package executor

import (
	"context"
	"time"
)

// Spec is everything needed to run one attempt. It is deliberately a plain
// value: an executor gets no access to the store, the engine, or the database.
type Spec struct {
	Command []string
	Workdir string

	// Env is the complete environment for the process. D10 requires it to be
	// exactly what the engine decided, not an inheritance of the daemon's own
	// environment -- a job must not pick up credentials by accident.
	Env []string

	Timeout time.Duration

	// Grace is how long the process gets between SIGTERM and SIGKILL (D6).
	Grace time.Duration

	// Output receives captured lines as they are produced.
	Output LineWriter
}

// Stream identifies which pipe a line came from.
type Stream string

const (
	StreamStdout Stream = "stdout"
	StreamStderr Stream = "stderr"
)

// LineWriter receives one captured line at a time.
//
// Line-at-a-time rather than an io.Writer because D6 promises timestamped,
// individually stored lines, and an io.Writer would hand us arbitrary chunk
// boundaries that split lines across writes.
type LineWriter interface {
	WriteLine(stream Stream, ts time.Time, line string)
}

// Result is how an attempt ended.
type Result struct {
	// ExitCode is the process's exit status. Nil when the process never
	// started or was killed before reporting one.
	ExitCode *int

	// TimedOut distinguishes D8's `timed_out` from an ordinary failure. They
	// are separate states because they call for different responses.
	TimedOut bool

	// Killed reports that the grace period expired and we sent SIGKILL, which
	// means the job did not get to clean up after itself.
	Killed bool

	StartedAt time.Time
	EndedAt   time.Time
}

// Succeeded reports the verdict. D6: the exit code is the verdict, and zero is
// the only success.
func (r Result) Succeeded() bool {
	return !r.TimedOut && r.ExitCode != nil && *r.ExitCode == 0
}

// Executor runs one attempt to completion.
//
// An error means the executor could not run the command at all -- a missing
// binary, an unreadable workdir. A command that ran and failed is not an error;
// it is a Result with a non-zero exit code. Keeping those distinct is what lets
// the engine tell "your job is broken" from "the engine is broken".
type Executor interface {
	Run(ctx context.Context, spec Spec) (Result, error)
	Name() string
}
