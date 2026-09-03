// Package api exposes the engine over HTTP.
//
// D15 makes this the real contract rather than a convenience layer: the daemon
// owns the database, and everything else -- the CLI, a future web UI, Almanac
// (D18) -- is a client of this API with no privileged access. The discipline
// that keeps that honest is that every capability the system has must be an
// endpoint, so there is exactly one code path into SQLite.
//
// Paths are versioned from the first commit. D18 turns the API into a contract
// consumed by another program, and an unversioned contract is one you cannot
// change without breaking someone silently.
package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/jdmorlan/job-engine/internal/engine"
	"github.com/jdmorlan/job-engine/internal/model"
)

// Server adapts an *engine.Engine to HTTP. It holds no state of its own.
type Server struct {
	engine *engine.Engine
	log    *slog.Logger
}

// New returns a Server. The logger is used for request-level problems only;
// the engine does its own logging.
func New(e *engine.Engine, log *slog.Logger) *Server {
	return &Server{engine: e, log: log}
}

// Handler returns the routed HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// Method-and-path patterns (Go 1.22+): a GET to an endpoint that only
	// accepts POST gets a 405 from the mux rather than a confusing 404.
	mux.HandleFunc("GET /v1/health", s.handleHealth)
	mux.HandleFunc("GET /v1/events", s.handleListEvents)
	mux.HandleFunc("POST /v1/events", s.handleEmitEvent)
	mux.HandleFunc("POST /v1/sync", s.handleSync)
	s.registerReads(mux)
	s.registerSecrets(mux)
	s.registerWorkers(mux)
	s.registerRuns(mux)
	s.registerSources(mux)
	s.registerEnrolment(mux)

	// Identity is attached once, here, so no handler has to remember to look
	// at r.TLS -- and so the rule that it comes only from a verified chain
	// lives in one place (D25).
	return withClientIdentity(mux)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.engine.Health(r.Context()))
}

func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, "limit must be a number")
			return
		}
		limit = n
	}

	events, err := s.engine.RecentEvents(r.Context(), limit)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if events == nil {
		events = []model.Event{} // an empty list, not JSON null
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

// EmitRequest is the body of POST /v1/events. It is the wire form of D16's
// single ingress: `je emit` posts this, and so can anything else.
type EmitRequest struct {
	Type      string          `json:"type"`
	Source    model.Source    `json:"source,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	DedupeKey *string         `json:"dedupe_key,omitempty"`
	Actor     string          `json:"actor,omitempty"`
}

// EmitResponse reports the stored event and whether it was a duplicate.
//
// Deduped is part of the response rather than an error because a repeated
// dedupe key is a success, not a failure: the caller asked for the event to
// exist, and it does. Telling them which of the two happened is P1.
type EmitResponse struct {
	Event   model.Event `json:"event"`
	Deduped bool        `json:"deduped"`
}

func (s *Server) handleEmitEvent(w http.ResponseWriter, r *http.Request) {
	var req EmitRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields() // a typo'd field is a mistake, not something to ignore
	if err := dec.Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Source == "" {
		req.Source = model.SourceAPI
	}

	event, deduped, err := s.engine.Emit(r.Context(), model.Event{
		Type:      req.Type,
		Source:    req.Source,
		Payload:   req.Payload,
		DedupeKey: req.DedupeKey,
		Actor:     req.Actor,
	})
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, EmitResponse{Event: event, Deduped: deduped})
}

// errorBody is the single error shape every endpoint returns, so a client only
// has to know one.
type errorBody struct {
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (s *Server) writeError(w http.ResponseWriter, code int, msg string) {
	var body errorBody
	body.Error.Message = msg
	writeJSON(w, code, body)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	// The response is already committed by WriteHeader, so an encode failure
	// cannot be reported to the client. Encoding into a buffer first would let
	// us 500 instead -- worth doing once a response is big enough to fail.
	_ = json.NewEncoder(w).Encode(v)
}

// handleSync reloads definitions from the source (D2, D19, D22).
//
// A POST because it changes what the engine will do, even though the argument
// is "read the files again". The alternative -- restarting the control plane to
// pick up a one-line YAML edit -- costs every in-flight run, which was a fair
// trade when the engine ran in your terminal and stopped being one when it
// became a container somewhere else.
//
// Atomic, per D19: a source that will not parse leaves the last good state
// serving and returns the error naming the file.
func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	result, err := s.engine.Sync(r.Context())
	if err != nil {
		// 422 rather than 500: the request was well-formed and the engine is
		// fine. What is wrong is the content of a file, and that distinction is
		// the difference between "fix your YAML" and "the engine is broken".
		s.writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}
