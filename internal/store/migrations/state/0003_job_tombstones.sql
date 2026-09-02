-- 0003_job_tombstones: a job whose definition file is gone.
--
-- D19 requires that deleting a file never deletes history: the job stops being
-- schedulable, but its runs, logs and cursor stay, because reverting a commit
-- must not erase the timeline. Until now that was recorded by stuffing a
-- sentinel into load_error, which made a deliberately removed job render as a
-- broken one -- and "load error" next to four jobs you just deleted on purpose
-- is exactly the kind of false alarm P1 is supposed to eliminate.
--
-- A removed job is a distinct state, so it gets a distinct column.

ALTER TABLE jobs ADD COLUMN removed_at TEXT;
