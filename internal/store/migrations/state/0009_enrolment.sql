-- 0009_enrolment: a worker's identity is issued, not claimed (D25 step 5).
--
-- The filename keeps the British spelling the rest of the codebase dropped, and
-- so does this line, because they have to match. It is not prose:
-- schema_migrations records the name of every migration that has run, so
-- renaming this file would make an applied migration look new and re-run it --
-- and `ALTER TABLE workers ADD COLUMN enrolled_at` fails the second time. A
-- name that is a key is not a thing to tidy.
--
-- Until now a worker was whatever name it sent: workerID(name) is
-- "worker-" + name, and ErrLabelTaken only refused a second claimant while the
-- first was online. That is fine while the trust boundary is the network (D19),
-- and it stops being fine the moment a recipient list decides who can read a
-- secret.
--
-- Enrollment writes the row before the worker has ever connected. What it may
-- call itself and what capabilities it may advertise are decided by whoever
-- minted its enrollment token, and registration can then only say "I am here"
-- rather than "I am the macos worker".
ALTER TABLE workers ADD COLUMN enrolled_at TEXT;

-- The certificate this identity presents, as a SHA-256 of the DER. Recorded so
-- a re-enrollment is visible as a changed fingerprint rather than as nothing at
-- all, and so `je workers` can say which identity is which.
--
-- Null for every worker that predates enrollment: they registered by claiming a
-- name, which stays true and stays readable. A migration that pretended
-- otherwise would be inventing history.
ALTER TABLE workers ADD COLUMN cert_fingerprint TEXT;
