package skills

import (
	"os"
	"path/filepath"
	"testing"
)

// writeSkill creates <dir>/<name>/SKILL.md with the given frontmatter body.
func writeSkill(t *testing.T, dir, name, content string) {
	t.Helper()
	sd := filepath.Join(dir, name)
	if err := os.MkdirAll(sd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sd, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverParsesFrontmatterAndSorts(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "zeta", "---\nname: zeta\ndescription: \"last one\"\n---\nbody\n")
	writeSkill(t, dir, "alpha", "---\nname: alpha\ndescription: first one\n---\nbody\n")
	// A directory without SKILL.md is ignored.
	if err := os.MkdirAll(filepath.Join(dir, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := Discover([]Source{{Label: "user", Dir: dir}})
	if len(got) != 2 {
		t.Fatalf("want 2 skills, got %d: %+v", len(got), got)
	}
	if got[0].Name != "alpha" || got[1].Name != "zeta" {
		t.Errorf("not sorted by name: %+v", got)
	}
	if got[0].Description != "first one" {
		t.Errorf("description parse wrong: %q", got[0].Description)
	}
	if got[1].Description != "last one" { // quotes stripped
		t.Errorf("quoted description not unquoted: %q", got[1].Description)
	}
	if got[0].Source != "user" {
		t.Errorf("source label not recorded: %q", got[0].Source)
	}
}

func TestDiscoverNameFallsBackToDir(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "no-name", "---\ndescription: has no name key\n---\n")
	got := Discover([]Source{{Label: "user", Dir: dir}})
	if len(got) != 1 || got[0].Name != "no-name" {
		t.Fatalf("expected name to fall back to dir, got %+v", got)
	}
}

func TestDiscoverFirstSourceWinsOnDuplicate(t *testing.T) {
	userDir, projDir := t.TempDir(), t.TempDir()
	writeSkill(t, userDir, "dup", "---\nname: dup\ndescription: from user\n---\n")
	writeSkill(t, projDir, "dup", "---\nname: dup\ndescription: from project\n---\n")

	got := Discover([]Source{{Label: "user", Dir: userDir}, {Label: "project", Dir: projDir}})
	if len(got) != 1 {
		t.Fatalf("want 1 deduped skill, got %d", len(got))
	}
	if got[0].Description != "from user" {
		t.Errorf("first source should win: %+v", got[0])
	}
}

func TestDiscoverSkipsMissingDirs(t *testing.T) {
	got := Discover([]Source{{Label: "x", Dir: ""}, {Label: "y", Dir: "/nonexistent/path/xyz"}})
	if len(got) != 0 {
		t.Errorf("missing dirs should yield nothing, got %+v", got)
	}
}
