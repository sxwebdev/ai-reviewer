package store_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sxwebdev/ai-reviewer/internal/dbtypes"
	"github.com/sxwebdev/ai-reviewer/internal/models"
	"github.com/sxwebdev/ai-reviewer/internal/store"
	"github.com/sxwebdev/ai-reviewer/internal/store/repos/repo_digestmessage"
	"github.com/sxwebdev/ai-reviewer/internal/store/repos/repo_digestrun"
	"github.com/sxwebdev/ai-reviewer/internal/store/repos/repo_finding"
	"github.com/sxwebdev/ai-reviewer/internal/store/repos/repo_review"
)

// These tests share one database and storetest.Store truncates it on every
// call, so nothing here may run with t.Parallel.

const (
	testProject = int64(42)
	testMR      = int64(7)
	testSHA     = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func reviewParams(status string) repo_review.CreateParams {
	return repo_review.CreateParams{
		ProjectID:    testProject,
		ProjectPath:  "team/group/repo",
		Team:         "core",
		MrIid:        testMR,
		HeadSha:      testSHA,
		Status:       status,
		PipelineJson: dbtypes.EmptyObject(),
		RiskJson:     dbtypes.EmptyObject(),
		Attempt:      1,
	}
}

func createReview(t *testing.T, st *store.Store, p repo_review.CreateParams) *models.MrReview {
	t.Helper()
	rev, err := st.Review().Create(t.Context(), p)
	if err != nil {
		t.Fatalf("create review (status=%q sha=%q): %v", p.Status, p.HeadSha, err)
	}
	return rev
}

func findingParams(reviewID uuid.UUID, fingerprint string) repo_finding.InsertParams {
	return repo_finding.InsertParams{
		ReviewID:     reviewID,
		ProjectID:    testProject,
		MrIid:        testMR,
		Fingerprint:  fingerprint,
		Severity:     "high",
		Category:     "correctness",
		FilePath:     "internal/service/review.go",
		Title:        "possible connection leak",
		Body:         "the rows are never closed",
		PositionJson: dbtypes.EmptyObject(),
		Pass:         "correctness",
		Verification: "confirmed",
	}
}

func insertFinding(t *testing.T, st *store.Store, p repo_finding.InsertParams) int64 {
	t.Helper()
	n, err := st.Finding().Insert(t.Context(), p)
	if err != nil {
		t.Fatalf("insert finding %q: %v", p.Fingerprint, err)
	}
	return n
}

func createDigestRun(t *testing.T, st *store.Store, slot string, attempt int32) *models.DigestRun {
	t.Helper()
	run, err := st.DigestRun().Create(t.Context(), repo_digestrun.CreateParams{
		Team:    "core",
		Slot:    slot,
		RunDate: time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC),
		Attempt: attempt,
		Status:  "built",
		Parts:   1,
		MrCount: 3,
	})
	if err != nil {
		t.Fatalf("create digest run: %v", err)
	}
	return run
}

func createDigestMessage(t *testing.T, st *store.Store, runID uuid.UUID, partNo int32, status string) *models.DigestMessage {
	t.Helper()
	msg, err := st.DigestMessage().Create(t.Context(), repo_digestmessage.CreateParams{
		DigestRunID: runID,
		PartNo:      partNo,
		PartsTotal:  1,
		Channel:     "#reviews",
		Payload:     dbtypes.JSON(`{"blocks":[]}`),
		Status:      status,
	})
	if err != nil {
		t.Fatalf("create digest message (status=%q): %v", status, err)
	}
	return msg
}
