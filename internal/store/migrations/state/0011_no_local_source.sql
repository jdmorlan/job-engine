-- 0011_no_local_source: definitions live in a repository, not in the engine's
-- data directory (D22).
--
-- 0007 seeded a built-in source called `local` of kind `dir`, meaning "wherever
-- the jobs directory is configured to be". That directory was inside the data
-- directory the engine owns, which put the one thing a person authors inside
-- the one place the engine is free to delete -- and made `je reset` need an
-- opinion about definitions it should never have had.
--
-- The deciding argument was not tidiness. A directory source never travelled: a
-- job whose code sat on the control plane's disk could only run on a worker
-- that shared that disk, so the kind was already broken the moment there were
-- two machines. Every source is a repository now, and every tree reaches a
-- worker by being fetched.
--
-- The jobs, chains and routes that came from `local` are tombstoned rather than
-- deleted, for the reason D19 gives everywhere else: the runs happened, and
-- `je runs` has to keep saying so a year from now.
UPDATE jobs   SET removed_at = datetime('now')
 WHERE removed_at IS NULL
   AND source IN (SELECT name FROM sources WHERE kind = 'dir');

UPDATE chains SET removed_at = datetime('now')
 WHERE removed_at IS NULL
   AND source IN (SELECT name FROM sources WHERE kind = 'dir');

DELETE FROM routes
 WHERE source_name IN (SELECT name FROM sources WHERE kind = 'dir');

-- The registration goes; the row stays tombstoned so the names above still
-- resolve to something the database can describe.
UPDATE sources SET removed_at = datetime('now')
 WHERE removed_at IS NULL AND kind = 'dir';
