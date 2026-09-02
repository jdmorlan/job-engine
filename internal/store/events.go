package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jdmorlan/job-engine/internal/model"
)

// timeFormat is how every timestamp in the database is written.
//
// RFC3339 with nanoseconds, always UTC, always the same width so that ordering
// as text matches ordering as time. Text rather than an integer because `je db`
// is a supported way to ask a question (D4) and "2026-09-02T14:03:11.000Z"
// answers it where 1788357791000 does not.
const timeFormat = "2006-01-02T15:04:05.000000000Z"

func formatTime(t time.Time) string { return t.UTC().Format(timeFormat) }

func parseTime(s string) (time.Time, error) { return time.Parse(timeFormat, s) }

// AppendEvent writes an event and returns it with its assigned id.
//
// If the event carries a DedupeKey that has been seen before, the existing
// event is returned unchanged and deduped is true. That is D16's guarantee: a
// source that fires the same key twice causes one run, not two.
func (s *Store) AppendEvent(ctx context.Context, e model.Event) (model.Event, bool, error) {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}

	var payload any
	if len(e.Payload) > 0 {
		payload = string(e.Payload)
	}

	err := s.state.QueryRowContext(ctx, `
		INSERT INTO events (
			type, source, payload, caused_by_event_id, caused_by_run_id,
			depth, dedupe_key, actor, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING id`,
		e.Type, string(e.Source), payload, e.CausedByEventID, e.CausedByRunID,
		e.Depth, e.DedupeKey, nullString(e.Actor), formatTime(e.CreatedAt),
	).Scan(&e.ID)

	if err != nil && e.DedupeKey != nil && isUniqueViolation(err) {
		existing, findErr := s.eventByDedupeKey(ctx, *e.DedupeKey)
		if findErr != nil {
			return model.Event{}, false, fmt.Errorf("resolving duplicate event: %w", findErr)
		}
		return existing, true, nil
	}
	if err != nil {
		return model.Event{}, false, fmt.Errorf("appending event %q: %w", e.Type, err)
	}
	return e, false, nil
}

// LastEventOfType returns the most recent event of a type, if there is one.
//
// This is what makes the engine able to say how long it was down: on start it
// asks for the last engine.stopped and compares (D16). sql.ErrNoRows is
// returned for "never happened", which the caller is expected to treat as a
// normal condition rather than a failure -- a first run has no history.
func (s *Store) LastEventOfType(ctx context.Context, eventType string) (model.Event, error) {
	return s.scanEvent(s.state.QueryRowContext(ctx, selectEvent+`
		WHERE type = ? ORDER BY id DESC LIMIT 1`, eventType))
}

// EventsCausedByRun returns everything a run emitted, oldest first.
func (s *Store) EventsCausedByRun(ctx context.Context, runID int64) ([]model.Event, error) {
	rows, err := s.state.QueryContext(ctx,
		selectEvent+` WHERE caused_by_run_id = ? ORDER BY id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.Event
	for rows.Next() {
		e, err := s.scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// EventByID returns one event.
func (s *Store) EventByID(ctx context.Context, id int64) (model.Event, error) {
	return s.scanEvent(s.state.QueryRowContext(ctx, selectEvent+` WHERE id = ?`, id))
}

// RecentEvents returns the newest events, most recent first.
func (s *Store) RecentEvents(ctx context.Context, limit int) ([]model.Event, error) {
	rows, err := s.state.QueryContext(ctx, selectEvent+` ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("listing events: %w", err)
	}
	defer rows.Close()

	var out []model.Event
	for rows.Next() {
		e, err := s.scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) eventByDedupeKey(ctx context.Context, key string) (model.Event, error) {
	return s.scanEvent(s.state.QueryRowContext(ctx, selectEvent+` WHERE dedupe_key = ?`, key))
}

const selectEvent = `
	SELECT id, type, source, payload, caused_by_event_id, caused_by_run_id,
	       depth, dedupe_key, actor, created_at
	FROM events`

// scanner is satisfied by both *sql.Row and *sql.Rows, so a single scan
// function serves the one-row and many-row queries.
type scanner interface{ Scan(dest ...any) error }

func (s *Store) scanEvent(sc scanner) (model.Event, error) {
	var (
		e         model.Event
		source    string
		payload   sql.NullString
		dedupeKey sql.NullString
		actor     sql.NullString
		createdAt string
	)
	if err := sc.Scan(
		&e.ID, &e.Type, &source, &payload, &e.CausedByEventID, &e.CausedByRunID,
		&e.Depth, &dedupeKey, &actor, &createdAt,
	); err != nil {
		return model.Event{}, err
	}
	e.Source = model.Source(source)
	if payload.Valid {
		e.Payload = json.RawMessage(payload.String)
	}
	if dedupeKey.Valid {
		e.DedupeKey = &dedupeKey.String
	}
	e.Actor = actor.String

	t, err := parseTime(createdAt)
	if err != nil {
		return model.Event{}, fmt.Errorf("event %d has unparseable created_at %q: %w", e.ID, createdAt, err)
	}
	e.CreatedAt = t
	return e, nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// isUniqueViolation reports whether err is a UNIQUE constraint failure.
//
// The modernc driver does not export a typed error for this, so we match on
// the message. Contained to this one function so that the string match is a
// single known wart rather than something spread through the package.
func isUniqueViolation(err error) bool {
	if err == nil || errors.Is(err, sql.ErrNoRows) {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}
