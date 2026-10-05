package scheduler

import (
	"strings"
	"testing"
	"time"
)

// mustSkipDays parses a skip set the tests below then exercise.
func mustSkipDays(t *testing.T, weekdays, dates []string) SkipDays {
	t.Helper()
	s, err := ParseSkipDays(weekdays, dates)
	if err != nil {
		t.Fatalf("ParseSkipDays(%v, %v): %v", weekdays, dates, err)
	}
	return s
}

func TestParseSkipDaysAcceptsBothSpellings(t *testing.T) {
	t.Parallel()

	s := mustSkipDays(t, []string{"sat", "Sunday", " MON ", "sat"}, nil)
	loc := time.UTC
	for _, tc := range []struct {
		day  time.Time
		want bool
	}{
		{time.Date(2026, 8, 22, 12, 0, 0, 0, loc), true},  // Saturday
		{time.Date(2026, 8, 23, 12, 0, 0, 0, loc), true},  // Sunday
		{time.Date(2026, 8, 24, 12, 0, 0, 0, loc), true},  // Monday
		{time.Date(2026, 8, 25, 12, 0, 0, 0, loc), false}, // Tuesday
	} {
		if got := s.Contains(tc.day); got != tc.want {
			t.Errorf("Contains(%s) = %t, want %t", tc.day.Weekday(), got, tc.want)
		}
	}
}

// TestParseSkipDaysDates covers the two forms and the difference between them:
// a full date is one day in history, an MM-DD is every year — which is the form
// a holiday list actually needs, because the other one silently expires.
func TestParseSkipDaysDates(t *testing.T) {
	t.Parallel()

	s := mustSkipDays(t, nil, []string{"2026-05-09", "01-01"})
	loc := time.UTC
	for _, tc := range []struct {
		name string
		day  time.Time
		want bool
	}{
		{"the one dated day", time.Date(2026, 5, 9, 9, 0, 0, 0, loc), true},
		{"the same date a year later", time.Date(2027, 5, 9, 9, 0, 0, 0, loc), false},
		{"the annual day", time.Date(2026, 1, 1, 9, 0, 0, 0, loc), true},
		{"the annual day next year", time.Date(2031, 1, 1, 9, 0, 0, 0, loc), true},
		{"an ordinary day", time.Date(2026, 5, 8, 9, 0, 0, 0, loc), false},
	} {
		if got := s.Contains(tc.day); got != tc.want {
			t.Errorf("%s: Contains(%s) = %t, want %t", tc.name, tc.day.Format(SkipDateLayout), got, tc.want)
		}
	}
}

// TestParseSkipDaysRejectsWhatWouldSilentlyNotSkip: every value here parses
// somewhere or reads as intent, and every one of them would leave the day
// un-skipped. A rejected config line is the only version of this failure anybody
// notices.
func TestParseSkipDaysRejectsMalformedValues(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		weekdays []string
		dates    []string
		want     string
	}{
		{name: "not a weekday", weekdays: []string{"funday"}, want: "is not a weekday"},
		{name: "numeric weekday", weekdays: []string{"6"}, want: "is not a weekday"},
		{name: "russian weekday", weekdays: []string{"суббота"}, want: "is not a weekday"},
		// Unpadded values are the ones that would parse leniently elsewhere and
		// then match no formatted date at all — a skip that quietly does nothing.
		{name: "unpadded date", dates: []string{"2026-1-1"}, want: "must be YYYY-MM-DD"},
		{name: "unpadded annual date", dates: []string{"1-1"}, want: "must be YYYY-MM-DD"},
		{name: "day out of range", dates: []string{"2026-02-30"}, want: "must be YYYY-MM-DD"},
		{name: "a month, not a day", dates: []string{"2026-01"}, want: "must be YYYY-MM-DD"},
		{name: "dotted", dates: []string{"01.01"}, want: "must be YYYY-MM-DD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseSkipDays(tc.weekdays, tc.dates)
			if err == nil {
				t.Fatalf("ParseSkipDays(%v, %v) = nil error", tc.weekdays, tc.dates)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestParseSkipDaysIgnoresBlanks(t *testing.T) {
	t.Parallel()

	s := mustSkipDays(t, []string{"", "  "}, []string{"", " "})
	if !s.Empty() {
		t.Errorf("blank entries produced a non-empty skip set: %q", s.Describe())
	}
}

// TestSkipDaysDescribe: the schedule has to be able to say what it skips, or
// "why did nothing arrive today" has no answer anywhere.
func TestSkipDaysDescribe(t *testing.T) {
	t.Parallel()

	if got := (SkipDays{}).Describe(); got != "" {
		t.Errorf("empty Describe() = %q, want empty so the caller can drop the clause", got)
	}
	got := mustSkipDays(t, []string{"sat", "sun"}, []string{"01-01", "2026-05-09"}).Describe()
	for _, want := range []string{"Saturday", "Sunday", "01-01", "2026-05-09"} {
		if !strings.Contains(got, want) {
			t.Errorf("Describe() = %q, want it to name %q", got, want)
		}
	}
}

func TestSkipsEveryWeekday(t *testing.T) {
	t.Parallel()

	all := []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}
	if !mustSkipDays(t, all, nil).SkipsEveryWeekday() {
		t.Error("all seven weekdays must be recognised as a schedule that never fires")
	}
	if mustSkipDays(t, all[:6], nil).SkipsEveryWeekday() {
		t.Error("six weekdays still leave a day to fire on")
	}
	// Dates alone can never make the claim: a date list is finite and the
	// schedule fires on every day it does not name.
	if mustSkipDays(t, nil, []string{"01-01"}).SkipsEveryWeekday() {
		t.Error("a date list must not be read as skipping every weekday")
	}
}

// TestNextStepsOverSkippedDays is the point of the whole feature: nothing is
// built on a skipped day, because River is never told to fire there.
func TestNextStepsOverSkippedDays(t *testing.T) {
	t.Parallel()

	loc := mustLoad(t, testTZ)
	d := Daily{
		Times: testTimes, Loc: loc,
		Skip: mustSkipDays(t, []string{"sat", "sun"}, nil),
	}

	// Friday, after the last slot → Monday morning, not Saturday.
	friday := time.Date(2026, 8, 21, 18, 0, 0, 0, loc)
	want := time.Date(2026, 8, 24, 9, 0, 0, 0, loc)
	if got := d.Next(friday); !got.Equal(want) {
		t.Errorf("Next(Friday 18:00) = %s, want %s", got, want)
	}

	// Called *on* a skipped day — which happens on the first tick after a
	// deploy — the next fire is still Monday.
	saturday := time.Date(2026, 8, 22, 10, 0, 0, 0, loc)
	if got := d.Next(saturday); !got.Equal(want) {
		t.Errorf("Next(Saturday 10:00) = %s, want %s", got, want)
	}

	// Within an ordinary day nothing changes.
	thursday := time.Date(2026, 8, 20, 10, 0, 0, 0, loc)
	wantThu := time.Date(2026, 8, 20, 14, 0, 0, 0, loc)
	if got := d.Next(thursday); !got.Equal(wantThu) {
		t.Errorf("Next(Thursday 10:00) = %s, want %s", got, wantThu)
	}
}

// TestNextStepsOverALongHolidayRun: the Russian new-year break is eight days,
// which is longer than the ordinary three-day search window — the window has to
// grow with the skip list or the schedule would report "never" and stay silent
// for good.
func TestNextStepsOverALongHolidayRun(t *testing.T) {
	t.Parallel()

	loc := mustLoad(t, testTZ)
	holidays := make([]string, 0, 8)
	for day := 1; day <= 8; day++ {
		holidays = append(holidays, time.Date(2027, 1, day, 0, 0, 0, 0, loc).Format(SkipDateLayout))
	}
	d := Daily{Times: testTimes, Loc: loc, Skip: mustSkipDays(t, nil, holidays)}

	from := time.Date(2026, 12, 31, 18, 0, 0, 0, loc)
	want := time.Date(2027, 1, 9, 9, 0, 0, 0, loc)
	if got := d.Next(from); !got.Equal(want) {
		t.Errorf("Next(New Year's Eve) = %s, want the first working day %s", got, want)
	}
}

// TestNextOnAScheduleThatSkipsEverything must terminate and say "never" rather
// than spin: a schedule with all seven weekdays skipped is a coherent way to say
// "this team wants no digest", and River reads neverTime as "do not fire".
func TestNextOnAScheduleThatSkipsEverything(t *testing.T) {
	t.Parallel()

	loc := mustLoad(t, testTZ)
	d := Daily{
		Times: testTimes, Loc: loc,
		Skip: mustSkipDays(t, []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}, nil),
	}
	if got := d.Next(time.Date(2026, 8, 20, 10, 0, 0, 0, loc)); !got.Equal(neverTime) {
		t.Errorf("Next = %s, want neverTime", got)
	}
}

// TestSkippedIsAnsweredInTheSchedulesOwnZone: "Saturday" means Saturday where
// the team is. An instant that is Friday evening in Moscow is already Saturday
// in Auckland, and reading the wrong one silences the wrong day.
func TestSkippedIsAnsweredInTheSchedulesOwnZone(t *testing.T) {
	t.Parallel()

	moscow := mustLoad(t, testTZ)
	d := Daily{Times: testTimes, Loc: moscow, Skip: mustSkipDays(t, []string{"sat"}, nil)}

	// 2026-08-21 23:00 in Auckland is 2026-08-21 14:00 in Moscow — a Friday
	// there, whatever the caller's clock says.
	auckland := mustLoad(t, "Pacific/Auckland")
	instant := time.Date(2026, 8, 22, 5, 0, 0, 0, auckland) // Saturday in Auckland
	if instant.In(moscow).Weekday() != time.Friday {
		t.Fatalf("fixture is wrong: %s in Moscow is %s", instant, instant.In(moscow).Weekday())
	}
	if d.Skipped(instant) {
		t.Error("a Saturday elsewhere must not skip the team's Friday")
	}
}

// TestNewDigestParsesSkipDays: the constructor is the only place config becomes
// a schedule, so a malformed skip list must fail there rather than be dropped.
func TestNewDigestParsesSkipDays(t *testing.T) {
	t.Parallel()

	d, err := NewDigest(DigestSpec{
		Slots: []string{"09:00"}, Timezone: testTZ,
		SkipWeekdays: []string{"sat"}, SkipDates: []string{"01-01"},
	})
	if err != nil {
		t.Fatalf("NewDigest: %v", err)
	}
	if !d.Skipped(time.Date(2026, 8, 22, 12, 0, 0, 0, d.Location())) {
		t.Error("the configured Saturday is not skipped")
	}
	if !d.Skipped(time.Date(2031, 1, 1, 12, 0, 0, 0, d.Location())) {
		t.Error("the configured annual holiday is not skipped")
	}

	if _, err := NewDigest(DigestSpec{
		Slots: []string{"09:00"}, Timezone: testTZ, SkipWeekdays: []string{"funday"},
	}); err == nil {
		t.Error("a malformed weekday must fail the constructor, not be ignored")
	}
}

// TestZeroSkipChangesNothing: a deployment that configures none of this must
// behave exactly as it did before the feature existed.
func TestZeroSkipChangesNothing(t *testing.T) {
	t.Parallel()

	loc := mustLoad(t, testTZ)
	plain := Daily{Times: testTimes, Loc: loc}
	for day := range 8 {
		at := time.Date(2026, 8, 17, 18, 0, 0, 0, loc).AddDate(0, 0, day)
		if plain.Skipped(at) {
			t.Errorf("%s is skipped by an unconfigured schedule", at.Weekday())
		}
		want := time.Date(2026, 8, 18, 9, 0, 0, 0, loc).AddDate(0, 0, day)
		if got := plain.Next(at); !got.Equal(want) {
			t.Errorf("Next(%s) = %s, want %s", at, got, want)
		}
	}
}
