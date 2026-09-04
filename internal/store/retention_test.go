package store

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jdmorlan/job-engine/internal/paths"
)

// D13 bounds cursor history by count rather than by age, and never expires the
// current value: it is tiny, and losing it means reprocessing from zero.
func TestCursorHistoryIsTrimmedButTheCursorIsNot(t *testing.T) {
	ctx := context.Background()
	s, err := Open(paths.Layout{Data: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	jobID := seedJobRow(t, s)
	const versions = 250
	for i := 0; i < versions; i++ {
		value, _ := json.Marshal(map[string]int{"since": i})
		if _, err := s.CommitState(ctx, StateVersion{JobID: jobID, Value: value}); err != nil {
			t.Fatalf("committing version %d: %v", i, err)
		}
	}
	current, err := s.CurrentState(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}

	trimmed, err := s.SweepState(ctx, StateHistoryVersions)
	if err != nil {
		t.Fatalf("SweepState: %v", err)
	}
	if want := int64(versions - StateHistoryVersions); trimmed != want {
		t.Errorf("trimmed %d version(s), want %d", trimmed, want)
	}

	kept, err := s.StateHistory(ctx, jobID, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != StateHistoryVersions {
		t.Errorf("%d versions kept, want %d", len(kept), StateHistoryVersions)
	}

	// The one that must never go, at any age or count.
	still, err := s.CurrentState(ctx, jobID)
	if err != nil {
		t.Fatalf("the current cursor did not survive trimming: %v", err)
	}
	if still.Version != current.Version || string(still.Value) != string(current.Value) {
		t.Errorf("the current cursor changed: v%d %s -> v%d %s",
			current.Version, current.Value, still.Version, still.Value)
	}

	// Idempotent: a second sweep with nothing to do must not keep cutting.
	again, err := s.SweepState(ctx, StateHistoryVersions)
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Errorf("a second trim removed %d more version(s)", again)
	}
}

// C4: trigger_state grows by a row per route per correlation key and nothing
// ever removed one. A trigger still inside its window is live work that
// `je waiting` is showing somebody, so only spent rows go.
func TestOnlySpentTriggersAreRemoved(t *testing.T) {
	ctx := context.Background()
	s, err := Open(paths.Layout{Data: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	jobID := seedJobRow(t, s)
	routeID := seedRouteRow(t, s, jobID)

	old := time.Now().Add(-90 * 24 * time.Hour)
	recent := time.Now()

	// fired long ago; expired long ago; still open, though its window started
	// long ago; and one that is simply recent.
	insertTrigger(t, s, routeID, "fired-old", old, old.Add(time.Hour), &old)
	insertTrigger(t, s, routeID, "expired-old", old, old.Add(time.Hour), nil)
	insertTrigger(t, s, routeID, "still-open", old, recent.Add(time.Hour), nil)
	insertTrigger(t, s, routeID, "recent", recent, recent.Add(time.Hour), nil)

	n, err := s.SweepTriggers(ctx, time.Now().Add(-30*24*time.Hour))
	if err != nil {
		t.Fatalf("SweepTriggers: %v", err)
	}
	if n != 2 {
		t.Errorf("removed %d trigger(s), want 2 (the fired one and the expired one)", n)
	}

	var left []string
	rows, err := s.state.QueryContext(ctx, `SELECT correlation_key FROM trigger_state ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		left = append(left, k)
	}
	if len(left) != 2 || left[0] != "recent" || left[1] != "still-open" {
		t.Errorf("kept %v, want the recent one and the one still inside its window", left)
	}
}

// seedJobRow inserts the minimum a job needs to exist, for tests that are about
// something else entirely.
func seedJobRow(t *testing.T, s *Store) int64 {
	t.Helper()
	ctx := context.Background()
	if _, err := s.state.ExecContext(ctx, `
		INSERT INTO sources (name, kind, added_at) VALUES ('t', 'github', ?)`,
		formatTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	if _, err := s.state.ExecContext(ctx, `
		INSERT INTO job_versions (definition_hash, definition, first_seen_at)
		VALUES ('h', '{}', ?)`, formatTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := s.state.QueryRowContext(ctx, `
		INSERT INTO jobs (name, source, definition_hash, file_path, enabled, loaded_at)
		VALUES ('t/job', 't', 'h', 'job.yaml', 1, ?) RETURNING id`,
		formatTime(time.Now())).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func seedRouteRow(t *testing.T, s *Store, jobID int64) int64 {
	t.Helper()
	var id int64
	if err := s.state.QueryRowContext(context.Background(), `
		INSERT INTO routes (chain_name, step_index, match, target_job_id, source,
		                    source_name, file_path, route_hash)
		VALUES ('c', 0, '{}', ?, 'chain_file', 't', 'c.yaml', 'rh') RETURNING id`,
		jobID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func insertTrigger(t *testing.T, s *Store, routeID int64, key string, started, expires time.Time, fired *time.Time) {
	t.Helper()
	var firedAt any
	if fired != nil {
		firedAt = formatTime(*fired)
	}
	if _, err := s.state.ExecContext(context.Background(), `
		INSERT INTO trigger_state
			(route_id, correlation_key, satisfied_conditions, window_started_at, expires_at, fired_at)
		VALUES (?, ?, '[]', ?, ?, ?)`,
		routeID, key, formatTime(started), formatTime(expires), firedAt); err != nil {
		t.Fatal(fmt.Errorf("seeding trigger %s: %w", key, err))
	}
}
