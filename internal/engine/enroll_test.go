package engine_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/jdmorlan/job-engine/internal/ca"
	"github.com/jdmorlan/job-engine/internal/engine"
	"github.com/jdmorlan/job-engine/internal/store"
)

// The hole this closes: a worker used to be whatever name it sent, advertising
// whatever labels it chose. Once a label gates access to anything, a machine
// that can declare `macos` can grant itself whatever `macos` reaches (D25).
func TestAnEnrolledWorkerCannotChooseItsOwnLabels(t *testing.T) {
	ctx := context.Background()
	e, _ := chainFixture(t, nil, nil)

	token, err := e.MintEnrollment(ctx, "laptop", []string{"macos"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Enroll(ctx, token, publicKeyPEM(t), "", nil, nil, ""); err != nil {
		t.Fatal(err)
	}

	// The worker now registers claiming something else entirely.
	registered, err := e.RegisterWorker(ctx, store.Worker{
		ID: engine.WorkerID("laptop"), Name: "impostor",
		Labels: []string{"production", "default"},
		Roles:  []string{store.RoleExecute}, Version: e.Health(ctx).Version,
	})
	if err != nil {
		t.Fatal(err)
	}

	if registered.Name != "laptop" {
		t.Errorf("name = %q, want laptop -- a worker cannot rename itself", registered.Name)
	}
	if len(registered.Labels) != 1 || registered.Labels[0] != "macos" {
		t.Errorf("labels = %v, want [macos] as decided at enrollment", registered.Labels)
	}
	if !registered.Enrolled() {
		t.Error("the worker does not report as enrolled")
	}
	if registered.Fingerprint == "" {
		t.Error("no certificate fingerprint was recorded")
	}
}

// A worker that never enrolled keeps working exactly as before. Enrollment is
// additive: the plaintext, claim-your-own-name path is what every existing
// deployment uses and it must not break underneath one.
func TestAnUnenrolledWorkerStillRegistersAsBefore(t *testing.T) {
	ctx := context.Background()
	e, _ := chainFixture(t, nil, nil)

	registered, err := e.RegisterWorker(ctx, store.Worker{
		ID: "worker-plain", Name: "plain", Labels: []string{"default"},
		Roles: []string{store.RoleExecute}, Version: e.Health(ctx).Version,
	})
	if err != nil {
		t.Fatal(err)
	}
	if registered.Name != "plain" || len(registered.Labels) != 1 {
		t.Fatalf("registered = %+v, want the claimed name and labels", registered)
	}
	if registered.Enrolled() {
		t.Error("a worker that never enrolled reports as enrolled")
	}
}

func TestAnIssuedCertificateNamesTheEnrolledWorker(t *testing.T) {
	ctx := context.Background()
	e, _ := chainFixture(t, nil, nil)

	token, err := e.MintEnrollment(ctx, "buildbox", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, caPEM, err := e.Enroll(ctx, token, publicKeyPEM(t), "", nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	block, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if cert.Subject.CommonName != "buildbox" {
		t.Errorf("common name = %q, want the enrolled name", cert.Subject.CommonName)
	}

	// A label is a capability, not an identity, so it must not be signed into
	// the certificate -- changing one would otherwise mean re-enrolling (D25).
	for _, name := range cert.DNSNames {
		if name == "default" {
			t.Error("a label was written into the certificate")
		}
	}
	if !strings.Contains(string(caPEM), "BEGIN CERTIFICATE") {
		t.Error("the worker was not given the authority to verify the control plane with")
	}
}

// One use, and nothing distinguishes the failures.
func TestATokenCannotBeRedeemedTwice(t *testing.T) {
	ctx := context.Background()
	e, _ := chainFixture(t, nil, nil)

	token, err := e.MintEnrollment(ctx, "once", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Enroll(ctx, token, publicKeyPEM(t), "", nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	_, _, err = e.Enroll(ctx, token, publicKeyPEM(t), "", nil, nil, "")
	if err == nil {
		t.Fatal("a token enrolled a second machine")
	}
	if !strings.Contains(err.Error(), ca.ErrBadToken.Error()) {
		t.Errorf("error = %v, want the same message every bad token gets", err)
	}
}

// Enrolling is a thing that happened to the fleet, so it belongs in the
// timeline like every other worker lifecycle event (D24).
func TestEnrollmentIsRecordedAsAnEvent(t *testing.T) {
	ctx := context.Background()
	e, _ := chainFixture(t, nil, nil)

	token, err := e.MintEnrollment(ctx, "recorded", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Enroll(ctx, token, publicKeyPEM(t), "", nil, nil, ""); err != nil {
		t.Fatal(err)
	}

	events, err := e.RecentEvents(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Type == engine.EventWorkerEnrolled {
			return
		}
	}
	t.Error("enrolling a worker left no event")
}

func publicKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}
