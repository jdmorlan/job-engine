package schedule

import (
	"fmt"
	"time"
)

// interval fires on a fixed grid aligned to local midnight.
//
// Alignment is the whole design. `every: 15m` means :00, :15, :30 and :45 --
// not "fifteen minutes after the last run finished". Two consequences follow,
// and both are the point:
//
//   - Windows are predictable. A human reading `every: 1h` expects the top of
//     the hour, and a schedule that drifts by however long the job took is a
//     schedule nobody can reason about.
//   - Missed windows are enumerable, which is what D9's catch-up needs. An
//     offset from the previous run has no concept of a window it missed.
//
// The grid resets at local midnight, so an interval that does not divide 24
// hours evenly (say 7m, or 90m) has one short window at the end of each day.
// That is a real wart, and the alternative -- aligning to the Unix epoch -- is
// worse: `every: 24h` would then fire at UTC midnight rather than local, which
// is exactly the "why did my 3am job run at 8pm" bug D19 warns about.
//
// DST: `every:` measures elapsed time, so on a 23- or 25-hour day the grid
// shifts by an hour for the rest of that day and resets at the next midnight.
// A 25-hour day gets one extra window and a 23-hour day one fewer. If what you
// want is a wall-clock time that survives DST -- "always 03:00" -- that is what
// cron is for, and the split between the two is deliberate.
type interval struct {
	every time.Duration
	loc   *time.Location
}

func newInterval(every time.Duration, loc *time.Location) (Schedule, error) {
	if every < time.Second {
		return nil, fmt.Errorf("every must be at least 1s, got %s", every)
	}
	if every > 24*time.Hour {
		// Beyond a day the grid-reset behaviour stops being intuitive, and a
		// cron expression says what you mean instead.
		return nil, fmt.Errorf("every must be at most 24h, got %s; use cron for longer periods", every)
	}
	return interval{every: every, loc: loc}, nil
}

func (i interval) String() string { return "every " + i.every.String() }

func (i interval) Next(after time.Time) time.Time {
	after = after.In(i.loc)

	// Two days is always enough: any interval of 24h or less has a window in
	// the day after the one containing `after`. The third is slack.
	for day := range 3 {
		start := startOfDay(after).AddDate(0, 0, day)
		end := startOfDay(start.AddDate(0, 0, 1))
		for t := start; t.Before(end); t = t.Add(i.every) {
			if t.After(after) {
				return t
			}
		}
	}
	return time.Time{}
}

func (i interval) Between(from, to time.Time, limit int) ([]time.Time, int) {
	return between(i, from, to, limit)
}

// startOfDay returns local midnight for the day containing t.
//
// Built from the wall-clock fields rather than by truncating, because
// Truncate operates on absolute time and would land in the wrong place on a
// day that is 23 or 25 hours long.
func startOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}
