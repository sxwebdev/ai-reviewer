package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sxwebdev/ai-reviewer/internal/dbtypes"
	"github.com/sxwebdev/ai-reviewer/internal/store"
	"github.com/sxwebdev/ai-reviewer/internal/store/repos/repo_digestmessage"
	"github.com/sxwebdev/ai-reviewer/internal/store/repos/repo_digestrun"
	"github.com/sxwebdev/ai-reviewer/internal/store/storetest"
)

// The slack_send claim is the only thing standing between "the worker died
// after chat.postMessage" and a silently lost digest (plan section 6.4).
func TestDigestMessageClaimForSend(t *testing.T) {
	st, _ := storetest.Store(t)
	run := createDigestRun(t, st, "09:00", 0)

	tests := []struct {
		name        string
		status      string
		wantBefore  string
		wantClaimed bool
		wantAfter   string
	}{
		{
			name:        "pending is the ordinary first delivery",
			status:      "pending",
			wantBefore:  "pending",
			wantClaimed: true,
			wantAfter:   "sending",
		},
		{
			// A crash between the POST and the result write. Resending is the
			// deliberate choice: a duplicate is noise, a lost digest is a team
			// that never learns it has work waiting.
			name:        "sending is a crash recovery and is resent",
			status:      "sending",
			wantBefore:  "sending",
			wantClaimed: true,
			wantAfter:   "sending",
		},
		{
			name:        "sent is a no-op",
			status:      "sent",
			wantBefore:  "sent",
			wantClaimed: false,
			wantAfter:   "sent",
		},
		{
			name:        "failed is a no-op",
			status:      "failed",
			wantBefore:  "failed",
			wantClaimed: false,
			wantAfter:   "failed",
		},
		{
			name:        "dry_run is a no-op",
			status:      "dry_run",
			wantBefore:  "dry_run",
			wantClaimed: false,
			wantAfter:   "dry_run",
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := createDigestMessage(t, st, run.ID, int32(i), tt.status)

			got, err := st.DigestMessage().ClaimForSend(t.Context(), msg.ID)
			if err != nil {
				t.Fatalf("ClaimForSend: %v", err)
			}
			if got.StatusBefore != tt.wantBefore {
				t.Errorf("got status_before %q, want %q", got.StatusBefore, tt.wantBefore)
			}
			if got.Claimed != tt.wantClaimed {
				t.Errorf("got claimed %v, want %v", got.Claimed, tt.wantClaimed)
			}

			after, err := st.DigestMessage().GetByID(t.Context(), msg.ID)
			if err != nil {
				t.Fatalf("GetByID: %v", err)
			}
			if after.Status != tt.wantAfter {
				t.Errorf("got stored status %q, want %q", after.Status, tt.wantAfter)
			}
		})
	}
}

// Keeping this set closed is what makes the five-way branch of section 6.4
// total: a status outside ClaimForSend's ('pending','sending') makes the job
// read "not claimed" as "already delivered" and the digest is dropped in
// silence — nothing errors, nobody notices.
//
// That used to be a CHECK constraint. It now lives in the application
// (dbtypes.MessageStatus + the store wrappers), so this test asserts the two
// halves that remain true of the database: every declared value is storable,
// and the column itself no longer judges — which is precisely why
// TestWrappersRejectInvalidStatus and TestNoDirectStatusWrites exist.
func TestDigestMessageAcceptsEveryDeclaredStatus(t *testing.T) {
	st, _ := storetest.Store(t)
	run := createDigestRun(t, st, "09:00", 0)

	for i, status := range dbtypes.MessageStatuses() {
		if _, err := st.CreateDigestMessage(t.Context(), repo_digestmessage.CreateParams{
			DigestRunID: run.ID,
			PartNo:      int32(i),
			PartsTotal:  int32(len(dbtypes.MessageStatuses())),
			Channel:     "#reviews",
			Payload:     dbtypes.EmptyObject(),
			Status:      string(status),
		}); err != nil {
			t.Errorf("declared status %q was rejected: %v", status, err)
		}
	}

	// The counterpart is deliberate, not an oversight: raw SQL can still write
	// anything, because the guard is in Go. Pinning it here means a future
	// re-added CHECK shows up as a failing test rather than as a surprise.
	if _, err := st.Pool().Exec(t.Context(),
		`UPDATE digest_messages SET status = 'delivered' WHERE digest_run_id = $1`, run.ID,
	); err != nil {
		t.Fatalf("raw UPDATE to an unknown status failed: %v — a CHECK constraint is back; fold it into dbtypes instead", err)
	}
}

// A second claim of a pending message reports 'sending', not 'pending': the CTE
// really does read the pre-UPDATE row, which is the entire reason it exists.
func TestDigestMessageClaimForSendReportsPreviousStatus(t *testing.T) {
	st, _ := storetest.Store(t)
	run := createDigestRun(t, st, "09:00", 0)
	msg := createDigestMessage(t, st, run.ID, 0, "pending")

	first, err := st.DigestMessage().ClaimForSend(t.Context(), msg.ID)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if first.StatusBefore != "pending" || !first.Claimed {
		t.Fatalf("first claim: got (%q, %v), want (pending, true)", first.StatusBefore, first.Claimed)
	}

	second, err := st.DigestMessage().ClaimForSend(t.Context(), msg.ID)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if second.StatusBefore != "sending" || !second.Claimed {
		t.Errorf("second claim: got (%q, %v), want (sending, true)", second.StatusBefore, second.Claimed)
	}
}

func TestDigestMessageMarkSentAndFailed(t *testing.T) {
	st, _ := storetest.Store(t)
	run := createDigestRun(t, st, "09:00", 0)

	sent := createDigestMessage(t, st, run.ID, 0, "sending")
	if err := st.DigestMessage().MarkSent(t.Context(), "1723540364.001", sent.ID); err != nil {
		t.Fatalf("MarkSent: %v", err)
	}
	got, err := st.DigestMessage().GetByID(t.Context(), sent.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Status != "sent" || got.SlackTs != "1723540364.001" || !got.SentAt.Valid {
		t.Errorf("after MarkSent: status=%q ts=%q sent_at valid=%v", got.Status, got.SlackTs, got.SentAt.Valid)
	}
	// A delivered message is a hard stop for the retry: claiming it again is a
	// no-op even though the job may run once more.
	claim, err := st.DigestMessage().ClaimForSend(t.Context(), sent.ID)
	if err != nil {
		t.Fatalf("ClaimForSend after MarkSent: %v", err)
	}
	if claim.Claimed {
		t.Error("a sent message was claimed again")
	}

	failed := createDigestMessage(t, st, run.ID, 1, "sending")
	if err := st.DigestMessage().MarkFailed(t.Context(), "ratelimited", failed.ID); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	got, err = st.DigestMessage().GetByID(t.Context(), failed.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Status != "failed" || got.Error != "ratelimited" {
		t.Errorf("after MarkFailed: status=%q error=%q", got.Status, got.Error)
	}

	// One part failing must not undo the part that was delivered.
	parts, err := st.DigestMessage().ListByRun(t.Context(), run.ID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	if len(parts) != 2 {
		t.Fatalf("got %d parts, want 2", len(parts))
	}
	if parts[0].Status != "sent" || parts[1].Status != "failed" {
		t.Errorf("got part statuses %q/%q, want sent/failed", parts[0].Status, parts[1].Status)
	}
}

// The unique index is the second layer of digest idempotency: N replicas cannot
// double a scheduled slot, while an explicit repeat takes the next attempt.
func TestDigestRunSlotUniqueness(t *testing.T) {
	st, _ := storetest.Store(t)

	createDigestRun(t, st, "09:00", 0)

	_, err := st.DigestRun().Create(t.Context(), repo_digestrun.CreateParams{
		Team:    "core",
		Slot:    "09:00",
		RunDate: time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC),
		Attempt: 0,
		Status:  "built",
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("duplicate scheduled slot: got err %v, want unique_violation", err)
	}

	next, err := st.DigestRun().NextAttempt(t.Context(), repo_digestrun.NextAttemptParams{
		Team:    "core",
		RunDate: time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC),
		Slot:    "09:00",
	})
	if err != nil {
		t.Fatalf("NextAttempt: %v", err)
	}
	if next != 1 {
		t.Fatalf("got next attempt %d, want 1", next)
	}
	forced := createDigestRun(t, st, "09:00", next)

	latest, err := st.DigestRun().GetBySlot(t.Context(), repo_digestrun.GetBySlotParams{
		Team:    "core",
		RunDate: time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC),
		Slot:    "09:00",
	})
	if err != nil {
		t.Fatalf("GetBySlot: %v", err)
	}
	if latest.ID != forced.ID {
		t.Errorf("got run %s (attempt %d), want the forced one %s", latest.ID, latest.Attempt, forced.ID)
	}

	// An untouched slot starts at attempt 0.
	fresh, err := st.DigestRun().NextAttempt(t.Context(), repo_digestrun.NextAttemptParams{
		Team:    "core",
		RunDate: time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC),
		Slot:    "17:30",
	})
	if err != nil {
		t.Fatalf("NextAttempt for an unused slot: %v", err)
	}
	if fresh != 0 {
		t.Errorf("got next attempt %d for an unused slot, want 0", fresh)
	}
}

// `digest --force` before a slot's scheduled firing leaves attempt 1 as the
// latest run, so the scheduled job must ask about attempt 0 specifically —
// otherwise it concludes the slot already ran and then collides with
// digest_runs_slot_uniq when it inserts anyway.
func TestDigestRunGetBySlotAttempt(t *testing.T) {
	st, _ := storetest.Store(t)
	runDate := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)

	forced := createDigestRun(t, st, "09:00", 1)

	latest, err := st.DigestRun().GetBySlot(t.Context(), repo_digestrun.GetBySlotParams{
		Team: "core", RunDate: runDate, Slot: "09:00",
	})
	if err != nil {
		t.Fatalf("GetBySlot: %v", err)
	}
	if latest.ID != forced.ID {
		t.Errorf("GetBySlot returned %s, want the forced run %s", latest.ID, forced.ID)
	}

	// The scheduled run has not happened yet, and this is how the job finds out.
	scheduled := repo_digestrun.GetBySlotAttemptParams{
		Team: "core", RunDate: runDate, Slot: "09:00", Attempt: 0,
	}
	if _, err := st.DigestRun().GetBySlotAttempt(t.Context(), scheduled); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("attempt 0 before the slot fires: got err %v, want pgx.ErrNoRows", err)
	}

	planned := createDigestRun(t, st, "09:00", 0)
	got, err := st.DigestRun().GetBySlotAttempt(t.Context(), scheduled)
	if err != nil {
		t.Fatalf("GetBySlotAttempt: %v", err)
	}
	if got.ID != planned.ID {
		t.Errorf("got run %s (attempt %d), want the scheduled one %s", got.ID, got.Attempt, planned.ID)
	}

	// A different slot on the same day is a different run.
	other := scheduled
	other.Slot = "17:30"
	if _, err := st.DigestRun().GetBySlotAttempt(t.Context(), other); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("another slot: got err %v, want pgx.ErrNoRows", err)
	}
}

// The digest job writes its run, its parts and the slack_send jobs in one
// commit, so both digest repos have to be bindable to the caller's transaction.
func TestDigestRunAndMessagesCommitTogether(t *testing.T) {
	st, _ := storetest.Store(t)

	var runID uuid.UUID
	err := st.RunInTx(t.Context(), func(tx pgx.Tx) error {
		run, err := st.DigestRun(store.WithTx(tx)).Create(t.Context(), repo_digestrun.CreateParams{
			Team:    "core",
			Slot:    "17:30",
			RunDate: time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC),
			Status:  "built",
			Parts:   2,
		})
		if err != nil {
			return err
		}
		runID = run.ID
		for part := range int32(2) {
			if _, err := st.DigestMessage(store.WithTx(tx)).Create(t.Context(), repo_digestmessage.CreateParams{
				DigestRunID: run.ID,
				PartNo:      part,
				PartsTotal:  2,
				Channel:     "#reviews",
				Payload:     dbtypes.JSON(`{"blocks":[]}`),
				Status:      "pending",
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RunInTx: %v", err)
	}

	parts, err := st.DigestMessage().ListByRun(t.Context(), runID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	if len(parts) != 2 {
		t.Fatalf("got %d parts after commit, want 2", len(parts))
	}

	// Dropping the run takes its messages with it, so a half-built digest
	// cannot leave orphaned parts for slack_send to pick up.
	if _, err := st.Pool().Exec(t.Context(), `DELETE FROM digest_runs WHERE id = $1`, runID); err != nil {
		t.Fatalf("delete run: %v", err)
	}
	var left int64
	if err := st.Pool().QueryRow(t.Context(), `SELECT count(*) FROM digest_messages`).Scan(&left); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if left != 0 {
		t.Errorf("got %d orphaned digest messages, want 0", left)
	}
}

func TestDigestRunSetStatus(t *testing.T) {
	st, _ := storetest.Store(t)
	run := createDigestRun(t, st, "09:00", 0)

	if err := st.DigestRun().SetStatus(t.Context(), repo_digestrun.SetStatusParams{
		Status:  "partial",
		Parts:   2,
		MrCount: 11,
		Error:   "failed to inspect 1 repository",
		ID:      run.ID,
	}); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	got, err := st.DigestRun().GetByID(t.Context(), run.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Status != "partial" || got.Parts != 2 || got.MrCount != 11 {
		t.Errorf("got status=%q parts=%d mr_count=%d, want partial/2/11", got.Status, got.Parts, got.MrCount)
	}
}
