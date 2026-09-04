package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/jdmorlan/job-engine/internal/engine"
	"github.com/jdmorlan/job-engine/internal/model"
)

// The write and streaming side of runs.
//
// Triggering a run is asynchronous on purpose: the request queues work and
// returns an id, and the caller follows the live stream. Blocking an HTTP
// request until a job finishes would tie a run's lifetime to a connection, and
// D8 already allows an hour-long job by default.
func (s *Server) registerRuns(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/retention/sweep", s.handleRetentionSweep)
	mux.HandleFunc("POST /v1/runs", s.handleTriggerRun)
	mux.HandleFunc("POST /v1/runs/{id}/retry", s.handleRetryRun)
	mux.HandleFunc("GET /v1/runs/{id}/detail", s.handleRunDetail)
	mux.HandleFunc("GET /v1/runs/{id}/stream", s.handleRunStream)
}

// TriggerRequest is the body of POST /v1/runs.
//
// Actor is a fallback, not the answer. D7 uses it to tell a human intervention
// from an automatic retry, and for that to mean anything it has to be true --
// so when the connection carries a verified certificate, the identity in it
// wins and this field is ignored (D25). A claim is only accepted from a caller
// that had nothing better to offer, which after the gate arms is nobody.
type TriggerRequest struct {
	Job   string `json:"job"`
	Actor string `json:"actor,omitempty"`
}

func (s *Server) handleTriggerRun(w http.ResponseWriter, r *http.Request) {
	var req TriggerRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Job == "" {
		s.writeError(w, http.StatusBadRequest, "job is required")
		return
	}

	run, err := s.engine.TriggerRun(r.Context(), req.Job,
		engine.RunOptions{Actor: actorOf(r, req.Actor)})
	switch {
	case errors.Is(err, engine.ErrOverlapSkipped):
		// 409: the request was well formed and the engine declined for a
		// reason the caller can understand and act on.
		s.writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		if s.handleLookupError(w, err, "job") {
			return
		}
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}

// handleRetentionSweep runs D13's sweep.
//
// A write, so the identity gate covers it the moment a deployment has one
// (D25) -- which matters more here than for most: this is the endpoint that
// deletes history, and the caller is ordinarily a worker running the engine's
// own job rather than a person.
func (s *Server) handleRetentionSweep(w http.ResponseWriter, r *http.Request) {
	result, err := s.engine.Sweep(r.Context(), actorOf(r, ""))
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// RetryRequest is the body of POST /v1/runs/{id}/retry.
//
// Actor is the same fallback as TriggerRequest's, and it matters more here:
// this endpoint exists so the history can say a person intervened, and a
// verified certificate outranks anything the body claims (D25).
type RetryRequest struct {
	Actor string `json:"actor,omitempty"`
}

// handleRetryRun adds an attempt to an existing run (D7).
//
// A separate endpoint from POST /v1/runs rather than a flag on it, because
// they produce different things: one creates a run, the other adds to one.
func (s *Server) handleRetryRun(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "run id must be a number")
		return
	}
	var req RetryRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil && err != io.EOF {
		s.writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	run, err := s.engine.RetryRun(r.Context(), id, actorOf(r, req.Actor))
	switch {
	case errors.Is(err, engine.ErrNotRetryable):
		// 409, the same as an overlap skip: the request was well formed and
		// the state of the run is the reason, which the caller can act on.
		s.writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		if s.handleLookupError(w, err, "run") {
			return
		}
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}

func (s *Server) handleRunDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "run id must be a number")
		return
	}
	detail, err := s.engine.RunDetail(r.Context(), id)
	if s.handleLookupError(w, err, "run") {
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

// handleRunStream streams a run's output as server-sent events.
//
// The sequence matters and is the whole correctness of this endpoint:
//
//  1. Subscribe to the live broker FIRST. A run started by the POST that
//     preceded this request is already producing output, and subscribing after
//     reading stored lines would lose everything written in between -- which is
//     precisely the window `je run` hits.
//  2. Then read what is already stored and send it.
//  3. Then forward live events, discarding any whose sequence number was
//     already covered by step 2.
//
// Doing it the other way round is the classic tail-follow race, and it loses
// exactly the first lines of a fast job -- the ones you were watching for.
func (s *Server) handleRunStream(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "run id must be a number")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeError(w, http.StatusInternalServerError, "streaming is not supported")
		return
	}

	run, err := s.engine.Run(r.Context(), id)
	if s.handleLookupError(w, err, "run") {
		return
	}

	events, unsubscribe := s.engine.WatchRun(id)
	defer unsubscribe()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Replay what is already durable, for the attempt that is in flight or was
	// the last one.
	//
	// Skipped for a run that is between attempts (D7). Its stored lines belong
	// to the attempt that just failed, and replaying them to somebody who
	// typed `je retry` would show them the old failure's output as though the
	// new attempt had produced it.
	replayed := map[int64]bool{}
	if run.Status != model.StatusQueued && run.Status != model.StatusRetrying {
		attempt := max(run.AttemptCount, 1)
		if lines, err := s.engine.Logs(r.Context(), id, attempt); err == nil {
			for _, l := range lines {
				replayed[l.Seq] = true
				sendSSE(w, flusher, engine.StreamEvent{
					Kind: engine.StreamLog, Seq: l.Seq, Attempt: l.Attempt,
					Stream: l.Stream, TS: l.TS, Line: l.Line,
				})
			}
		}
	}

	// A run that already finished has nothing live to send. Close it out now
	// rather than leaving the client waiting for a `done` that will never come.
	if run.Status.Terminal() {
		sendSSE(w, flusher, engine.StreamEvent{
			Kind: engine.StreamDone, Status: run.Status, TS: time.Now(),
		})
		return
	}

	// Keep-alives so an idle stream is not closed by anything in between.
	keepAlive := time.NewTicker(20 * time.Second)
	defer keepAlive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepAlive.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		case ev, open := <-events:
			if !open {
				return
			}
			if ev.Kind == engine.StreamLog && replayed[ev.Seq] {
				continue // already sent from storage
			}
			sendSSE(w, flusher, ev)
			if ev.Kind == engine.StreamDone {
				return
			}
		}
	}
}

func sendSSE(w http.ResponseWriter, flusher http.Flusher, ev engine.StreamEvent) {
	body, err := json.Marshal(ev)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Kind, body)
	flusher.Flush()
}
