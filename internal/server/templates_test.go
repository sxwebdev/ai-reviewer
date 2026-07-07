package server

import (
	"bytes"
	"testing"

	"github.com/sxwebdev/ai-reviewer/internal/state"
)

func TestParseTemplates(t *testing.T) {
	if _, err := parseTemplates(); err != nil {
		t.Fatalf("parseTemplates: %v", err)
	}
}

// TestTemplatesRender executes every page with representative data so that
// field/function errors (which go build does not catch) fail here.
func TestTemplatesRender(t *testing.T) {
	tmpl, err := parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	line := int64(148)
	mrIID := int64(42)
	mr := &state.MergeRequest{ID: 3, IID: 42, Title: "Test MR", WebURL: "https://gitlab.test/mr/42",
		SourceBranch: "feat", TargetBranch: "main", HeadSHA: "5fff58b17ea2", CreatedAt: 1700000000000}
	rev := &state.Review{ID: "rev1", RiskLevel: "high", OverallRecommendation: "request_changes",
		Summary: "a summary", CostUSD: 0.93, LLMModel: "claude-opus-4-8", DurationMS: 72000}
	// Header switches + run-form skills exercise the new template branches
	// (model select .ID/.Label, agentic select, skills fieldset).
	hdrUI := UIConfig{Host: "h", AgentMode: true, PipelineMode: "standard",
		PipelineModes: []string{"cheap", "standard", "deep"},
		Models:        []ModelChoice{{ID: "claude-opus-4-8", Label: "Opus 4.8"}, {ID: "claude-sonnet-5", Label: "Sonnet 5"}}}
	find := &state.Finding{ID: "f1", Severity: "blocking", Category: "correctness", Title: "bug",
		Body: "details", FilePath: "internal/eval/eval.go", NewLine: &line, Status: "proposed",
		ValidationError: "approximate location", EditedAt: 1700000001000}
	// diff pane: one file with a pinned finding, one binary file, one unanchored.
	diff := diffVM{Captured: true, HasFindings: true,
		Files: []diffFileVM{
			{DisplayPath: "internal/eval/eval.go", Kind: "modified", Expanded: true, FindingCount: 1,
				Hunks: []diffHunkVM{{Header: "@@ -146,4 +146,4 @@", Lines: []diffLineVM{
					{Kind: "ctx", OldLine: "146", NewLine: "146", Content: "if err != nil {"},
					{Kind: "add", NewLine: "148", Content: "p := new(0.3)", Findings: []*state.Finding{find}},
				}}}},
			{DisplayPath: "logo.png", Kind: "new", IsBinary: true},
		},
		Unanchored: []*state.Finding{find}}

	cases := map[string]any{
		"dashboard": dashboardVM{baseVM: baseVM{UI: UIConfig{Host: "h"}}, MRs: []dashItem{
			{DashboardRow: state.DashboardRow{ID: 1, IID: 5, Title: "reviewed", Author: "alice", CreatedAt: 1700000000000, HeadSHA: "abc", ReviewHeadSHA: "abc", RiskLevel: "high", Findings: 2, Drafted: 1, Published: 2}},
			{DashboardRow: state.DashboardRow{ID: 2, IID: 6, Title: "fresh", HeadSHA: "def"}},
			{DashboardRow: state.DashboardRow{ID: 3, IID: 7, Title: "moved", HeadSHA: "new", ReviewHeadSHA: "old", RiskLevel: "low", Findings: 1}},
		}},
		"mr": mrVM{baseVM: baseVM{UI: hdrUI}, MR: mr, Review: rev, ProposedCount: 1, ApprovedCount: 1, DraftedCount: 1,
			Groups:        []findingGroup{{Severity: "blocking", Items: []*state.Finding{find}}},
			PublishPhrase: "PUBLISH 1 COMMENTS", CostLabel: "≈$0.9300 (covered by subscription)", Diff: diff,
			AgentMode: true, UsedSkills: []string{"go-test"},
			AvailableSkills: []skillOption{{Name: "go-test", Description: "Go testing house style"}, {Name: "commit"}},
			PastReviews:     []pastReviewVM{{ID: "revOld", When: "now", HeadSHA: "abc", RiskLevel: "low", Status: "done", Findings: 0}}},
		"jobs":     jobsVM{baseVM: baseVM{UI: UIConfig{Host: "h"}}, Jobs: []*state.Job{{ID: "j1", Type: "review", Status: "failed", Error: "boom", MRIID: &mrIID, ProgressCurrent: 2, ProgressTotal: 5}}},
		"memory":   memoryVM{baseVM: baseVM{UI: UIConfig{Host: "h"}}, Items: []*state.ReviewMemory{{ID: "m1", Type: "false_positive", Scope: "project", Title: "t", Body: "b", Enabled: true}}},
		"settings": settingsVM{baseVM: baseVM{UI: UIConfig{Host: "h"}}, Cfg: UIConfig{Host: "h", LLMModel: "opus", CommentLanguage: "auto", SeverityThreshold: "medium", MaxComments: 12, AgentMode: true, SubscriptionAuth: true}},
	}
	for page, data := range cases {
		t.Run(page, func(t *testing.T) {
			var buf bytes.Buffer
			if err := tmpl[page].ExecuteTemplate(&buf, "base.gohtml", data); err != nil {
				t.Fatalf("execute %s: %v", page, err)
			}
			if buf.Len() == 0 {
				t.Fatalf("%s rendered empty", page)
			}
		})
	}
}

// TestReviewSectionFragment exercises the htmx fragment directly, in both the
// running (polling) and completed states.
func TestReviewSectionFragment(t *testing.T) {
	tmpl, err := parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	mr := &state.MergeRequest{ID: 1, WebURL: "https://x"}
	hist := &state.Review{ID: "old", HeadSHA: "0badf00d", RiskLevel: "low"}
	find := &state.Finding{ID: "f", Severity: "low", Title: "t", Body: "b", Status: "proposed"}
	for _, vm := range []mrVM{
		{baseVM: baseVM{UI: UIConfig{Host: "h"}}, MR: mr, Running: true, Progress: "2/5"},
		{baseVM: baseVM{UI: UIConfig{Host: "h"}}, MR: mr, JobStatus: "failed", JobError: "not logged in"},
		{baseVM: baseVM{UI: UIConfig{Host: "h"}}, MR: mr, Historical: true, Review: hist,
			Groups: []findingGroup{{Severity: "low", Items: []*state.Finding{find}}}},
	} {
		var buf bytes.Buffer
		if err := tmpl["mr"].ExecuteTemplate(&buf, "review-section", vm); err != nil {
			t.Fatalf("review-section: %v", err)
		}
	}
}

// TestHealthFragment renders the async-loaded health-checks fragment.
func TestHealthFragment(t *testing.T) {
	tmpl, err := parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	vm := healthVM{Checks: []HealthCheck{
		{Name: "git", Status: "ok", Detail: "/usr/bin/git"},
		{Name: "gitlab token", Status: "fail", Detail: "not set"},
		{Name: "claude auth", Status: "warn", Detail: "using existing login"},
	}}
	var buf bytes.Buffer
	if err := tmpl["settings"].ExecuteTemplate(&buf, "health-checks", vm); err != nil {
		t.Fatalf("health-checks: %v", err)
	}
	if buf.Len() == 0 {
		t.Fatal("health-checks rendered empty")
	}
}
