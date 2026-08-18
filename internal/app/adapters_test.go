package app

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sxwebdev/ai-reviewer/internal/domain"
	"github.com/sxwebdev/ai-reviewer/internal/jobs"
	"github.com/sxwebdev/ai-reviewer/internal/service"
)

// TestToServicePersistPreservesNil is the dry-run contract at the seam: a nil
// hand-off must survive the conversion, because the service reads a non-nil one
// as permission to enqueue publication.
func TestToServicePersistPreservesNil(t *testing.T) {
	t.Parallel()
	if got := toServicePersist(nil); got != nil {
		t.Fatal("a nil hand-off became non-nil; every dry run would enqueue publication")
	}

	want := errors.New("insert failed")
	id := uuid.New()
	var gotID uuid.UUID
	converted := toServicePersist(func(_ context.Context, _ pgx.Tx, reviewID uuid.UUID) error {
		gotID = reviewID
		return want
	})
	if converted == nil {
		t.Fatal("a non-nil hand-off was dropped")
	}
	// The conversion must forward, not swallow: an insert that fails has to
	// roll the review's own transaction back.
	if err := converted(t.Context(), nil, id); !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
	if gotID != id {
		t.Errorf("review id = %s, want %s", gotID, id)
	}
}

func TestToServiceRequestCarriesEveryField(t *testing.T) {
	t.Parallel()
	req := jobs.ReviewRequest{
		Team: "payments", ProjectPath: "backend/payments",
		ProjectID: 42, MRIID: 7, HeadSHA: "deadbeef", Publish: true,
	}
	got := toServiceRequest(req)
	want := service.ReviewRequest{
		Team: "payments", ProjectPath: "backend/payments",
		ProjectID: 42, MRIID: 7, HeadSHA: "deadbeef", Publish: true,
	}
	if got != want {
		t.Errorf("request = %+v, want %+v", got, want)
	}
}

func TestFromServiceOutcome(t *testing.T) {
	t.Parallel()
	if got := fromServiceOutcome(nil); got != nil {
		t.Error("a nil outcome must stay nil, not become a zero-valued success")
	}

	id := uuid.New()
	got := fromServiceOutcome(&service.ReviewOutcome{
		ReviewID: id, Status: "reviewed", Findings: 3, RiskLevel: "high",
		CostUSD: 0.42, DurationMS: 1234, SkipReason: domain.ReasonUpToDate,
	})
	if got.ReviewID != id || got.Status != "reviewed" || got.Findings != 3 ||
		got.RiskLevel != "high" || got.CostUSD != 0.42 || got.DurationMS != 1234 ||
		got.SkipReason != domain.ReasonUpToDate {
		t.Errorf("outcome = %+v", got)
	}
}

// TestFromServiceScanDropsThePublishDecision: the scanner enqueues reviews with
// Publish unset so the decision is taken when the job runs. Carrying a value
// across here would pin it at queue time and make a later `--publish` insert
// indistinguishable from the scanner's — the two would dedupe into one job that
// silently does the wrong thing.
func TestFromServiceScanDropsThePublishDecision(t *testing.T) {
	t.Parallel()
	if got := fromServiceScan(nil); got != nil {
		t.Error("a nil scan result must stay nil")
	}

	ids := []uuid.UUID{uuid.New()}
	res := fromServiceScan(&service.ScanResult{
		Candidates: []service.ReviewRequest{
			{Team: "payments", ProjectPath: "backend/payments", ProjectID: 1, MRIID: 5, HeadSHA: "aaa", Publish: true},
		},
		StalePublish: ids,
		Snapshots:    []domain.MergeRequestSnapshot{{}},
		Failed:       true,
	})
	if len(res.Candidates) != 1 {
		t.Fatalf("candidates = %d, want 1", len(res.Candidates))
	}
	c := res.Candidates[0]
	if c.Publish {
		t.Error("the scanner's publish decision leaked into the job args")
	}
	if c.Team != "payments" || c.ProjectID != 1 || c.MRIID != 5 || c.HeadSHA != "aaa" {
		t.Errorf("candidate = %+v", c)
	}
	if len(res.StalePublish) != 1 || res.StalePublish[0] != ids[0] {
		t.Errorf("stale publications = %v", res.StalePublish)
	}
	if !res.Failed {
		t.Error("the partial-result flag was lost; the digest would claim complete data")
	}
	if len(res.Snapshots) != 1 {
		t.Errorf("snapshots = %d, want 1", len(res.Snapshots))
	}
}

func TestFromServiceDigest(t *testing.T) {
	t.Parallel()
	if got := fromServiceDigest(nil); got != nil {
		t.Error("a nil digest outcome must stay nil")
	}
	runID, msg := uuid.New(), uuid.New()
	got := fromServiceDigest(&service.DigestOutcome{
		RunID: runID, Messages: []uuid.UUID{msg}, Status: "built", MRCount: 9, LinearIssueCount: 4,
	})
	if got.RunID != runID || got.Status != "built" || got.MRCount != 9 || got.LinearIssueCount != 4 ||
		len(got.Messages) != 1 || got.Messages[0] != msg {
		t.Errorf("outcome = %+v", got)
	}
}
