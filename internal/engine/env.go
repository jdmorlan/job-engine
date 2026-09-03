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

// buildEnv assembles the environment for one attempt (D6), minus the four
// values only the worker can know.
//
// The full v1 protocol, and nothing else. A job that wants more has to declare
// it, which is what makes the set auditable.
//
// JOB_WORKDIR and the three output channel paths are added by the worker, on
// the machine where the files will actually exist (D20). That split is what
// keeps D6 unchanged across the network: the job still writes to local files
// and never learns that the engine is somewhere else.
func (e *Engine) buildEnv(
	ctx context.Context,
	job store.Job,
	def *jobdef.Definition,
	run store.Run,
	attempt store.Attempt,
	cause model.Event,
	stateIn store.StateVersion,
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

		// D14. State arrives in the environment because inputs carry no
		// commit-on-success requirement -- that is what makes the three output
		// channels files and this one not.
		"JE_STATE="+string(stateIn.Value),
	)

	// D10: only declared secrets, and the whole store is never handed over.
	//
	// Only the ones this control plane actually holds. A declared secret the
	// store does not have is expected to be encrypted in the job's own source,
	// where the control plane cannot read it and the worker can -- so it is
	// left out of the environment entirely and its name is dispatched instead
	// (D25). Building a process environment is the worker's job under C11; this
	// is the part of it that has not moved yet.
	held, err := e.storeHeldSecrets(def.Secrets)
	if err != nil {
		return nil, fmt.Errorf("job %s: %w", job.Slug, err)
	}
	for _, name := range def.Secrets {
		if value, ok := held[name]; ok {
			env = append(env, name+"="+value)
		}
	}

	// D14: engine-owned and read-only, so the two facts that must not be
	// confused -- what you processed, and when you last ran -- cannot be.
	lastSuccess, lastErr := e.store.LastSuccessAt(ctx, job.ID)
	switch {
	case lastErr == nil:
		env = append(env, "JE_LAST_SUCCESS_AT="+lastSuccess.UTC().Format(time.RFC3339))
	case isNoRows(lastErr):
		// Never succeeded. The variable is absent rather than empty, so a job
		// can tell "never" from "at the zero time".
	default:
		return nil, lastErr
	}

	return env, nil
}

// storeHeldSecrets resolves the declared secrets this control plane has, and
// says nothing about the ones it does not.
//
// Distinct from Store.Resolve, which fails on an unknown name. Absence is not
// an error here: it means the value lives in the job's source, and load-time
// validation has already refused any name that is in neither place (D25).
func (e *Engine) storeHeldSecrets(declared []string) (map[string]string, error) {
	if len(declared) == 0 {
		return nil, nil
	}
	missing, err := e.secrets.Missing(declared)
	if err != nil {
		return nil, err
	}
	absent := make(map[string]bool, len(missing))
	for _, name := range missing {
		absent[name] = true
	}
	present := make([]string, 0, len(declared))
	for _, name := range declared {
		if !absent[name] {
			present = append(present, name)
		}
	}
	return e.secrets.Resolve(present)
}

// repoHeldSecrets are the declared names this control plane cannot supply, and
// which the worker is therefore expected to decrypt for itself.
func (e *Engine) repoHeldSecrets(declared []string) ([]string, error) {
	if len(declared) == 0 {
		return nil, nil
	}
	return e.secrets.Missing(declared)
}
