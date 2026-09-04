package engine_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jdmorlan/job-engine/internal/engine"
	"github.com/jdmorlan/job-engine/internal/model"
	"github.com/jdmorlan/job-engine/internal/store"
)

// TestControlPlaneRunsNothingWithoutAWorker is C11 stated as a test.
//
// It is the constraint the whole of D20 rests on, and the one most likely to be
// quietly relaxed later for convenience -- so it is pinned here rather than
// left as a property that happens to hold.
func TestControlPlaneRunsNothingWithoutAWorker(t *testing.T) {
	ctx := context.Background()
	e, _ := jobFixture(t, "orphan", `echo hi`)

	run, err := e.TriggerRun(ctx, qual("orphan"), engine.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// No worker registered. Give the control plane every chance to execute it.
	time.Sleep(300 * time.Millisecond)

	after, err := e.Run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != model.StatusQueued {
		t.Fatalf("status = %s, want queued: the control plane executed a job", after.Status)
	}
}

// TestUnservableWorkIsVisibleNotSilent is C8. A run queued for a label nothing
// serves is the failure mode the split introduced, and it must never look like
// ordinary queueing.
func TestUnservableWorkIsVisibleNotSilent(t *testing.T) {
	ctx := context.Background()
	e, _ := jobFixture(t, "macjob", `echo hi`, "runs_on: macos")

	if _, err := e.TriggerRun(ctx, qual("macjob"), engine.RunOptions{}); err != nil {
		t.Fatal(err)
	}

	waiting, err := e.Waiting(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(waiting.Unservable) != 1 || waiting.Unservable[0].Label != "macos" {
		t.Fatalf("unservable = %+v, want one entry for macos", waiting.Unservable)
	}
	if !waiting.NeedsAttention() {
		t.Error("work nobody can take did not need attention")
	}
}

// TestJobsArePinnedNotPlaced is C3: a worker sees only the runs whose label it
// advertises, so there is no placement decision to get wrong.
func TestJobsArePinnedNotPlaced(t *testing.T) {
	ctx := context.Background()
	e, _ := jobFixture(t, "macjob", `echo hi`, "runs_on: macos")

	generalist, err := e.RegisterWorker(ctx, store.Worker{
		ID: "general", Name: "general",
		Labels: []string{store.DefaultLabel}, Roles: []string{store.RoleExecute},
		Version: e.Health(ctx).Version,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := e.TriggerRun(ctx, qual("macjob"), engine.RunOptions{}); err != nil {
		t.Fatal(err)
	}

	dispatch, err := e.Claim(ctx, generalist.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dispatch != nil {
		t.Fatal("a worker without the macos label was handed a macos job")
	}

	mac, err := e.RegisterWorker(ctx, store.Worker{
		ID: "mac", Name: "mac",
		Labels: []string{"macos"}, Roles: []string{store.RoleExecute},
		Version: e.Health(ctx).Version,
	})
	if err != nil {
		t.Fatal(err)
	}
	dispatch, err = e.Claim(ctx, mac.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dispatch == nil {
		t.Fatal("the worker advertising macos was not given the job")
	}
}

// TestDuplicateLabelIsRefused answers D20's second open question.
//
// An accidental second laptop is far likelier than a deliberate pair, and the
// cost of guessing wrong is a job running on the wrong machine at 3am.
func TestDuplicateLabelIsRefused(t *testing.T) {
	ctx := context.Background()
	e, _ := jobFixture(t, "any", `echo hi`)

	first := store.Worker{
		ID: "one", Name: "one", Labels: []string{"macos"},
		Roles: []string{store.RoleExecute}, Version: e.Health(ctx).Version,
	}
	if _, err := e.RegisterWorker(ctx, first); err != nil {
		t.Fatal(err)
	}

	second := first
	second.ID, second.Name = "two", "two"
	_, err := e.RegisterWorker(ctx, second)
	if err == nil {
		t.Fatal("a second worker claimed a label that was already served")
	}
	if !strings.Contains(err.Error(), "already advertises") {
		t.Errorf("error = %v", err)
	}

	// Re-registering under the same id is a restart, not a conflict.
	if _, err := e.RegisterWorker(ctx, first); err != nil {
		t.Errorf("a restarting worker could not rejoin: %v", err)
	}
}

// TestVersionSkewIsRefusedLoudly is C10.
func TestVersionSkewIsRefusedLoudly(t *testing.T) {
	ctx := context.Background()
	e, _ := jobFixture(t, "any", `echo hi`)

	_, err := e.RegisterWorker(ctx, store.Worker{
		ID: "old", Name: "old", Labels: []string{"other"},
		Roles: []string{store.RoleExecute}, Version: "v0.0.1-ancient",
	})
	if err == nil {
		t.Fatal("a worker on a different version was admitted")
	}
	if !strings.Contains(err.Error(), "version") {
		t.Errorf("error = %v, want it to say which versions disagree", err)
	}
}

// TestAStaleWorkerIsRefusedWorkNotJustRegistration is the other half of C10,
// and the half that was missing.
//
// Registration checks the version once, at startup. A worker that was already
// running when the control plane upgraded never registers again, so before this
// it kept claiming and executing at its old version indefinitely -- the exact
// silent incompatibility C10 exists to prevent, and a real one: a v0.3.x worker
// knows nothing about the source revisions a v0.4 dispatch carries.
func TestAStaleWorkerIsRefusedWorkNotJustRegistration(t *testing.T) {
	ctx := context.Background()
	e, _ := jobFixture(t, "any", `echo hi`)

	w, err := e.RegisterWorker(ctx, store.Worker{
		ID: "w1", Name: "w1", Labels: []string{store.DefaultLabel},
		Roles: []string{store.RoleExecute}, Version: e.Health(ctx).Version,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.TriggerRun(ctx, qual("any"), engine.RunOptions{}); err != nil {
		t.Fatal(err)
	}

	// The upgrade happens under it.
	if err := engine.StaleWorkerForTest(e, w, "v0.0.1-ancient"); err != nil {
		t.Fatal(err)
	}

	dispatch, err := e.Claim(ctx, "w1")
	if err == nil {
		t.Fatal("a worker on an old version was handed work")
	}
	if dispatch != nil {
		t.Error("a refused claim still returned a dispatch")
	}
	if !errors.Is(err, engine.ErrVersionSkew) {
		t.Errorf("error = %v, want ErrVersionSkew so the API can answer 409", err)
	}
	if !strings.Contains(err.Error(), "v0.0.1-ancient") {
		t.Errorf("error = %v, want it to name the worker's version", err)
	}
}

// The refusal has to leave a trace. D24 settled that a worker lifecycle
// operation is not a job -- but it still owes the timeline a record, and a
// worker polling every few seconds must not write one per poll.
func TestRefusingAWorkerIsRecordedOnce(t *testing.T) {
	ctx := context.Background()
	e, _ := jobFixture(t, "any", `echo hi`)

	w, err := e.RegisterWorker(ctx, store.Worker{
		ID: "w1", Name: "w1", Labels: []string{store.DefaultLabel},
		Roles: []string{store.RoleExecute}, Version: e.Health(ctx).Version,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.StaleWorkerForTest(e, w, "v0.0.1-ancient"); err != nil {
		t.Fatal(err)
	}

	for range 5 {
		if _, err := e.Claim(ctx, "w1"); err == nil {
			t.Fatal("expected the claim to be refused")
		}
	}

	events, err := e.RecentEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	refusals := 0
	for _, ev := range events {
		if ev.Type == engine.EventWorkerRefused {
			refusals++
			if !strings.Contains(string(ev.Payload), "v0.0.1-ancient") {
				t.Errorf("payload = %s, want the worker's version in it", ev.Payload)
			}
		}
	}
	if refusals != 1 {
		t.Errorf("five refused claims recorded %d events, want exactly 1", refusals)
	}
}

// TestExpiredLeaseIsLostNotFailed is C6, and the naming matters.
//
// We do not know the job failed -- only that we stopped hearing about it. `lost`
// says that; `failed` would assert something untrue in the one place a person
// goes to find out what happened.
func TestExpiredLeaseIsLostNotFailed(t *testing.T) {
	ctx := context.Background()

	// A clock the test controls, so the lease can expire without waiting.
	now := time.Now()
	clock := func() time.Time { return now }
	e, _ := jobFixtureAt(t, "vanisher", `echo hi`, clock)

	workerID := ensureWorker(t, e)
	run, err := e.TriggerRun(ctx, qual("vanisher"), engine.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Claim(ctx, workerID); err != nil {
		t.Fatal(err)
	}

	// The worker goes away without ever reporting.
	now = now.Add(engine.LeaseTTL + time.Minute)

	n, err := e.ExpireLeases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expired %d leases, want 1", n)
	}

	after, err := e.Run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != model.StatusLost {
		t.Errorf("status = %s, want lost", after.Status)
	}
	if !strings.Contains(after.Error, "stopped responding") {
		t.Errorf("error = %q, want it to say what we actually know", after.Error)
	}

	events, err := e.RecentEvents(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	var sawLost bool
	for _, ev := range events {
		if ev.Type == engine.EventRunLost {
			sawLost = true
		}
	}
	if !sawLost {
		t.Error("a lost run left no event; the gap in the timeline is the bug")
	}
}

// TestFencingRejectsALateResult is C7.
//
// The window C6 describes cannot be closed, but it can be bounded: a worker
// that reappears after its lease expired must not be able to write state into a
// run the control plane has already given up on.
func TestFencingRejectsALateResult(t *testing.T) {
	ctx := context.Background()

	now := time.Now()
	clock := func() time.Time { return now }
	e, _ := jobFixtureAt(t, "slowpoke", `echo hi`, clock)

	workerID := ensureWorker(t, e)
	run, err := e.TriggerRun(ctx, qual("slowpoke"), engine.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	dispatch, err := e.Claim(ctx, workerID)
	if err != nil {
		t.Fatal(err)
	}

	now = now.Add(engine.LeaseTTL + time.Minute)
	if _, err := e.ExpireLeases(ctx); err != nil {
		t.Fatal(err)
	}

	// The worker comes back and tries to report success.
	exit := 0
	err = e.Complete(ctx, dispatch.RunID, workerID, engine.Completion{
		Result:   executorSuccess(&exit),
		StateOut: []byte(`{"since":"2026-01-01T00:00:00Z"}`),
	})
	if err == nil {
		t.Fatal("a revoked worker was allowed to report a result")
	}

	after, err := e.Run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != model.StatusLost {
		t.Errorf("status = %s, want the run to stay lost", after.Status)
	}

	// The decisive check: the cursor must not have moved.
	job, err := e.Job(ctx, "slowpoke")
	if err != nil {
		t.Fatal(err)
	}
	history, err := e.StateHistory(ctx, job.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range history {
		if v.SetByRun != nil && *v.SetByRun == run.ID {
			t.Fatal("a fenced worker committed a cursor after its run was lost")
		}
	}
}

// TestHeartbeatRevokesRunsTheWorkerNoLongerHolds is the other half of C7: the
// worker learns its claim is gone and can stop, rather than finding out by
// having its result refused after the job has already run to completion.
func TestHeartbeatRevokesRunsTheWorkerNoLongerHolds(t *testing.T) {
	ctx := context.Background()

	now := time.Now()
	clock := func() time.Time { return now }
	e, _ := jobFixtureAt(t, "holder", `echo hi`, clock)

	workerID := ensureWorker(t, e)
	if _, err := e.TriggerRun(ctx, qual("holder"), engine.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	dispatch, err := e.Claim(ctx, workerID)
	if err != nil {
		t.Fatal(err)
	}

	// Still held: nothing revoked.
	revoked, err := e.Heartbeat(ctx, workerID, []int64{dispatch.RunID})
	if err != nil {
		t.Fatal(err)
	}
	if len(revoked) != 0 {
		t.Fatalf("revoked = %v on a healthy heartbeat", revoked)
	}

	now = now.Add(engine.LeaseTTL + time.Minute)
	if _, err := e.ExpireLeases(ctx); err != nil {
		t.Fatal(err)
	}

	revoked, err = e.Heartbeat(ctx, workerID, []int64{dispatch.RunID})
	if err != nil {
		t.Fatal(err)
	}
	if len(revoked) != 1 || revoked[0] != dispatch.RunID {
		t.Errorf("revoked = %v, want run %d", revoked, dispatch.RunID)
	}
}

// TestSecretsReachTheWorkerAndOnlyDeclaredOnes is D10 across the seam.
//
// The Dispatch carries resolved values because the worker has to build the
// process environment. What it must not carry is anything the job did not
// declare -- adding a token for one job cannot widen what another can read,
// and that rule has to survive being serialised onto the wire.
func TestSecretsReachTheWorkerAndOnlyDeclaredOnes(t *testing.T) {
	ctx := context.Background()
	e, _ := jobFixture(t, "needy", `echo hi`, "secrets: [DECLARED_TOKEN]")

	if err := e.Secrets().Set("DECLARED_TOKEN", "sk-declared-123456"); err != nil {
		t.Fatal(err)
	}
	if err := e.Secrets().Set("OTHER_TOKEN", "sk-other-9999999"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	workerID := ensureWorker(t, e)
	if _, err := e.TriggerRun(ctx, qual("needy"), engine.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	dispatch, err := e.Claim(ctx, workerID)
	if err != nil {
		t.Fatal(err)
	}

	env := strings.Join(dispatch.Env, "\n")
	if !strings.Contains(env, "DECLARED_TOKEN=sk-declared-123456") {
		t.Error("the declared secret did not reach the worker")
	}
	if strings.Contains(env, "sk-other-9999999") {
		t.Error("an undeclared secret was sent to the worker")
	}
}

// TestDispatchOmitsTheChannelPathsTheWorkerOwns pins the D6 split.
//
// The three output channels and JOB_WORKDIR are added by the worker, on the
// machine where the files will exist. If the control plane sent paths from its
// own filesystem the job would write into a directory that is not there.
func TestDispatchOmitsTheChannelPathsTheWorkerOwns(t *testing.T) {
	ctx := context.Background()
	e, _ := jobFixture(t, "plain", `echo hi`)

	workerID := ensureWorker(t, e)
	if _, err := e.TriggerRun(ctx, qual("plain"), engine.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	dispatch, err := e.Claim(ctx, workerID)
	if err != nil {
		t.Fatal(err)
	}

	env := strings.Join(dispatch.Env, "\n")
	for _, name := range []string{
		"JOB_WORKDIR=", "JOB_STATE_OUT_FILE=", "JOB_OUTPUT_FILE=", "JOB_EVENTS_FILE=",
	} {
		if strings.Contains(env, name) {
			t.Errorf("%s came from the control plane; the worker owns that path", name)
		}
	}
	// The rest of the protocol must be there.
	for _, name := range []string{"JOB_ID=", "RUN_ID=", "ATTEMPT=", "JE_STATE="} {
		if !strings.Contains(env, name) {
			t.Errorf("%s is missing from the dispatch", name)
		}
	}
}

// TestSyncPicksUpANewJobWithoutARestart is the gap the split made painful.
//
// Restarting to pick up a YAML edit was tolerable when the engine ran in your
// terminal. Once it is a container somewhere else, a restart costs every
// in-flight run for a change that touches none of them.
func TestSyncPicksUpANewJobWithoutARestart(t *testing.T) {
	ctx := context.Background()
	e, _ := jobFixture(t, "first", `echo one`)

	jobs, err := e.Jobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("started with %d jobs, want 1", len(jobs))
	}

	writeJob(t, treeDir(e), "second", `echo two`)

	result, err := e.Sync(ctx)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result.Loaded != 2 {
		t.Errorf("loaded = %d, want 2", result.Loaded)
	}

	if jobs, err = e.Jobs(ctx); err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Errorf("after sync there are %d jobs, want 2", len(jobs))
	}

	// P1: the reload is in the timeline, so "why did this job appear at 3am?"
	// has an answer without a log file somebody has to still have.
	events, err := e.RecentEvents(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	var synced bool
	for _, ev := range events {
		if ev.Type == engine.EventDefinitionsSynced {
			synced = true
		}
	}
	if !synced {
		t.Error("a sync left no event")
	}
}

// TestSyncIsAtomic is D19's rule: one unparseable file rejects the whole sync
// and the last good state keeps serving.
//
// The alternative -- applying the files that happened to parse -- leaves the
// engine running a configuration that exists in no commit and that no file
// describes, which is the state you cannot reason about at 2am.
func TestSyncIsAtomic(t *testing.T) {
	ctx := context.Background()
	e, _ := jobFixture(t, "good", `echo fine`)

	// A new valid job and a broken one land together.
	writeJob(t, treeDir(e), "alsogood", `echo also fine`)
	if err := os.WriteFile(
		filepath.Join(treeDir(e), "broken.yaml"),
		[]byte("command: [\"/bin/sh\"]\nthis_is: not a field\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := e.Sync(ctx); err == nil {
		t.Fatal("a sync containing an unparseable file succeeded")
	}

	jobs, err := e.Jobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Slug != qual("good") {
		t.Errorf("jobs = %+v, want only the previously loaded one", jobs)
	}
}

// TestSyncTombstonesRatherThanDeletes is D19: deleting a file stops future runs
// and never erases history, because reverting a commit must not lose the
// timeline.
func TestSyncTombstonesRatherThanDeletes(t *testing.T) {
	ctx := context.Background()
	e, _ := jobFixture(t, "doomed", `echo hi`)

	if _, err := runJob(t, e, "doomed", engine.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	job, err := e.Job(ctx, "doomed")
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(filepath.Join(treeDir(e), "doomed.yaml")); err != nil {
		t.Fatal(err)
	}
	result, err := e.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Removed != 1 {
		t.Errorf("removed = %d, want 1", result.Removed)
	}

	runs, err := e.Runs(ctx, job.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Errorf("a deleted job lost its history: %d runs remain", len(runs))
	}
}

func writeJob(t *testing.T, dir, name, script string) {
	t.Helper()
	body := "command: [\"/bin/sh\", \"-c\", \"" + script + "\"]\n"
	if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestWorkdirIsResolvedByTheWorker is the bug a containerised control plane
// with a native worker would have hit immediately.
//
// The control plane used to resolve `workdir` against its own filesystem, so it
// would hand /var/lib/je/jobs to a worker on a laptop -- a path that is not
// there, or worse, one that is and is wrong. A path belongs to whoever will use
// it, which is the same rule the output channels already follow.
func TestWorkdirIsResolvedByTheWorker(t *testing.T) {
	ctx := context.Background()
	e, _ := jobFixture(t, "plain", `echo hi`)

	workerID := ensureWorker(t, e)
	if _, err := e.TriggerRun(ctx, qual("plain"), engine.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	dispatch, err := e.Claim(ctx, workerID)
	if err != nil {
		t.Fatal(err)
	}

	if dispatch.Workdir != "" {
		t.Errorf("workdir = %q, want it unresolved: the job declared none, and only "+
			"the worker knows where its own jobs live", dispatch.Workdir)
	}
}

// TestDeclaredAbsoluteWorkdirSurvivesTheWire keeps the other half honest: an
// absolute workdir is the job author's explicit choice and must travel intact.
func TestDeclaredAbsoluteWorkdirSurvivesTheWire(t *testing.T) {
	ctx := context.Background()
	e, _ := jobFixture(t, "elsewhere", `echo hi`, "workdir: /tmp")

	workerID := ensureWorker(t, e)
	if _, err := e.TriggerRun(ctx, qual("elsewhere"), engine.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	dispatch, err := e.Claim(ctx, workerID)
	if err != nil {
		t.Fatal(err)
	}
	if dispatch.Workdir != "/tmp" {
		t.Errorf("workdir = %q, want /tmp", dispatch.Workdir)
	}
}

// A job whose language no online worker can prepare is visibly queued, not
// dispatched to a worker that would fail it (D28/C8).
//
// The label and the runtime are different questions and a job can be stuck on
// either: this worker advertises the right label and would happily take the
// run, and the machine has no toolchain for it. Without this the failure is a
// job that dies at 3am with "pnpm is not installed"; with it, the work is
// waiting and says which worker to fix.
func TestAJobWhoseLanguageNothingServesIsUnservable(t *testing.T) {
	ctx := context.Background()
	e, _ := jobFixture(t, "ingest", `echo hi`, "language: typescript")

	// Online, right label, wrong toolchain.
	if _, err := e.RegisterWorker(ctx, store.Worker{
		ID: "w", Name: "w",
		Labels:   []string{store.DefaultLabel},
		Roles:    []string{store.RoleExecute},
		Version:  e.Health(ctx).Version,
		Runtimes: []string{"go"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.TriggerRun(ctx, qual("ingest"), engine.RunOptions{}); err != nil {
		t.Fatal(err)
	}

	waiting, err := e.Waiting(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(waiting.Unservable) != 0 {
		t.Errorf("the label was served, so it should not be unservable: %v", waiting.Unservable)
	}
	if len(waiting.UnservedRuntimes) != 1 {
		t.Fatalf("unserved runtimes = %v, want typescript", waiting.UnservedRuntimes)
	}
	if got := waiting.UnservedRuntimes[0].Language; got != "typescript" {
		t.Errorf("language = %q, want typescript", got)
	}
	if !waiting.NeedsAttention() {
		t.Error("work nobody can prepare did not need attention")
	}

	// Installing the toolchain and restarting the worker clears the state,
	// without the job changing at all. Re-registering the same worker is
	// exactly what a restart does, and it is why runtimes update on
	// registration where labels do not.
	if _, err := e.RegisterWorker(ctx, store.Worker{
		ID: "w", Name: "w",
		Labels:   []string{store.DefaultLabel},
		Roles:    []string{store.RoleExecute},
		Version:  e.Health(ctx).Version,
		Runtimes: []string{"go", "typescript"},
	}); err != nil {
		t.Fatal(err)
	}
	waiting, err = e.Waiting(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(waiting.UnservedRuntimes) != 0 {
		t.Errorf("still unservable after a capable worker joined: %v", waiting.UnservedRuntimes)
	}
}
