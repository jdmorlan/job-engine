// Package ca is the control plane's certificate authority (D25 step 5).
//
// Deliberately the smallest CA that answers the question "which worker is
// this?", and no more. D25's scope rule is Jay's: *we aren't trying to solve PKI
// for every app out there, we just want an encapsulated version of it for our
// system.* Concretely that means:
//
//   - One authority, one level. No chains, no cross-signing, no intermediates.
//   - Short-lived leaves. Which is why there is no revocation list: a
//     certificate nobody renews stops working on its own, and revocation is the
//     part of PKI that is genuinely hard to get right.
//   - One enrolment moment, which already exists as `je worker join`.
//
// That is a few hundred lines of crypto/x509 rather than a second service. If
// workers ever need certificates for their *own* services, this is the wrong
// tool and step-ca is the right one -- and that is not this.
package ca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
	// caLifetime is long because rotating the authority means re-enrolling
	// every worker, and that should be a deliberate migration rather than a
	// surprise on a Tuesday.
	caLifetime = 10 * 365 * 24 * time.Hour

	// LeafLifetime is short because it is what replaces revocation. A worker
	// renews while it is healthy; one that stops being trusted stops working
	// within a day without anybody distributing a revocation list.
	//
	// A variable rather than a constant so that a test can watch a renewal
	// happen instead of waiting a day for one. Nothing in the product changes
	// it, and the ordinary lifetime is the value here.
	defaultLeafLifetime = 24 * time.Hour

	// RenewBefore is when a worker should ask for a new certificate. A third of
	// the lifetime, so a worker that is asleep or unreachable for a few hours
	// still has time to renew before it is turned away.
	defaultRenewBefore = 8 * time.Hour
)

// LeafLifetime and RenewBefore are how long an issued identity lasts and how
// early a worker replaces it. See the constants above for why they are what
// they are; they are variables only so a test can compress them.
var (
	LeafLifetime = defaultLeafLifetime
	RenewBefore  = defaultRenewBefore
)

// Authority signs worker identities.
type Authority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	dir  string
}

// Open loads the authority for a data directory, creating it on first use.
//
// Created rather than configured: a control plane with no CA cannot identify
// anything, and asking somebody to run a setup command before the system works
// is the kind of step that ends up skipped and then missed.
func Open(dir string) (*Authority, error) {
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")

	cert, key, err := load(certPath, keyPath)
	switch {
	case err == nil:
		return &Authority{cert: cert, key: key, dir: dir}, nil
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	return create(dir, certPath, keyPath)
}

func create(dir, certPath, keyPath string) (*Authority, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := serialNumber()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "je control plane CA"},
		NotBefore:             now.Add(-time.Minute), // clock skew between machines
		NotAfter:              now.Add(caLifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true, // one level, and it is enforced rather than assumed
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	if err := writePEM(certPath, "CERTIFICATE", der, 0o644); err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := writePEM(keyPath, "EC PRIVATE KEY", keyDER, 0o600); err != nil {
		return nil, err
	}
	return &Authority{cert: cert, key: key, dir: dir}, nil
}

// CertPEM is the authority's own certificate, which is public: a worker needs
// it to verify the control plane, and anybody may have it.
func (a *Authority) CertPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: a.cert.Raw})
}

// Issue signs a certificate for one worker.
//
// The name is the worker's identity and goes in the common name, so "which
// worker is this?" is answered by the certificate rather than by a string the
// worker sent (D25). Labels are *not* in here on purpose: a capability is not an
// identity, and putting them in a signed document would make changing a label an
// act of re-enrolment.
func (a *Authority) Issue(name string, publicKey any) ([]byte, error) {
	if name == "" {
		return nil, errors.New("a certificate needs a worker name")
	}
	serial, err := serialNumber()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(LeafLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, a.cert, publicKey, a.key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

// Pool returns the authority as a verifier, for checking a presented
// certificate.
func (a *Authority) Pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(a.cert)
	return pool
}

func load(certPath, keyPath string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, err
	}
	certBlock, _ := pem.Decode(certPEM)
	keyBlock, _ := pem.Decode(keyPEM)
	if certBlock == nil || keyBlock == nil {
		return nil, nil, fmt.Errorf("%s or its key is not PEM", certPath)
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

func writePEM(path, kind string, der []byte, mode os.FileMode) error {
	body := pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der})
	return os.WriteFile(path, body, mode)
}

func serialNumber() (*big.Int, error) {
	// 128 random bits, which is what makes a serial unguessable as well as
	// unique -- a counter would leak how many workers have ever enrolled.
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}

// ServerCertificate issues the control plane's own TLS certificate.
//
// Self-issued from the same authority the workers verify against, which is what
// makes this closed: there is no public CA involved, no domain to own, and no
// certificate to renew by hand. A worker trusts this control plane because it
// enrolled with it, and for no other reason.
//
// hosts are the addresses it will be reached on. An IP goes in as an IP SAN and
// a name as a DNS SAN, because a client checks one or the other and getting it
// wrong fails as an unhelpful handshake error.
func (a *Authority) ServerCertificate(hosts []string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := serialNumber()
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "je control plane"},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(caLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
			continue
		}
		template.DNSNames = append(template.DNSNames, host)
	}
	// Always reachable as localhost, because that is what the CLI on the same
	// machine uses and forgetting it makes the local case the broken one.
	template.DNSNames = append(template.DNSNames, "localhost")
	template.IPAddresses = append(template.IPAddresses, net.ParseIP("127.0.0.1"), net.ParseIP("::1"))

	der, err := x509.CreateCertificate(rand.Reader, template, a.cert, &key.PublicKey, a.key)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	)
}

// ServerTLS is the control plane's TLS configuration.
//
// VerifyClientCertIfGiven rather than RequireAndVerify, deliberately. The CLI
// and the web client are clients too and have no certificate; requiring one
// would make identity a thing that breaks every read command. A presented
// certificate is verified against this authority and becomes an identity; an
// absent one is simply nobody, and the endpoints that need an identity say so.
func (a *Authority) ServerTLS(hosts []string) (*tls.Config, error) {
	cert, err := a.ServerCertificate(hosts)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    a.Pool(),
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// FingerprintPEM is how an authority is named when somebody has to compare it
// by eye or paste it into a command.
//
// SHA-256 of the certificate's DER, hex. The same value `openssl x509 -noout
// -fingerprint -sha256` prints, minus the colons, so it can be checked with a
// tool that is already on the machine.
func FingerprintPEM(certPEM []byte) string {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return ""
	}
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:])
}
