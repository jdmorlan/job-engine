// Package model holds the domain nouns from F1: Event, Source, Trigger, Job,
// Run, Attempt, Executor, Job State, Node.
//
// It deliberately depends on nothing but the standard library. Both the store
// and the engine import it, so if it grew a dependency on either we would have
// an import cycle -- and more importantly, these types are the shared
// vocabulary of the whole program and should not be shaped by how they are
// persisted or served.
package model

import (
	"encoding/json"
	"time"
)

// Status is the lifecycle of a Run or an Attempt.
//
// The value set is duplicated in the schema's CHECK constraints. That
// duplication is intentional: the database refuses a status the code does not
// know about, which turns a typo into an error instead of a silent row.
type Status string

const (
	StatusQueued  Status = "queued"
	StatusRunning Status = "running"

	// StatusRetrying is D7's gap between two attempts: the last one failed,
	// another is due, and the run has not finished. Non-terminal, because the
	// question "did the 3am sync succeed?" does not have an answer yet.
	//
	// Its own status rather than a return to `queued` because a reader has to
	// be able to tell "waiting for a worker" from "waiting out a backoff after
	// a failure", and because a run that has already executed twice is not
	// queued in any sense a person means by the word.
	StatusRetrying    Status = "retrying"
	StatusSucceeded   Status = "succeeded"
	StatusFailed      Status = "failed"
	StatusInterrupted Status = "interrupted" // D5: we were killed, not the job
	StatusCancelled   Status = "cancelled"
	StatusTimedOut    Status = "timed_out" // D8: distinct from failed on purpose

	// StatusLost is D20/C6: the worker holding this run stopped heartbeating,
	// and "it died" is indistinguishable from "it is partitioned and still
	// running your job". Deliberately not `failed` -- we do not know that it
	// failed, and asserting so in the one place a person goes to find out what
	// happened would be a lie (P1).
	StatusLost Status = "lost"
)

// Terminal reports whether the status is final. Anything not terminal at
// daemon startup is a run we were in the middle of when we died (D5).
func (s Status) Terminal() bool {
	switch s {
	case StatusSucceeded, StatusFailed, StatusInterrupted, StatusCancelled,
		StatusTimedOut, StatusLost:
		return true
	}
	return false
}

// Source names where an event came from. It is a plain string rather than a
// closed enum because D16's whole point is that the engine knows nothing about
// the outside world -- `je emit` lets anything be a source.
type Source string

const (
	SourceEngine   Source = "engine"   // the engine talking about itself
	SourceSchedule Source = "schedule" // a clock reached a window
	SourceJob      Source = "job"      // JOB_EVENTS_FILE (D6/D17)
	SourceCLI      Source = "cli"      // `je emit`, `je run`, `je retry`
	SourceAPI      Source = "api"      // another program, e.g. Almanac (D18)
)

// Engine lifecycle event types. D16 makes the engine's own downtime a visible,
// queryable fact rather than an unexplained hole in the timeline -- which is
// what lets D9's catch-up behaviour be understood after the fact.
const (
	EventEngineStarted = "engine.started"
	EventEngineStopped = "engine.stopped"
)

// Event is the spine of the system. Everything that happens is one.
type Event struct {
	ID     int64  `json:"id"`
	Type   string `json:"type"`
	Source Source `json:"source"`

	// Payload is an opaque JSON object supplied by whoever emitted the event.
	// The engine never interprets it; triggers match against it (D3).
	//
	// json.RawMessage rather than []byte, and the difference is not cosmetic:
	// encoding/json base64-encodes a []byte, so a payload would arrive at an
	// API client as an opaque string instead of the object that was emitted.
	// RawMessage passes the bytes through verbatim in both directions.
	Payload json.RawMessage `json:"payload,omitempty"`

	// CausedByEventID and CausedByRunID carry the causation chain that
	// `je why <run>` renders. Depth is the loop guard (D3): we refuse past 10.
	CausedByEventID *int64 `json:"caused_by_event_id,omitempty"`
	CausedByRunID   *int64 `json:"caused_by_run_id,omitempty"`
	Depth           int    `json:"depth"`

	// DedupeKey, when set, is unique across all events: a source that fires
	// twice produces one event, not two runs (D16).
	DedupeKey *string `json:"dedupe_key,omitempty"`

	// Actor is who is responsible, when that is a person. It is what lets the
	// attempt list distinguish a human retry from an automatic one (D7).
	Actor     string    `json:"actor,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}
