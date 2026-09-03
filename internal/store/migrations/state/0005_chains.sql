-- 0005_chains: make D17's routes table writable, and name the flows.
--
-- The routes table has been in the schema since 0001 with nothing writing to
-- it. Two things were missing before it could hold a projection of the files.
--
-- First, a natural key. A route authored in a chain file is identified by its
-- position in that file -- (chain, step) -- which is what lets a reload update
-- a rule in place instead of inserting a duplicate every sync.
CREATE UNIQUE INDEX routes_chain_step_key ON routes (chain_name, step_index)
    WHERE chain_name IS NOT NULL;

-- Second, a tombstone. Runs record which rule fired them (D11), so a route row
-- is referenced by history and can never be deleted -- foreign keys are on, and
-- deleting one would either fail or orphan the provenance of every run it
-- caused. Same rule as jobs in 0003: removing the file stops the rule firing
-- and keeps the record of when it did.
ALTER TABLE routes ADD COLUMN removed_at TEXT;

-- A chain is a naming and display grouping, NOT a runtime entity (D17). There
-- is no chain lock, no chain state machine and no chain-level execution; the
-- steps fire independently as ordinary routes, and this table holds only what
-- a person needs to read one: its name, what its author said it is for, and
-- which file it came from.
--
-- It earns a table anyway, because a chain is a *nameable* thing and a route is
-- not. "The daily-weather chain is stalled at step 2" is something you can say,
-- alert on and show status for; "route #3" is not.
CREATE TABLE chains (
    name        TEXT PRIMARY KEY,   -- the file name, as with jobs
    description TEXT,
    file_path   TEXT NOT NULL,
    loaded_at   TEXT NOT NULL,
    removed_at  TEXT                -- the file is gone; its routes stopped firing
);
