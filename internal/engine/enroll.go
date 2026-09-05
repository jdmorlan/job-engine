package engine

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"

	"slices"

	"filippo.io/age"
	"github.com/jdmorlan/job-engine/internal/ca"
	"github.com/jdmorlan/job-engine/internal/store"
)

// ErrNotEnrolled is a worker endpoint reached without an issued identity, on a
// listener that requires one.
var ErrNotEnrolled = errors.New("this connection presented no enrolled identity")

// Authority returns the control plane's certificate authority, creating it on
// first use.
//
// Lazy because a deployment that never enrolls a worker should never write a CA
// key. Once created it is loaded from disk every time, so restarting does not
// silently become a different authority and invalidate every certificate ever
// issued.
func (e *Engine) Authority() (*ca.Authority, error) {
	e.authorityOnce.Do(func() {
		e.authority, e.authorityErr = ca.Open(e.opts.Layout.CADir())
	})
	return e.authority, e.authorityErr
}

// MintEnrollment issues a one-time token for a named identity with fixed labels
// and roles.
//
// All three are decided here rather than by the machine that redeems it. That
// is the whole change: a capability is not an identity (D25), and a worker that
// advertises its own `macos` can grant itself whatever a label gates. Whoever
// runs this decides what the machine is allowed to claim to be -- including
// whether it is a client, which is the role that can mint further identities.
func (e *Engine) MintEnrollment(ctx context.Context, name string, labels, roles []string) (string, error) {
	if name == "" {
		return "", errors.New("an enrollment needs a worker name")
	}
	if len(roles) == 0 {
		roles = []string{store.RoleExecute}
	}
	// A client is a person at a terminal. It advertises no capability, so a
	// label would be a capability nothing can act on -- and `default` in
	// particular would put it in the pool `je waiting` counts as able to serve
	// work it will never claim.
	if len(labels) == 0 && !slices.Contains(roles, store.RoleClient) {
		labels = []string{store.DefaultLabel}
	}
	// Created here so that a failure to write the CA key is reported by the
	// command that asked for a token, not later by a worker that cannot enroll.
	if _, err := e.Authority(); err != nil {
		return "", fmt.Errorf("preparing the certificate authority: %w", err)
	}
	return e.tokens.Issue(name, labels, roles)
}

// Enroll redeems a token and issues a certificate for the public key presented.
//
// The row is written before the worker has ever connected, which is the
// ordering that matters: registration afterwards can only report liveness,
// because name and labels are already decided and the store refuses to let a
// registration change them.
func (e *Engine) Enroll(ctx context.Context, token string, publicKeyPEM []byte, asName string, asLabels, asRoles []string, ageRecipient string) (certPEM, caPEM []byte, err error) {
	grant, err := e.tokens.Redeem(token)
	if err != nil {
		return nil, nil, err
	}

	name, labels, roles := grant.Worker, grant.Labels, grant.Roles
	if len(roles) == 0 {
		roles = []string{store.RoleExecute}
	}
	if grant.SelfNamed {
		// A bootstrap token from this machine's own data directory. The holder
		// names itself, because the person running the worker is the person
		// running the control plane -- there is no second party here for a
		// fixed name to protect (D25).
		name, labels = asName, asLabels
		if name == "" {
			return nil, nil, errors.New("a worker enrolling locally must say what it is called")
		}
		// Roles too, for the same reason: this is the person who runs the
		// control plane, so `je identity join` beside it must be able to ask
		// for a client identity without minting a token to hand to itself.
		if len(asRoles) > 0 {
			roles = asRoles
		}
		if len(labels) == 0 && !slices.Contains(roles, store.RoleClient) {
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
	if err := e.store.EnrollWorker(ctx, store.Worker{
		ID:           WorkerID(name),
		Name:         name,
		Labels:       labels,
		Roles:        roles,
		RegisteredAt: now,
		EnrolledAt:   &now,
		Fingerprint:  fingerprint,
		AgeRecipient: ageRecipient,
	}); err != nil {
		return nil, nil, err
	}

	e.recordWorkerEvent(ctx, EventWorkerEnrolled, store.Worker{Name: name, Labels: labels}, "")
	e.log.Info("identity enrolled", "name", name, "roles", roles,
		"labels", labels, "fingerprint", fingerprint[:12])
	return certPEM, authority.CertPEM(), nil
}

// WorkerID is the row id for a worker name.
//
// Here as well as in the worker package because enrollment writes the row before
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
// setup step from `je up` and `docker compose up` without inventing a
// weaker rule to justify it.
func (e *Engine) BootstrapToken() (string, error) {
	if _, err := e.Authority(); err != nil {
		return "", err
	}
	return e.tokens.IssueBootstrap()
}

// ErrUnidentified is a mutating request from a caller that proved nothing, on a
// deployment that has issued a client identity (D25).
var ErrUnidentified = errors.New("this request changes something and presented no identity")

// RequireIdentity is the gate on writing.
//
// Reads are ungated and stay that way: a certificate answers "who is this", and
// "who may look" is a question D25 explicitly does not ask -- N1 keeps RBAC
// out, and the CLI and the web client have no identity to prove.
//
// Writing is different, and the rule is one sentence: once a deployment has
// issued a client identity, a request that changes something must present one.
// Not a role check -- any verified identity passes, because a worker holding a
// certificate this authority signed is not an anonymous caller. The role only
// decides when the gate arms.
//
// Armed by the deployment's own state rather than by configuration. `je enroll
// --client` is the deliberate act, and until somebody performs it there is no
// identity to require and refusing writes would mean nothing worked at all.
func (e *Engine) RequireIdentity(ctx context.Context, identity string) error {
	if identity != "" {
		return nil
	}
	armed, err := e.IdentityRequired(ctx)
	if err != nil {
		// A failure to read the workers table must not become an open door.
		// This is the one place where "cannot tell" has to mean "no".
		return err
	}
	if !armed {
		return nil
	}
	return ErrUnidentified
}

// IdentityRequired reports whether this deployment has armed the gate, which it
// does by having issued a client identity.
func (e *Engine) IdentityRequired(ctx context.Context) (bool, error) {
	armed, err := e.store.AnyClientIdentity(ctx)
	if err != nil {
		return false, fmt.Errorf("checking whether an identity is required: %w", err)
	}
	return armed, nil
}

// RegisterAgeRecipient binds an age public key to the identity making the
// request (D25).
//
// The name is the caller's verified certificate, never a field in the body, so
// this cannot be done on somebody else's behalf -- which is the entire reason
// the binding is worth more than a pasted key. `je secret recipients add
// <source> <worker>` can then resolve a name to a key the control plane learned
// from the machine itself.
//
// Replacing an existing key is allowed and is not silent: a machine that runs
// `je worker keygen` again has genuinely changed which key it reads with, and
// refusing would leave the control plane's record wrong. What it cannot do is
// make the old ciphertext readable again, which is why the CLI refuses to
// overwrite the key file itself.
func (e *Engine) RegisterAgeRecipient(ctx context.Context, name, recipient string) error {
	if name == "" {
		return ErrNotEnrolled
	}
	if _, err := age.ParseX25519Recipient(recipient); err != nil {
		return fmt.Errorf("%q is not an age public key: %w", recipient, err)
	}
	if err := e.store.RecordAgeRecipient(ctx, WorkerID(name), recipient); err != nil {
		return fmt.Errorf("recording the age key for %q: %w", name, err)
	}
	e.log.Info("age key registered", "identity", name, "recipient", recipient)
	return nil
}

// RecipientFor resolves an identity's name to the age public key it reads with.
//
// The lookup behind `je secret recipients add <source> <name>`: it turns "this
// machine may read production credentials" from a string somebody pasted into a
// statement about an identity this control plane issued.
func (e *Engine) RecipientFor(ctx context.Context, name string) (string, error) {
	w, err := e.store.WorkerByID(ctx, WorkerID(name))
	if err != nil {
		return "", fmt.Errorf("no identity named %q is enrolled here", name)
	}
	if !w.Enrolled() {
		return "", fmt.Errorf(
			"%q registered by claiming that name and was never enrolled, so this "+
				"control plane has nothing to bind a key to", name)
	}
	if w.AgeRecipient == "" {
		return "", fmt.Errorf(
			"%q has not registered an age key.\n"+
				"On that machine:  je worker keygen", name)
	}
	return w.AgeRecipient, nil
}
