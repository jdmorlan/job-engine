package api

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
)

// The read side of the API.
//
// D15 makes this the reason the CLI is a thin client rather than a second
// program that opens the database: one code path into SQLite, and a web UI or
// Almanac (D18) becomes another client of the same endpoints rather than a
// rewrite. It became urgent with the scheduler -- until these existed, the one
// process that schedules held the data directory lock, so `je waiting` could
// not answer the question it exists to answer while anything was happening.
func (s *Server) registerReads(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/jobs", s.handleListJobs)
	mux.HandleFunc("GET /v1/jobs/{slug}", s.handleGetJob)
	mux.HandleFunc("GET /v1/jobs/{slug}/explain", s.handleExplain)
	mux.HandleFunc("GET /v1/jobs/{slug}/state", s.handleGetState)
	mux.HandleFunc("GET /v1/jobs/{slug}/state/history", s.handleStateHistory)
	mux.HandleFunc("GET /v1/runs", s.handleListRuns)
	mux.HandleFunc("GET /v1/runs/{id}", s.handleGetRun)
	mux.HandleFunc("GET /v1/runs/{id}/logs", s.handleRunLogs)
	mux.HandleFunc("GET /v1/waiting", s.handleWaiting)
	mux.HandleFunc("GET /v1/chains", s.handleListChains)
	mux.HandleFunc("GET /v1/chains/{name}", s.handleGetChain)
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.engine.Jobs(r.Context())
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": orEmpty(jobs)})
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	job, err := s.engine.Job(r.Context(), r.PathValue("slug"))
	if s.handleLookupError(w, err, "job") {
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleExplain(w http.ResponseWriter, r *http.Request) {
	x, err := s.engine.Explain(r.Context(), r.PathValue("slug"))
	if s.handleLookupError(w, err, "job") {
		return
	}
	writeJSON(w, http.StatusOK, x)
}

func (s *Server) handleGetState(w http.ResponseWriter, r *http.Request) {
	job, err := s.engine.Job(r.Context(), r.PathValue("slug"))
	if s.handleLookupError(w, err, "job") {
		return
	}
	state, err := s.engine.CurrentState(r.Context(), job.ID)
	if errors.Is(err, sql.ErrNoRows) {
		// A job with no cursor yet is a normal state, not a missing resource.
		// 200 with a null keeps the client from having to treat "never run"
		// as an error condition.
		writeJSON(w, http.StatusOK, map[string]any{"state": nil})
		return
	}
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": state})
}

func (s *Server) handleStateHistory(w http.ResponseWriter, r *http.Request) {
	job, err := s.engine.Job(r.Context(), r.PathValue("slug"))
	if s.handleLookupError(w, err, "job") {
		return
	}
	versions, err := s.engine.StateHistory(r.Context(), job.ID, intParam(r, "limit", 20))
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": orEmpty(versions)})
}

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	var jobID int64
	if slug := r.URL.Query().Get("job"); slug != "" {
		job, err := s.engine.Job(r.Context(), slug)
		if s.handleLookupError(w, err, "job") {
			return
		}
		jobID = job.ID
	}
	runs, err := s.engine.Runs(r.Context(), jobID, intParam(r, "limit", 20))
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": orEmpty(runs)})
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "run id must be a number")
		return
	}
	run, err := s.engine.Run(r.Context(), id)
	if s.handleLookupError(w, err, "run") {
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) handleRunLogs(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "run id must be a number")
		return
	}
	run, err := s.engine.Run(r.Context(), id)
	if s.handleLookupError(w, err, "run") {
		return
	}

	attempt := intParam(r, "attempt", 0)
	if attempt == 0 {
		attempt = run.AttemptCount
	}
	lines, err := s.engine.Logs(r.Context(), id, attempt)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": orEmpty(lines), "attempt": attempt})
}

func (s *Server) handleListChains(w http.ResponseWriter, r *http.Request) {
	chains, err := s.engine.Chains(r.Context())
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"chains": orEmpty(chains)})
}

func (s *Server) handleGetChain(w http.ResponseWriter, r *http.Request) {
	chain, err := s.engine.Chain(r.Context(), r.PathValue("name"))
	if s.handleLookupError(w, err, "chain") {
		return
	}
	writeJSON(w, http.StatusOK, chain)
}

func (s *Server) handleWaiting(w http.ResponseWriter, r *http.Request) {
	waiting, err := s.engine.Waiting(r.Context())
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, waiting)
}

// handleLookupError turns a missing row into a 404 and reports whether the
// request is finished.
func (s *Server) handleLookupError(w http.ResponseWriter, err error, what string) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, sql.ErrNoRows):
		s.writeError(w, http.StatusNotFound, "no such "+what)
	default:
		s.writeError(w, http.StatusInternalServerError, err.Error())
	}
	return true
}

func intParam(r *http.Request, name string, fallback int) int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return n
}

// orEmpty renders a nil slice as [] rather than null, so a client never has to
// special-case "no rows".
func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
