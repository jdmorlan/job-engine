package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/jdmorlan/job-engine/internal/jobdef"
	"github.com/jdmorlan/job-engine/internal/model"
)

// DefaultLabel is the capability every job has unless it says otherwise (C3).
//
// It exists so the first job somebody writes does not need to know that
// placement is a concept. A control plane ships with a worker advertising it
// (C12), so `runs_on` unset means "anywhere the deployment provides".
const DefaultLabel = "default"

// Worker is one registered data plane member (D20, F1).
//
// It holds no durable state of its own (C2) -- this row is a registration and
// a lease, and losing it costs the worker nothing but its in-flight runs.
type Worker struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Labels []string `json:"labels"`
	Roles  []string `json:"roles"`

	// AgeRecipient is the public half of the key this identity reads encrypted
	// secrets with, when it has registered one (D25).
	//
	// Public by construction, so it travels in views freely: it is what you
	// encrypt *to*, and holding it grants nothing. What matters is that the
	// control plane learned it from the identity itself rather than from
	// somebody pasting it.
	AgeRecipient string     `json:"age_recipient,omitempty"`
	Version      string     `json:"version"`
	RegisteredAt time.Time  `json:"registered_at"`
	LastSeenAt   time.Time  `json:"last_seen_at"`
	GoneAt       *time.Time `json:"gone_at,omitempty"`

	// EnrolledAt is when this identity was issued, and Fingerprint is the
	// certificate it presents (D25 step 5).
	//
	// Both nil for a worker that registered by claiming a name, which is what
	// every worker did before enrolment existed and what any worker on a
	// plaintext listener still does. Absent rather than faked: "this identity
	// was issued" is a different fact from "this worker said it was called
	// that", and the view should not blur them.
	EnrolledAt  *time.Time `json:"enrolled_at,omitempty"`
	Fingerprint string     `json:"cert_fingerprint,omitempty"`
}

// Enrolled reports whether this worker's identity was issued rather than
// claimed. Only an enrolled worker's labels can be trusted, because only those
// were decided by somebody other than the worker.
func (w Worker) Enrolled() bool { return w.EnrolledAt != nil }

// Online reports whether the lease was renewed recently enough.
//
// C5: liveness is a lease with a timeout, decided unilaterally by the control
// plane. There is no election and nothing to agree with the worker about --
// this side is the sole writer and is correct by definition.
func (w Worker) Online(now time.Time, ttl time.Duration) bool {
	return w.GoneAt == nil && now.Sub(w.LastSeenAt) < ttl
}

// RegisterWorker records a worker, or refreshes an existing registration.
//
// Re-registering under the same id is how a restarted worker rejoins: C2 says
// it lost nothing but its in-flight runs, so there is nothing to reconcile.
func (s *Store) RegisterWorker(ctx context.Context, w Worker) (Worker, error) {
	labels, err := json.Marshal(w.Labels)
	if err != nil {
		return Worker{}, err
	}
	roles, err := json.Marshal(w.Roles)
	if err != nil {
		return Worker{}, err
	}
	_, err = s.state.ExecContext(ctx, `
		INSERT INTO workers (id, name, labels, version, roles, registered_at, last_seen_at, gone_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, NULL)
		ON CONFLICT(id) DO UPDATE SET
			-- An enrolled worker cannot rename itself or change what it can do.
			-- Its name and labels were decided by whoever minted its enrolment
			-- token, and registration is only "I am here" (D25).
			--
			-- Enforced here rather than in the engine because this is the write:
			-- a caller that forgot the rule cannot get round it, and there is
			-- exactly one place to look to know whether the rule holds.
			name = CASE WHEN workers.enrolled_at IS NULL
			            THEN excluded.name ELSE workers.name END,
			labels = CASE WHEN workers.enrolled_at IS NULL
			              THEN excluded.labels ELSE workers.labels END,
			version = excluded.version, roles = excluded.roles,
			last_seen_at = excluded.last_seen_at, gone_at = NULL`,
		w.ID, w.Name, string(labels), w.Version, string(roles),
		formatTime(w.RegisteredAt), formatTime(w.LastSeenAt))
	if err != nil {
		return Worker{}, fmt.Errorf("registering worker: %w", err)
	}
	// Read back rather than returning what was sent: for an enrolled worker the
	// two differ, and the stored row is the true one.
	stored, err := s.WorkerByID(ctx, w.ID)
	if err != nil {
		return Worker{}, err
	}
	return stored, nil
}

// TouchWorker renews a lease (C5).
func (s *Store) TouchWorker(ctx context.Context, id string, at time.Time) error {
	res, err := s.state.ExecContext(ctx,
		`UPDATE workers SET last_seen_at = ?, gone_at = NULL WHERE id = ?`,
		formatTime(at), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// Workers lists every registration, most recently seen first.
func (s *Store) Workers(ctx context.Context) ([]Worker, error) {
	rows, err := s.state.QueryContext(ctx, `
		SELECT id, name, labels, version, roles, registered_at, last_seen_at, gone_at,
		       enrolled_at, cert_fingerprint, age_recipient
		FROM workers ORDER BY last_seen_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Worker
	for rows.Next() {
		var w Worker
		var labels, roles string
		var goneAt, enrolledAt, fingerprint, recipient sql.NullString
		var registered, lastSeen string
		if err := rows.Scan(&w.ID, &w.Name, &labels, &w.Version, &roles,
			&registered, &lastSeen, &goneAt, &enrolledAt, &fingerprint,
			&recipient); err != nil {
			return nil, err
		}
		w.AgeRecipient = recipient.String
		if enrolledAt.Valid {
			at, err := parseTime(enrolledAt.String)
			if err != nil {
				return nil, err
			}
			w.EnrolledAt = &at
		}
		w.Fingerprint = fingerprint.String
		if err := json.Unmarshal([]byte(labels), &w.Labels); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(roles), &w.Roles); err != nil {
			return nil, err
		}
		if w.RegisteredAt, err = parseTime(registered); err != nil {
			return nil, err
		}
		if w.LastSeenAt, err = parseTime(lastSeen); err != nil {
			return nil, err
		}
		if goneAt.Valid {
			t, err := parseTime(goneAt.String)
			if err != nil {
				return nil, err
			}
			w.GoneAt = &t
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// WorkerByID returns one registration.
func (s *Store) WorkerByID(ctx context.Context, id string) (Worker, error) {
	all, err := s.Workers(ctx)
	if err != nil {
		return Worker{}, err
	}
	for _, w := range all {
		if w.ID == id {
			return w, nil
		}
	}
	return Worker{}, sql.ErrNoRows
}

// ClaimNextRunForWorker atomically leases the oldest queued run this worker is
// allowed to take, returning sql.ErrNoRows when there is nothing for it.
//
// Atomic because several workers pull from the same queue, and two workers
// executing one run would double-fire the job -- the same failure the data
// directory lock prevents between processes, one level down.
//
// The label filter is C3: jobs are pinned, not placed. A worker sees only the
// runs whose `runs_on` it advertises, so there is no work stealing to prevent
// and no placement decision to make -- the job already made it.
func (s *Store) ClaimNextRunForWorker(
	ctx context.Context, workerID string, labels []string, at time.Time, lease time.Duration,
) (Run, error) {
	if len(labels) == 0 {
		return Run{}, sql.ErrNoRows
	}

	tx, err := s.state.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()

	// Oldest first, so the queue is fair and a job that has been waiting does
	// not starve behind newer arrivals.
	//
	// The NOT EXISTS clause is what makes `overlap: queue` mean what it says:
	// a second run of the same job waits for the first to finish rather than
	// running beside it. Without it two workers would happily claim both, and
	// a queued backlog would execute concurrently -- which is the one thing
	// the author ruled out by not choosing `allow`.
	args := []any{string(model.StatusQueued), string(jobdef.OverlapAllow), string(model.StatusRunning)}
	placeholders := ""
	for i, label := range labels {
		if i > 0 {
			placeholders += ","
		}
		placeholders += "?"
		args = append(args, label)
	}

	var id int64
	err = tx.QueryRowContext(ctx, `
		SELECT r.id FROM runs r
		WHERE r.status = ?
		  AND (r.overlap = ? OR NOT EXISTS (
		        SELECT 1 FROM runs o WHERE o.job_id = r.job_id AND o.status = ?))
		  AND r.runs_on IN (`+placeholders+`)
		ORDER BY r.queued_at, r.id LIMIT 1`, args...).Scan(&id)
	if err != nil {
		return Run{}, err
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE runs SET status = ?, started_at = ?, worker_id = ?, lease_expires_at = ?
		WHERE id = ?`,
		string(model.StatusRunning), formatTime(at), workerID,
		formatTime(at.Add(lease)), id); err != nil {
		return Run{}, err
	}
	if err := tx.Commit(); err != nil {
		return Run{}, err
	}
	return s.RunByID(ctx, id)
}

// RenewRunLease extends the lease on a run a worker is still executing.
//
// It returns false when the run is no longer leased to this worker, which is
// C7's fencing signal: the worker's claim was revoked while it was out of
// contact, and it must kill the process and discard the result rather than
// reporting back into a run the control plane has already moved on from.
func (s *Store) RenewRunLease(
	ctx context.Context, runID int64, workerID string, at time.Time, lease time.Duration,
) (bool, error) {
	res, err := s.state.ExecContext(ctx, `
		UPDATE runs SET lease_expires_at = ?
		WHERE id = ? AND worker_id = ? AND status = ?`,
		formatTime(at.Add(lease)), runID, workerID, string(model.StatusRunning))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// RunsHeldBy returns the ids of runs currently leased to a worker.
func (s *Store) RunsHeldBy(ctx context.Context, workerID string) ([]int64, error) {
	rows, err := s.state.QueryContext(ctx,
		`SELECT id FROM runs WHERE worker_id = ? AND status = ?`,
		workerID, string(model.StatusRunning))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ExpiredLeases returns runs whose lease has run out.
//
// These are C6's ambiguous case and the reason `lost` exists as a status: the
// worker may be dead, or it may be partitioned and still running the job. The
// control plane cannot tell, so it stops waiting and says so.
func (s *Store) ExpiredLeases(ctx context.Context, now time.Time) ([]Run, error) {
	rows, err := s.state.QueryContext(ctx, `
		SELECT id FROM runs
		WHERE status = ? AND lease_expires_at IS NOT NULL AND lease_expires_at < ?`,
		string(model.StatusRunning), formatTime(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]Run, 0, len(ids))
	for _, id := range ids {
		run, err := s.RunByID(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, nil
}

// MarkWorkerGone records that a lease expired, keeping the row for history.
func (s *Store) MarkWorkerGone(ctx context.Context, id string, at time.Time) error {
	_, err := s.state.ExecContext(ctx,
		`UPDATE workers SET gone_at = ? WHERE id = ? AND gone_at IS NULL`,
		formatTime(at), id)
	return err
}

// LabelsCovered reports which of the given labels an online worker advertises.
//
// C8 needs this: a job pinned to a label nothing serves is waiting forever, and
// that must appear as a visible waiting state rather than a silent backlog.
func (s *Store) LabelsCovered(ctx context.Context, now time.Time, ttl time.Duration) (map[string]bool, error) {
	workers, err := s.Workers(ctx)
	if err != nil {
		return nil, err
	}
	covered := map[string]bool{}
	for _, w := range workers {
		if !w.Online(now, ttl) || !slices.Contains(w.Roles, RoleExecute) {
			continue
		}
		for _, label := range w.Labels {
			covered[label] = true
		}
	}
	return covered, nil
}

// The roles an identity may carry (F1, D20, D25).
//
// receive is here because the phone case in F1 is the reason `worker` is a role
// rather than an entity, and leaving the column single-valued now would make
// that a migration later.
//
// client is a person at a terminal rather than a machine that runs jobs, and it
// is the one role that changes how the control plane behaves: the first client
// identity a deployment issues is what turns on the requirement that a mutating
// request prove who it is (D25). Before that there is nobody to be, and
// refusing every write would only mean nothing worked.
const (
	RoleExecute = "execute"
	RoleReceive = "receive"
	RoleClient  = "client"
)

// EnrolWorker writes an identity before the worker has ever connected.
//
// The row exists first, and that ordering is the point: what this machine may
// call itself and what capabilities it may advertise are decided here, by
// whoever minted its enrolment token, and registration can then only report
// that it is alive (D25).
//
// Re-enrolment overwrites the fingerprint and keeps the row, so a rebuilt
// machine rejoining is visible as a changed certificate rather than as a second
// worker with the same name.
func (s *Store) EnrolWorker(ctx context.Context, w Worker) error {
	labels, err := json.Marshal(w.Labels)
	if err != nil {
		return err
	}
	roles, err := json.Marshal(w.Roles)
	if err != nil {
		return err
	}
	now := formatTime(w.RegisteredAt)
	_, err = s.state.ExecContext(ctx, `
		INSERT INTO workers (id, name, labels, version, roles,
		                     registered_at, last_seen_at, gone_at,
		                     enrolled_at, cert_fingerprint, age_recipient)
		VALUES (?, ?, ?, '', ?, ?, ?, NULL, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name, labels = excluded.labels, roles = excluded.roles,
			enrolled_at = excluded.enrolled_at,
			cert_fingerprint = excluded.cert_fingerprint,
			-- A re-enrolment that brought no key keeps the one on record. A
			-- worker re-enrolling is the same machine with the same age key,
			-- and clearing it would silently make every secret encrypted to it
			-- unreadable.
			age_recipient = COALESCE(NULLIF(excluded.age_recipient, ''), workers.age_recipient)`,
		w.ID, w.Name, string(labels), string(roles),
		now, now, now, w.Fingerprint, w.AgeRecipient)
	if err != nil {
		return fmt.Errorf("enrolling worker: %w", err)
	}
	return nil
}

// AnyClientIdentity reports whether this deployment has ever issued a client
// identity, which is what arms the requirement that mutations be identified
// (D25).
//
// A property of the deployment rather than a setting, deliberately. There is no
// flag to turn this on and no file to forget: running `je enrol --client` is
// the act, and it is one somebody does on purpose. The alternative -- a config
// value -- is a thing that can be true while no certificate exists, which is
// the state where nobody can do anything.
//
// Enrolled rows only. A row that merely registered by claiming a name is not an
// identity, and counting one would arm the gate on the strength of a claim.
func (s *Store) AnyClientIdentity(ctx context.Context) (bool, error) {
	rows, err := s.state.QueryContext(ctx,
		`SELECT roles FROM workers WHERE enrolled_at IS NOT NULL`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return false, err
		}
		var roles []string
		if err := json.Unmarshal([]byte(raw), &roles); err != nil {
			continue // a row we cannot read is not evidence of a client
		}
		if slices.Contains(roles, RoleClient) {
			return true, nil
		}
	}
	return false, rows.Err()
}

// RecordAgeRecipient binds an age public key to an identity that already
// exists.
//
// The later half of the binding: a worker enrolled before it had a key, or one
// that ran `je worker keygen` afterwards, registers it over its own mTLS
// connection. The name comes from the verified certificate and never from a
// body, so this cannot be done on somebody else's behalf (D25).
func (s *Store) RecordAgeRecipient(ctx context.Context, id, recipient string) error {
	res, err := s.state.ExecContext(ctx,
		`UPDATE workers SET age_recipient = ? WHERE id = ? AND enrolled_at IS NOT NULL`,
		recipient, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Enrolled rows only. A row that registered by claiming a name is not
		// an identity, and letting one register a key would put a pasted claim
		// back into the one place D25 took it out of.
		return sql.ErrNoRows
	}
	return nil
}

// RecordFingerprint updates which certificate an identity presents.
//
// Separate from EnrolWorker because renewal must not touch name, labels or
// roles: those were decided at enrolment and a renewal is not an opportunity to
// revisit them.
func (s *Store) RecordFingerprint(ctx context.Context, id, fingerprint string) error {
	_, err := s.state.ExecContext(ctx,
		`UPDATE workers SET cert_fingerprint = ? WHERE id = ?`, fingerprint, id)
	return err
}
