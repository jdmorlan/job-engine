package store

import (
	"context"
	"fmt"
	"time"
)

// LogLine is one captured line of job output (D6).
type LogLine struct {
	RunID   int64     `json:"run_id"`
	Attempt int       `json:"attempt"`
	Seq     int64     `json:"seq"`
	Stream  string    `json:"stream"` // "stdout" or "stderr"
	TS      time.Time `json:"ts"`
	Line    string    `json:"line"`
}

// Stream names, matching the schema's CHECK constraint.
const (
	StreamStdout = "stdout"
	StreamStderr = "stderr"
)

// AppendLogs writes a batch of lines.
//
// Batched rather than one insert per line because a chatty job can emit
// thousands of lines a second, and a transaction per line would make the engine
// the bottleneck rather than the job. The engine buffers and calls this.
func (s *Store) AppendLogs(ctx context.Context, lines []LogLine) error {
	if len(lines) == 0 {
		return nil
	}

	tx, err := s.logs.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO logs (run_id, attempt_number, seq, stream, ts, line)
		VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, l := range lines {
		if _, err := stmt.ExecContext(ctx,
			l.RunID, l.Attempt, l.Seq, l.Stream, formatTime(l.TS), l.Line,
		); err != nil {
			return fmt.Errorf("appending log line: %w", err)
		}
	}
	return tx.Commit()
}

// ReadLogs returns the captured output of one attempt, in emission order.
//
// Ordered by seq rather than by timestamp: a fast job can emit several lines
// within the same clock tick, and ordering by ts would shuffle them. The
// interleaving of stdout and stderr is information -- it is how you see which
// line the error came after.
func (s *Store) ReadLogs(ctx context.Context, runID int64, attempt int) ([]LogLine, error) {
	rows, err := s.logs.QueryContext(ctx, `
		SELECT run_id, attempt_number, seq, stream, ts, line
		FROM logs WHERE run_id = ? AND attempt_number = ? ORDER BY seq`,
		runID, attempt)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LogLine
	for rows.Next() {
		var (
			l  LogLine
			ts string
		)
		if err := rows.Scan(&l.RunID, &l.Attempt, &l.Seq, &l.Stream, &ts, &l.Line); err != nil {
			return nil, err
		}
		if l.TS, err = parseTime(ts); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
