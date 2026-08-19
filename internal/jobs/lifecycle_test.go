package jobs_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sxwebdev/ai-reviewer/internal/domain"
	"github.com/sxwebdev/ai-reviewer/internal/jobs"
	"github.com/sxwebdev/ai-reviewer/internal/scheduler"
)

// TestNewDigestArgsNamesTheSlot is what makes the digest_runs unique index able
// to distinguish a repeat from a fresh slot: the CLI and the scheduler must
// derive the same (slot, run_date) from the same instant.
func TestNewDigestArgsNamesTheSlot(t *testing.T) {
	t.Parallel()
	sched, err := scheduler.NewDigest([]string{"09:00", "14:00", "17:30"}, "Europe/Moscow")
	if err != nil {
		t.Fatal(err)
	}

	msk := sched.Loc
	at := func(h, m int) time.Time { return time.Date(2026, 8, 13, h, m, 0, 0, msk) }

	cases := []struct {
		name        string
		at          time.Time
		wantSlot    string
		wantRunDate string
	}{
		// The scheduled firings themselves.
		{"exactly 09:00", at(9, 0), "09:00", "2026-08-13"},
		{"exactly 14:00", at(14, 0), "14:00", "2026-08-13"},
		{"exactly 17:30", at(17, 30), "17:30", "2026-08-13"},
		// River's enqueuer fires near the instant, not on it. A job constructed
		// half a minute late must still name its own slot, or it would file
		// itself under a slot that does not exist and the digest_runs unique
		// index would stop protecting the real one.
		{"a few seconds late", at(9, 0).Add(31 * time.Second), "09:00", "2026-08-13"},
		// A manual run after the last slot belongs to that slot.
		{"manual run at 18:33", at(18, 33), "17:30", "2026-08-13"},
		{"manual run at 11:00", at(11, 0), "09:00", "2026-08-13"},
		// Between the middle and last slots — the window the middle slot added.
		{"manual run at 16:00", at(16, 0), "14:00", "2026-08-13"},
		// Before the first slot of the day the run belongs to yesterday's last.
		{"manual run at 03:00", at(3, 0), "17:30", "2026-08-12"},
		{"one second before 09:00", at(9, 0).Add(-time.Second), "17:30", "2026-08-12"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := jobs.NewDigestArgs("payments", sched, tc.at, 0)
			if got.Slot != tc.wantSlot || got.RunDate != tc.wantRunDate {
				t.Errorf("= %s %s, want %s %s", got.RunDate, got.Slot, tc.wantRunDate, tc.wantSlot)
			}
		})
	}

	// The zone is the schedule's, not the caller's: 22:00 UTC is already the
	// 14th in Moscow, and its most recent slot is the 13th's 17:30.
	night := time.Date(2026, 8, 13, 22, 0, 0, 0, time.UTC) // 01:00 MSK on the 14th
	if got := jobs.NewDigestArgs("payments", sched, night, 0); got.RunDate != "2026-08-13" || got.Slot != "17:30" {
		t.Errorf("night = %s %s, want 2026-08-13 17:30", got.RunDate, got.Slot)
	}

	// --force takes the next attempt and changes nothing else.
	base := jobs.NewDigestArgs("payments", sched, at(9, 0), 0)
	forced := jobs.NewDigestArgs("payments", sched, at(9, 0), 1)
	if forced.Attempt != 1 || forced.Slot != base.Slot || forced.RunDate != base.RunDate {
		t.Errorf("forced = %+v, want the same slot with attempt 1", forced)
	}

	// A schedule with no location must resolve in UTC, never in container-local
	// time — the dependency the whole type exists to remove.
	utcNoon := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	if got := jobs.NewDigestArgs("payments", scheduler.Daily{Times: []scheduler.Clock{{Hour: 9}, {Hour: 14}, {Hour: 17, Minute: 30}}}, utcNoon, 0); got.RunDate != "2026-08-13" || got.Slot != "09:00" {
		t.Errorf("= %s %s, want 2026-08-13 09:00 in UTC", got.RunDate, got.Slot)
	}

	// A schedule with no slots at all cannot name one; it must degrade to the
	// instant rather than panic or return a zero date.
	if got := jobs.NewDigestArgs("payments", scheduler.Daily{}, utcNoon, 0); got.RunDate != "2026-08-13" {
		t.Errorf("an empty schedule = %+v", got)
	}
}

func TestServiceAccessors(t *testing.T) {
	f := newFixture(t)
	teams := f.teams
	svc := f.newService(t, jobs.Config{Teams: teams, PublishEnabled: true}, newDeps(nil, nil, nil))

	if svc.Name() != "river-jobs" {
		t.Errorf("Name() = %q", svc.Name())
	}
	if svc.Interval() <= 0 {
		t.Errorf("Interval() = %s, want a positive poll period", svc.Interval())
	}
	if !svc.PublishEnabled() {
		t.Error("PublishEnabled() lost the configured value")
	}
	if got := svc.Teams(); len(got) != len(teams) || got[0].Name != teams[0].Name {
		t.Errorf("Teams() = %+v", got)
	}
	// One schedule per team, each in that team's own zone — the fixture's two
	// teams are deliberately in different ones. A single shared schedule would
	// pass a test that only ever asked about the first team.
	for _, want := range []struct{ team, tz, first string }{
		{teams[0].Name, "Europe/Moscow", "09:00"},
		{teams[1].Name, "Europe/Lisbon", "10:00"},
	} {
		sched, ok := svc.ScheduleFor(want.team)
		if !ok {
			t.Errorf("ScheduleFor(%q) found nothing", want.team)
			continue
		}
		if sched.Loc == nil || sched.Loc.String() != want.tz {
			t.Errorf("ScheduleFor(%q).Loc = %v, want %s", want.team, sched.Loc, want.tz)
		}
		if got := sched.Times[0].String(); got != want.first {
			t.Errorf("ScheduleFor(%q) first slot = %s, want %s", want.team, got, want.first)
		}
	}
	// Team names are matched the way config validates them: unique
	// case-insensitively, so the lookup must be too or a manual `digest --team`
	// typed in another case would name a slot nobody scheduled.
	if _, ok := svc.ScheduleFor(strings.ToUpper(teams[0].Name)); !ok {
		t.Error("ScheduleFor is case-sensitive; --team is not")
	}
	if _, ok := svc.ScheduleFor("no-such-team"); ok {
		t.Error("ScheduleFor invented a schedule for an unconfigured team")
	}
}

// TestHealthyChecksTheLocalDependenciesOnly is §14.4: /readyz asks the pool and
// the River client, and nothing else. A probe that called GitLab would turn
// their outage into ours.
func TestHealthyChecksTheLocalDependenciesOnly(t *testing.T) {
	f := newFixture(t)
	sc := &fakeScanner{}
	svc := f.newService(t, jobs.Config{}, newDeps(nil, sc, nil))

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- svc.Start(ctx) }()

	// Start is asynchronous; the client reports healthy once it is up.
	var err error
	for range 100 {
		if err = svc.Healthy(t.Context()); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("Healthy on a running service: %v", err)
	}
	if len(sc.inspects) != 0 {
		t.Errorf("the readiness probe walked repositories: %v", sc.inspects)
	}

	cancel()
	<-done
	stopCtx, stopCancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
	defer stopCancel()
	if err := svc.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if err := svc.Healthy(t.Context()); err == nil {
		t.Error("a stopped River client must report unhealthy")
	}
}

// TestWaitReportsATimeout: --wait must give up with a clear error rather than
// blocking forever on a job no worker is running.
func TestWaitReportsATimeout(t *testing.T) {
	f := newFixture(t)
	q := f.newQueue(t)
	res, err := q.EnqueueReview(t.Context(), f.reviewArgs())
	if err != nil {
		t.Fatal(err)
	}

	row, err := q.Wait(t.Context(), res.ID(), 10*time.Millisecond, 50*time.Millisecond)
	if !errors.Is(err, jobs.ErrWaitTimeout) {
		t.Fatalf("err = %v, want ErrWaitTimeout", err)
	}
	// The row is still returned so the caller can report where the job got to.
	if row == nil || row.ID != res.ID() {
		t.Errorf("row = %+v, want the job being waited on", row)
	}

	if _, err := q.JobGet(t.Context(), 999999); err == nil {
		t.Error("a missing job must be an error")
	}
}

// TestReviewWorkerSkipReason: a merge request that turned out not to need a
// review is a completed job, not a failed one — otherwise every up-to-date MR
// would show up as an error.
func TestReviewWorkerSkipReason(t *testing.T) {
	f := newFixture(t)
	rev := &fakeReviewer{outcome: &jobs.ReviewOutcome{Status: "skipped", SkipReason: domain.ReasonUpToDate}}
	svc := f.newService(t, jobs.Config{}, newDeps(rev, nil, nil))

	if err := jobs.WorkReview(t.Context(), svc, f.reviewArgs()); err != nil {
		t.Fatalf("a skipped review must not fail the job: %v", err)
	}
}

// TestReviewWorkerNilOutcome guards the "nothing to do, no error" contract: the
// worker must not dereference a nil outcome.
func TestReviewWorkerNilOutcome(t *testing.T) {
	f := newFixture(t)
	rev := &fakeReviewer{nilOutcome: true}
	svc := f.newService(t, jobs.Config{}, newDeps(rev, nil, nil))

	if err := jobs.WorkReview(t.Context(), svc, f.reviewArgs()); err != nil {
		t.Fatalf("a nil outcome must not fail the job: %v", err)
	}
}

func TestReviewWorkerFailurePropagates(t *testing.T) {
	f := newFixture(t)
	want := errors.New("llm refused")
	rev := &fakeReviewer{runErr: want}
	svc := f.newService(t, jobs.Config{}, newDeps(rev, nil, nil))

	err := jobs.WorkReview(t.Context(), svc, f.reviewArgs())
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want the review error", err)
	}
	// The error text must identify the merge request; a bare "llm refused" in a
	// shared log is unactionable.
	for _, want := range []string{f.repo(0), "!7", f.token} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}
