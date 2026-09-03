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
	cert, caPEM, err := s.engine.Enrol(r.Context(), req.Token, []byte(req.PublicKey))
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
