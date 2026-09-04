// Types for the job-engine shim (D21).
//
// They live here, in the package the worker writes, rather than in a file the
// scaffold copies into your repository. A copied declaration is a second thing
// to keep in step with the shim, and keeping two things in step across versions
// is exactly the cost D21 refuses to pay -- the shim is materialised by the
// binary that runs your job, so there is no version pair to be wrong, and its
// types should not reintroduce one.
//
// Every member below maps onto the protocol (D6). Nothing here can do anything
// a job in another language cannot do by reading the same environment variables
// and writing the same three files.

/** A cursor is any JSON object. The engine never looks inside it (D14). */
export type State = Record<string, unknown>;

/** The payload of the event that caused this run. */
export type EventPayload = Record<string, unknown>;

export interface Je {
  /**
   * Your cursor, as the engine last committed it.
   *
   * Seeded on the very first run with that run's start time, so it is never
   * empty and never the epoch -- there is no recovering from having already
   * hammered somebody's API for all of history.
   *
   * Reading it does not move it. Use `setState`.
   */
  readonly state: State;

  /**
   * When this job last succeeded, or null if it never has.
   *
   * Engine-owned and not writable: it is derived from the run history rather
   * than stored, so it cannot drift from what actually happened.
   */
  readonly lastSuccessAt: Date | null;

  /** The payload of the event that triggered this run (D17). */
  readonly event: EventPayload;

  /**
   * Move the cursor.
   *
   * Last write wins, and nothing is committed unless the job exits zero -- so a
   * job that fails after calling this leaves the cursor exactly where it was.
   * That is the whole point of D14: a failure cannot silently skip records.
   */
  setState(next: State): void;

  /**
   * Emit an event other jobs can be triggered by.
   *
   * The engine attributes it to this run, so a chain step firing on it can be
   * traced back here without the job doing anything (D17).
   */
  emit(type: string, payload?: EventPayload): void;

  /** Structured output, for whatever reads this run's result. */
  output(value: unknown): void;
}

declare const je: Je;
export default je;

export declare const state: State;
export declare const lastSuccessAt: Date | null;
export declare const event: EventPayload;
export declare const setState: Je["setState"];
export declare const emit: Je["emit"];
export declare const output: Je["output"];
