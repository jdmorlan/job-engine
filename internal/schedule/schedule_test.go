package schedule_test

import (
	"testing"
	"time"

	"github.com/jdmorlan/job-engine/internal/schedule"
)

// denver observes DST, which makes it a better test location than UTC for
// anything involving wall-clock reasoning.
func denver(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		t.Fatalf("loading timezone: %v", err)
	}
	return loc
}

func at(t *testing.T, loc *time.Location, s string) time.Time {
	t.Helper()
	parsed, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
	if err != nil {
		t.Fatalf("parsing %q: %v", s, err)
	}
	return parsed
}

func TestCronNext(t *testing.T) {
	loc := time.UTC
	tests := []struct {
		expr string
		from string
		want string
	}{
		{"*/15 * * * *", "2026-09-02 10:07", "2026-09-02 10:15"},
		{"0 * * * *", "2026-09-02 10:07", "2026-09-02 11:00"},
		{"0 3 * * *", "2026-09-02 10:07", "2026-09-03 03:00"},
		{"30 2 * * *", "2026-09-02 01:00", "2026-09-02 02:30"},
		{"0 0 1 * *", "2026-09-02 10:07", "2026-10-01 00:00"},
		{"@daily", "2026-09-02 10:07", "2026-09-03 00:00"},
		{"@hourly", "2026-09-02 10:07", "2026-09-02 11:00"},
		{"0 9 * * mon", "2026-09-02 10:07", "2026-09-07 09:00"}, // 2 Sep 2026 is a Wednesday
		{"0 0 29 2 *", "2026-09-02 10:07", "2028-02-29 00:00"},  // next leap year
		{"5,35 * * * *", "2026-09-02 10:07", "2026-09-02 10:35"},
		{"0 9-17 * * *", "2026-09-02 18:30", "2026-09-03 09:00"},
	}
	for _, tc := range tests {
		t.Run(tc.expr, func(t *testing.T) {
			s, err := schedule.ParseCron(tc.expr, loc)
			if err != nil {
				t.Fatalf("ParseCron: %v", err)
			}
			got := s.Next(at(t, loc, tc.from))
			want := at(t, loc, tc.want)
			if !got.Equal(want) {
				t.Errorf("Next(%s) = %s, want %s", tc.from, got, want)
			}
		})
	}
}

// TestCronDayOfMonthAndWeekAreOred pins cron's oddest rule. Everyone is
// surprised by it once; diverging from every other cron would be worse.
func TestCronDayOfMonthAndWeekAreOred(t *testing.T) {
	loc := time.UTC
	s, err := schedule.ParseCron("0 0 1 * mon", loc)
	if err != nil {
		t.Fatal(err)
	}
	// From mid-September 2026: the next Monday is the 7th, and the 1st of
	// October also matches. The Monday must come first, proving it is an OR
	// rather than an AND.
	got := s.Next(at(t, loc, "2026-09-02 12:00"))
	if want := at(t, loc, "2026-09-07 00:00"); !got.Equal(want) {
		t.Errorf("Next = %s, want %s (day-of-month and day-of-week are ORed)", got, want)
	}
}

// TestCronSpringForward covers the hour that does not exist.
//
// On 8 March 2026 Denver jumps 02:00 -> 03:00, so "30 2 * * *" has no
// wall-clock time to run at. Two wrong answers are available and we reject
// both: skipping the day (a nightly job silently not running once a year, the
// invisible gap P1 exists to prevent) and running early at 01:30, which is
// what Go's time.Date normalises to and which quietly processes an hour less
// data than the author asked for.
//
// The right answer is the moment the clock resumes.
func TestCronSpringForward(t *testing.T) {
	loc := denver(t)
	s, err := schedule.ParseCron("30 2 * * *", loc)
	if err != nil {
		t.Fatal(err)
	}

	got := s.Next(at(t, loc, "2026-03-08 00:00"))
	if got.IsZero() {
		t.Fatal("no window found on the spring-forward day")
	}
	if got.Day() != 8 {
		t.Fatalf("Next = %s; the job was skipped on the spring-forward day", got)
	}

	intended := time.Date(2026, 3, 8, 2, 30, 0, 0, loc)
	if got.Before(intended) {
		t.Errorf("Next = %s, which is before the intended 02:30; a job that runs early "+
			"processes less data than it was asked to", got.Format("15:04 MST"))
	}
	if want := time.Date(2026, 3, 8, 3, 0, 0, 0, loc); !got.Equal(want) {
		t.Errorf("Next = %s, want %s (the first real instant after the gap)",
			got.Format("15:04 MST"), want.Format("15:04 MST"))
	}
}

// TestCronFallBack covers the hour that happens twice.
//
// On 1 November 2026 Denver repeats 01:00-02:00. A job at 01:30 must fire
// once, not twice -- a duplicate nightly run is a data bug for anything
// non-idempotent.
func TestCronFallBack(t *testing.T) {
	loc := denver(t)
	s, err := schedule.ParseCron("30 1 * * *", loc)
	if err != nil {
		t.Fatal(err)
	}

	from := at(t, loc, "2026-10-31 12:00")
	to := at(t, loc, "2026-11-02 12:00")
	windows, truncated := s.Between(from, to, 100)
	if truncated != 0 {
		t.Fatalf("unexpectedly truncated %d windows", truncated)
	}

	var onFallBackDay int
	for _, w := range windows {
		if w.Month() == time.November && w.Day() == 1 {
			onFallBackDay++
		}
	}
	if onFallBackDay != 1 {
		t.Errorf("01:30 fired %d times on the fall-back day, want exactly 1: %v",
			onFallBackDay, windows)
	}
}

func TestIntervalIsAlignedToMidnight(t *testing.T) {
	loc := time.UTC
	s := schedule.MustParse(schedule.Spec{Every: 15 * time.Minute, Location: loc})

	// Aligned, not "15 minutes from now". A human reading `every: 15m` expects
	// :00, :15, :30, :45.
	got := s.Next(at(t, loc, "2026-09-02 10:07"))
	if want := at(t, loc, "2026-09-02 10:15"); !got.Equal(want) {
		t.Errorf("Next = %s, want %s", got, want)
	}

	got = s.Next(at(t, loc, "2026-09-02 10:15"))
	if want := at(t, loc, "2026-09-02 10:30"); !got.Equal(want) {
		t.Errorf("Next after a window = %s, want %s", got, want)
	}
}

func TestIntervalCrossesMidnight(t *testing.T) {
	loc := time.UTC
	s := schedule.MustParse(schedule.Spec{Every: time.Hour, Location: loc})
	got := s.Next(at(t, loc, "2026-09-02 23:30"))
	if want := at(t, loc, "2026-09-03 00:00"); !got.Equal(want) {
		t.Errorf("Next = %s, want %s", got, want)
	}
}

// TestBetweenEnumeratesMissedWindows is the behaviour D9's catch-up is built
// on: a gap is a specific, countable set of windows, not just elapsed time.
func TestBetweenEnumeratesMissedWindows(t *testing.T) {
	loc := time.UTC
	s := schedule.MustParse(schedule.Spec{Every: 15 * time.Minute, Location: loc})

	// The laptop slept from 03:00 to 05:00.
	from := at(t, loc, "2026-09-02 03:00")
	to := at(t, loc, "2026-09-02 05:00")

	windows, truncated := s.Between(from, to, 100)
	if truncated != 0 {
		t.Errorf("truncated = %d, want 0", truncated)
	}
	// (03:00, 05:00] on a 15m grid is 03:15 through 05:00 inclusive: 8 windows.
	if len(windows) != 8 {
		t.Fatalf("got %d windows, want 8: %v", len(windows), windows)
	}
	if !windows[0].Equal(at(t, loc, "2026-09-02 03:15")) {
		t.Errorf("first window = %s, want 03:15 (the boundary is exclusive)", windows[0])
	}
	if !windows[7].Equal(to) {
		t.Errorf("last window = %s, want 05:00 (the end is inclusive)", windows[7])
	}
}

// TestBetweenReportsTruncation matters because D9's `all` policy would
// otherwise be a denial of service against yourself: a month asleep with
// every:15m is nearly 3000 runs.
func TestBetweenReportsTruncation(t *testing.T) {
	loc := time.UTC
	s := schedule.MustParse(schedule.Spec{Every: 15 * time.Minute, Location: loc})

	from := at(t, loc, "2026-08-01 00:00")
	to := at(t, loc, "2026-09-01 00:00")

	windows, truncated := s.Between(from, to, 10)
	if len(windows) != 10 {
		t.Errorf("got %d windows, want the limit of 10", len(windows))
	}
	if truncated == 0 {
		t.Error("truncation was not reported; the operator would never learn what was skipped")
	}
	if got := len(windows) + truncated; got != 31*96 {
		t.Errorf("total windows = %d, want %d", got, 31*96)
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name string
		spec schedule.Spec
	}{
		{"neither", schedule.Spec{}},
		{"both", schedule.Spec{Every: time.Minute, Cron: "* * * * *"}},
		{"too short", schedule.Spec{Every: time.Millisecond}},
		{"too long", schedule.Spec{Every: 48 * time.Hour}},
		{"four fields", schedule.Spec{Cron: "* * * *"}},
		{"bad minute", schedule.Spec{Cron: "99 * * * *"}},
		{"bad step", schedule.Spec{Cron: "*/0 * * * *"}},
		{"reversed range", schedule.Spec{Cron: "30-10 * * * *"}},
		{"nonsense", schedule.Spec{Cron: "abc * * * *"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := schedule.Parse(tc.spec); err == nil {
				t.Error("accepted an invalid schedule")
			}
		})
	}
}

func TestNamesAndAliases(t *testing.T) {
	loc := time.UTC
	s, err := schedule.ParseCron("0 0 * JAN SUN", loc)
	if err != nil {
		t.Fatalf("names rejected: %v", err)
	}
	got := s.Next(at(t, loc, "2026-12-01 00:00"))
	if got.Month() != time.January || got.Weekday() != time.Sunday {
		t.Errorf("Next = %s, want a Sunday in January", got)
	}

	// 7 is Sunday too, which is conventional.
	if _, err := schedule.ParseCron("0 0 * * 7", loc); err != nil {
		t.Errorf("day-of-week 7 rejected: %v", err)
	}
}
