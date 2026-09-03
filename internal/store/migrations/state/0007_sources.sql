-- 0007_sources: definitions come from named places, not from "the jobs
-- directory" (D22).
--
-- D19 built the Source interface and said definition loading must be pluggable.
-- What is new here is the plural: several registered sources, with authority
-- per source rather than global, so a scratch job on local disk and a fleet of
-- repo-managed jobs coexist instead of being a mode switch.
CREATE TABLE sources (
    name        TEXT PRIMARY KEY,
    kind        TEXT NOT NULL CHECK (kind IN ('dir', 'github')),

    -- A filesystem path for a dir, or owner/repo for github. Empty for the
    -- built-in local source, which means "wherever the jobs directory is
    -- configured to be" -- so JE_JOBS_DIR keeps working and nobody has to
    -- register anything to write their first job.
    location    TEXT NOT NULL DEFAULT '',
    subpath     TEXT NOT NULL DEFAULT '',   -- --path jobs, within the tree

    -- github only. ref is what was asked for ("main", "v1.4", a sha) and
    -- revision is what it resolved to, which is the thing D11 needs: without a
    -- recorded commit, "what ran?" is unanswerable for a job whose code came
    -- from a moving branch.
    ref          TEXT NOT NULL DEFAULT '',
    revision     TEXT NOT NULL DEFAULT '',
    token_secret TEXT NOT NULL DEFAULT '',  -- name of the secret holding a token (D10)

    synced_at   TEXT,
    last_error  TEXT,      -- this source's last sync failed; the others still serve

    -- Unregistering tombstones rather than deletes, the same as a removed job
    -- file (D19). The jobs it loaded keep their history, and their names keep
    -- carrying this source -- so the row has to stay for `je runs` to remain
    -- readable, and for the foreign key those names rely on to hold.
    removed_at  TEXT,
    added_at    TEXT NOT NULL
);

-- The built-in one. Registering nothing must still give you somewhere to put a
-- job file, so this row exists from the first start and cannot be removed.
INSERT INTO sources (name, kind, location, added_at)
VALUES ('local', 'dir', '', datetime('now'));

-- Which source a definition came from. Authority is per source: a broken
-- weather repo must not tombstone the home jobs, so the "files that have gone"
-- sweep is scoped by this column rather than run across the whole table.
ALTER TABLE jobs   ADD COLUMN source TEXT NOT NULL DEFAULT 'local' REFERENCES sources(name);
ALTER TABLE chains ADD COLUMN source TEXT NOT NULL DEFAULT 'local' REFERENCES sources(name);

ALTER TABLE routes ADD COLUMN source_name TEXT NOT NULL DEFAULT 'local' REFERENCES sources(name);

CREATE INDEX jobs_source_idx   ON jobs (source);
CREATE INDEX chains_source_idx ON chains (source);
CREATE INDEX routes_source_idx ON routes (source_name);

-- jobs.name and chains.name stay the *qualified* name, and stay unique across
-- the whole table. Two repos will eventually both contain a sync.yaml and you
-- may own neither of them, so identity has to carry the source (D22).
--
-- The local source qualifies to the bare slug rather than to "local/ingest".
-- That is a deliberate departure from D22's "nothing is a special case", for
-- the same reason `runs_on` has a default: the first job somebody writes should
-- not have to know that sources are a concept. It also means this migration
-- rewrites no names and breaks no history. Uniqueness still holds by
-- construction -- "ingest" and "weather/ingest" are different strings.
