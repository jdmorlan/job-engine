package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `je try` is an authoring harness, not the daemonless path D20 removed. These
// tests are mostly about what it does NOT do: no control plane, no database, no
// run, no cursor -- and the engine's own checks rather than a second opinion.

func writeJob(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTryRunsAJobAndSaysWhatWouldBeCommitted(t *testing.T) {
	env, out := demoEnv(t)
	dir := t.TempDir()
	writeJob(t, dir, "hello", `
command: ["/bin/sh", "-c", "echo working; echo '{\"since\":\"2026-09-04T00:00:00Z\"}' > \"$JOB_STATE_OUT_FILE\"; echo '{\"rows\":3}' > \"$JOB_OUTPUT_FILE\"; echo '{\"type\":\"done\",\"payload\":{}}' >> \"$JOB_EVENTS_FILE\""]
`)
	if err := runTry(context.Background(), env, []string{"--dir", dir, "hello"}); err != nil {
		t.Fatalf("je try: %v", err)
	}
	text := out.String()
	for _, want := range []string{"working", "would have succeeded", "cursor", "2026-09-04", "rows", "done"} {
		if !strings.Contains(text, want) {
			t.Errorf("output does not mention %q:\n%s", want, text)
		}
	}
}

// The failure this command exists to catch early: a job that exits zero and
// breaks the contract. In a real run that is a failed run with a message you
// have to go and read; here it is the thing on your screen.
func TestTryCatchesAContractBreakOnASuccessfulExit(t *testing.T) {
	env, out := demoEnv(t)
	dir := t.TempDir()
	writeJob(t, dir, "sloppy", `
command: ["/bin/sh", "-c", "echo '[1,2,3]' > \"$JOB_STATE_OUT_FILE\""]
`)
	err := runTry(context.Background(), env, []string{"--dir", dir, "sloppy"})
	if err == nil {
		t.Fatal("a job that wrote a JSON array as its cursor was reported as fine")
	}
	if !strings.Contains(out.String(), "broke the protocol") {
		t.Errorf("output does not explain what is wrong:\n%s", out.String())
	}
}

// D14's rule, visible before anybody runs the job for real: a failed run
// commits nothing, so the harness must not report a cursor it wrote.
func TestTryReportsNoCommitForAFailedJob(t *testing.T) {
	env, out := demoEnv(t)
	dir := t.TempDir()
	writeJob(t, dir, "doomed", `
command: ["/bin/sh", "-c", "echo '{\"since\":\"2099-01-01T00:00:00Z\"}' > \"$JOB_STATE_OUT_FILE\"; exit 4"]
`)
	if err := runTry(context.Background(), env, []string{"--dir", dir, "doomed"}); err == nil {
		t.Fatal("a failing job was reported as fine")
	}
	text := out.String()
	if !strings.Contains(text, "exited 4") {
		t.Errorf("output does not say how it failed:\n%s", text)
	}
	if !strings.Contains(text, "nothing would be committed") {
		t.Errorf("output does not say the cursor stays put:\n%s", text)
	}
	if strings.Contains(text, "2099") {
		t.Error("the harness reported a cursor from a run that failed")
	}
}

// The definition goes through the same parser the engine uses, so a file that
// would not load fails here rather than after a push and a sync.
func TestTryRefusesADefinitionTheEngineWouldRefuse(t *testing.T) {
	env, _ := demoEnv(t)
	dir := t.TempDir()
	writeJob(t, dir, "broken", "overlap: sometimes\ncommand: [\"true\"]\n")
	err := runTry(context.Background(), env, []string{"--dir", dir, "broken"})
	if err == nil || !strings.Contains(err.Error(), "overlap must be") {
		t.Errorf("error = %v, want the engine's own validation", err)
	}
}

func TestTryChecksItsFlagsBeforeRunningAnything(t *testing.T) {
	env, _ := demoEnv(t)
	dir := t.TempDir()
	writeJob(t, dir, "hello", "command: [\"/bin/sh\", \"-c\", \"echo ran > "+
		filepath.Join(dir, "evidence")+"\"]\n")

	err := runTry(context.Background(), env, []string{"--dir", dir, "--state", "not json", "hello"})
	if err == nil || !strings.Contains(err.Error(), "JSON object") {
		t.Errorf("error = %v, want a complaint about --state", err)
	}
	// A typo in a flag should not cost a job execution to discover.
	if _, err := os.Stat(filepath.Join(dir, "evidence")); err == nil {
		t.Error("the job ran despite a bad flag")
	}
}

func TestTryNamesTheFileItCouldNotFind(t *testing.T) {
	env, _ := demoEnv(t)
	err := runTry(context.Background(), env, []string{"--dir", t.TempDir(), "missing"})
	if err == nil || !strings.Contains(err.Error(), "missing.yaml") {
		t.Errorf("error = %v, want it to name the path it looked at", err)
	}
}
