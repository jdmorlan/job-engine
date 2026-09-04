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
	"errors"
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
	// lives in one place (D25). The gate on writing wraps it, because it reads
	// what that middleware put there.
	return withClientIdentity(s.requireIdentityToWrite(mux))
}

// enrolmentExempt are the two write endpoints that cannot require an identity,
// because they are how one is obtained.
//
// POST /v1/enrol is authenticated by the token in its body -- a credential that
// exists precisely for a caller with nothing else. POST /v1/enrol/renew does
// its own check and is stricter than this one: it requires a certificate
// whether or not the gate is armed.
//
// Minting a token is deliberately NOT here. It is the most consequential write
// in the system -- it decides what a machine may call itself -- and a
// deployment that has armed the gate should not let an unidentified caller
// create identities. On the control plane's own machine `je identity join`
// needs no token, so this cannot lock anybody out of their own deployment.
var enrolmentExempt = map[string]bool{
	"POST /v1/enrol":       true,
	"POST /v1/enrol/renew": true,
}

// requireIdentityToWrite refuses a mutating request that proved nothing, once
// this deployment has issued a client identity (D25).
//
// Gated by HTTP method rather than by a list of routes, which is the whole
// reason it is written this way: a list is a thing somebody forgets to add to,
// and the endpoint they forget is by definition the one nobody thought about.
// Every write is covered the day it is added, and an exemption has to be made
// deliberately and named above.
//
// Reads stay open. A certificate answers "who is this", and D25 does not ask
// "who may look" -- N1 keeps RBAC out, and the CLI and the web client read
// constantly with no identity to prove.
func (s *Server) requireIdentityToWrite(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		default:
			next.ServeHTTP(w, r)
			return
		}
		// Matched on the path rather than the routed pattern: this runs before
		// the mux, so there is no pattern yet. Both exempt paths are literal.
		if enrolmentExempt[r.Method+" "+r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}

		err := s.engine.RequireIdentity(r.Context(), IdentityOf(r.Context()))
		switch {
		case errors.Is(err, engine.ErrUnidentified):
			s.writeError(w, http.StatusUnauthorized,
				"this control plane has issued a client identity, so a request that "+
					"changes something has to present one.\n"+
					"This connection presented no certificate.\n\n"+
					"Get one for this machine:\n"+
					"  je enrol <name> --client        (on the control plane)\n"+
					"  je identity join --token <t> --ca-pin <fp> --addr <host:port>\n\n"+
					"Beside the control plane itself, `je identity join` needs no token.")
			return
		case err != nil:
			s.writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		next.ServeHTTP(w, r)
	})
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
		Actor:     actorOf(r, req.Actor),
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
