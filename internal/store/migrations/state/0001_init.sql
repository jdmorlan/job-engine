-- 0001_init: the data model from PROPOSAL.md Appendix A.
--
-- Conventions used throughout, chosen for legibility in `je db` (D4 says the
-- CLI can drop you into a SQLite shell, so the rows have to be readable by a
-- human at 2am -- P1 applies to the storage layer too):
--
--   * Timestamps are TEXT, RFC3339 with nanoseconds, always UTC.
--   * JSON columns are TEXT holding a JSON object.
--   * Surrogate keys are INTEGER PRIMARY KEY (SQLite rowid aliases), so runs
--     and events have the small readable ids the CLI prints ("run 8821").
--
-- Q1 discipline: no gratuitous SQLite-isms. Nothing here would need rewriting
-- against Postgres beyond the autoincrement spelling.

-- Events are the spine. F1: a schedule is not a special mode, it is a source
-- that emits events; so is a manual invocation; so is a finished run.
CREATE TABLE events (
    id                 INTEGER PRIMARY KEY,
    type               TEXT    NOT NULL,          -- "run.succeeded", "homekit.motion"
    source             TEXT    NOT NULL,          -- "schedule", "cli", "job", "engine"
    payload            TEXT,                      -- json object
    caused_by_event_id INTEGER REFERENCES events(id),
    caused_by_run_id   INTEGER REFERENCES runs(id),
    depth              INTEGER NOT NULL DEFAULT 0, -- D3 loop guard: refuse past 10
    dedupe_key         TEXT,                       -- D16: `je emit --dedupe-key`
    actor              TEXT,                       -- who/what caused it, for D7
    created_at         TEXT    NOT NULL
);

CREATE INDEX events_type_created_idx ON events (type, created_at);
CREATE INDEX events_caused_by_run_idx ON events (caused_by_run_id);
-- Dedupe is an engine-enforced uniqueness, not a hint: two sources firing the
-- same key must produce one event, not two runs (D16).
CREATE UNIQUE INDEX events_dedupe_key_idx ON events (dedupe_key) WHERE dedupe_key IS NOT NULL;

-- The currently loaded definitions. This table is a projection of the files on
-- disk (D2) -- it is rebuilt on load, never hand-edited.
CREATE TABLE jobs (
    id              INTEGER PRIMARY KEY,
    name            TEXT    NOT NULL UNIQUE,
    definition_hash TEXT    NOT NULL REFERENCES job_versions(definition_hash),
    file_path       TEXT    NOT NULL,
    enabled         INTEGER NOT NULL DEFAULT 1,
    loaded_at       TEXT    NOT NULL,
    load_error      TEXT,   -- the file did not parse
    config_error    TEXT    -- it parsed but cannot run: D10's missing secret
);

-- Immutable, deduped snapshots of every definition we have ever loaded (D11).
-- A run points at one of these, so run detail can show the exact definition it
-- executed under even after the file has changed.
CREATE TABLE job_versions (
    definition_hash TEXT PRIMARY KEY,
    definition      TEXT NOT NULL,  -- json, normalized before hashing
    first_seen_at   TEXT NOT NULL
);

-- D17: one trigger table regardless of where the trigger was authored. A
-- job-local `on:` block and a step in chains/<name>.yaml both compile to rows
-- here, which is what lets `je routes` show a single resolved truth (P3).
--
-- chain_name is a naming and display grouping. It is NOT a runtime entity --
-- there is no chain lock, no chain state machine, no chain-level execution.
CREATE TABLE routes (
    id            INTEGER PRIMARY KEY,
    target_job_id INTEGER NOT NULL REFERENCES jobs(id),
    match         TEXT    NOT NULL,  -- json: the event pattern
    route_hash    TEXT    NOT NULL,  -- D11: runs record which rule fired them
    chain_name    TEXT,              -- null when authored job-local
    step_index    INTEGER,           -- null when authored job-local
    source        TEXT    NOT NULL CHECK (source IN ('job_file', 'chain_file')),
    file_path     TEXT    NOT NULL,
    enabled       INTEGER NOT NULL DEFAULT 1,
    load_error    TEXT
);

CREATE INDEX routes_target_job_idx ON routes (target_job_id);
CREATE INDEX routes_chain_idx ON routes (chain_name, step_index);

-- A run is a unit of intent, caused by exactly one event (D7). It has 1..N
-- attempts. "Did the 3am sync eventually succeed?" is a question about a run.
CREATE TABLE runs (
    id                   INTEGER PRIMARY KEY,
    job_id               INTEGER NOT NULL REFERENCES jobs(id),
    definition_hash      TEXT    NOT NULL REFERENCES job_versions(definition_hash),
    triggering_event_id  INTEGER REFERENCES events(id),
    triggering_route_id  INTEGER REFERENCES routes(id),
    route_hash           TEXT,      -- D11/D17: route provenance, snapshotted
    status               TEXT    NOT NULL CHECK (status IN (
                             'queued','running','succeeded','failed',
                             'interrupted','cancelled','timed_out')),
    queued_at            TEXT    NOT NULL,
    started_at           TEXT,
    ended_at             TEXT,
    attempt_count        INTEGER NOT NULL DEFAULT 0,
    state_version_in     INTEGER,   -- D14: which cursor this run started from
    output               TEXT,      -- json, from JOB_OUTPUT_FILE
    error                TEXT
);

CREATE INDEX runs_job_queued_idx ON runs (job_id, queued_at);
CREATE INDEX runs_status_idx ON runs (status);

-- An attempt is one execution. It carries its own causation so the history can
-- distinguish "automatic retry" from "a human intervened" (D7).
CREATE TABLE attempts (
    id                  INTEGER PRIMARY KEY,
    run_id              INTEGER NOT NULL REFERENCES runs(id),
    attempt_number      INTEGER NOT NULL,
    triggering_event_id INTEGER REFERENCES events(id),
    actor               TEXT,      -- "system" for an automatic retry, else a person
    status              TEXT    NOT NULL CHECK (status IN (
                            'queued','running','succeeded','failed',
                            'interrupted','cancelled','timed_out')),
    started_at          TEXT,
    ended_at            TEXT,
    exit_code           INTEGER,
    executor            TEXT,      -- 'process' | 'container' (D1)
    container_id        TEXT,      -- D5: reattach by id to recover the exit code
    error               TEXT,
    UNIQUE (run_id, attempt_number)
);

-- D14. Append-only; the current cursor is the row with max(version). Trimmed
-- to the last 100 versions by system.retention (D13), and never auto-expired
-- beyond that -- losing the current cursor means reprocessing from zero.
CREATE TABLE job_state (
    job_id       INTEGER NOT NULL REFERENCES jobs(id),
    version      INTEGER NOT NULL,
    value        TEXT    NOT NULL,  -- json object, opaque to the engine, 1MB cap
    set_by_run_id INTEGER REFERENCES runs(id),
    set_by_actor TEXT,              -- null for engine commits, else the person
    created_at   TEXT    NOT NULL,
    PRIMARY KEY (job_id, version)
);

-- D3. The table ships in v1 so fan-in is not a migration in v1.1; nothing
-- writes to it yet. Keyed on route_id now that routes exist (D17).
CREATE TABLE trigger_state (
    id                    INTEGER PRIMARY KEY,
    route_id              INTEGER NOT NULL REFERENCES routes(id),
    correlation_key       TEXT    NOT NULL,
    satisfied_conditions  TEXT    NOT NULL,  -- json
    window_started_at     TEXT    NOT NULL,
    expires_at            TEXT,
    fired_at              TEXT,
    UNIQUE (route_id, correlation_key)
);
