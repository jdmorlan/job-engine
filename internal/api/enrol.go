package api

import (
	"errors"
	"net/http"

	"github.com/jdmorlan/job-engine/internal/ca"
)

// registerEnrolment wires the identity endpoints (D25 step 5).
func (s *Server) registerEnrolment(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/enrol/tokens", s.handleMintEnrolment)
	mux.HandleFunc("POST /v1/enrol", s.handleEnrol)
	mux.HandleFunc("POST /v1/enrol/renew", s.handleRenew)
	mux.HandleFunc("GET /v1/ca", s.handleCA)
}

// MintEnrolmentRequest names the worker being enrolled and what it will be
// allowed to advertise.
type MintEnrolmentRequest struct {
	Name   string   `json:"name"`
	Labels []string `json:"labels,omitempty"`
}

// MintEnrolmentResponse carries the token itself, which is the only time it
// exists in readable form -- the control plane keeps a hash.
type MintEnrolmentResponse struct {
	Token   string   `json:"token"`
	Name    string   `json:"name"`
	Labels  []string `json:"labels"`
	Expires string   `json:"expires_in"`

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
	token, err := s.engine.MintEnrolment(r.Context(), req.Name, req.Labels)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	labels := req.Labels
	if len(labels) == 0 {
		labels = []string{"default"}
	}
	authority, err := s.engine.Authority()
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, MintEnrolmentResponse{
		Token: token, Name: req.Name, Labels: labels,
		Expires:       ca.TokenLifetime.String(),
		CAFingerprint: ca.FingerprintPEM(authority.CertPEM()),
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
	cert, caPEM, err := s.engine.Enrol(r.Context(), req.Token, []byte(req.PublicKey), req.Name, req.Labels)
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
