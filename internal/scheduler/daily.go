// Package scheduler holds the service's wall-clock schedules. It is pure: no
// I/O, no config, no logging — the only consumer is River's periodic-job
// registration, which needs a river.PeriodicSchedule.
package scheduler

import (
	"fmt"
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

// DigestTZ is the zone the digest slots are expressed in. Not configurable:
// the team reads the digest at Moscow office hours (plan §6.6).
const DigestTZ = "Europe/Moscow"

// DigestTimes are the two daily digest slots. 09:00 and 16:30 are a product
// requirement, deliberately not exposed in config.
var DigestTimes = []Clock{{Hour: 9, Minute: 0}, {Hour: 16, Minute: 30}}

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

// Slot is a scheduled firing: the name written to digest_runs.slot and the
// instant it names. At is always in the schedule's location, so At's calendar
// date is the run_date that belongs with Name.
type Slot struct {
	Name string
	At   time.Time
}

// Daily fires every day at each of Times, interpreted in Loc. It implements
// river.PeriodicSchedule, so River drives it directly.
type Daily struct {
	Times []Clock
	Loc   *time.Location
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
	for day := range daySearchSpan {
		year, month, dayOfMonth := cur.AddDate(0, 0, day).Date()

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
// that index: a manual run at 18:33 filed under "18:33" collides with nothing
// and sends a second copy of the 16:30 digest. The periodic path has the same
// exposure, because River's enqueuer fires near the scheduled instant, not
// exactly on it — a job constructed at 09:00:31 must still say "09:00".
//
// ok is false only when the schedule has no Times and therefore cannot name a
// slot at all; the returned Slot is then zero. Callers must handle that
// explicitly instead of persisting a name this type never produced.
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

// NewDigest builds the digest schedule, loading Europe/Moscow. The error is
// surfaced (rather than panicking) so startup validation can report a broken
// tzdata build as a config failure — see plan §7.3.
func NewDigest() (Daily, error) {
	loc, err := time.LoadLocation(DigestTZ)
	if err != nil {
		return Daily{}, fmt.Errorf("load timezone %s: %w", DigestTZ, err)
	}
	return Daily{Times: DigestTimes, Loc: loc}, nil
}
