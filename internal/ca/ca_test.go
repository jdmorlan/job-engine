package ca_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"
	"time"

	"github.com/jdmorlan/job-engine/internal/ca"
)

func TestAnAuthorityIsCreatedOnceAndReloaded(t *testing.T) {
	dir := t.TempDir()

	first, err := ca.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ca.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// A control plane restarting must not become a different authority: every
	// certificate it ever issued would stop verifying.
	if string(first.CertPEM()) != string(second.CertPEM()) {
		t.Fatal("reopening the authority produced a different CA")
	}
}

func TestAnIssuedCertificateVerifiesAndNamesTheWorker(t *testing.T) {
	authority, err := ca.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	certPEM, err := authority.Issue("macbook", &key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}

	if cert.Subject.CommonName != "macbook" {
		t.Errorf("common name = %q, want the worker's identity", cert.Subject.CommonName)
	}
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:     authority.Pool(),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Fatalf("an issued certificate did not verify against its own CA: %v", err)
	}

	// Short-lived is what replaces revocation, so it has to actually be short.
	if life := cert.NotAfter.Sub(cert.NotBefore); life > 25*time.Hour {
		t.Errorf("leaf lifetime is %v, want about %v -- long leaves need a "+
			"revocation story this CA deliberately does not have", life, ca.LeafLifetime)
	}
}

// Another authority's certificate must not verify. Trivially true, and worth
// pinning: it is the whole basis for "which worker is this".
func TestAForeignCertificateDoesNotVerify(t *testing.T) {
	ours, err := ca.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := ca.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)

	certPEM, err := theirs.Issue("impostor", &key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certPEM)
	cert, _ := x509.ParseCertificate(block.Bytes)

	if _, err := cert.Verify(x509.VerifyOptions{Roots: ours.Pool()}); err == nil {
		t.Fatal("a certificate from another authority verified")
	}
}

func TestATokenEnrolsExactlyOnce(t *testing.T) {
	tokens := ca.NewTokens()

	token, err := tokens.Issue("macbook", []string{"macos"})
	if err != nil {
		t.Fatal(err)
	}
	worker, labels, err := tokens.Redeem(token)
	if err != nil {
		t.Fatal(err)
	}
	if worker != "macbook" {
		t.Errorf("worker = %q, want macbook", worker)
	}
	if len(labels) != 1 || labels[0] != "macos" {
		t.Errorf("labels = %v, want [macos] as chosen by whoever minted the token", labels)
	}

	// Replay is the attack a bearer token invites, so one use is one use.
	if _, _, err := tokens.Redeem(token); err == nil {
		t.Fatal("a token enrolled a second worker")
	}
}

// Every failure looks the same, so guessing tokens learns nothing about which
// guesses were close.
func TestEveryBadTokenFailsIdentically(t *testing.T) {
	tokens := ca.NewTokens()
	used, err := tokens.Issue("w", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := tokens.Redeem(used); err != nil {
		t.Fatal(err)
	}

	for _, token := range []string{used, "never-existed", ""} {
		_, _, err := tokens.Redeem(token)
		if err == nil {
			t.Fatalf("token %q was accepted", token)
		}
		if !strings.Contains(err.Error(), "not valid") {
			t.Errorf("token %q gave %q, want the same message as every other failure", token, err)
		}
	}
}

// The store must never hold a usable credential, so a memory dump or a future
// persistence bug cannot hand one out.
func TestTokensAreNotStoredInUsableForm(t *testing.T) {
	tokens := ca.NewTokens()
	token, err := tokens.Issue("w", nil)
	if err != nil {
		t.Fatal(err)
	}
	if tokens.Outstanding() != 1 {
		t.Fatalf("Outstanding() = %d, want 1", tokens.Outstanding())
	}
	// The only handle the API offers is a count -- there is deliberately no way
	// to ask which tokens exist.
	if _, _, err := tokens.Redeem(token); err != nil {
		t.Fatal(err)
	}
	if tokens.Outstanding() != 0 {
		t.Error("a redeemed token is still outstanding")
	}
}

// A control plane's own certificate has to verify as a server, and be reachable
// as localhost -- which is what the CLI on the same machine uses, and the case
// most easily forgotten.
func TestTheServerCertificateVerifiesForLocalhostAndItsAddress(t *testing.T) {
	authority, err := ca.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := authority.ServerTLS([]string{"192.168.1.50"})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cfg.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}

	for _, host := range []string{"localhost", "127.0.0.1", "192.168.1.50"} {
		if err := leaf.VerifyHostname(host); err != nil {
			t.Errorf("the control plane is not reachable as %s: %v", host, err)
		}
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     authority.Pool(),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("the server certificate does not verify against its own CA: %v", err)
	}
}

// A client certificate is verified if presented and not required, because the
// CLI and the web client have none and read endpoints need none. Requiring one
// would make identity a thing that breaks every read command.
func TestClientCertificatesAreVerifiedButNotRequired(t *testing.T) {
	authority, err := ca.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := authority.ServerTLS(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClientAuth != tls.VerifyClientCertIfGiven {
		t.Errorf("ClientAuth = %v, want VerifyClientCertIfGiven", cfg.ClientAuth)
	}
	if cfg.ClientCAs == nil {
		t.Error("no client CA pool, so a presented certificate could not be verified")
	}
	if cfg.MinVersion < tls.VersionTLS12 {
		t.Errorf("MinVersion = %x, want at least TLS 1.2", cfg.MinVersion)
	}
}

// The fingerprint is what somebody pastes into `--ca-pin`, so it has to be
// stable and to actually identify this authority rather than any other.
func TestTheFingerprintIdentifiesTheAuthority(t *testing.T) {
	ours, err := ca.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := ca.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	mine := ca.FingerprintPEM(ours.CertPEM())
	if len(mine) != 64 {
		t.Errorf("fingerprint = %q, want 64 hex characters of SHA-256", mine)
	}
	if mine != ca.FingerprintPEM(ours.CertPEM()) {
		t.Error("the fingerprint is not stable")
	}
	if mine == ca.FingerprintPEM(theirs.CertPEM()) {
		t.Error("two different authorities share a fingerprint")
	}
	if ca.FingerprintPEM([]byte("not a certificate")) != "" {
		t.Error("garbage produced a fingerprint, which would compare equal to nothing safely")
	}
}
