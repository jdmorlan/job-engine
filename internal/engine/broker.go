package engine

import (
	"sync"
	"time"

	"github.com/jdmorlan/job-engine/internal/model"
)

// StreamEventKind distinguishes what is arriving on a run's live stream.
type StreamEventKind string

const (
	StreamLog      StreamEventKind = "log"
	StreamStatus   StreamEventKind = "status"
	StreamDone     StreamEventKind = "done"
	StreamOverflow StreamEventKind = "overflow"
)

// StreamEvent is one thing that happened to a run, delivered live.
type StreamEvent struct {
	Kind StreamEventKind `json:"kind"`

	// Seq orders log lines within an attempt and lets a subscriber that also
	// read stored lines discard the overlap. Zero for non-log events.
	Seq     int64        `json:"seq,omitempty"`
	Attempt int          `json:"attempt,omitempty"`
	Stream  string       `json:"stream,omitempty"`
	TS      time.Time    `json:"ts"`
	Line    string       `json:"line,omitempty"`
	Status  model.Status `json:"status,omitempty"`

	// Detail is a sentence a client cannot compose for itself. It carries the
	// retry notice -- which attempt failed, how long the wait is, how many are
	// left -- because someone watching `je run` should see the gap explained
	// rather than a terminal that goes quiet for five minutes (D7, P1).
	Detail string `json:"detail,omitempty"`
}

// subscriberBuffer is how many events a slow subscriber may fall behind by.
//
// Beyond it we send StreamOverflow and stop, rather than blocking. Blocking
// would be far worse than dropping: the publisher is the log sink, on the path
// of a running job, and a subscriber on a bad network could stall the job
// itself. The database still has every line, so an overflow costs the client a
// re-read rather than the data.
const subscriberBuffer = 512

// logBroker fans a run's live output out to whoever is watching.
//
// The database remains the record; this is a courtesy channel that lets
// `je run` feel like running the command yourself. Nothing here is durable and
// nothing depends on it: with no subscribers, publishing is two map lookups.
type logBroker struct {
	mu   sync.Mutex
	next int64
	subs map[int64]map[int64]chan StreamEvent // runID -> subscriber id -> channel
}

func newLogBroker() *logBroker {
	return &logBroker{subs: map[int64]map[int64]chan StreamEvent{}}
}

// Subscribe returns a channel of events for one run and a function to stop
// listening. The cancel function must be called, or the subscription leaks.
func (b *logBroker) Subscribe(runID int64) (<-chan StreamEvent, func()) {
	ch := make(chan StreamEvent, subscriberBuffer)

	b.mu.Lock()
	b.next++
	id := b.next
	if b.subs[runID] == nil {
		b.subs[runID] = map[int64]chan StreamEvent{}
	}
	b.subs[runID][id] = ch
	b.mu.Unlock()

	return ch, func() { b.unsubscribe(runID, id) }
}

func (b *logBroker) unsubscribe(runID, id int64) {
	b.mu.Lock()
	defer b.mu.Unlock()

	subs, ok := b.subs[runID]
	if !ok {
		return
	}
	if ch, ok := subs[id]; ok {
		close(ch)
		delete(subs, id)
	}
	if len(subs) == 0 {
		delete(b.subs, runID)
	}
}

// Publish delivers an event to every subscriber of a run.
//
// Never blocks. A subscriber that cannot keep up is sent StreamOverflow and
// dropped, because the alternative is stalling a running job to wait for a
// terminal that stopped reading.
func (b *logBroker) Publish(runID int64, ev StreamEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for id, ch := range b.subs[runID] {
		select {
		case ch <- ev:
		default:
			// Tell them why they are being cut off, if there is room to say
			// so, then close. The stored logs are complete either way.
			select {
			case ch <- StreamEvent{Kind: StreamOverflow, TS: time.Now()}:
			default:
			}
			close(ch)
			delete(b.subs[runID], id)
		}
	}
}

// Watchers reports how many subscribers a run has, so the publisher can skip
// the work of building an event nobody will receive.
func (b *logBroker) Watchers(runID int64) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs[runID])
}

// WatchRun subscribes to a run's live events.
//
// Callers that also read stored log lines should subscribe *first* and then
// read, discarding streamed lines whose Seq they already have. Subscribing
// second would lose everything written in between, which is exactly the window
// `je run` hits: the POST that starts the run returns before the stream opens.
func (e *Engine) WatchRun(runID int64) (<-chan StreamEvent, func()) {
	return e.broker.Subscribe(runID)
}
