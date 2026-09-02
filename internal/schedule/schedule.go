// Package schedule computes when a job's clock says it should run.
//
// It is deliberately free of any notion of the engine: given a specification
// and a moment, it answers "when is the next window?" and "which windows fall
// in this range?". That second question is the one that matters, and it is why
// this package exists rather than a ticker.
//
// D9's catch-up policy is only expressible because windows are a *grid* rather
// than an offset from the last run. A laptop that slept from 03:00 to 08:00 has
// missed a specific, enumerable set of windows, and the engine can decide what
// to do about each. An interval measured from the previous run cannot answer
// that question at all -- it just starts counting again, and the gap becomes
// invisible. That distinction is most of what separates this from cron.
package schedule

import (
	"errors"
	"fmt"
	"time"
)

// Schedule answers when a job should run.
type Schedule interface {
	// Next returns the first window strictly after the given time.
	Next(after time.Time) time.Time

	// Between returns every window in (from, to], oldest first.
	//
	// The limit caps how many are returned; more are reported by the second
	// return value. A laptop asleep for a month with `every: 15m` has missed
	// nearly three thousand windows, and firing them all is a denial of
	// service against yourself (D9's `all` policy needs a bound).
	Between(from, to time.Time, limit int) (windows []time.Time, truncated int)

	// String renders the schedule the way the author wrote it.
	String() string
}

// Spec is the parsed form of a job's `on:` entry.
type Spec struct {
	// Every, when non-zero, makes this an aligned interval schedule.
	Every time.Duration

	// Cron, when non-empty, is a five-field cron expression.
	Cron string

	// Location is the timezone the schedule is expressed in. Nil means the
	// system's local time, which is what a human means when they write 03:00
	// without saying more.
	Location *time.Location
}

// Parse turns a spec into a Schedule.
func Parse(s Spec) (Schedule, error) {
	loc := s.Location
	if loc == nil {
		loc = time.Local
	}

	switch {
	case s.Every > 0 && s.Cron != "":
		return nil, errors.New("a schedule has either every or cron, not both")
	case s.Every > 0:
		return newInterval(s.Every, loc)
	case s.Cron != "":
		return ParseCron(s.Cron, loc)
	default:
		return nil, errors.New("a schedule needs either every or cron")
	}
}

// MustParse is Parse for tests and constants.
func MustParse(s Spec) Schedule {
	sched, err := Parse(s)
	if err != nil {
		panic(fmt.Sprintf("schedule.MustParse(%+v): %v", s, err))
	}
	return sched
}

// between is the shared implementation of Between for both schedule kinds.
//
// Written once here rather than twice because the walk is identical and the
// only thing that varies is Next -- and a subtly different bound in one of two
// copies is exactly the bug that would make catch-up fire one window too many
// at 3am.
func between(s Schedule, from, to time.Time, limit int) ([]time.Time, int) {
	if !to.After(from) {
		return nil, 0
	}
	var (
		out       []time.Time
		truncated int
	)
	for t := s.Next(from); !t.IsZero() && !t.After(to); t = s.Next(t) {
		if len(out) < limit {
			out = append(out, t)
			continue
		}
		truncated++
		// Keep walking rather than stopping, so the caller can report how many
		// windows were actually missed. "3 fired, 2877 skipped" is a fact
		// somebody needs at 8am; "3 fired" alone is misleading.
		if truncated > maxWalk {
			break
		}
	}
	return out, truncated
}

// maxWalk bounds the count above, so a pathological range cannot spin forever.
const maxWalk = 100_000
