package store

import (
	"context"
	"fmt"
	"os"
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

// LogSpace is what a reclaim found and what it did about it (D13).
//
// Returned rather than logged, because this is the body of an ordinary job:
// `system/retention` prints it, so "how much did last night's sweep get back?"
// is a question with an answer in `je logs` rather than in a metric nobody
// collects.
type LogSpace struct {
	// Converted reports that this call turned incremental auto-vacuum on for a
	// database that predates it, which costs one full VACUUM and happens once
	// in the life of a deployment.
	Converted bool `json:"converted"`

	PagesFreed  int   `json:"pages_freed"`
	PagesLeft   int   `json:"pages_left"`
	BytesBefore int64 `json:"bytes_before"`
	BytesAfter  int64 `json:"bytes_after"`
}

// Reclaimed reports whether the file actually got smaller.
func (l LogSpace) Reclaimed() int64 { return l.BytesBefore - l.BytesAfter }

// reclaimChunk is how many pages one incremental_vacuum call moves.
//
// The number exists to bound how long a single statement holds the write lock,
// which is the whole reason for preferring incremental to a full VACUUM.
// Measured on a 17MB logs database: 500 pages is about 2MB and under 7ms,
// while the full VACUUM it replaces is one uninterruptible pass over the file
// and wants twice its size in free space.
const reclaimChunk = 500

// ReclaimLogSpace returns freed pages to the operating system.
//
// Deleting rows does not shrink a SQLite database -- the pages go on a freelist
// and get reused -- so a retention sweep that only deleted would stop the file
// growing and never give a byte back. That is not what the schema comment on
// this database promises, and it is not what somebody watching a disk fill up
// means by retention.
//
// maxPages caps the work; 0 means "until the freelist is empty".
func (s *Store) ReclaimLogSpace(ctx context.Context, maxPages int) (LogSpace, error) {
	out := LogSpace{BytesBefore: fileSize(s.logsPath)}

	mode, err := s.pragmaInt(ctx, "auto_vacuum")
	if err != nil {
		return out, err
	}
	if mode != autoVacuumIncremental {
		// A database from before D13. incremental_vacuum on it is a silent
		// no-op -- it returns in microseconds having freed nothing, because
		// the pages it would move have no pointer map to update -- so the
		// conversion is not optional politeness. Silently reclaiming nothing
		// forever is precisely the failure P1 rules out, which is why this
		// reports that it happened rather than doing it quietly.
		if err := s.convertToIncremental(ctx); err != nil {
			return out, err
		}
		out.Converted = true
		out.BytesAfter = fileSize(s.logsPath)
		return out, nil
	}

	before, err := s.pragmaInt(ctx, "freelist_count")
	if err != nil {
		return out, err
	}
	// A chunk at a time, so no single statement holds the write lock for long.
	// The freelist is re-read each pass rather than counted down, because it is
	// the database's own answer and a concurrent write may have added to it.
	asked := 0
	for {
		left, err := s.pragmaInt(ctx, "freelist_count")
		if err != nil {
			return out, err
		}
		out.PagesLeft = left
		if left == 0 || (maxPages > 0 && asked >= maxPages) {
			break
		}
		chunk := min(reclaimChunk, left)
		if maxPages > 0 {
			chunk = min(chunk, maxPages-asked)
		}
		if _, err := s.logs.ExecContext(ctx,
			fmt.Sprintf(`PRAGMA incremental_vacuum(%d)`, chunk)); err != nil {
			return out, fmt.Errorf("reclaiming log space: %w", err)
		}
		asked += chunk
	}
	out.PagesFreed = before - out.PagesLeft

	// The pages are free inside the file the moment the vacuum commits, but in
	// WAL mode the file itself does not shrink until a checkpoint moves the
	// change into it. Without this the disk is unchanged and the sweep would
	// report a reclaim nobody can see.
	if _, err := s.logs.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return out, fmt.Errorf("checkpointing after a reclaim: %w", err)
	}
	out.BytesAfter = fileSize(s.logsPath)
	return out, nil
}

// autoVacuumIncremental is SQLite's value for auto_vacuum = INCREMENTAL.
const autoVacuumIncremental = 2

// convertToIncremental turns auto-vacuum on for a database created without it.
//
// The pragma alone does nothing to an existing database: SQLite records the
// setting only when the file is rewritten, so the VACUUM is what applies it.
// Neither statement may run inside a transaction, which is why this is Go and
// not a migration file -- every .sql migration runs in one.
func (s *Store) convertToIncremental(ctx context.Context) error {
	if _, err := s.logs.ExecContext(ctx, `PRAGMA auto_vacuum = INCREMENTAL`); err != nil {
		return fmt.Errorf("enabling incremental auto-vacuum: %w", err)
	}
	if _, err := s.logs.ExecContext(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("rewriting the logs database: %w", err)
	}
	if _, err := s.logs.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("checkpointing after the rewrite: %w", err)
	}
	return nil
}

func (s *Store) pragmaInt(ctx context.Context, name string) (int, error) {
	var v int
	if err := s.logs.QueryRowContext(ctx, `PRAGMA `+name).Scan(&v); err != nil {
		return 0, fmt.Errorf("reading %s: %w", name, err)
	}
	return v, nil
}

func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}
