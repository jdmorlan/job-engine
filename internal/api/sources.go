package api

import (
	"encoding/json"
	"net/http"

	"github.com/jdmorlan/job-engine/internal/store"
)

// The source registry over HTTP (D22).
//
// These are endpoints rather than a config file the CLI edits, for the reason
// D15 gives: the CLI has no privileged path to anything. A control plane in a
// container has a config the host cannot write, so "registering a repo" has to
// be a request, the same as every other capability.
func (s *Server) registerSources(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/sources", s.handleListSources)
	mux.HandleFunc("POST /v1/sources", s.handleAddSource)
	mux.HandleFunc("POST /v1/sources/{name}/sync", s.handleSyncSource)
	mux.HandleFunc("DELETE /v1/sources/{name}", s.handleRemoveSource)
}

func (s *Server) handleListSources(w http.ResponseWriter, r *http.Request) {
	sources, err := s.engine.Sources(r.Context())
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": orEmpty(sources)})
}

// AddSourceRequest registers a place definitions come from.
type AddSourceRequest struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Location string `json:"location"`
	Subpath  string `json:"subpath,omitempty"`
	Ref      string `json:"ref,omitempty"`

	// TokenSecret names a secret in the engine's store, never a token. A
	// credential must not travel through this API, and there is nowhere for it
	// to be stored if it did (D10).
	TokenSecret string `json:"token_secret,omitempty"`
}

func (s *Server) handleAddSource(w http.ResponseWriter, r *http.Request) {
	var req AddSourceRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	result, err := s.engine.AddSource(r.Context(), store.Source{
		Name:        req.Name,
		Kind:        req.Kind,
		Location:    req.Location,
		Subpath:     req.Subpath,
		Ref:         req.Ref,
		TokenSecret: req.TokenSecret,
	})
	if err != nil {
		// 422 rather than 500 for the same reason sync uses it: the request was
		// well formed and the engine is fine. What is wrong is a path, a name,
		// or the content of a file.
		s.writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (s *Server) handleSyncSource(w http.ResponseWriter, r *http.Request) {
	result, err := s.engine.SyncSource(r.Context(), r.PathValue("name"))
	if s.handleLookupError(w, err, "source") {
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleRemoveSource(w http.ResponseWriter, r *http.Request) {
	removed, err := s.engine.RemoveSource(r.Context(), r.PathValue("name"))
	if s.handleLookupError(w, err, "source") {
		return
	}
	// The count is the part worth returning: removing a registration
	// tombstones jobs, and how many is the thing somebody wants confirmed.
	writeJSON(w, http.StatusOK, map[string]any{"tombstoned": removed})
}
