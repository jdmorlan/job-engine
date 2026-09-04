-- 0015_system_source: the engine's own work is a source too (P2).
--
-- Retention has to run somewhere, and P2 settled where: as an ordinary job.
-- That needs a source for it to belong to, because every job name carries one
-- (D22) and there is no unqualified job left.
--
-- `system` is not an exception to D27 so much as its limiting case. The rule
-- there is that code which cannot travel is not a source -- a script on the
-- control plane's disk can never run on a worker anywhere else. These
-- definitions run `je`, which is on every worker by definition, because C10
-- requires a worker to be the same version as the control plane. It is the one
-- tree that is already everywhere.
--
-- `dir` stays in the constraint although nothing may register one any more.
-- 0011 tombstoned those rows rather than deleting them, so that the jobs they
-- loaded keep names the database can still describe, and a CHECK that refused
-- them would refuse the history it was careful to preserve. Kinds you cannot
-- create and rows you must keep are two different questions.

CREATE TABLE sources_new (
    name         TEXT PRIMARY KEY,
    kind         TEXT NOT NULL CHECK (kind IN ('dir', 'github', 'system')),
    location     TEXT NOT NULL DEFAULT '',
    subpath      TEXT NOT NULL DEFAULT '',
    ref          TEXT NOT NULL DEFAULT '',
    revision     TEXT NOT NULL DEFAULT '',
    token_secret TEXT NOT NULL DEFAULT '',
    synced_at    TEXT,
    last_error   TEXT,
    removed_at   TEXT,
    added_at     TEXT NOT NULL
);

INSERT INTO sources_new (
    name, kind, location, subpath, ref, revision, token_secret,
    synced_at, last_error, removed_at, added_at
) SELECT
    name, kind, location, subpath, ref, revision, token_secret,
    synced_at, last_error, removed_at, added_at
FROM sources;

DROP TABLE sources;
ALTER TABLE sources_new RENAME TO sources;
