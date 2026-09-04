// The job-engine shim for Node and TypeScript (D21).
//
// It is sugar over the protocol (D6) and it may never be more than that: every
// line below reads an environment variable the engine sets, or writes one of
// the three output files it reads back. A job that imports nothing does exactly
// as much as one that imports this, which is the rule that keeps "any language
// participates fully" true rather than aspirational.
//
// Written into node_modules by the worker that is about to run your job, so
// `import je from "je"` resolves from any depth without a path. There is no
// package to install and no version to match: the shim is materialised by the
// same binary that executes the job.
import { appendFileSync, writeFileSync } from "node:fs";

const readJSON = (raw, fallback) => {
  if (!raw) return fallback;
  try {
    return JSON.parse(raw);
  } catch {
    return fallback;
  }
};

const channel = (name) => {
  const path = process.env[name];
  if (!path) {
    // Running outside the engine. Said plainly rather than swallowed: a job
    // that silently discards its cursor is the failure D14 exists to prevent.
    throw new Error(
      `${name} is not set, so this is not running under the job engine. ` +
        `Use \`je try\` to run it here, or \`je run\` to run it for real.`,
    );
  }
  return path;
};

const je = {
  /** Your cursor, seeded on the first run (D14). Read-only; use setState. */
  state: readJSON(process.env.JE_STATE, {}),

  /** When this job last succeeded, or null if it never has. Engine-owned. */
  lastSuccessAt: process.env.JE_LAST_SUCCESS_AT
    ? new Date(process.env.JE_LAST_SUCCESS_AT)
    : null,

  /** The payload of the event that triggered this run. */
  event: readJSON(process.env.EVENT_PAYLOAD, {}),

  /**
   * Move the cursor. Last write wins, and nothing is committed unless the job
   * exits zero -- so a job that fails after calling this leaves the cursor
   * exactly where it was (D14).
   */
  setState(next) {
    writeFileSync(channel("JOB_STATE_OUT_FILE"), JSON.stringify(next));
  },

  /** Emit an event other jobs can be triggered by (D17). */
  emit(type, payload = {}) {
    appendFileSync(
      channel("JOB_EVENTS_FILE"),
      JSON.stringify({ type, payload }) + "\n",
    );
  },

  /** Structured output, for whatever reads this run's result. */
  output(value) {
    writeFileSync(channel("JOB_OUTPUT_FILE"), JSON.stringify(value));
  },
};

export default je;
export const { state, lastSuccessAt, event, setState, emit, output } = je;
