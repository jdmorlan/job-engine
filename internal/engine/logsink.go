package engine

import (
	"context"
	"path/filepath"
	"sync"
	"time"

	"github.com/jdmorlan/job-engine/internal/executor"
	"github.com/jdmorlan/job-engine/internal/store"
)

func filepathDir(path string) string { return filepath.Dir(path) }

// logFlushInterval bounds how stale the stored log can be while a job runs.
// D6 promises logs are streamed as well as stored, and `je logs -f` against a
// still-running job should not be a minute behind.
const logFlushInterval = 250 * time.Millisecond

// logFlushSize forces a write once enough lines have accumulated, so a chatty
// job does not build an unbounded buffer between ticks.
const logFlushSize = 256

// logSink buffers captured lines and writes them to the logs database.
//
// Buffered because a transaction per line would make the engine the bottleneck
// rather than the job; flushed on a timer so that buffering never turns into
// invisibility. It satisfies executor.LineWriter.
type logSink struct {
	engine  *Engine
	runID   int64
	attempt int
	live    func(stream string, ts time.Time, line string)

	mu     sync.Mutex
	buf    []store.LogLine
	seq    int64
	closed bool

	done chan struct{}
	stop chan struct{}
	once sync.Once
}

func newLogSink(e *Engine, runID int64, attempt int, live func(string, time.Time, string)) *logSink {
	s := &logSink{
		engine:  e,
		runID:   runID,
		attempt: attempt,
		live:    live,
		done:    make(chan struct{}),
		stop:    make(chan struct{}),
	}
	go s.flushLoop()
	return s
}

// WriteLine records one line. It is called from the executor's two scanner
// goroutines, so it must be safe for concurrent use.
func (s *logSink) WriteLine(stream executor.Stream, ts time.Time, line string) {
	if s.live != nil {
		// Called before taking the lock so that terminal output stays
		// immediate even while a flush is in progress.
		s.live(string(stream), ts, line)
	}

	s.mu.Lock()
	s.seq++
	s.buf = append(s.buf, store.LogLine{
		RunID:   s.runID,
		Attempt: s.attempt,
		Seq:     s.seq,
		Stream:  string(stream),
		TS:      ts,
		Line:    line,
	})
	full := len(s.buf) >= logFlushSize
	s.mu.Unlock()

	if full {
		s.flush()
	}
}

// Close flushes anything buffered and stops the timer. Safe to call twice.
func (s *logSink) Close() {
	s.once.Do(func() {
		close(s.stop)
		<-s.done
		s.flush()
	})
}

func (s *logSink) flushLoop() {
	defer close(s.done)
	ticker := time.NewTicker(logFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.flush()
		case <-s.stop:
			return
		}
	}
}

func (s *logSink) flush() {
	s.mu.Lock()
	if len(s.buf) == 0 {
		s.mu.Unlock()
		return
	}
	batch := s.buf
	s.buf = nil
	s.mu.Unlock()

	// A fresh context, not the run's: the run being cancelled or timing out is
	// exactly when its final log lines matter most, and writing them with a
	// dead context would discard the evidence.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := s.engine.store.AppendLogs(ctx, batch); err != nil {
		// Logged, not returned. Losing a log batch must not fail an otherwise
		// successful run -- but it must not be silent either.
		s.engine.log.Error("writing job logs",
			"run", s.runID, "attempt", s.attempt, "lines", len(batch), "error", err)
	}
}
