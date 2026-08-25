package scheduler

import (
	"slices"
	"testing"
	"time"

	"github.com/riverqueue/river"
)

// testTimes is this package's own fixture, deliberately not the product
// schedule: Daily's job is snapping and ordering, and a test that read the
// configured default would start failing whenever a product decision changed
// without anything in this package being wrong. The default itself is pinned in
// internal/config, next to the tag that declares it.
const testTZ = "Europe/Moscow"

var testTimes = []Clock{{Hour: 9, Minute: 0}, {Hour: 14, Minute: 0}, {Hour: 17, Minute: 30}}

// setLocal points time.Local at name for the duration of the test. Every
// assertion below constructs its times explicitly, so the schedule must be
// unaffected — that is exactly what this proves, on a host in any zone.
// Deliberately not parallel: time.Local is process-global.
func setLocal(t *testing.T, name string) {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	prev := time.Local
	time.Local = loc
	t.Cleanup(func() { time.Local = prev })
}

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return loc
}

// TestDailyNext runs the full slot table twice, with the host clock in two very
// different zones (Asia/Tokyo is +9, ahead of Moscow, so a naive local-time
// implementation lands on the wrong day).
func TestDailyNext(t *testing.T) {
	for _, host := range []string{"UTC", "Asia/Tokyo"} {
		t.Run("host="+host, func(t *testing.T) {
			setLocal(t, host)

			msk := mustLoad(t, testTZ)
			d := Daily{Times: testTimes, Loc: msk}

			at := func(y int, m time.Month, day, h, min, sec int) time.Time {
				return time.Date(y, m, day, h, min, sec, 0, msk)
			}

			cases := []struct {
				name    string
				current time.Time
				want    time.Time
			}{
				{"midnight", at(2026, 8, 13, 0, 0, 0), at(2026, 8, 13, 9, 0, 0)},
				{"before first slot", at(2026, 8, 13, 8, 0, 0), at(2026, 8, 13, 9, 0, 0)},
				{"one second before first slot", at(2026, 8, 13, 8, 59, 59), at(2026, 8, 13, 9, 0, 0)},
				// Strictly-after is the River contract: called with the instant a run
				// fired, Next must return the following run, not the same one.
				{"exactly at first slot", at(2026, 8, 13, 9, 0, 0), at(2026, 8, 13, 14, 0, 0)},
				{"one second after first slot", at(2026, 8, 13, 9, 0, 1), at(2026, 8, 13, 14, 0, 0)},
				{"between first and middle", at(2026, 8, 13, 12, 0, 0), at(2026, 8, 13, 14, 0, 0)},
				{"exactly at middle slot", at(2026, 8, 13, 14, 0, 0), at(2026, 8, 13, 17, 30, 0)},
				{"between middle and last", at(2026, 8, 13, 16, 0, 0), at(2026, 8, 13, 17, 30, 0)},
				{"one second before last slot", at(2026, 8, 13, 17, 29, 59), at(2026, 8, 13, 17, 30, 0)},
				{"exactly at last slot", at(2026, 8, 13, 17, 30, 0), at(2026, 8, 14, 9, 0, 0)},
				{"after last slot", at(2026, 8, 13, 18, 0, 0), at(2026, 8, 14, 9, 0, 0)},
				{"across midnight", at(2026, 8, 13, 23, 59, 59), at(2026, 8, 14, 9, 0, 0)},
				{"across month boundary", at(2026, 8, 31, 20, 0, 0), at(2026, 9, 1, 9, 0, 0)},
				{"across year boundary", at(2026, 12, 31, 20, 0, 0), at(2027, 1, 1, 9, 0, 0)},
				// 05:30Z is 08:30 MSK: the caller's zone must not matter.
				{"current in UTC", time.Date(2026, 8, 13, 5, 30, 0, 0, time.UTC), at(2026, 8, 13, 9, 0, 0)},
				// Zones are named explicitly rather than taken from time.Local: the
				// row used to build its instant from the host, so the instant under
				// test differed between the two subtests and they agreed only because
				// both answers happened to be 09:00 the next day. The middle slot
				// ended that coincidence, which is the bug this shape prevents.
				//
				// 23:00 UTC on the 13th is already 02:00 MSK on the 14th, so the next
				// firing is that morning.
				{"instant past Moscow midnight", time.Date(2026, 8, 13, 23, 0, 0, 0, time.UTC), at(2026, 8, 14, 9, 0, 0)},
				// 23:00 Tokyo on the 13th is 17:00 MSK — the host date has rolled over
				// while Moscow still has a slot to come, so a naive local-time
				// implementation answers the 14th here and is wrong.
				{"instant in a third zone", time.Date(2026, 8, 13, 23, 0, 0, 0, mustLoad(t, "Asia/Tokyo")), at(2026, 8, 13, 17, 30, 0)},
			}

			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					got := d.Next(tc.current)
					if !got.Equal(tc.want) {
						t.Fatalf("Next(%s) = %s, want %s",
							tc.current.In(msk).Format(time.RFC3339),
							got.In(msk).Format(time.RFC3339),
							tc.want.Format(time.RFC3339))
					}
					if !got.After(tc.current) {
						t.Errorf("Next must be strictly after current: got %s, current %s", got, tc.current)
					}
					// The instant Next returned is itself a slot, so SlotAt must
					// name it rather than snapping to an earlier one.
					slot, ok := d.SlotAt(got)
					if !ok {
						t.Fatalf("SlotAt could not name the instant Next returned: %s", got)
					}
					if !slot.At.Equal(got) {
						t.Errorf("SlotAt(%s).At = %s, want the same instant", got, slot.At)
					}
					if !slices.ContainsFunc(testTimes, func(c Clock) bool { return c.String() == slot.Name }) {
						t.Errorf("slot = %q, want one of the schedule under test %v", slot.Name, testTimes)
					}
				})
			}
		})
	}
}

// TestDailyNextSequence walks several days by feeding each result back in, the
// way River does, and pins the exact sequence across a day boundary.
func TestDailyNextSequence(t *testing.T) {
	setLocal(t, "Asia/Tokyo")
	msk := mustLoad(t, testTZ)
	d := Daily{Times: testTimes, Loc: msk}

	want := []time.Time{
		time.Date(2026, 8, 13, 9, 0, 0, 0, msk),
		time.Date(2026, 8, 13, 14, 0, 0, 0, msk),
		time.Date(2026, 8, 13, 17, 30, 0, 0, msk),
		time.Date(2026, 8, 14, 9, 0, 0, 0, msk),
		time.Date(2026, 8, 14, 14, 0, 0, 0, msk),
		time.Date(2026, 8, 14, 17, 30, 0, 0, msk),
		time.Date(2026, 8, 15, 9, 0, 0, 0, msk),
	}

	cur := time.Date(2026, 8, 13, 3, 0, 0, 0, msk)
	for i, w := range want {
		cur = d.Next(cur)
		if !cur.Equal(w) {
			t.Fatalf("step %d = %s, want %s", i, cur.In(msk).Format(time.RFC3339), w.Format(time.RFC3339))
		}
	}
}

// TestDailyNextAcrossDST uses a DST-observing zone (Moscow has none) to prove
// the schedule keeps firing exactly once per day through both transitions:
// no skipped day in spring, no duplicate day in autumn.
func TestDailyNextAcrossDST(t *testing.T) {
	setLocal(t, "UTC")
	berlin := mustLoad(t, "Europe/Berlin")
	// 02:30 sits inside the spring-forward gap and is ambiguous on fall-back day.
	d := Daily{Times: []Clock{{Hour: 2, Minute: 30}}, Loc: berlin}

	for _, tc := range []struct {
		name  string
		start time.Time
		days  []int
	}{
		// 2026-03-29: 02:00 CET → 03:00 CEST, 02:30 does not exist.
		{"spring forward", time.Date(2026, 3, 27, 0, 0, 0, 0, berlin), []int{27, 28, 29, 30, 31}},
		// 2026-10-25: 03:00 CEST → 02:00 CET, 02:30 happens twice.
		{"fall back", time.Date(2026, 10, 23, 0, 0, 0, 0, berlin), []int{23, 24, 25, 26, 27}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cur := tc.start
			for _, wantDay := range tc.days {
				next := d.Next(cur)
				if !next.After(cur) {
					t.Fatalf("Next did not advance: %s → %s", cur, next)
				}
				if got := next.In(berlin).Day(); got != wantDay {
					t.Fatalf("fired on day %d, want %d (at %s)", got, wantDay, next.In(berlin).Format(time.RFC3339))
				}
				cur = next
			}
		})
	}
}

func TestDailyNextIsRiverPeriodicSchedule(t *testing.T) {
	msk := mustLoad(t, testTZ)
	var sched river.PeriodicSchedule = Daily{Times: testTimes, Loc: msk}

	got := sched.Next(time.Date(2026, 8, 13, 10, 0, 0, 0, msk))
	if want := time.Date(2026, 8, 13, 14, 0, 0, 0, msk); !got.Equal(want) {
		t.Errorf("through river.PeriodicSchedule: got %s, want %s", got, want)
	}
}

func TestDailyNextDegenerate(t *testing.T) {
	setLocal(t, "Asia/Tokyo")

	t.Run("no times never fires", func(t *testing.T) {
		// Must be the far-future sentinel, not the zero time: a past time would
		// make River's enqueuer spin.
		got := Daily{Loc: time.UTC}.Next(time.Now())
		if !got.Equal(neverTime) {
			t.Errorf("got %s, want the never sentinel %s", got, neverTime)
		}
	})

	t.Run("nil location falls back to UTC not host local", func(t *testing.T) {
		d := Daily{Times: []Clock{{Hour: 9}}}
		got := d.Next(time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC))
		want := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
		if !got.Equal(want) {
			t.Errorf("got %s, want %s", got, want)
		}
	})
}

func TestNewDigest(t *testing.T) {
	setLocal(t, "Asia/Tokyo")

	d, err := NewDigest(DigestSpec{Slots: []string{"09:00", "14:00", "17:30"}, Timezone: testTZ})
	if err != nil {
		t.Fatalf("NewDigest: %v", err)
	}
	if d.Loc == nil || d.Loc.String() != testTZ {
		t.Fatalf("location = %v, want %s", d.Loc, testTZ)
	}

	// The configured strings become the schedule in order, and each firing names
	// itself rather than the slot before it.
	want := []string{"09:00", "14:00", "17:30"}
	at := time.Date(2026, 8, 13, 0, 0, 0, 0, d.Loc)
	for i, w := range want {
		at = d.Next(at)
		if got := at.In(d.Loc).Format(SlotLayout); got != w {
			t.Errorf("slot %d = %s, want %s", i+1, got, w)
		}
		if slot, ok := d.SlotAt(at); !ok || slot.Name != w {
			t.Errorf("SlotAt(slot %d) = %q (ok=%v), want %s", i+1, slot.Name, ok, w)
		}
	}
	// And the day wraps back to the first slot rather than producing a fourth.
	if got := d.Next(at).In(d.Loc).Format("2006-01-02 15:04"); got != "2026-08-14 09:00" {
		t.Errorf("after the last slot = %s, want 2026-08-14 09:00", got)
	}

	// Order in config must not matter: Next and SlotAt scan Times rather than
	// assuming it is sorted, so an operator listing the slots out of order gets
	// the same schedule instead of a silently broken one.
	shuffled, err := NewDigest(DigestSpec{Slots: []string{"17:30", "09:00", "14:00"}, Timezone: testTZ})
	if err != nil {
		t.Fatalf("NewDigest(shuffled): %v", err)
	}
	if got := shuffled.Next(time.Date(2026, 8, 13, 0, 0, 0, 0, d.Loc)); got.Format(SlotLayout) != "09:00" {
		t.Errorf("shuffled first firing = %s, want 09:00", got.Format(SlotLayout))
	}

	// A schedule this service cannot name must not be constructible.
	if _, err := NewDigest(DigestSpec{Timezone: testTZ}); err == nil {
		t.Error("NewDigest(DigestSpec{Timezone: testTZ}) succeeded; a Daily with no slots cannot name a digest")
	}
	if _, err := NewDigest(DigestSpec{Slots: []string{"9:00"}, Timezone: testTZ}); err == nil {
		t.Error("NewDigest accepted an unpadded slot, which never round-trips through Clock.String")
	}
}

func TestParseClock(t *testing.T) {
	t.Parallel()
	t.Run("accepted", func(t *testing.T) {
		t.Parallel()
		for in, want := range map[string]Clock{
			"00:00":    {Hour: 0, Minute: 0},
			"09:00":    {Hour: 9, Minute: 0},
			"17:30":    {Hour: 17, Minute: 30},
			"23:59":    {Hour: 23, Minute: 59},
			"  14:00 ": {Hour: 14, Minute: 0},
		} {
			got, err := ParseClock(in)
			if err != nil || got != want {
				t.Errorf("ParseClock(%q) = (%v, %v), want %v", in, got, err, want)
			}
		}
	})

	t.Run("rejected", func(t *testing.T) {
		t.Parallel()
		// "9:00" is the one that matters: time.Parse accepts it, and accepting it
		// here would put a name in digest_runs.slot that Clock.String never writes,
		// so the same slot would appear under two spellings.
		for _, in := range []string{"", "9:00", "09:0", "24:00", "09:60", "0900", "09:00:00", "9am", "09-00"} {
			if got, err := ParseClock(in); err == nil {
				t.Errorf("ParseClock(%q) = %v, want an error", in, got)
			}
		}
	})
}

func TestParseClocksRejectsEmptyAndDuplicates(t *testing.T) {
	t.Parallel()
	if _, err := ParseClocks(nil); err == nil {
		t.Error("an empty schedule was accepted")
	}
	if _, err := ParseClocks([]string{"09:00", "14:00", "09:00"}); err == nil {
		t.Error("a duplicated slot was accepted; the second can only be absorbed by the unique index")
	}
	got, err := ParseClocks([]string{"09:00", "17:30"})
	if err != nil || len(got) != 2 || got[0].String() != "09:00" || got[1].String() != "17:30" {
		t.Errorf("ParseClocks = (%v, %v)", got, err)
	}
}

// TestDailySlotAt pins the snapping. Every expectation here is a name from
// the schedule's own Times and a calendar date — a formatting-only implementation reproduces
// only the "exactly on a slot" rows and fails the rest.
func TestDailySlotAt(t *testing.T) {
	for _, host := range []string{"UTC", "Asia/Tokyo"} {
		t.Run("host="+host, func(t *testing.T) {
			setLocal(t, host)

			msk := mustLoad(t, testTZ)
			d := Daily{Times: testTimes, Loc: msk}

			at := func(y int, m time.Month, day, h, min, sec int) time.Time {
				return time.Date(y, m, day, h, min, sec, 0, msk)
			}

			cases := []struct {
				name        string
				current     time.Time
				wantSlot    string
				wantRunDate string
			}{
				// Exactly on a slot: the instant belongs to its own slot, never to
				// the previous one.
				{"exactly at 09:00", at(2026, 8, 13, 9, 0, 0), "09:00", "2026-08-13"},
				{"exactly at 14:00", at(2026, 8, 13, 14, 0, 0), "14:00", "2026-08-13"},
				{"exactly at 17:30", at(2026, 8, 13, 17, 30, 0), "17:30", "2026-08-13"},
				// River fires near the instant, not on it.
				{"31 seconds after 09:00", at(2026, 8, 13, 9, 0, 31), "09:00", "2026-08-13"},
				{"31 seconds after 14:00", at(2026, 8, 13, 14, 0, 31), "14:00", "2026-08-13"},
				// Between the slots. The middle slot is what makes these rows worth
				// having: an afternoon instant now belongs to 14:00, not to 09:00.
				{"between 09:00 and 14:00", at(2026, 8, 13, 11, 0, 0), "09:00", "2026-08-13"},
				{"one second before 14:00", at(2026, 8, 13, 13, 59, 59), "09:00", "2026-08-13"},
				{"between 14:00 and 17:30", at(2026, 8, 13, 16, 0, 0), "14:00", "2026-08-13"},
				{"one second before 17:30", at(2026, 8, 13, 17, 29, 59), "14:00", "2026-08-13"},
				// After the last slot — the case that motivated this method.
				{"manual run at 18:33", at(2026, 8, 13, 18, 33, 0), "17:30", "2026-08-13"},
				{"just before midnight", at(2026, 8, 13, 23, 59, 59), "17:30", "2026-08-13"},
				// Before the first slot of the day: yesterday's last slot, and
				// crucially yesterday's date.
				{"midnight", at(2026, 8, 13, 0, 0, 0), "17:30", "2026-08-12"},
				{"early morning", at(2026, 8, 13, 3, 0, 0), "17:30", "2026-08-12"},
				{"one second before 09:00", at(2026, 8, 13, 8, 59, 59), "17:30", "2026-08-12"},
				// Across month and year boundaries, backwards.
				{"first instant of a month", at(2026, 9, 1, 0, 30, 0), "17:30", "2026-08-31"},
				{"first instant of a year", at(2027, 1, 1, 2, 0, 0), "17:30", "2026-12-31"},
				// The caller's zone must not matter: 22:00 UTC is already 01:00 MSK
				// on the 14th, so the run belongs to the 13th's 17:30.
				{"current in UTC, past Moscow midnight", time.Date(2026, 8, 13, 22, 0, 0, 0, time.UTC), "17:30", "2026-08-13"},
				// 12:00 Tokyo on the 13th is 06:00 MSK — before the first slot.
				// Constructed in Tokyo explicitly, never in time.Local: a case whose
				// expectation depends on the host is exactly what this test is for.
				{"current in a third zone", time.Date(2026, 8, 13, 12, 0, 0, 0, mustLoad(t, "Asia/Tokyo")), "17:30", "2026-08-12"},
			}

			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					slot, ok := d.SlotAt(tc.current)
					if !ok {
						t.Fatalf("SlotAt(%s) could not name a slot", tc.current)
					}
					if slot.Name != tc.wantSlot {
						t.Errorf("slot = %q, want %q", slot.Name, tc.wantSlot)
					}
					if got := slot.At.Format("2006-01-02"); got != tc.wantRunDate {
						t.Errorf("run date = %s, want %s", got, tc.wantRunDate)
					}
					// The named slot must be a real firing, at or before t.
					if slot.At.After(tc.current) {
						t.Errorf("slot %s is after the instant %s", slot.At, tc.current)
					}
					if slot.At.Location() != msk {
						t.Errorf("slot instant is in %s, want the schedule's zone", slot.At.Location())
					}
					if got := slot.At.Format(SlotLayout); got != slot.Name {
						t.Errorf("Name %q does not describe At %s", slot.Name, got)
					}
				})
			}
		})
	}
}

// TestDailySlotAtRoundTripsNext ties the two methods together: stepping the
// schedule forward and asking which slot each firing belongs to must return
// that same firing, on every step across a day boundary.
func TestDailySlotAtRoundTripsNext(t *testing.T) {
	setLocal(t, "Asia/Tokyo")
	msk := mustLoad(t, testTZ)
	d := Daily{Times: testTimes, Loc: msk}

	cur := time.Date(2026, 8, 13, 3, 0, 0, 0, msk)
	for range 6 {
		cur = d.Next(cur)
		slot, ok := d.SlotAt(cur)
		if !ok {
			t.Fatalf("SlotAt could not name %s", cur)
		}
		if !slot.At.Equal(cur) {
			t.Fatalf("SlotAt(%s).At = %s, want the firing itself", cur, slot.At)
		}
	}
}

func TestDailySlotAtDegenerate(t *testing.T) {
	setLocal(t, "Asia/Tokyo")

	t.Run("no times cannot name a slot", func(t *testing.T) {
		// The caller must see this, not a plausible-looking string: a name that
		// is not in Times defeats the digest_runs unique index.
		slot, ok := Daily{Loc: time.UTC}.SlotAt(time.Now())
		if ok {
			t.Errorf("a schedule with no slots named %q", slot.Name)
		}
		if slot != (Slot{}) {
			t.Errorf("slot = %+v, want the zero value", slot)
		}
	})

	t.Run("nil location resolves in UTC not host local", func(t *testing.T) {
		d := Daily{Times: testTimes}
		// 12:00 UTC is past 09:00 UTC; in Tokyo-local terms it would be the 13th
		// at 21:00, and in Moscow terms 15:00 — all three would still say 09:00,
		// so use an instant where the zones disagree: 00:30 UTC on the 13th is
		// 09:30 in Tokyo, so a host-local implementation would answer 09:00/13th.
		got, ok := d.SlotAt(time.Date(2026, 8, 13, 0, 30, 0, 0, time.UTC))
		if !ok {
			t.Fatal("SlotAt could not name a slot")
		}
		if got.Name != "17:30" || got.At.Format("2006-01-02") != "2026-08-12" {
			t.Errorf("= %s %s, want 2026-08-12 17:30 in UTC", got.At.Format("2006-01-02"), got.Name)
		}
	})
}

// TestDailySlotAtAcrossDST checks the backwards scan in a DST-observing zone:
// every instant of the transition days must still resolve to a real firing.
func TestDailySlotAtAcrossDST(t *testing.T) {
	setLocal(t, "UTC")
	berlin := mustLoad(t, "Europe/Berlin")
	d := Daily{Times: []Clock{{Hour: 2, Minute: 30}}, Loc: berlin}

	for _, start := range []time.Time{
		time.Date(2026, 3, 29, 0, 0, 0, 0, berlin),  // spring forward
		time.Date(2026, 10, 25, 0, 0, 0, 0, berlin), // fall back
	} {
		for hour := range 24 {
			at := start.Add(time.Duration(hour) * time.Hour)
			slot, ok := d.SlotAt(at)
			if !ok {
				t.Fatalf("SlotAt(%s) could not name a slot", at)
			}
			if slot.At.After(at) {
				t.Errorf("SlotAt(%s) = %s, which is in the future", at, slot.At)
			}
			if diff := at.Sub(slot.At); diff > 25*time.Hour {
				t.Errorf("SlotAt(%s) = %s, %s earlier — a firing was skipped", at, slot.At, diff)
			}
		}
	}
}

func TestClockString(t *testing.T) {
	for _, tc := range []struct {
		c    Clock
		want string
	}{
		{Clock{Hour: 9, Minute: 0}, "09:00"},
		{Clock{Hour: 16, Minute: 30}, "16:30"},
		{Clock{Hour: 0, Minute: 5}, "00:05"},
	} {
		if got := tc.c.String(); got != tc.want {
			t.Errorf("Clock%v.String() = %q, want %q", tc.c, got, tc.want)
		}
	}
}
