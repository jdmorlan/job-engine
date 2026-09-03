package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Identity comes only from a chain the TLS stack verified. A certificate a
// client made up, claiming any name it likes, contributes nothing -- which is
// the difference between an identity and a string somebody sent (D25).
func TestIdentityComesOnlyFromAVerifiedChain(t *testing.T) {
	var seen string
	handler := withClientIdentity(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = IdentityOf(r.Context())
	}))

	claimed := &x509.Certificate{Subject: pkix.Name{CommonName: "macbook"}}

	t.Run("unverified is nobody", func(t *testing.T) {
		seen = "unset"
		r := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
		// Presented but never verified: exactly what a self-signed certificate
		// claiming somebody else's name looks like.
		r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{claimed}}
		handler.ServeHTTP(httptest.NewRecorder(), r)
		if seen != "" {
			t.Errorf("identity = %q, want empty for an unverified certificate", seen)
		}
	})

	t.Run("verified is somebody", func(t *testing.T) {
		seen = "unset"
		r := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
		r.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{claimed}}}
		handler.ServeHTTP(httptest.NewRecorder(), r)
		if seen != "macbook" {
			t.Errorf("identity = %q, want macbook", seen)
		}
	})

	t.Run("plaintext is nobody", func(t *testing.T) {
		seen = "unset"
		r := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
		handler.ServeHTTP(httptest.NewRecorder(), r)
		if seen != "" {
			t.Errorf("identity = %q, want empty on a plaintext connection", seen)
		}
	})
}

func TestIdentityOfIsEmptyForAPlainContext(t *testing.T) {
	if got := IdentityOf(context.Background()); got != "" {
		t.Errorf("IdentityOf = %q, want empty", got)
	}
}
