package engine

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/jdmorlan/job-engine/internal/jobdef"
	"github.com/jdmorlan/job-engine/internal/model"
	"github.com/jdmorlan/job-engine/internal/store"
)

// passthroughEnv is the only part of the engine's own environment a job sees.
//
// D10 requires injection to be explicit and minimal: "a job doesn't inherit
// your whole environment by accident." But a job that cannot find `node` or
// `python` is useless, so this is the smallest set that makes commands
// resolvable and locale-correct. Everything else -- credentials above all --
// must be declared.
var passthroughEnv = []string{
	"PATH",
	"HOME",
	"LANG",
	"LC_ALL",
	"TZ", // D9/D19: schedules mean local time to a human
	"TMPDIR",
	"SHELL",
}

// buildEnv assembles the complete environment for one attempt (D6).
//
// The full v1 protocol, and nothing else. A job that wants more has to declare
// it, which is what makes the set auditable.
func (e *Engine) buildEnv(
	ctx context.Context,
	job store.Job,
	def *jobdef.Definition,
	run store.Run,
	attempt store.Attempt,
	cause model.Event,
	stateIn store.StateVersion,
	ch channels,
) ([]string, error) {
	if len(stateIn.Value) > jobdef.MaxStateBytes {
		// Should be impossible -- CommitState enforces the cap on the way in --
		// but exec would fail with a confusing E2BIG rather than saying why.
		return nil, fmt.Errorf("job %s has %d bytes of state, over the %d byte limit",
			job.Slug, len(stateIn.Value), jobdef.MaxStateBytes)
	}

	env := make([]string, 0, len(passthroughEnv)+12)
	for _, key := range passthroughEnv {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}

	payload := string(cause.Payload)
	if payload == "" {
		payload = "{}"
	}

	env = append(env,
		"JOB_ID="+job.Slug,
		"RUN_ID="+strconv.FormatInt(run.ID, 10),
		"ATTEMPT="+strconv.Itoa(attempt.Number),
		"TRIGGERED_BY="+cause.Type,
		"EVENT_PAYLOAD="+payload,
		"JOB_WORKDIR="+ch.dir(),

		// D14. State arrives in the environment because inputs carry no
		// commit-on-success requirement -- that is what makes the three output
		// channels files and this one not.
		"JE_STATE="+string(stateIn.Value),

		// The three output channels (D6, D14, D17).
		"JOB_STATE_OUT_FILE="+ch.stateOut,
		"JOB_OUTPUT_FILE="+ch.output,
		"JOB_EVENTS_FILE="+ch.events,
	)

	// D14: engine-owned and read-only, so the two facts that must not be
	// confused -- what you processed, and when you last ran -- cannot be.
	lastSuccess, err := e.store.LastSuccessAt(ctx, job.ID)
	switch {
	case err == nil:
		env = append(env, "JE_LAST_SUCCESS_AT="+lastSuccess.UTC().Format(time.RFC3339))
	case isNoRows(err):
		// Never succeeded. The variable is absent rather than empty, so a job
		// can tell "never" from "at the zero time".
	default:
		return nil, err
	}

	return env, nil
}

// dir is the scratch directory holding the output channels, exposed to the job
// as JOB_WORKDIR so it has somewhere to put intermediate files that the engine
// will clean up.
func (c channels) dir() string { return filepathDir(c.stateOut) }
