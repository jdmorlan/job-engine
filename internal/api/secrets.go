package api

import (
	"encoding/json"
	"net/http"
)

// The secret endpoints (D10, D15).
//
// There is no GET for a value, and there never will be: the store hands values
// only to the engine building a job's environment. What travels here is
// metadata in both directions and a value in one.
func (s *Server) registerSecrets(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/secrets", s.handleListSecrets)
	mux.HandleFunc("PUT /v1/secrets/{name}", s.handleSetSecret)
	mux.HandleFunc("DELETE /v1/secrets/{name}", s.handleDeleteSecret)
}

func (s *Server) handleListSecrets(w http.ResponseWriter, r *http.Request) {
	view, err := s.engine.SecretsView(r.Context())
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// SetSecretRequest is the body of PUT /v1/secrets/{name}.
type SetSecretRequest struct {
	Value string `json:"value"`
}

func (s *Server) handleSetSecret(w http.ResponseWriter, r *http.Request) {
	var req SetSecretRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	result, err := s.engine.SetSecret(r.Context(), r.PathValue("name"), req.Value)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleDeleteSecret(w http.ResponseWriter, r *http.Request) {
	if err := s.engine.DeleteSecret(r.Context(), r.PathValue("name")); err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": r.PathValue("name"), "deleted": true})
}
