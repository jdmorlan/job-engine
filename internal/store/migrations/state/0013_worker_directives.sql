-- 0013_worker_directives: telling a worker to do something, from wherever you are.
--
-- `je upgrade` acts on the machine it runs on, which is the whole answer for one
-- box and half of it for two (D26). A control plane already knows every worker,
-- its version, and holds an authenticated connection to it -- so "restart that
-- worker" is reachable over a channel that exists, and the only thing missing
-- was somewhere to write the request down.
--
-- A column rather than a queue, deliberately. A directive is a *desired state*,
-- not a message: asking twice for a restart is one restart, and a worker that
-- was offline for an hour should act on the latest request rather than replay
-- an hour of them. Superseding is the correct behaviour and a queue would have
-- to be taught it.
--
-- Cleared when delivered rather than when completed. What follows a restart
-- directive is the worker exiting, and a worker that exits cannot report that
-- it did -- so delivery is the only acknowledgement there can be, and pretending
-- otherwise would leave a directive stuck forever.
ALTER TABLE workers ADD COLUMN directive TEXT;
ALTER TABLE workers ADD COLUMN directive_at TEXT;
