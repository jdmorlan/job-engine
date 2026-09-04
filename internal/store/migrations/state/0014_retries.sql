-- 0014_retries: D7's automatic half.
--
-- A run is a unit of intent with 1..N attempts, and that was already true in
-- the schema -- what was missing is the state a run sits in *between* two of
-- them. A failed attempt with another one due is not `queued` (nothing has
-- been queued; this run has already run) and it is certainly not `failed`
-- (nothing has failed yet but one attempt). Overloading either would make the
-- one column everybody reads say something untrue for the length of a backoff,
-- which is exactly the class of thing P1 rules out -- and is why `lost`,
-- `interrupted` and `timed_out` are all their own status rather than shades of
-- `failed`.
--
-- So: `retrying`, non-terminal, meaning "the last attempt failed and the next
-- one is due at next_attempt_at".

-- When this run becomes claimable again. NULL means immediately, which is what
-- every run that has never been retried says.
--
-- A column on the run rather than a timer in memory, because the wait has to
-- survive a control plane restart: a job whose retry was scheduled for five
-- minutes from now must still be retried if the process dies at minute two.
-- Nothing schedules a wake-up for it either -- workers poll, so a run becomes
-- claimable simply by the clock passing it.
ALTER TABLE runs ADD COLUMN next_attempt_at TEXT;

-- The status CHECK has to be rebuilt to admit 'retrying'. Same dance as 0004:
-- SQLite cannot alter a constraint in place, so the table is rebuilt and the
-- rows are copied.
CREATE TABLE runs_new (
    id                   INTEGER PRIMARY KEY,
    job_id               INTEGER NOT NULL REFERENCES jobs(id),
    definition_hash      TEXT    NOT NULL REFERENCES job_versions(definition_hash),
    triggering_event_id  INTEGER REFERENCES events(id),
    triggering_route_id  INTEGER REFERENCES routes(id),
    route_hash           TEXT,
    source_revision      TEXT,
    status               TEXT    NOT NULL CHECK (status IN (
                             'queued','running','retrying','succeeded','failed',
                             'interrupted','cancelled','timed_out','lost')),
    queued_at            TEXT    NOT NULL,
    started_at           TEXT,
    ended_at             TEXT,
    attempt_count        INTEGER NOT NULL DEFAULT 0,
    state_version_in     INTEGER,
    output               TEXT,
    error                TEXT,
    overlap              TEXT    NOT NULL DEFAULT 'skip',
    worker_id            TEXT REFERENCES workers(id),
    lease_expires_at     TEXT,
    runs_on              TEXT    NOT NULL DEFAULT 'default',
    next_attempt_at      TEXT
);

INSERT INTO runs_new (
    id, job_id, definition_hash, triggering_event_id, triggering_route_id,
    route_hash, source_revision, status, queued_at, started_at, ended_at,
    attempt_count, state_version_in, output, error, overlap, worker_id,
    lease_expires_at, runs_on, next_attempt_at
) SELECT
    id, job_id, definition_hash, triggering_event_id, triggering_route_id,
    route_hash, source_revision, status, queued_at, started_at, ended_at,
    attempt_count, state_version_in, output, error, overlap, worker_id,
    lease_expires_at, runs_on, next_attempt_at
FROM runs;

DROP TABLE runs;
ALTER TABLE runs_new RENAME TO runs;

CREATE INDEX runs_job_queued_idx ON runs (job_id, queued_at);
CREATE INDEX runs_status_idx ON runs (status);
CREATE INDEX runs_claimable_idx ON runs (status, runs_on, queued_at);
