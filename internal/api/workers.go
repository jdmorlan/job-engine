package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/jdmorlan/job-engine/internal/engine"
	"github.com/jdmorlan/job-engine/internal/store"
)

// The data plane's endpoints (D20).
//
// These are the only way work leaves the control plane and the only way results
// come back (C1). A worker has no other channel: no database handle, no shared
// filesystem, no side door for "just this one thing".
func (s *Server) registerWorkers(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/workers", s.handleListWorkers)
	mux.HandleFunc("POST /v1/workers", s.handleRegisterWorker)
	mux.HandleFunc("POST /v1/workers/{id}/heartbeat", s.handleHeartbeat)
	mux.HandleFunc("POST /v1/workers/{id}/claim", s.handleClaim)
	mux.HandleFunc("POST /v1/runs/{id}/logs", s.handleAppendLogs)
	mux.HandleFunc("POST /v1/runs/{id}/complete", s.handleComplete)
}

func (s *Server) handleListWorkers(w http.ResponseWriter, r *http.Request) {
	workers, err := s.engine.Workers(r.Context())
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if workers == nil {
		workers = []engine.WorkerView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"workers": workers})
}

func (s *Server) handleRegisterWorker(w http.ResponseWriter, r *http.Request) {
	var req store.Worker
	if !decodeBody(s, w, r, &req) {
		return
	}

	saved, err := s.engine.RegisterWorker(r.Context(), req)
	switch {
	case errors.Is(err, engine.ErrVersionSkew), errors.Is(err, engine.ErrLabelTaken):
		// C10 and D20's duplicate-label answer are both refusals a human has to
		// act on, so they get a status that says "your request was wrong"
		// rather than "the server broke".
		s.writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, saved)
}

// HeartbeatRequest renews a lease and names the runs the worker believes it
// still holds.
type HeartbeatRequest struct {
	Holding []int64 `json:"holding,omitempty"`
}

// HeartbeatResponse carries C7's fencing list: runs the worker thinks it holds
// and does not.
type HeartbeatResponse struct {
	Revoked []int64 `json:"revoked,omitempty"`
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	var req HeartbeatRequest
	if !decodeBody(s, w, r, &req) {
		return
	}

	revoked, err := s.engine.Heartbeat(r.Context(), r.PathValue("id"), req.Holding)
	if err != nil {
		s.writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, HeartbeatResponse{Revoked: revoked})
}

// ClaimResponse carries work, or nothing.
//
// A null dispatch rather than a 404: having no work is the ordinary state of an
// idle worker, and spending an error status on it would make the logs of a
// healthy system look like the logs of a broken one.
type ClaimResponse struct {
	Dispatch *engine.Dispatch `json:"dispatch"`
}

func (s *Server) handleClaim(w http.ResponseWriter, r *http.Request) {
	dispatch, err := s.engine.Claim(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ClaimResponse{Dispatch: dispatch})
}

// AppendLogsRequest is a batch of captured lines.
type AppendLogsRequest struct {
	Attempt int              `json:"attempt"`
	Lines   []engine.LogLine `json:"lines"`
}

func (s *Server) handleAppendLogs(w http.ResponseWriter, r *http.Request) {
	runID, ok := s.pathInt(w, r, "id")
	if !ok {
		return
	}
	var req AppendLogsRequest
	if !decodeBody(s, w, r, &req) {
		return
	}

	if err := s.engine.AppendLogs(r.Context(), runID, req.Attempt, req.Lines); err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// CompleteRequest reports how an attempt ended.
type CompleteRequest struct {
	WorkerID string `json:"worker_id"`
	engine.Completion
}

func (s *Server) handleComplete(w http.ResponseWriter, r *http.Request) {
	runID, ok := s.pathInt(w, r, "id")
	if !ok {
		return
	}
	var req CompleteRequest
	if !decodeBody(s, w, r, &req) {
		return
	}

	err := s.engine.Complete(r.Context(), runID, req.WorkerID, req.Completion)
	switch {
	case errors.Is(err, engine.ErrLeaseLost):
		// C7. The worker is meant to discard its result on seeing this, so the
		// status has to be one it can distinguish from a transient failure it
		// should retry.
		s.writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// decodeBody reads a JSON body, or writes the error and reports false.
//
// An absent body is allowed and leaves the target zero, because two of these
// endpoints (claim, and a heartbeat holding nothing) have nothing to say.
func decodeBody(s *Server, w http.ResponseWriter, r *http.Request, target any) bool {
	if r.ContentLength == 0 {
		return true
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return false
	}
	return true
}

func (s *Server) pathInt(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	n, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, name+" must be a number")
		return 0, false
	}
	return n, true
}
