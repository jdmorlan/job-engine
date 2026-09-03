-- 0009_enrolment: a worker's identity is issued, not claimed (D25 step 5).
--
-- Until now a worker was whatever name it sent: workerID(name) is
-- "worker-" + name, and ErrLabelTaken only refused a second claimant while the
-- first was online. That is fine while the trust boundary is the network (D19),
-- and it stops being fine the moment a recipient list decides who can read a
-- secret.
--
-- Enrolment writes the row before the worker has ever connected. What it may
-- call itself and what capabilities it may advertise are decided by whoever
-- minted its enrolment token, and registration can then only say "I am here"
-- rather than "I am the macos worker".
ALTER TABLE workers ADD COLUMN enrolled_at TEXT;

-- The certificate this identity presents, as a SHA-256 of the DER. Recorded so
-- a re-enrolment is visible as a changed fingerprint rather than as nothing at
-- all, and so `je workers` can say which identity is which.
--
-- Null for every worker that predates enrolment: they registered by claiming a
-- name, which stays true and stays readable. A migration that pretended
-- otherwise would be inventing history.
ALTER TABLE workers ADD COLUMN cert_fingerprint TEXT;
