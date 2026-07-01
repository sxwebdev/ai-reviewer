package review

import (
	"strings"
	"testing"
)

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
