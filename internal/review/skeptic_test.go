package review

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/sxwebdev/ai-reviewer/internal/llm"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func vf(sev, title string, conf float64) ValidatedFinding {
	return ValidatedFinding{
		Title: title, Body: "body", Severity: sev, Category: "correctness",
		FilePath: "main.go", Confidence: conf,
		Source: llm.Finding{Line: 2, LineKind: "new"},
	}
}

func TestApplyVerdicts(t *testing.T) {
	batch := []ValidatedFinding{
		vf("high", "refuted one", 0.9),
		vf("high", "uncertain one", 0.9),
		vf("high", "confirmed one", 0.6),
		vf("high", "duplicate one", 0.9),
		vf("high", "unassessed one", 0.9),
		vf("blocking", "blocking refuted", 0.9),
	}
	verdicts := []llm.FindingVerdict{
		{Index: 1, Verdict: "refuted", Reason: "code checks this"},
		{Index: 2, Verdict: "uncertain", Reason: "cannot tell"},
		{Index: 3, Verdict: "confirmed", Confidence: 0.95},
		{Index: 4, Verdict: "confirmed", DuplicateOf: 3},
		// index 5: no verdict
		{Index: 6, Verdict: "refuted", Reason: "looks fine"},
	}
	out := applyVerdicts(batch, verdicts, discardLog())

	byTitle := map[string]ValidatedFinding{}
	for _, f := range out {
		byTitle[f.Title] = f
	}
	if _, ok := byTitle["refuted one"]; ok {
		t.Error("refuted finding must be dropped")
	}
	if _, ok := byTitle["duplicate one"]; ok {
		t.Error("duplicate finding must be dropped")
	}
	u := byTitle["uncertain one"]
	if u.Verification != VerificationUncertain || u.Confidence != 0.5 || !u.Source.RequiresHumanCheck {
		t.Errorf("uncertain handling wrong: %+v", u)
	}
	if !strings.Contains(u.ValidationError, "unverified: cannot tell") {
		t.Errorf("uncertain must carry the reason: %q", u.ValidationError)
	}
	c := byTitle["confirmed one"]
	if c.Verification != VerificationConfirmed || c.Confidence != 0.95 {
		t.Errorf("confirmed must boost confidence: %+v", c)
	}
	if byTitle["unassessed one"].Verification != VerificationUnverified {
		t.Errorf("missing verdict must mark unverified: %+v", byTitle["unassessed one"])
	}
	b := byTitle["blocking refuted"]
	if b.Title == "" {
		t.Fatal("blocking finding must never be dropped by the skeptic")
	}
	if b.Verification != VerificationUncertain || !strings.Contains(b.ValidationError, "skeptic disputed") {
		t.Errorf("blocking refutation must demote, not drop: %+v", b)
	}
}

func TestApplyVerdictsDuplicateGuards(t *testing.T) {
	t.Run("blocking duplicate is demoted, not dropped", func(t *testing.T) {
		batch := []ValidatedFinding{vf("high", "kept", 0.9), vf("blocking", "blocker dup", 0.9)}
		verdicts := []llm.FindingVerdict{
			{Index: 1, Verdict: "confirmed", Confidence: 0.9},
			{Index: 2, Verdict: "confirmed", DuplicateOf: 1},
		}
		out := applyVerdicts(batch, verdicts, discardLog())
		if len(out) != 2 {
			t.Fatalf("blocking duplicate must survive: %v", titlesOf(out))
		}
		b := out[1]
		if b.Verification != VerificationUncertain || !strings.Contains(b.ValidationError, "duplicate of finding 1") {
			t.Errorf("blocking duplicate must be demoted with a note: %+v", b)
		}
	})

	t.Run("mutual duplicates keep the smaller index", func(t *testing.T) {
		batch := []ValidatedFinding{vf("high", "first", 0.9), vf("high", "second", 0.9)}
		verdicts := []llm.FindingVerdict{
			{Index: 1, Verdict: "confirmed", DuplicateOf: 2},
			{Index: 2, Verdict: "confirmed", DuplicateOf: 1},
		}
		out := applyVerdicts(batch, verdicts, discardLog())
		if len(out) != 1 || out[0].Title != "first" {
			t.Fatalf("mutual duplicates must keep the first: %v", titlesOf(out))
		}
	})

	t.Run("duplicate_of a refuted target is not honored", func(t *testing.T) {
		batch := []ValidatedFinding{vf("high", "refuted target", 0.9), vf("high", "pointing dup", 0.9)}
		verdicts := []llm.FindingVerdict{
			{Index: 1, Verdict: "refuted", Reason: "wrong"},
			{Index: 2, Verdict: "confirmed", DuplicateOf: 1},
		}
		out := applyVerdicts(batch, verdicts, discardLog())
		if len(out) != 1 || out[0].Title != "pointing dup" {
			t.Fatalf("dup of a refuted target must survive (else the bug vanishes twice): %v", titlesOf(out))
		}
		if out[0].Verification != VerificationConfirmed {
			t.Errorf("surviving finding keeps its own verdict: %+v", out[0])
		}
	})
}

func TestSkepticStageRefutesViaEngine(t *testing.T) {
	fake := llm.NewFake(&llm.ReviewResponse{
		Summary: "ok", RiskLevel: "medium", OverallRecommendation: "comment",
		Findings: []llm.Finding{
			{Severity: "high", Category: "correctness", FilePath: "main.go", LineKind: "new", Line: 2,
				Title: "real bug", Body: "b", Confidence: 0.9},
			{Severity: "high", Category: "correctness", FilePath: "main.go", LineKind: "new", Line: 3,
				Title: "fake bug", Body: "b", Confidence: 0.9},
		},
	})
	fake.JSONFn = func(req llm.Request) (string, error) {
		if req.JSONSchema != llm.VerdictJSONSchema {
			return "", errors.New("unexpected CompleteJSON call")
		}
		if !req.AgentMode || req.WorkDir == "" {
			t.Errorf("skeptic must run in agent mode with a workdir: %+v", req)
		}
		if !strings.Contains(req.Prompt, "Finding 1") || !strings.Contains(req.Prompt, "Finding 2") {
			t.Errorf("skeptic prompt must number findings:\n%s", req.Prompt)
		}
		v, _ := json.Marshal(llm.VerdictResponse{Verdicts: []llm.FindingVerdict{
			{Index: 1, Verdict: "confirmed", Confidence: 0.95},
			{Index: 2, Verdict: "refuted", Reason: "guarded two lines above"},
		}})
		return string(v), nil
	}

	in := testInput(t, PipelineConfig{VerifyMode: VerifySkeptic})
	in.WorkDir = t.TempDir() // no go.mod → build verifier is a no-op
	in.AgentMode = true
	fake.Response.CostUSD = 0.02
	fake.JSONCost = 0.03

	res, err := pipelineEngine(fake).Review(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 1 || res.Findings[0].Title != "real bug" {
		t.Fatalf("refuted finding must be dropped: %+v", res.Findings)
	}
	if res.Findings[0].Verification != VerificationConfirmed {
		t.Errorf("surviving finding must be confirmed: %+v", res.Findings[0])
	}
	// Skeptic spend must be accounted: pass cost + skeptic batch cost.
	if res.CostUSD != 0.05 {
		t.Errorf("CostUSD must include the skeptic batch: got %v, want 0.05", res.CostUSD)
	}
	var skepticRep *PassReport
	for i := range res.PassReports {
		if res.PassReports[i].Name == "skeptic" {
			skepticRep = &res.PassReports[i]
		}
	}
	if skepticRep == nil || skepticRep.CostUSD != 0.03 {
		t.Errorf("skeptic must appear in pass reports with its cost: %+v", res.PassReports)
	}
}

func TestSkepticFailureKeepsFindingsUnverified(t *testing.T) {
	fake := llm.NewFake(&llm.ReviewResponse{
		Summary: "ok", RiskLevel: "medium", OverallRecommendation: "comment",
		Findings: []llm.Finding{
			{Severity: "high", Category: "correctness", FilePath: "main.go", LineKind: "new", Line: 2,
				Title: "bug", Body: "b", Confidence: 0.9},
		},
	})
	fake.JSONFn = func(req llm.Request) (string, error) { return "", errors.New("skeptic down") }

	in := testInput(t, PipelineConfig{VerifyMode: VerifySkeptic})
	in.WorkDir = t.TempDir()

	res, err := pipelineEngine(fake).Review(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 1 || res.Findings[0].Verification != VerificationUnverified {
		t.Fatalf("skeptic failure must keep findings unverified: %+v", res.Findings)
	}
}

func TestSkepticSkippedWithoutWorkdir(t *testing.T) {
	fake := llm.NewFake(&llm.ReviewResponse{
		Summary: "ok", RiskLevel: "medium", OverallRecommendation: "comment",
		Findings: []llm.Finding{
			{Severity: "high", Category: "correctness", FilePath: "main.go", LineKind: "new", Line: 2,
				Title: "bug", Body: "b", Confidence: 0.9},
		},
	})
	res, err := pipelineEngine(fake).Review(t.Context(), testInput(t, PipelineConfig{VerifyMode: VerifySkeptic}))
	if err != nil {
		t.Fatal(err)
	}
	if fake.Calls != 1 {
		t.Errorf("no workdir → skeptic must not run, got %d calls", fake.Calls)
	}
	if res.Findings[0].Verification != "" {
		t.Errorf("verification must be empty when skeptic never ran: %+v", res.Findings[0])
	}
}

func TestVerificationRankOrdering(t *testing.T) {
	fs := []ValidatedFinding{
		func() ValidatedFinding {
			f := vf("high", "uncertain", 0.9)
			f.Verification = VerificationUncertain
			return f
		}(),
		func() ValidatedFinding {
			f := vf("high", "confirmed", 0.4)
			f.Verification = VerificationConfirmed
			return f
		}(),
		vf("high", "plain", 0.6),
	}
	rankFindings(fs)
	want := []string{"confirmed", "plain", "uncertain"}
	for i, w := range want {
		if fs[i].Title != w {
			t.Fatalf("rank order wrong: got %v", []string{fs[0].Title, fs[1].Title, fs[2].Title})
		}
	}
}
