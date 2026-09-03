package engine_test

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/jdmorlan/job-engine/internal/engine"
	"github.com/jdmorlan/job-engine/internal/store"
)

// Renewal is what makes a 24-hour certificate affordable: the lifetime is what
// lets this CA skip revocation entirely, and a short lifetime is only tolerable
// if nobody has to think about it (D25).
func TestRenewalIssuesAFreshCertificateForTheSameIdentity(t *testing.T) {
	ctx := context.Background()
	e, _ := chainFixture(t, nil, nil)

	token, err := e.MintEnrolment(ctx, "laptop", []string{"macos"})
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := e.Enrol(ctx, token, publicKeyPEM(t), "", nil)
	if err != nil {
		t.Fatal(err)
	}

	// A new keypair, which is the point: a key that leaked stops being useful
	// when its certificate expires rather than living as long as the worker.
	second, err := e.Renew(ctx, "laptop", publicKeyPEM(t))
	if err != nil {
		t.Fatalf("renewing: %v", err)
	}
	if string(first) == string(second) {
		t.Fatal("renewal returned the same certificate")
	}

	block, _ := pem.Decode(second)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if cert.Subject.CommonName != "laptop" {
		t.Errorf("common name = %q, want the identity being renewed", cert.Subject.CommonName)
	}

	// Renewal is not an opportunity to change what a worker is allowed to be.
	w, err := e.Workers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(w) != 1 {
		t.Fatalf("workers = %d, want 1 -- renewal must not create a second identity", len(w))
	}
	if len(w[0].Labels) != 1 || w[0].Labels[0] != "macos" {
		t.Errorf("labels = %v, want [macos] unchanged by renewal", w[0].Labels)
	}
	if w[0].Fingerprint == "" {
		t.Error("no fingerprint recorded")
	}
}

// The fingerprint follows the certificate actually in use, so `je workers` names
// the identity presenting rather than the one first issued.
func TestRenewalUpdatesTheRecordedFingerprint(t *testing.T) {
	ctx := context.Background()
	e, _ := chainFixture(t, nil, nil)

	token, _ := e.MintEnrolment(ctx, "laptop", nil)
	if _, _, err := e.Enrol(ctx, token, publicKeyPEM(t), "", nil); err != nil {
		t.Fatal(err)
	}
	before := workerNamed(t, e, "laptop").Fingerprint

	if _, err := e.Renew(ctx, "laptop", publicKeyPEM(t)); err != nil {
		t.Fatal(err)
	}
	after := workerNamed(t, e, "laptop").Fingerprint

	if before == after {
		t.Error("the recorded fingerprint did not follow the renewed certificate")
	}
	if after == "" {
		t.Error("the fingerprint was cleared rather than updated")
	}
}

// A worker that registered by claiming a name has no issued identity, and
// renewing one would hand out an identity while skipping the step that decides
// what it is allowed to be.
func TestAnUnenrolledWorkerCannotRenewIntoAnIdentity(t *testing.T) {
	ctx := context.Background()
	e, _ := chainFixture(t, nil, nil)

	if _, err := e.RegisterWorker(ctx, store.Worker{
		ID: engine.WorkerID("plain"), Name: "plain",
		Labels: []string{store.DefaultLabel}, Roles: []string{store.RoleExecute},
		Version: e.Health(ctx).Version,
	}); err != nil {
		t.Fatal(err)
	}

	_, err := e.Renew(ctx, "plain", publicKeyPEM(t))
	if err == nil {
		t.Fatal("a worker that never enrolled was issued a certificate by renewing")
	}
	if !strings.Contains(err.Error(), "never enrolled") {
		t.Errorf("error = %v, want it to say this worker was never enrolled", err)
	}
}

func TestRenewingAnUnknownWorkerIsRefused(t *testing.T) {
	ctx := context.Background()
	e, _ := chainFixture(t, nil, nil)

	if _, err := e.Renew(ctx, "nobody", publicKeyPEM(t)); err == nil {
		t.Fatal("renewed a certificate for a worker that does not exist")
	}
	if _, err := e.Renew(ctx, "", publicKeyPEM(t)); err == nil {
		t.Fatal("renewed a certificate for an empty identity")
	}
}

func workerNamed(t *testing.T, e *engine.Engine, name string) store.Worker {
	t.Helper()
	workers, err := e.Workers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range workers {
		if w.Name == name {
			return w.Worker
		}
	}
	t.Fatalf("no worker named %q", name)
	return store.Worker{}
}
