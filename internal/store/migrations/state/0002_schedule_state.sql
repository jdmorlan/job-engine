-- 0002_schedule_state: where each schedule last fired.
--
-- This one row per schedule is what makes D9's catch-up policy expressible.
-- Knowing "the job last ran at 03:00" is not enough, because it cannot
-- distinguish "the engine was asleep and missed twenty windows" from "the job
-- is on an hourly schedule and it is 03:59". Recording the last *window* -- a
-- point on the schedule's grid rather than a wall-clock moment -- means the
-- set of windows missed during any gap is exactly enumerable.
--
-- Keyed by position in the job's `on:` list, since a job may have several
-- schedules and each catches up independently.

CREATE TABLE schedule_state (
    job_id         INTEGER NOT NULL REFERENCES jobs(id),
    schedule_index INTEGER NOT NULL,
    last_window_at TEXT    NOT NULL,
    updated_at     TEXT    NOT NULL,
    PRIMARY KEY (job_id, schedule_index)
);

-- The overlap policy in force when a run was queued (D8).
--
-- Denormalised onto the run rather than read from the definition, for two
-- reasons. The claim query needs it to decide whether a second run of the same
-- job may start concurrently, and doing that with a join to the definition
-- snapshot would mean parsing JSON in SQL. And it is more correct: a run
-- executes under the policy that was in force when it was queued, which is the
-- same principle D11 applies to the definition itself.
ALTER TABLE runs ADD COLUMN overlap TEXT NOT NULL DEFAULT 'skip';
