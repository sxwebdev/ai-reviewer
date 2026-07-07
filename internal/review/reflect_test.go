package review

import (
	"strings"
	"testing"

	"github.com/sxwebdev/ai-reviewer/internal/llm"
)

func reflFinding(sev, title, pass string, conf float64) llm.Finding {
	return llm.Finding{
		Severity: sev, Category: "correctness", FilePath: "main.go", LineKind: "new", Line: 2,
		Title: title, Body: "b", Confidence: conf, PassName: pass,
	}
}

func TestApplyReflect(t *testing.T) {
	pre := &llm.ReviewResponse{
		Summary: "s", RiskLevel: "high", OverallRecommendation: "request_changes",
		Findings: []llm.Finding{
			reflFinding("high", "kept bug", "correctness", 0.9),
			reflFinding("blocking", "removed blocker", "security", 0.95),
			reflFinding("low", "removed nit", "general", 0.4),
		},
		CostUSD: 0.05,
	}
	// Reflection kept only the first finding, with PassName lost in the round-trip.
	post := &llm.ReviewResponse{
		Summary: "s", RiskLevel: "high", OverallRecommendation: "request_changes",
		Findings: []llm.Finding{reflFinding("high", "kept bug", "", 0.9)},
		CostUSD:  pre.CostUSD,
	}

	got := applyReflect(pre, post, discardLog())

	if got.Raw == "" || !strings.Contains(got.Raw, "kept bug") {
		t.Errorf("Raw must be re-marshaled after reflect: %q", got.Raw)
	}
	if got.Findings[0].PassName != "correctness" {
		t.Errorf("PassName provenance must be restored: %+v", got.Findings[0])
	}
	if len(got.Findings) != 2 {
		t.Fatalf("removed blocker must be restored (and removed nit must stay removed): %+v", got.Findings)
	}
	restored := got.Findings[1]
	if restored.Title != "removed blocker" || !restored.RequiresHumanCheck || restored.Confidence > 0.5 {
		t.Errorf("restored blocker must be demoted for human check: %+v", restored)
	}
	if restored.PassName != "security" {
		t.Errorf("restored blocker keeps its provenance: %+v", restored)
	}
}

func TestApplyReflectDropsAddedFindings(t *testing.T) {
	pre := &llm.ReviewResponse{Findings: []llm.Finding{reflFinding("high", "real bug", "general", 0.9)}}
	post := &llm.ReviewResponse{Findings: []llm.Finding{
		reflFinding("high", "real bug", "", 0.9),
		reflFinding("high", "hallucinated new bug", "", 0.9), // not in pre
	}}
	got := applyReflect(pre, post, discardLog())
	if len(got.Findings) != 1 || got.Findings[0].Title != "real bug" {
		t.Fatalf("reflection may only remove or demote, never add: %+v", got.Findings)
	}
}

func TestApplyReflectRewordedBlockerNotDuplicated(t *testing.T) {
	pre := &llm.ReviewResponse{Findings: []llm.Finding{reflFinding("blocking", "nil deref in handler", "general", 0.9)}}
	// The model reworded the blocker's title: the reworded copy is an
	// addition (dropped) and the original is restored demoted — exactly one
	// finding must survive.
	post := &llm.ReviewResponse{Findings: []llm.Finding{reflFinding("blocking", "possible nil pointer dereference", "", 0.9)}}
	got := applyReflect(pre, post, discardLog())
	if len(got.Findings) != 1 {
		t.Fatalf("reworded blocker must not produce near-duplicates: %+v", got.Findings)
	}
	f := got.Findings[0]
	if f.Title != "nil deref in handler" || !f.RequiresHumanCheck || f.Confidence > 0.5 {
		t.Errorf("original blocker must be restored demoted: %+v", f)
	}
}

func TestApplyReflectNoChanges(t *testing.T) {
	pre := &llm.ReviewResponse{Findings: []llm.Finding{reflFinding("blocking", "b1", "general", 0.9)}}
	post := &llm.ReviewResponse{Findings: []llm.Finding{reflFinding("blocking", "b1", "", 0.9)}}
	got := applyReflect(pre, post, discardLog())
	if len(got.Findings) != 1 {
		t.Fatalf("present blocker must not be duplicated: %+v", got.Findings)
	}
	if got.Findings[0].PassName != "general" {
		t.Errorf("provenance restore failed: %+v", got.Findings[0])
	}
}

func TestReflectCostCountedOnSuccess(t *testing.T) {
	fake := llm.NewFake(&llm.ReviewResponse{
		Summary: "ok", RiskLevel: "low", OverallRecommendation: "comment",
		Findings: []llm.Finding{passFinding("high", "correctness", "bug", 0.9)},
		CostUSD:  1.0,
	})
	// The reflect call keeps the finding and costs 0.25 on its own.
	fake.JSON = `{"summary":"ok","risk_level":"low","overall_recommendation":"comment",` +
		`"findings":[{"severity":"high","category":"correctness","file_path":"main.go",` +
		`"line_kind":"new","line":2,"title":"bug","body":"body of bug","confidence":0.9}]}`
	fake.JSONCost = 0.25

	res, err := pipelineEngine(fake).Review(t.Context(), testInput(t, PipelineConfig{VerifyMode: VerifyReflect}))
	if err != nil {
		t.Fatal(err)
	}
	if res.CostUSD != 1.25 {
		t.Errorf("successful reflection must add its cost: got %v, want 1.25", res.CostUSD)
	}
	var reflectRep *PassReport
	for i := range res.PassReports {
		if res.PassReports[i].Name == "reflect" {
			reflectRep = &res.PassReports[i]
		}
	}
	if reflectRep == nil || reflectRep.CostUSD != 0.25 {
		t.Errorf("reflect must appear in pass reports with its cost: %+v", res.PassReports)
	}
}

func TestMaxCommentsZeroAndNegativeUseDefault(t *testing.T) {
	for _, mc := range []int{0, -1} {
		fake := llm.NewFake(&llm.ReviewResponse{
			Summary: "ok", RiskLevel: "low", OverallRecommendation: "comment",
			Findings: []llm.Finding{passFinding("high", "correctness", "bug", 0.9)},
		})
		in := testInput(t, PipelineConfig{})
		p := *DefaultProfile()
		p.MaxComments = mc
		in.Profile = &p
		res, err := pipelineEngine(fake).Review(t.Context(), in)
		if err != nil {
			t.Fatalf("MaxComments=%d: %v", mc, err)
		}
		if len(res.Findings) != 1 {
			t.Errorf("MaxComments=%d must mean default, not drop-everything: %+v", mc, res.Findings)
		}
	}
}
