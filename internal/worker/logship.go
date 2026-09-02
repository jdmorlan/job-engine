package worker

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jdmorlan/job-engine/internal/engine"
	"github.com/jdmorlan/job-engine/internal/executor"
)

// flushInterval bounds how stale the stored log can be while a job runs.
// D6 promises logs are streamed as well as stored, and `je logs -f` against a
// still-running job should not be a minute behind.
const flushInterval = 250 * time.Millisecond

// flushSize forces a send once enough lines have accumulated, so a chatty job
// does not build an unbounded buffer between ticks.
const flushSize = 256

// logShipper buffers captured lines and posts them to the control plane.
//
// This is the buffering that used to happen inside the engine, moved to the far
// side of D20's seam. The reason is unchanged -- a round trip per line would
// make the transport the bottleneck rather than the job -- but the stakes went
// up, since the round trip is now a network call rather than a transaction.
//
// It satisfies executor.LineWriter.
type logShipper struct {
	client  *Client
	runID   int64
	attempt int
	log     *slog.Logger

	mu     sync.Mutex
	buf    []engine.LogLine
	closed bool

	done chan struct{}
	stop chan struct{}
	once sync.Once
}

func newLogShipper(c *Client, runID int64, attempt int, log *slog.Logger) *logShipper {
	s := &logShipper{
		client: c, runID: runID, attempt: attempt, log: log,
		done: make(chan struct{}), stop: make(chan struct{}),
	}
	go s.flushLoop()
	return s
}

// WriteLine records one line. It is called from the executor's two scanner
// goroutines, so it must be safe for concurrent use.
//
// Note what is absent: redaction. The worker sends what the process printed and
// the control plane strips secrets on the way into storage (D10), so a worker
// that is buggy -- or that somebody has tampered with -- cannot put a secret
// into the permanent record.
func (s *logShipper) WriteLine(stream executor.Stream, ts time.Time, line string) {
	s.mu.Lock()
	s.buf = append(s.buf, engine.LogLine{Stream: string(stream), TS: ts, Line: line})
	full := len(s.buf) >= flushSize
	s.mu.Unlock()

	if full {
		s.flush()
	}
}

// Close flushes anything buffered and stops the timer. Safe to call twice.
func (s *logShipper) Close() {
	s.once.Do(func() {
		close(s.stop)
		<-s.done
		s.flush()
	})
}

func (s *logShipper) flushLoop() {
	defer close(s.done)
	ticker := time.NewTicker(flushInterval)
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

func (s *logShipper) flush() {
	s.mu.Lock()
	if len(s.buf) == 0 {
		s.mu.Unlock()
		return
	}
	batch := s.buf
	s.buf = nil
	s.mu.Unlock()

	// Detached from the run's context on purpose: a job that was cancelled
	// still printed the lines explaining why, and losing them is losing the
	// only account of what happened (P1).
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := s.client.AppendLogs(ctx, s.runID, s.attempt, batch); err != nil {
		// Dropped rather than retried forever. The alternative is a worker that
		// blocks a finished job because the control plane is briefly away, and
		// the run's outcome matters more than its log tail.
		s.log.Warn("shipping logs", "run", s.runID, "lines", len(batch), "error", err)
	}
}
