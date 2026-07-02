package review

import (
	"slices"
	"strings"
	"testing"
)

func factorWeight(r RiskReport, name string) float64 {
	for _, f := range r.Factors {
		if f.Name == name {
			return f.Weight
		}
	}
	return 0
}

func TestComputeRiskFactors(t *testing.T) {
	cases := []struct {
		name   string
		in     RiskInput
		factor string
		want   float64
	}{
		{"diff size scales", RiskInput{LinesAdded: 300, LinesRemoved: 100}, "diff_size", 10},
		{"diff size saturates", RiskInput{LinesAdded: 5000}, "diff_size", 25},
		{"spread saturates", RiskInput{FilesChanged: 30}, "diff_spread", 10},
		{"churn hot files", RiskInput{ChurnByFile: map[string]int{"a.go": 7, "b.go": 2}}, "churn", 3},
		{"bugfix magnets", RiskInput{FixesByFile: map[string]int{"a.go": 3}}, "bugfix_history", 5},
		{"sensitive paths", RiskInput{SensitiveHits: []string{"auth/x.go", "go.mod"}}, "sensitive_paths", 10},
		{"no tests", RiskInput{BehaviorFiles: 3}, "no_tests", 10},
		{"tests touched clears no_tests", RiskInput{BehaviorFiles: 3, TestsTouched: true}, "no_tests", 0},
		{"new deps", RiskInput{NewDependencies: []string{"left-pad"}}, "new_dependencies", 5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := ComputeRisk(c.in)
			if got := factorWeight(r, c.factor); got != c.want {
				t.Errorf("factor %s weight = %v, want %v (report %+v)", c.factor, got, c.want, r)
			}
		})
	}
}

func TestComputeRiskLevels(t *testing.T) {
	if r := ComputeRisk(RiskInput{}); r.Score != 0 || r.Level != "low" {
		t.Errorf("empty input must be low/0: %+v", r)
	}
	high := ComputeRisk(RiskInput{
		LinesAdded: 2000, FilesChanged: 20,
		SensitiveHits: []string{"auth/a.go", "auth/b.go", "go.mod"},
		BehaviorFiles: 5,
	})
	if high.Level != "high" && high.Level != "critical" {
		t.Errorf("big risky diff must be high+: %+v", high)
	}
	if high.Score > 100 {
		t.Errorf("score must clamp at 100: %d", high.Score)
	}
}

func TestDetectNewDependencies(t *testing.T) {
	goMod := fileDiff(t, "go.mod", "go.mod", "@@ -1,2 +1,4 @@\n module m\n+require (\n+\tgithub.com/pkg/errors v0.9.1\n go 1.26\n")
	pkg := fileDiff(t, "package.json", "package.json", "@@ -1,3 +1,4 @@\n {\n   \"dependencies\": {\n+    \"left-pad\": \"^1.3.0\",\n   }\n")
	reqs := fileDiff(t, "requirements.txt", "requirements.txt", "@@ -1,1 +1,2 @@\n flask==2.0\n+requests>=2.28\n")
	other := fileDiff(t, "main.go", "main.go", "@@ -1,1 +1,2 @@\n x\n+github.com/fake/dep v1.0.0\n")

	got := DetectNewDependencies([]*FileDiff{goMod, pkg, reqs, other})
	for _, want := range []string{"github.com/pkg/errors", "left-pad", "requests"} {
		if !slices.Contains(got, want) {
			t.Errorf("missing dependency %q in %v", want, got)
		}
	}
	if len(got) != 3 {
		t.Errorf("non-manifest files must not contribute: %v", got)
	}
}

func TestDetectNewDependenciesIgnoresNonDepFields(t *testing.T) {
	// A release MR bumping its own version (and other digit-valued fields)
	// must not count as adding dependencies.
	versionBump := fileDiff(t, "package.json", "package.json",
		"@@ -1,4 +1,4 @@\n {\n-  \"version\": \"2.0.0\",\n+  \"version\": \"2.1.0\",\n   \"name\": \"app\"\n }\n")
	if got := DetectNewDependencies([]*FileDiff{versionBump}); len(got) != 0 {
		t.Errorf("version bump must not count as a dependency: %v", got)
	}

	// Dep-looking line after the dependencies block CLOSED must not count.
	closedBlock := fileDiff(t, "package.json", "package.json",
		"@@ -1,5 +1,6 @@\n {\n   \"dependencies\": {\n   },\n+  \"port\": \"8080\",\n   \"name\": \"app\"\n }\n")
	if got := DetectNewDependencies([]*FileDiff{closedBlock}); len(got) != 0 {
		t.Errorf("field after closed deps block must not count: %v", got)
	}

	// devDependencies context still counts.
	dev := fileDiff(t, "package.json", "package.json",
		"@@ -1,3 +1,4 @@\n {\n   \"devDependencies\": {\n+    \"vitest\": \"^1.0.0\",\n   }\n")
	if got := DetectNewDependencies([]*FileDiff{dev}); len(got) != 1 || got[0] != "vitest" {
		t.Errorf("devDependencies entry must count: %v", got)
	}
}

func TestWriteRiskSection(t *testing.T) {
	in := ReviewInput{
		Title: "T", ProjectPath: "g/r",
		Files: []*FileDiff{fileDiff(t, "main.go", "main.go", mapDiff)},
		Risk: &RiskReport{Score: 62, Level: "high", Factors: []RiskFactor{
			{Name: "churn", Weight: 6, Detail: "internal/auth/token.go (14 commits)"},
		}},
	}
	got := BuildUserPrompt(in)
	if !strings.Contains(got, "Score 62/100 (high)") || !strings.Contains(got, "internal/auth/token.go (14 commits)") {
		t.Errorf("risk section missing:\n%s", got)
	}
	in.Risk = nil
	if strings.Contains(BuildUserPrompt(in), "Deterministic risk assessment") {
		t.Error("nil risk must render no section")
	}
}
