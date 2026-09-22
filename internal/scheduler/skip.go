package scheduler

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// The two forms a skipped date may be written in.
//
// The annual form exists because the holidays a team actually configures repeat:
// writing 2026-01-01 means somebody has to remember to write 2027-01-01 next
// December, and the failure of forgetting is silent — the digest simply starts
// arriving on a holiday again.
const (
	// SkipDateLayout is one specific day: "2026-01-01".
	SkipDateLayout = "2006-01-02"
	// SkipAnnualLayout is the same day every year: "01-01".
	SkipAnnualLayout = "01-02"
)

// weekdayNames maps every spelling a configured weekday may take.
//
// Both the short and the long form, because a config file is written by hand and
// "sat" and "saturday" are the same intent — rejecting one of them would be a
// rule with no purpose. Nothing else is accepted: a numeric weekday would have
// to pick between Sunday-first and Monday-first, and getting that wrong moves the
// skipped day by one without anything looking wrong.
var weekdayNames = map[string]time.Weekday{
	"mon": time.Monday, "monday": time.Monday,
	"tue": time.Tuesday, "tuesday": time.Tuesday,
	"wed": time.Wednesday, "wednesday": time.Wednesday,
	"thu": time.Thursday, "thursday": time.Thursday,
	"fri": time.Friday, "friday": time.Friday,
	"sat": time.Saturday, "saturday": time.Saturday,
	"sun": time.Sunday, "sunday": time.Sunday,
}

// SkipDays is the set of days a schedule does not fire on: weekdays that repeat
// every week, and dates that are either one specific day or the same day every
// year.
//
// The zero value skips nothing, which is what makes the feature invisible to a
// deployment that does not configure it.
type SkipDays struct {
	weekdays map[time.Weekday]bool
	// dates holds both forms under their own layouts, so a lookup is two map
	// probes rather than a scan.
	dates map[string]bool
	// raw keeps what the operator wrote, in order, for Describe. A schedule that
	// cannot say what it skips is one nobody can debug: "why did nothing arrive
	// today" has no other answer.
	rawWeekdays []string
	rawDates    []string
}

// ParseSkipDays reads the configured skip lists.
//
// It is the only parser for them, for the same reason ParseClocks is the only
// one for slots: config validation, `doctor` and the running schedule must agree
// on what a value means, and the way they stop agreeing is a second parser that
// is a little more lenient.
func ParseSkipDays(weekdays, dates []string) (SkipDays, error) {
	s := SkipDays{}
	for _, raw := range weekdays {
		name := strings.ToLower(strings.TrimSpace(raw))
		if name == "" {
			continue
		}
		day, ok := weekdayNames[name]
		if !ok {
			return SkipDays{}, fmt.Errorf(
				"skip weekday %q is not a weekday (write mon, tue, wed, thu, fri, sat or sun)", raw)
		}
		if s.weekdays == nil {
			s.weekdays = make(map[time.Weekday]bool, len(weekdays))
		}
		// A repeat is accepted rather than rejected: unlike a duplicated slot,
		// which would name two indistinguishable digest runs, skipping a day twice
		// means exactly what skipping it once means.
		if !s.weekdays[day] {
			s.rawWeekdays = append(s.rawWeekdays, day.String())
		}
		s.weekdays[day] = true
	}

	for _, raw := range dates {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		key, err := parseSkipDate(value)
		if err != nil {
			return SkipDays{}, err
		}
		if s.dates == nil {
			s.dates = make(map[string]bool, len(dates))
		}
		if !s.dates[key] {
			s.rawDates = append(s.rawDates, key)
		}
		s.dates[key] = true
	}
	return s, nil
}

// parseSkipDate normalises one configured date to the key Contains looks up.
//
// The value is kept verbatim rather than reformatted, which is safe because both
// layouts are fixed-width: time.Parse rejects "2026-1-1" outright rather than
// accepting it into a key that could never match a zero-padded formatted date.
// That rejection is the whole point — a skip date that silently matches nothing
// is worse than a config line that fails to load, because the digest simply
// arrives on the holiday and nothing anywhere says why.
func parseSkipDate(value string) (string, error) {
	for _, layout := range []string{SkipDateLayout, SkipAnnualLayout} {
		if _, err := time.Parse(layout, value); err == nil {
			return value, nil
		}
	}
	return "", fmt.Errorf(
		"skip date %q must be YYYY-MM-DD for one day (2026-01-01) or MM-DD for every year (01-01)", value)
}

// Empty reports whether nothing is skipped.
func (s SkipDays) Empty() bool { return len(s.weekdays) == 0 && len(s.dates) == 0 }

// Contains reports whether t's calendar day is skipped. t must already be in the
// schedule's location: which day an instant falls on is exactly the question the
// zone answers, and taking a Location here would let a caller pass one that
// disagrees with the schedule's own.
func (s SkipDays) Contains(t time.Time) bool {
	if s.weekdays[t.Weekday()] {
		return true
	}
	if len(s.dates) == 0 {
		return false
	}
	return s.dates[t.Format(SkipDateLayout)] || s.dates[t.Format(SkipAnnualLayout)]
}

// Describe renders the skip set for `doctor` and for logs. Empty renders as ""
// so a caller can drop the clause entirely.
func (s SkipDays) Describe() string {
	var parts []string
	if len(s.rawWeekdays) > 0 {
		parts = append(parts, "weekdays "+strings.Join(s.rawWeekdays, ", "))
	}
	if len(s.rawDates) > 0 {
		dates := append([]string(nil), s.rawDates...)
		sort.Strings(dates)
		parts = append(parts, "dates "+strings.Join(dates, ", "))
	}
	return strings.Join(parts, "; ")
}

// SkipsEveryWeekday reports whether all seven weekdays are skipped, i.e. the
// schedule can never fire again.
//
// It is not an error — "this team wants no scheduled digest" is a coherent thing
// to configure, and there is no other switch for it — but it is the one skip
// configuration whose consequence is total, so `doctor` says it in words rather
// than leaving an operator to work it out from a list of seven names.
func (s SkipDays) SkipsEveryWeekday() bool { return len(s.weekdays) == 7 }
