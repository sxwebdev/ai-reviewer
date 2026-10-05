// Package scheduler holds the service's wall-clock schedules. It is pure: no
// I/O, no config, no logging — the only consumer is River's periodic-job
// registration, which needs a river.PeriodicSchedule.
package scheduler

import (
	"errors"
	"fmt"
	"strings"
	"time"

	// The digest fires at fixed Europe/Moscow wall-clock times, and the
	// container's local zone is deliberately never used. Scratch/distroless
	// images ship no tzdata, so time.LoadLocation("Europe/Moscow") would fail
	// there; embedding the IANA database (~450 KB) makes the schedule depend on
	// the binary rather than on the image. The import lives in this package, next
	// to the code that loads the zone, so it cannot be dropped by an unrelated
	// edit to main.
	_ "time/tzdata"

	"github.com/riverqueue/river"
)

// SlotLayout is how a slot name is encoded, in digest_runs.slot and everywhere
// else. Exported so a caller that has to describe a non-slot instant (a
// schedule with no times — see SlotAt) uses the same encoding.
const SlotLayout = "15:04"

// Clock is a wall-clock time of day, interpreted in a Daily's location.
type Clock struct {
	Hour   int
	Minute int
}

// String renders the slot as "HH:MM" — the same form stored in
// digest_runs.slot, so the two cannot drift.
func (c Clock) String() string { return fmt.Sprintf("%02d:%02d", c.Hour, c.Minute) }

// ParseClock reads one configured slot in the same "HH:MM" form String writes,
// which is what keeps the config vocabulary and digest_runs.slot identical.
//
// It exists here rather than in the config package so exactly one parser answers
// what a slot string means. time.Parse is not enough on its own: it accepts a
// single-digit hour ("9:00") and would silently give the schedule a name that
// never round-trips through String, so the round trip is asserted instead of
// assumed.
func ParseClock(s string) (Clock, error) {
	t, err := time.Parse(SlotLayout, strings.TrimSpace(s))
	if err != nil {
		return Clock{}, fmt.Errorf("slot %q must be HH:MM in 24-hour form", s)
	}
	c := Clock{Hour: t.Hour(), Minute: t.Minute()}
	if c.String() != strings.TrimSpace(s) {
		return Clock{}, fmt.Errorf("slot %q must be zero-padded HH:MM (write %q)", s, c)
	}
	return c, nil
}

// ParseClocks reads a whole configured schedule, rejecting an empty list and
// duplicates.
//
// Empty is refused because a Daily with no Times cannot name a slot at all: it
// would leave every digest unnamed rather than merely unscheduled, and SlotAt
// reports that as a failure the caller has to handle. Duplicates are refused
// because two identical names are indistinguishable in digest_runs and the
// second one can only ever be absorbed by the unique index — an operator writing
// one meant something else.
func ParseClocks(values []string) ([]Clock, error) {
	if len(values) == 0 {
		return nil, errors.New("at least one digest slot is required")
	}
	out := make([]Clock, 0, len(values))
	seen := make(map[Clock]bool, len(values))
	for _, v := range values {
		c, err := ParseClock(v)
		if err != nil {
			return nil, err
		}
		if seen[c] {
			return nil, fmt.Errorf("slot %s is listed twice", c)
		}
		seen[c] = true
		out = append(out, c)
	}
	return out, nil
}

// Slot is a scheduled firing: the name written to digest_runs.slot and the
// instant it names. At is always in the schedule's location, so At's calendar
// date is the run_date that belongs with Name.
type Slot struct {
	Name string
	At   time.Time
}

// Daily fires every day at each of Times, interpreted in Loc, except on the days
// Skip names. It implements river.PeriodicSchedule, so River drives it directly.
type Daily struct {
	Times []Clock
	Loc   *time.Location
	// Skip removes whole days from the schedule — weekends, holidays. It is
	// enforced in Next, i.e. the digest is never *built* on a skipped day rather
	// than built and withheld: a digest nobody will read costs dozens of GitLab
	// requests and a Slack directory load.
	Skip SkipDays
}

// Compile-time proof that Daily satisfies River's periodic-schedule contract
// (verified against river@v0.40.0/periodic_job.go: `Next(current) time.Time`).
var _ river.PeriodicSchedule = Daily{}

// neverTime is the maximum representable time.Time, mirroring River's own
// NeverSchedule sentinel. A degenerate schedule must return *this*, not the
// zero time: a zero time is in the past, and the periodic-job enqueuer would
// re-fire on it in a tight loop.
var neverTime = time.Unix(1<<63-62135596801, 999999999)

// daySearchSpan bounds the scan in either direction. With at least one slot a
// hit is guaranteed on day 0 or 1; the extra day is slack for locations whose
// DST transition shifts a slot's wall time.
const daySearchSpan = 3

// skipSearchSpan bounds the forward scan when days are skipped. A little over a
// year, because the annual skip form means a run of skipped days can in
// principle stretch that far, and because the loop must terminate on a schedule
// that skips everything — which is a configuration this package deliberately
// allows (see SkipDays.SkipsEveryWeekday) and answers with neverTime.
const skipSearchSpan = 370

// Location resolves the schedule's zone. A nil Loc would mean "container local
// time", which is exactly the dependency this type exists to remove, so it
// degrades to UTC rather than to the host.
func (d Daily) Location() *time.Location {
	if d.Loc == nil {
		return time.UTC
	}
	return d.Loc
}

// Next returns the first slot strictly after current, which is what River's
// periodic enqueuer expects: called with the instant a run fired, it must
// return the *following* run, never the same one again.
func (d Daily) Next(current time.Time) time.Time {
	if len(d.Times) == 0 {
		return neverTime
	}
	loc := d.Location()
	cur := current.In(loc)
	span := daySearchSpan
	if !d.Skip.Empty() {
		span = skipSearchSpan
	}
	for day := range span {
		candidate := cur.AddDate(0, 0, day)
		// Whole days are removed here rather than at delivery, which is what makes
		// a skipped day cost nothing: River never inserts the job, so nothing is
		// built and nothing is sent.
		if d.Skip.Contains(candidate) {
			continue
		}
		year, month, dayOfMonth := candidate.Date()

		var best time.Time
		for _, c := range d.Times {
			// time.Date normalises a wall time a DST spring-forward skipped, so a
			// slot inside the gap fires at the following real instant instead of
			// vanishing, and an ambiguous fall-back slot resolves to its first
			// occurrence. Moscow has no DST; this keeps the type honest anywhere.
			t := time.Date(year, month, dayOfMonth, c.Hour, c.Minute, 0, 0, loc)
			// Compare instants, not wall clocks: current may arrive in any zone.
			if !t.After(current) {
				continue
			}
			if best.IsZero() || t.Before(best) {
				best = t
			}
		}
		if !best.IsZero() {
			return best
		}
	}
	return neverTime
}

// SlotAt returns the scheduled slot an instant belongs to: the most recent slot
// at or before t, resolved in the schedule's own location. An instant earlier
// than the first slot of its day belongs to the previous day's last slot, so the
// returned Slot.At always carries the matching run_date.
//
// Snapping — rather than formatting t — is the whole point. digest_runs is keyed
// by (team, run_date, slot, attempt), so a name that is not one of Times defeats
// that index: a manual run after the last slot, filed under its own clock time,
// collides with nothing and sends a second copy of that slot's digest. The periodic path has the
// same exposure, because River's enqueuer fires near the scheduled instant, not
// exactly on it — a job constructed half a minute late must still name its own
// slot.
//
// ok is false only when the schedule has no Times and therefore cannot name a
// slot at all; the returned Slot is then zero. Callers must handle that
// explicitly instead of persisting a name this type never produced.
//
// Skipped days are deliberately not consulted: this answers "which slot does
// this instant belong to", which a manual `ai-reviewer digest` on a Saturday
// still needs a correct answer to. Whether a digest fires is Next's question,
// and Skipped's.
func (d Daily) SlotAt(t time.Time) (Slot, bool) {
	if len(d.Times) == 0 {
		return Slot{}, false
	}
	loc := d.Location()
	local := t.In(loc)

	for day := range daySearchSpan {
		year, month, dayOfMonth := local.AddDate(0, 0, -day).Date()

		var best time.Time
		for _, c := range d.Times {
			// Same DST normalisation as Next: a slot a spring-forward skipped
			// resolves to the following real instant, and if that pushes it past t
			// it simply is not the slot t belongs to.
			at := time.Date(year, month, dayOfMonth, c.Hour, c.Minute, 0, 0, loc)
			// "At or before": an instant exactly on a slot belongs to that slot,
			// not to the previous one.
			if at.After(t) {
				continue
			}
			if best.IsZero() || at.After(best) {
				best = at
			}
		}
		if !best.IsZero() {
			return Slot{Name: best.Format(SlotLayout), At: best}, true
		}
	}
	// Unreachable with at least one slot: yesterday's slots are always before
	// today's instants. Reported honestly rather than guessed at.
	return Slot{}, false
}

// Skipped reports whether the day t falls on is one the schedule does not fire
// on, resolved in the schedule's own location.
//
// Exported because two callers outside this package need the same answer for
// their own reasons: the RunOnStart path, which must not fire a slot that Next
// would have stepped over, and the CLI, which tells an operator that the day
// they are asking about is a skipped one.
func (d Daily) Skipped(t time.Time) bool { return d.Skip.Contains(t.In(d.Location())) }

// DigestSpec is one team's schedule as the operator wrote it: the strings, not
// the parsed values.
//
// A struct rather than four positional arguments because two of them are
// []string and swapping those silently produces a schedule that parses, runs and
// skips the wrong days.
type DigestSpec struct {
	Slots    []string
	Timezone string
	// SkipWeekdays are weekday names: mon…sun, long or short.
	SkipWeekdays []string
	// SkipDates are YYYY-MM-DD (one day) or MM-DD (every year).
	SkipDates []string
}

// NewDigest builds one digest schedule from the configured slots, zone and skip
// days. Every error is surfaced rather than panicking so startup validation can
// report a malformed value as a config failure.
//
// The values arrive as the strings the operator wrote, not as Clocks and
// weekdays, so this is the only place that turns config into a schedule and no
// caller can assemble a Daily out of values the parsers would have rejected.
//
// An empty timezone is rejected rather than defaulted: Daily.Location() degrades
// a nil zone to UTC so the type stays safe, but silently scheduling a team's
// digest in UTC because a config key was blank would move every slot by hours
// without anything saying so. The default belongs to the config schema.
func NewDigest(spec DigestSpec) (Daily, error) {
	times, err := ParseClocks(spec.Slots)
	if err != nil {
		return Daily{}, err
	}
	loc, err := LoadLocation(spec.Timezone)
	if err != nil {
		return Daily{}, err
	}
	skip, err := ParseSkipDays(spec.SkipWeekdays, spec.SkipDates)
	if err != nil {
		return Daily{}, err
	}
	return Daily{Times: times, Loc: loc, Skip: skip}, nil
}

// LoadLocation resolves a configured IANA zone name. Exported so config
// validation and doctor answer "is this zone loadable" exactly the way the
// schedule will, including on a stripped image with no tzdata — which this
// package embeds, so it should never fail, but the failure it guards is
// invisible until the first missed digest.
func LoadLocation(name string) (*time.Location, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("timezone is required")
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("timezone %q cannot be loaded (missing tzdata in the image?): %w", name, err)
	}
	return loc, nil
}
