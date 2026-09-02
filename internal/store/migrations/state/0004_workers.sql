-- 0004_workers: D20's data plane.
--
-- C11 makes the control plane a non-executor, so a run now has to record which
-- worker holds it and until when. C2 keeps the worker itself almost stateless:
-- this table is a registration and a lease, not a queue -- the queue is still
-- the runs table, and a worker never has work that the control plane cannot see.

CREATE TABLE workers (
    id           TEXT PRIMARY KEY,     -- assigned by the control plane at registration
    name         TEXT NOT NULL,        -- what a human calls this machine
    labels       TEXT NOT NULL,        -- json array; C3 pins jobs to these
    version      TEXT NOT NULL,        -- C10 refuses skew rather than negotiating
    roles        TEXT NOT NULL,        -- json array: 'execute' today, 'receive' later
    registered_at TEXT NOT NULL,
    last_seen_at TEXT NOT NULL,        -- C5: the lease is derived from this
    gone_at      TEXT                  -- set when the lease expired; kept for history
);

-- C8: "which worker serves label X, and is it alive?" must be answerable
-- cheaply, because it is on the status view rather than behind a flag.
CREATE INDEX workers_last_seen_idx ON workers (last_seen_at);

-- The run side of the lease. A queued run has no worker; a claimed run has one
-- and an expiry, and the expiry is what lets the control plane decide a worker
-- is gone without asking anyone (C5: a lease, not an election).
ALTER TABLE runs ADD COLUMN worker_id TEXT REFERENCES workers(id);
ALTER TABLE runs ADD COLUMN lease_expires_at TEXT;

-- C3: which worker may take this run. Snapshotted onto the run at enqueue time
-- for the same reason the definition is (D11): a definition reloaded between
-- enqueue and dispatch must not change where an already-queued run goes.
ALTER TABLE runs ADD COLUMN runs_on TEXT NOT NULL DEFAULT 'default';

-- 'lost' is a new terminal status, and it needs the CHECK constraints rebuilt.
--
-- C6: when a worker stops heartbeating, "it died" and "it is partitioned and
-- still running your job" are indistinguishable. `lost` is the honest name for
-- that outcome -- it is not `failed`, because we do not know that it failed,
-- and calling it `failed` would assert something untrue in the one place a
-- person goes to find out what happened (P1).
CREATE TABLE runs_new (
    id                   INTEGER PRIMARY KEY,
    job_id               INTEGER NOT NULL REFERENCES jobs(id),
    definition_hash      TEXT    NOT NULL REFERENCES job_versions(definition_hash),
    triggering_event_id  INTEGER REFERENCES events(id),
    triggering_route_id  INTEGER REFERENCES routes(id),
    route_hash           TEXT,
    status               TEXT    NOT NULL CHECK (status IN (
                             'queued','running','succeeded','failed',
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
    runs_on              TEXT    NOT NULL DEFAULT 'default'
);

INSERT INTO runs_new (
    id, job_id, definition_hash, triggering_event_id, triggering_route_id,
    route_hash, status, queued_at, started_at, ended_at, attempt_count,
    state_version_in, output, error, overlap, worker_id, lease_expires_at, runs_on
) SELECT
    id, job_id, definition_hash, triggering_event_id, triggering_route_id,
    route_hash, status, queued_at, started_at, ended_at, attempt_count,
    state_version_in, output, error, overlap, worker_id, lease_expires_at, runs_on
FROM runs;

DROP TABLE runs;
ALTER TABLE runs_new RENAME TO runs;

CREATE INDEX runs_job_queued_idx ON runs (job_id, queued_at);
CREATE INDEX runs_status_idx ON runs (status);
CREATE INDEX runs_claimable_idx ON runs (status, runs_on, queued_at);

CREATE TABLE attempts_new (
    id                  INTEGER PRIMARY KEY,
    run_id              INTEGER NOT NULL REFERENCES runs(id),
    attempt_number      INTEGER NOT NULL,
    triggering_event_id INTEGER REFERENCES events(id),
    actor               TEXT,
    status              TEXT    NOT NULL CHECK (status IN (
                            'queued','running','succeeded','failed',
                            'interrupted','cancelled','timed_out','lost')),
    started_at          TEXT,
    ended_at            TEXT,
    exit_code           INTEGER,
    executor            TEXT,
    container_id        TEXT,
    error               TEXT,
    worker_id           TEXT REFERENCES workers(id),
    UNIQUE (run_id, attempt_number)
);

INSERT INTO attempts_new (
    id, run_id, attempt_number, triggering_event_id, actor, status,
    started_at, ended_at, exit_code, executor, container_id, error
) SELECT
    id, run_id, attempt_number, triggering_event_id, actor, status,
    started_at, ended_at, exit_code, executor, container_id, error
FROM attempts;

DROP TABLE attempts;
ALTER TABLE attempts_new RENAME TO attempts;
