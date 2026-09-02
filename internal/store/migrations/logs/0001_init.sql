-- 0001_init: the logs database.
--
-- D4 puts logs in a separate DB file from state. The reason is operational
-- rather than aesthetic: log volume dwarfs everything else, and retention
-- (D13) reclaims space by vacuuming. Vacuuming a multi-gigabyte log file must
-- not block the schedule loop's writes to state, and a corrupted or deleted
-- log file must not take the run history with it.

CREATE TABLE logs (
    run_id         INTEGER NOT NULL,   -- no FK: different database file
    attempt_number INTEGER NOT NULL,
    seq            INTEGER NOT NULL,   -- monotonic within an attempt; preserves
                                       -- interleaving that identical timestamps lose
    stream         TEXT    NOT NULL CHECK (stream IN ('stdout', 'stderr')),
    ts             TEXT    NOT NULL,
    line           TEXT    NOT NULL,   -- D10: secret values redacted before this
                                       -- touches disk, not at render time
    PRIMARY KEY (run_id, attempt_number, seq)
);

CREATE INDEX logs_ts_idx ON logs (ts);
