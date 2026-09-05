-- 0017_dev_source: the definitions you are editing, before they are anywhere.
--
-- D27 removed directory sources, and the argument was about production: a
-- source is what the engine runs from, and code on the control plane's disk
-- cannot travel to a worker anywhere else. That reasoning is untouched.
--
-- What it left behind is a hole in authoring. Every source is a repository, so
-- the loop for a job you are *writing* was edit, commit, push, sync, run --
-- through GitHub, every time. The first answer to that was `je try`, a second
-- executor in the CLI that ran the job itself and reported what would have
-- happened. It drifted from the real one within hours of shipping, in exactly
-- the way D20/C11 said a second executor drifts: its environment was not the
-- environment a job actually gets.
--
-- So `dev` is one reserved source name, always a local directory, re-read on
-- every run. It is not a kind you can register: there is no `je source add
-- --dev`, and nothing about a repository source changes. A job from it is
-- called dev/<name>, which means its runs, its cursor and its events are its
-- own and cannot be mistaken for -- or collide with -- the same job served from
-- a repository.
--
-- The limitation D27 named is still true and is now stated rather than
-- discovered: this only works where the control plane can see the directory,
-- which is your own machine.
CREATE TABLE sources_new (
    name         TEXT PRIMARY KEY,
    kind         TEXT NOT NULL CHECK (kind IN ('dir', 'github', 'system', 'dev')),
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
