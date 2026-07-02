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
