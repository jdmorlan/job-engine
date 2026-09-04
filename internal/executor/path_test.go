package executor

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A program is found on the PATH the job was given, not the one this process
// happens to have.
//
// Worth a test because the failure is invisible in review: os/exec resolves
// Command[0] when the command is *built*, using the parent's PATH, and setting
// cmd.Env afterwards does not change what it found. A job whose dependencies
// put a binary in its own tree therefore failed with "executable file not found
// in $PATH" while the binary sat right there (D28). It took two fixes -- Path
// and Err -- and the first alone looked correct and did nothing.
func TestAProgramIsFoundOnTheJobsOwnPath(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "only-here")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho found me\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	result, err := Process{}.Run(context.Background(), Spec{
		Command: []string{"only-here"},
		Workdir: dir,
		// Deliberately the only entry: if resolution used the parent's PATH
		// this cannot possibly work, and if it silently fell back to the
		// parent's PATH the test would be meaningless.
		Env:    []string{"PATH=" + dir},
		Output: writerSink{&out},
	})
	if err != nil {
		t.Fatalf("running a program from the job's own PATH: %v", err)
	}
	if result.ExitCode == nil || *result.ExitCode != 0 {
		t.Errorf("exit code = %v, want 0", result.ExitCode)
	}
	if got := out.String(); got == "" {
		t.Error("the program produced no output, so it probably did not run")
	}
}

// And a program that is genuinely absent still fails, rather than the
// resolution above quietly turning a missing binary into something else.
func TestAMissingProgramStillFails(t *testing.T) {
	_, err := Process{}.Run(context.Background(), Spec{
		Command: []string{"definitely-not-a-real-program"},
		Workdir: t.TempDir(),
		Env:     []string{"PATH=" + t.TempDir()},
		Output:  writerSink{&bytes.Buffer{}},
	})
	if err == nil {
		t.Fatal("a missing program did not fail")
	}
}

// writerSink collects the lines a job produced.
type writerSink struct{ buf *bytes.Buffer }

func (w writerSink) WriteLine(_ Stream, _ time.Time, line string) {
	w.buf.WriteString(line)
	w.buf.WriteString("\n")
}
