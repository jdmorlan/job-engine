package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Partial satisfaction of a fan-in trigger (D3).
//
// The table is the one 0001_init shipped: D3 put `trigger_state` in the schema
// from day one precisely so that building fan-in later would not be a
// migration, and it was right. What was left open was the shape of
// `satisfied_conditions`, which is this file's only real decision.
//
// One row per route, holding the conditions currently met. Not a row per
// condition: the question asked on every incoming event is "is this set
// complete, and recently enough", and a set is one read rather than N.
//
// `correlation_key` is carried through as "" and reserved. It anticipates
// fan-in *per key* -- "when both feeds for 2026-09-04 have landed" -- which D3
// does not specify and no job has needed. Leaving it in the key means adding it
// later is a feature rather than a migration, which was the point of the column
// existing at all.

// Satisfaction is one condition of a fan-in that has been met, and what met it.
type Satisfaction struct {
	ConditionIndex int       `json:"condition"`
	EventID        int64     `json:"event_id"`
	SatisfiedAt    time.Time `json:"satisfied_at"`
}

// TriggerWindow is the current, partly-satisfied state of one fan-in.
type TriggerWindow struct {
	RouteID   int64
	Satisfied []Satisfaction
	StartedAt time.Time
	ExpiresAt time.Time
}

// satisfiedJSON is how the set is stored, keyed by condition index so that a
// second event for a condition replaces the first rather than accumulating.
type satisfiedJSON map[string]struct {
	EventID int64     `json:"event_id"`
	At      time.Time `json:"at"`
}

// SatisfyCondition records that a condition has been met, and returns the
// window as it now stands.
//
// Read-modify-write in one transaction, because two events arriving together
// must not each write a set that omits the other's condition -- which would
// leave a fan-in permanently one short while looking fine.
func (s *Store) SatisfyCondition(ctx context.Context, routeID int64, condition int,
	eventID int64, at time.Time, within time.Duration) (TriggerWindow, error) {

	tx, err := s.state.BeginTx(ctx, nil)
	if err != nil {
		return TriggerWindow{}, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	set := satisfiedJSON{}
	var raw, started string
	err = tx.QueryRowContext(ctx, `
		SELECT satisfied_conditions, window_started_at FROM trigger_state
		WHERE route_id = ? AND correlation_key = ''`, routeID).Scan(&raw, &started)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Nothing pending: this event opens a window.
	case err != nil:
		return TriggerWindow{}, err
	default:
		if err := json.Unmarshal([]byte(raw), &set); err != nil {
			return TriggerWindow{}, fmt.Errorf("reading a pending trigger: %w", err)
		}
	}

	// The window is a sliding lookback from the event that just arrived, so
	// anything older than it falls out here rather than being swept later.
	// That is what makes this restart-safe: the answer depends on stored times
	// and this event, never on how long the process has been running.
	cutoff := at.Add(-within)
	for key, entry := range set {
		if entry.At.Before(cutoff) {
			delete(set, key)
		}
	}
	set[fmt.Sprint(condition)] = struct {
		EventID int64     `json:"event_id"`
		At      time.Time `json:"at"`
	}{EventID: eventID, At: at}

	window := TriggerWindow{RouteID: routeID}
	window.StartedAt = at
	for key, entry := range set {
		var index int
		if _, err := fmt.Sscanf(key, "%d", &index); err != nil {
			continue
		}
		window.Satisfied = append(window.Satisfied, Satisfaction{
			ConditionIndex: index, EventID: entry.EventID, SatisfiedAt: entry.At,
		})
		if entry.At.Before(window.StartedAt) {
			window.StartedAt = entry.At
		}
	}
	window.ExpiresAt = window.StartedAt.Add(within)

	body, err := json.Marshal(set)
	if err != nil {
		return TriggerWindow{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO trigger_state
			(route_id, correlation_key, satisfied_conditions, window_started_at, expires_at)
		VALUES (?, '', ?, ?, ?)
		ON CONFLICT(route_id, correlation_key) DO UPDATE SET
			satisfied_conditions = excluded.satisfied_conditions,
			window_started_at    = excluded.window_started_at,
			expires_at           = excluded.expires_at`,
		routeID, string(body), formatTime(window.StartedAt), formatTime(window.ExpiresAt),
	); err != nil {
		return TriggerWindow{}, fmt.Errorf("recording a satisfied condition: %w", err)
	}
	return window, tx.Commit()
}

// PendingTriggers returns every route with a partly-satisfied window, for the
// view that makes this feature worth having (D3: "why hasn't the rollup run?").
func (s *Store) PendingTriggers(ctx context.Context) (map[int64]TriggerWindow, error) {
	rows, err := s.state.QueryContext(ctx, `
		SELECT route_id, satisfied_conditions, window_started_at, expires_at
		FROM trigger_state WHERE correlation_key = '' AND fired_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[int64]TriggerWindow{}
	for rows.Next() {
		var w TriggerWindow
		var raw, started string
		var expires sql.NullString
		if err := rows.Scan(&w.RouteID, &raw, &started, &expires); err != nil {
			return nil, err
		}
		if w.StartedAt, err = parseTime(started); err != nil {
			return nil, err
		}
		if expires.Valid {
			if w.ExpiresAt, err = parseTime(expires.String); err != nil {
				return nil, err
			}
		}
		set := satisfiedJSON{}
		if err := json.Unmarshal([]byte(raw), &set); err != nil {
			continue // a row this program cannot read is not worth failing the view over
		}
		for key, entry := range set {
			var index int
			if _, err := fmt.Sscanf(key, "%d", &index); err != nil {
				continue
			}
			w.Satisfied = append(w.Satisfied, Satisfaction{
				ConditionIndex: index, EventID: entry.EventID, SatisfiedAt: entry.At,
			})
		}
		out[w.RouteID] = w
	}
	return out, rows.Err()
}

// ClearTrigger consumes the events that satisfied a route, so the same set
// cannot fire it twice (D3's once_per_window).
func (s *Store) ClearTrigger(ctx context.Context, routeID int64) error {
	_, err := s.state.ExecContext(ctx,
		`DELETE FROM trigger_state WHERE route_id = ? AND correlation_key = ''`, routeID)
	return err
}
