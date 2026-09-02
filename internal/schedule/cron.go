package schedule

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// cronSchedule is a parsed five-field cron expression.
//
// Fields are bitmasks rather than lists, so matching is a shift and a mask.
// That matters less for speed than for the walk in Next, which tests a lot of
// candidate minutes and stays readable only because each test is one line.
type cronSchedule struct {
	minute uint64 // bits 0-59
	hour   uint64 // bits 0-23
	dom    uint64 // bits 1-31
	month  uint64 // bits 1-12
	dow    uint64 // bits 0-6, Sunday = 0

	// domRestricted and dowRestricted record whether the author narrowed each
	// day field, which decides how the two combine. See dayMatches.
	domRestricted bool
	dowRestricted bool

	expr string
	loc  *time.Location
}

// aliases are the conventional shorthands.
var aliases = map[string]string{
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
	"@monthly":  "0 0 1 * *",
	"@weekly":   "0 0 * * 0",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@hourly":   "0 * * * *",
}

var monthNames = map[string]int{
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
	"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
}

var dayNames = map[string]int{
	"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
}

// ParseCron parses a five-field cron expression in the given location.
func ParseCron(expr string, loc *time.Location) (Schedule, error) {
	if loc == nil {
		loc = time.Local
	}
	original := expr

	if expanded, ok := aliases[strings.ToLower(strings.TrimSpace(expr))]; ok {
		expr = expanded
	}

	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron %q has %d fields, want 5: minute hour day-of-month month day-of-week",
			original, len(fields))
	}

	c := &cronSchedule{expr: original, loc: loc}
	var err error
	if c.minute, err = parseField(fields[0], 0, 59, nil); err != nil {
		return nil, fmt.Errorf("cron %q: minute: %w", original, err)
	}
	if c.hour, err = parseField(fields[1], 0, 23, nil); err != nil {
		return nil, fmt.Errorf("cron %q: hour: %w", original, err)
	}
	if c.dom, err = parseField(fields[2], 1, 31, nil); err != nil {
		return nil, fmt.Errorf("cron %q: day of month: %w", original, err)
	}
	if c.month, err = parseField(fields[3], 1, 12, monthNames); err != nil {
		return nil, fmt.Errorf("cron %q: month: %w", original, err)
	}
	if c.dow, err = parseField(fields[4], 0, 7, dayNames); err != nil {
		return nil, fmt.Errorf("cron %q: day of week: %w", original, err)
	}
	// Both 0 and 7 mean Sunday, which is conventional and worth accepting.
	if c.dow&(1<<7) != 0 {
		c.dow |= 1 << 0
		c.dow &^= 1 << 7
	}

	c.domRestricted = strings.TrimSpace(fields[2]) != "*"
	c.dowRestricted = strings.TrimSpace(fields[4]) != "*"
	return c, nil
}

func (c *cronSchedule) String() string { return c.expr }

// Next returns the first matching minute strictly after the given time.
//
// The walk happens in **wall-clock fields**, not in real time, and that choice
// is the whole of this function's DST behaviour. Walking real time gets both
// transitions wrong, and both wrongly in the direction that hurts:
//
//   - Spring forward. On the day 02:00 jumps to 03:00, a real-time walk never
//     observes minute 30 of hour 2, so "30 2 * * *" silently does not run that
//     day. A nightly job quietly skipping once a year is precisely the
//     invisible gap P1 exists to prevent.
//   - Fall back. On the day 01:00-02:00 repeats, a real-time walk observes
//     01:30 twice and fires twice. A duplicate run is a data bug for anything
//     not idempotent, which D5 already says we cannot assume.
//
// Iterating wall-clock fields fixes the second for free: 01:30 is generated
// once, and time.Date resolves it to the first of the two instants. The first
// needs the explicit branch below.
//
// The walk skips whole months and whole days when the date fields cannot
// match, so "0 0 29 2 *" is found in a few hundred iterations rather than by
// testing two million minutes.
func (c *cronSchedule) Next(after time.Time) time.Time {
	after = after.In(c.loc)
	cur := after.Truncate(time.Minute).Add(time.Minute)

	y, mo, d := cur.Date()
	h, mi := cur.Hour(), cur.Minute()
	limit := after.AddDate(5, 0, 0)

	for range maxCronIterations {
		if time.Date(y, mo, d, 0, 0, 0, 0, c.loc).After(limit) {
			// Five years without a match means the expression can never match,
			// such as 30 February.
			return time.Time{}
		}

		if c.month&(1<<uint(mo)) == 0 {
			y, mo, d = firstOfNextMonth(y, mo, c.loc)
			h, mi = 0, 0
			continue
		}
		if !c.dayMatchesWall(y, mo, d) {
			y, mo, d = nextWallDay(y, mo, d, c.loc)
			h, mi = 0, 0
			continue
		}
		if c.hour&(1<<uint(h)) == 0 {
			h, mi = h+1, 0
			if h > 23 {
				y, mo, d = nextWallDay(y, mo, d, c.loc)
				h = 0
			}
			continue
		}
		if c.minute&(1<<uint(mi)) == 0 {
			y, mo, d, h, mi = advanceMinute(y, mo, d, h, mi, c.loc)
			continue
		}

		// The wall-clock candidate matches every field. Turn it into an instant.
		t := time.Date(y, mo, d, h, mi, 0, 0, c.loc)

		if t.Hour() == h && t.Minute() == mi {
			// The wall time exists. This is the ordinary path, and on a
			// fall-back day it is the first of the two instants.
			if t.After(after) {
				return t
			}
		} else if resumed := firstRealInstant(y, mo, d, h, mi, c.loc); !resumed.IsZero() {
			// The wall time does not exist: we are inside the hour that
			// spring-forward removed. Fire at the first real instant at or
			// after the intended time -- the moment the clock resumes -- which
			// is what every other cron does.
			//
			// time.Date cannot be used directly here. Given a time in the gap
			// it normalises to *some* real instant, and for Denver's 02:30 it
			// picks 01:30, an hour early. For a nightly job that expects the
			// day's data to have landed, running early is worse than running
			// late: it silently processes less than it should.
			//
			// The matchesInstant guard prevents a double fire when the instant
			// we resume at is itself a scheduled window -- "30 2,3 * * *",
			// where 02:30 resumes onto a real 03:30 -- leaving that one to be
			// produced by its own iteration.
			if !c.matchesInstant(resumed) && resumed.After(after) {
				return resumed
			}
		}

		y, mo, d, h, mi = advanceMinute(y, mo, d, h, mi, c.loc)
	}
	return time.Time{}
}

// firstRealInstant finds the first existing wall-clock time at or after the
// given fields, for a time that falls inside a DST gap.
//
// Scanned a minute at a time rather than computed, because locating a zone
// transition exactly means reaching for internals that time does not expose.
// A gap is an hour in every jurisdiction that has one, and the bound below is
// generous enough for the historical oddities that were not.
func firstRealInstant(y int, mo time.Month, d, h, mi int, loc *time.Location) time.Time {
	const maxGapMinutes = 24 * 60
	for range maxGapMinutes {
		y, mo, d, h, mi = advanceMinute(y, mo, d, h, mi, loc)
		if t := time.Date(y, mo, d, h, mi, 0, 0, loc); t.Hour() == h && t.Minute() == mi {
			return t
		}
	}
	return time.Time{}
}

// maxCronIterations bounds the walk. Each pass advances by at least a minute,
// and usually by a day or a month, so this is far more than five years needs.
const maxCronIterations = 1_000_000

// matchesInstant reports whether a real instant's wall clock matches every
// field, used only to avoid double-firing across a spring-forward jump.
func (c *cronSchedule) matchesInstant(t time.Time) bool {
	return c.month&(1<<uint(t.Month())) != 0 &&
		c.dayMatches(t) &&
		c.hour&(1<<uint(t.Hour())) != 0 &&
		c.minute&(1<<uint(t.Minute())) != 0
}

// dayMatchesWall applies the day rule to a calendar date.
//
// The weekday is taken at noon, which is never inside a DST transition and so
// cannot be shifted onto the wrong day.
func (c *cronSchedule) dayMatchesWall(y int, mo time.Month, d int) bool {
	return c.dayMatches(time.Date(y, mo, d, 12, 0, 0, 0, c.loc))
}

// The calendar helpers below all route through time.Date, which normalises
// out-of-range fields, so month and year rollover needs no arithmetic of its
// own -- day 32 of September becomes 2 October without a special case.

func nextWallDay(y int, mo time.Month, d int, loc *time.Location) (int, time.Month, int) {
	return time.Date(y, mo, d+1, 12, 0, 0, 0, loc).Date()
}

func firstOfNextMonth(y int, mo time.Month, loc *time.Location) (int, time.Month, int) {
	return time.Date(y, mo+1, 1, 12, 0, 0, 0, loc).Date()
}

func advanceMinute(y int, mo time.Month, d int, h, mi int, loc *time.Location) (int, time.Month, int, int, int) {
	mi++
	if mi <= 59 {
		return y, mo, d, h, mi
	}
	mi = 0
	h++
	if h <= 23 {
		return y, mo, d, h, mi
	}
	y, mo, d = nextWallDay(y, mo, d, loc)
	return y, mo, d, 0, 0
}

func (c *cronSchedule) Between(from, to time.Time, limit int) ([]time.Time, int) {
	return between(c, from, to, limit)
}

// dayMatches implements cron's day rule, including the odd part.
//
// When both day-of-month and day-of-week are restricted, a day matches if
// *either* does -- not both. "0 0 1 * MON" is the first of the month and every
// Monday, not Mondays that fall on the first. This surprises everyone once, it
// is what every other cron does, and diverging from it would be worse than the
// surprise.
func (c *cronSchedule) dayMatches(t time.Time) bool {
	domHit := c.dom&(1<<uint(t.Day())) != 0
	dowHit := c.dow&(1<<uint(t.Weekday())) != 0

	switch {
	case c.domRestricted && c.dowRestricted:
		return domHit || dowHit
	case c.domRestricted:
		return domHit
	case c.dowRestricted:
		return dowHit
	default:
		return true
	}
}

// parseField parses one cron field into a bitmask.
//
// Supported: "*", "n", "a-b", "*/n", "a-b/n", and comma-separated lists of
// those. Names are accepted where the caller supplies a table.
func parseField(field string, min, max int, names map[string]int) (uint64, error) {
	field = strings.TrimSpace(field)
	if field == "" {
		return 0, fmt.Errorf("empty")
	}

	var mask uint64
	for _, part := range strings.Split(field, ",") {
		bits, err := parseRange(strings.TrimSpace(part), min, max, names)
		if err != nil {
			return 0, err
		}
		mask |= bits
	}
	if mask == 0 {
		return 0, fmt.Errorf("%q matches nothing", field)
	}
	return mask, nil
}

func parseRange(part string, min, max int, names map[string]int) (uint64, error) {
	step := 1
	if slash := strings.Index(part, "/"); slash >= 0 {
		var err error
		if step, err = strconv.Atoi(part[slash+1:]); err != nil || step < 1 {
			return 0, fmt.Errorf("%q has an invalid step", part)
		}
		part = part[:slash]
	}

	lo, hi := min, max
	switch {
	case part == "*":
		// The full range, already set.
	case strings.Contains(part, "-"):
		bounds := strings.SplitN(part, "-", 2)
		var err error
		if lo, err = parseValue(bounds[0], names); err != nil {
			return 0, err
		}
		if hi, err = parseValue(bounds[1], names); err != nil {
			return 0, err
		}
	default:
		v, err := parseValue(part, names)
		if err != nil {
			return 0, err
		}
		lo, hi = v, v
	}

	if lo < min || hi > max || lo > hi {
		return 0, fmt.Errorf("%q is outside %d-%d", part, min, max)
	}

	var mask uint64
	for v := lo; v <= hi; v += step {
		mask |= 1 << uint(v)
	}
	return mask, nil
}

func parseValue(s string, names map[string]int) (int, error) {
	s = strings.TrimSpace(s)
	if names != nil {
		if v, ok := names[strings.ToLower(s)]; ok {
			return v, nil
		}
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number", s)
	}
	return v, nil
}
