package api

import (
	"errors"
	"net/http"
	"slices"

	"github.com/jdmorlan/job-engine/internal/ca"
	"github.com/jdmorlan/job-engine/internal/store"
)

// registerEnrolment wires the identity endpoints (D25 step 5).
func (s *Server) registerEnrolment(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/enrol/tokens", s.handleMintEnrolment)
	mux.HandleFunc("POST /v1/enrol", s.handleEnrol)
	mux.HandleFunc("POST /v1/enrol/renew", s.handleRenew)
	mux.HandleFunc("GET /v1/ca", s.handleCA)
	mux.HandleFunc("POST /v1/identity/age-key", s.handleRegisterAgeKey)
	mux.HandleFunc("GET /v1/identities/{name}/age-key", s.handleAgeKeyFor)
}

// MintEnrolmentRequest names the worker being enrolled and what it will be
// allowed to advertise.
type MintEnrolmentRequest struct {
	Name   string   `json:"name"`
	Labels []string `json:"labels,omitempty"`

	// Roles is what the identity is for. Empty means a worker, which is what
	// every enrolment was before clients existed (D25).
	Roles []string `json:"roles,omitempty"`
}

// MintEnrolmentResponse carries the token itself, which is the only time it
// exists in readable form -- the control plane keeps a hash.
type MintEnrolmentResponse struct {
	Token   string   `json:"token"`
	Name    string   `json:"name"`
	Labels  []string `json:"labels"`
	Roles   []string `json:"roles"`
	Expires string   `json:"expires_in"`

	// ArmsIdentity reports that redeeming this token will make identity
	// mandatory for mutations on this deployment, because it is the first
	// client identity. Said at the moment it can be acted on rather than
	// discovered later by a command that stops working (D25/P1).
	ArmsIdentity bool `json:"arms_identity,omitempty"`

	// CAFingerprint is the SHA-256 of the authority's certificate, so the
	// machine redeeming this token can verify the control plane *before*
	// sending it.
	//
	// Without it enrolment is a bearer credential handed to whoever answers the
	// address, and a token stolen in transit is a worker identity. The pin
	// travels with the token because they are pasted together anyway.
	CAFingerprint string `json:"ca_fingerprint"`
}

func (s *Server) handleMintEnrolment(w http.ResponseWriter, r *http.Request) {
	var req MintEnrolmentRequest
	if !decodeBody(s, w, r, &req) {
		return
	}
	// Asked before minting, so that "this is the first one" is true of the
	// state the caller is changing rather than of the state after it.
	armed, err := s.engine.IdentityRequired(r.Context())
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	roles := req.Roles
	if len(roles) == 0 {
		roles = []string{store.RoleExecute}
	}
	token, err := s.engine.MintEnrolment(r.Context(), req.Name, req.Labels, roles)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	labels := req.Labels
	if len(labels) == 0 && !slices.Contains(roles, store.RoleClient) {
		labels = []string{store.DefaultLabel}
	}
	authority, err := s.engine.Authority()
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, MintEnrolmentResponse{
		Token: token, Name: req.Name, Labels: labels, Roles: roles,
		Expires:       ca.TokenLifetime.String(),
		CAFingerprint: ca.FingerprintPEM(authority.CertPEM()),
		ArmsIdentity:  !armed && slices.Contains(roles, store.RoleClient),
	})
}

// EnrolRequest is a worker redeeming a token with the public half of a key it
// generated and did not send.
type EnrolRequest struct {
	Token     string `json:"token"`
	PublicKey string `json:"public_key"` // PEM, PKIX

	// Name and Labels are honoured only for a bootstrap token, where the
	// enrolling process is on the control plane's own machine and names itself.
	// For every other token they are ignored, because what the identity may be
	// was decided when the token was minted.
	Name   string   `json:"name,omitempty"`
	Labels []string `json:"labels,omitempty"`
	Roles  []string `json:"roles,omitempty"`

	// AgeRecipient is the public half of the key this machine reads encrypted
	// secrets with, bound to the identity at the moment it is decided (D25).
	//
	// Sent here rather than registered afterwards so that a fresh worker is one
	// step, not two. Optional: a machine with no key enrols without one and
	// registers it later over its own mTLS connection.
	AgeRecipient string `json:"age_recipient,omitempty"`
}

// EnrolResponse is the issued identity and the authority to verify the control
// plane with. No private key crosses this boundary in either direction.
type EnrolResponse struct {
	Certificate string `json:"certificate"`
	CA          string `json:"ca"`
}

func (s *Server) handleEnrol(w http.ResponseWriter, r *http.Request) {
	var req EnrolRequest
	if !decodeBody(s, w, r, &req) {
		return
	}
	cert, caPEM, err := s.engine.Enrol(r.Context(), req.Token, []byte(req.PublicKey), req.Name, req.Labels, req.Roles, req.AgeRecipient)
	switch {
	case errors.Is(err, ca.ErrBadToken):
		// One status and one sentence for expired, used and never-issued
		// alike. Telling them apart tells somebody guessing which guesses were
		// close.
		s.writeError(w, http.StatusForbidden, err.Error())
		return
	case err != nil:
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, EnrolResponse{
		Certificate: string(cert), CA: string(caPEM),
	})
}

// handleCA serves the authority's own certificate, which is public: a client
// needs it to verify the control plane, and it is not a credential.
func (s *Server) handleCA(w http.ResponseWriter, r *http.Request) {
	authority, err := s.engine.Authority()
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Write(authority.CertPEM())
}

// RenewRequest carries a fresh public key. There is no token: the connection's
// own certificate is the credential.
type RenewRequest struct {
	PublicKey string `json:"public_key"`
}

// RenewResponse is the reissued identity.
type RenewResponse struct {
	Certificate string `json:"certificate"`
}

func (s *Server) handleRenew(w http.ResponseWriter, r *http.Request) {
	// Only a request that already proved who it is may renew, and it may only
	// renew itself -- the name comes from the verified certificate and is never
	// read from the body, so there is nothing to ask for on somebody else's
	// behalf.
	name := IdentityOf(r.Context())
	if name == "" {
		s.writeError(w, http.StatusUnauthorized,
			"renewal is authenticated by the certificate being replaced, and this "+
				"connection presented none")
		return
	}

	var req RenewRequest
	if !decodeBody(s, w, r, &req) {
		return
	}
	cert, err := s.engine.Renew(r.Context(), name, []byte(req.PublicKey))
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, RenewResponse{Certificate: string(cert)})
}

// AgeKeyRequest registers the public half of this machine's secret-reading key.
type AgeKeyRequest struct {
	Recipient string `json:"recipient"`
}

// AgeKeyResponse is the key an identity reads with. Public by construction: it
// is what you encrypt to, and holding it grants nothing.
type AgeKeyResponse struct {
	Name      string `json:"name"`
	Recipient string `json:"recipient"`
}

func (s *Server) handleRegisterAgeKey(w http.ResponseWriter, r *http.Request) {
	// The identity comes from the certificate and never from the body, so a
	// machine can register a key for itself and for nothing else.
	name := IdentityOf(r.Context())
	if name == "" {
		s.writeError(w, http.StatusUnauthorized,
			"registering an age key is authenticated by this machine's own "+
				"certificate, and this connection presented none")
		return
	}
	var req AgeKeyRequest
	if !decodeBody(s, w, r, &req) {
		return
	}
	if err := s.engine.RegisterAgeRecipient(r.Context(), name, req.Recipient); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, AgeKeyResponse{Name: name, Recipient: req.Recipient})
}

func (s *Server) handleAgeKeyFor(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	recipient, err := s.engine.RecipientFor(r.Context(), name)
	if err != nil {
		s.writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, AgeKeyResponse{Name: name, Recipient: recipient})
}
