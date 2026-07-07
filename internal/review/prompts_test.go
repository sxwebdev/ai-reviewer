package review

import (
	"strings"
	"testing"
)

func TestBuildUserPromptAgentModeProtocol(t *testing.T) {
	in := ReviewInput{
		Title: "T", ProjectPath: "g/r",
		Files:     []*FileDiff{fileDiff(t, "main.go", "main.go", mapDiff)},
		AgentMode: true, WorkDir: "/tmp/wt",
	}
	got := BuildUserPrompt(in)
	if !strings.Contains(got, "investigate before asserting") {
		t.Error("agent mode prompt missing investigation protocol")
	}
	if !strings.Contains(got, "Grep for its callers") {
		t.Error("agent mode prompt missing caller-check mandate")
	}
	if strings.Contains(got, "NO tools") {
		t.Error("agent mode prompt must not contain the degraded notice")
	}
	// Annotated diff: explicit line numbers instead of a raw unified diff.
	if !strings.Contains(got, `2 + | import (`) {
		t.Errorf("prompt missing annotated diff lines:\n%s", got)
	}
}

func TestBuildUserPromptDegradedNotice(t *testing.T) {
	in := ReviewInput{
		Title: "T", ProjectPath: "g/r",
		Files: []*FileDiff{fileDiff(t, "main.go", "main.go", mapDiff)},
	}
	got := BuildUserPrompt(in)
	if !strings.Contains(got, "NO tools and NO repository access") {
		t.Error("non-agent prompt missing degraded-mode notice")
	}
	if strings.Contains(got, "investigate before asserting") {
		t.Error("non-agent prompt must not mandate investigation")
	}
}

func TestBuildSystemPromptChangedLinesRule(t *testing.T) {
	got := BuildSystemPrompt(DefaultProfile())
	if !strings.Contains(got, "even when your investigation covered unchanged code") {
		t.Error("system prompt missing changed-lines anchoring rule")
	}
}

func TestBuildSystemPromptLanguage(t *testing.T) {
	cases := map[string]string{
		"ru":   "Write every comment in Russian",
		"en":   "Write every comment in English",
		"auto": "the same language as the MR description",
		"":     "the same language as the MR description",
	}
	for lang, want := range cases {
		p := DefaultProfile()
		p.Language = lang
		got := BuildSystemPrompt(p)
		if !strings.Contains(got, want) {
			t.Errorf("language %q: prompt missing %q\n---\n%s", lang, want, got)
		}
	}
}
