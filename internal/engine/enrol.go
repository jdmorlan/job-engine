package engine

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"

	"github.com/jdmorlan/job-engine/internal/ca"
	"github.com/jdmorlan/job-engine/internal/store"
)

// ErrNotEnrolled is a worker endpoint reached without an issued identity, on a
// listener that requires one.
var ErrNotEnrolled = errors.New("this connection presented no enrolled identity")

// Authority returns the control plane's certificate authority, creating it on
// first use.
//
// Lazy because a deployment that never enrols a worker should never write a CA
// key. Once created it is loaded from disk every time, so restarting does not
// silently become a different authority and invalidate every certificate ever
// issued.
func (e *Engine) Authority() (*ca.Authority, error) {
	e.authorityOnce.Do(func() {
		e.authority, e.authorityErr = ca.Open(e.opts.Layout.CADir())
	})
	return e.authority, e.authorityErr
}

// MintEnrolment issues a one-time token for a named worker with fixed labels.
//
// Labels are decided here rather than by the machine that redeems it. That is
// the whole change: a capability is not an identity (D25), and a worker that
// advertises its own `macos` can grant itself whatever a label gates. Whoever
// runs this decides what the machine is allowed to claim to be.
func (e *Engine) MintEnrolment(ctx context.Context, name string, labels []string) (string, error) {
	if name == "" {
		return "", errors.New("an enrolment needs a worker name")
	}
	if len(labels) == 0 {
		labels = []string{store.DefaultLabel}
	}
	// Created here so that a failure to write the CA key is reported by the
	// command that asked for a token, not later by a worker that cannot enrol.
	if _, err := e.Authority(); err != nil {
		return "", fmt.Errorf("preparing the certificate authority: %w", err)
	}
	return e.tokens.Issue(name, labels)
}

// Enrol redeems a token and issues a certificate for the public key presented.
//
// The row is written before the worker has ever connected, which is the
// ordering that matters: registration afterwards can only report liveness,
// because name and labels are already decided and the store refuses to let a
// registration change them.
func (e *Engine) Enrol(ctx context.Context, token string, publicKeyPEM []byte, asName string, asLabels []string) (certPEM, caPEM []byte, err error) {
	grant, err := e.tokens.Redeem(token)
	if err != nil {
		return nil, nil, err
	}

	name, labels := grant.Worker, grant.Labels
	if grant.SelfNamed {
		// A bootstrap token from this machine's own data directory. The holder
		// names itself, because the person running the worker is the person
		// running the control plane -- there is no second party here for a
		// fixed name to protect (D25).
		name, labels = asName, asLabels
		if name == "" {
			return nil, nil, errors.New("a worker enrolling locally must say what it is called")
		}
		if len(labels) == 0 {
			labels = []string{store.DefaultLabel}
		}
	}
	authority, err := e.Authority()
	if err != nil {
		return nil, nil, err
	}

	block, _ := pem.Decode(publicKeyPEM)
	if block == nil {
		return nil, nil, errors.New("the enrolling worker sent no public key")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("the enrolling worker's public key is unusable: %w", err)
	}

	certPEM, err = authority.Issue(name, pub)
	if err != nil {
		return nil, nil, err
	}
	leaf, _ := pem.Decode(certPEM)
	fingerprint := Fingerprint(leaf.Bytes)

	now := e.now()
	if err := e.store.EnrolWorker(ctx, store.Worker{
		ID:           WorkerID(name),
		Name:         name,
		Labels:       labels,
		Roles:        []string{store.RoleExecute},
		RegisteredAt: now,
		EnrolledAt:   &now,
		Fingerprint:  fingerprint,
	}); err != nil {
		return nil, nil, err
	}

	e.recordWorkerEvent(ctx, EventWorkerEnrolled, store.Worker{Name: name, Labels: labels}, "")
	e.log.Info("worker enrolled", "worker", name, "labels", labels, "fingerprint", fingerprint[:12])
	return certPEM, authority.CertPEM(), nil
}

// WorkerID is the row id for a worker name.
//
// Here as well as in the worker package because enrolment writes the row before
// any worker exists to compute it, and the two must agree or an enrolled
// identity would never match the registration that follows.
func WorkerID(name string) string { return "worker-" + name }

// Fingerprint is how a certificate is named in views and logs.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// Renew issues a fresh certificate for an identity that already holds a valid
// one.
//
// Authenticated by the certificate being replaced rather than by a token, which
// is the whole point: a worker that is already trusted should never need a human
// to keep it trusted. A token exists to bootstrap an identity from nothing, and
// renewal is not that.
//
// This is what makes a 24-hour leaf affordable. Short lifetimes are how this CA
// avoids needing revocation at all (see the ca package), and short lifetimes are
// only tolerable if nobody has to think about them.
func (e *Engine) Renew(ctx context.Context, name string, publicKeyPEM []byte) (certPEM []byte, err error) {
	if name == "" {
		return nil, ErrNotEnrolled
	}
	worker, err := e.store.WorkerByID(ctx, WorkerID(name))
	if err != nil {
		return nil, fmt.Errorf("no enrolled worker named %q", name)
	}
	if !worker.Enrolled() {
		// A worker that registered by claiming a name has nothing to renew,
		// and issuing one here would hand out an identity without the step
		// that decides what it is allowed to be.
		return nil, fmt.Errorf("worker %q was never enrolled", name)
	}

	authority, err := e.Authority()
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(publicKeyPEM)
	if block == nil {
		return nil, errors.New("the renewing worker sent no public key")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("the renewing worker's public key is unusable: %w", err)
	}

	certPEM, err = authority.Issue(name, pub)
	if err != nil {
		return nil, err
	}
	leaf, _ := pem.Decode(certPEM)

	// The recorded fingerprint follows the certificate in use, so `je workers`
	// names the identity that is actually presenting rather than the one first
	// issued.
	if err := e.store.RecordFingerprint(ctx, worker.ID, Fingerprint(leaf.Bytes)); err != nil {
		return nil, err
	}
	e.log.Info("worker certificate renewed", "worker", name)
	return certPEM, nil
}

// BootstrapToken mints the token this control plane leaves in its own data
// directory, for workers on the same machine (D25).
//
// The trust anchor is the filesystem: the directory it lands in is 0700 and
// already contains the CA private key, so a process that can read the token
// could sign its own certificates anyway. Recognising that is what removes the
// setup step from `je quickstart` and `docker compose up` without inventing a
// weaker rule to justify it.
func (e *Engine) BootstrapToken() (string, error) {
	if _, err := e.Authority(); err != nil {
		return "", err
	}
	return e.tokens.IssueBootstrap()
}
