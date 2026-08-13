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

// Repos commonly expose .claude/skills/<name> as a symlink to a shared skills
// directory; ReadDir reports those entries as non-dirs, so discovery must not
// gate on DirEntry.IsDir().
func TestDiscoverFollowsSymlinkedSkillDirs(t *testing.T) {
	root := t.TempDir()
	shared, claude := filepath.Join(root, "skills"), filepath.Join(root, ".claude", "skills")
	writeSkill(t, shared, "integration-tests", "---\nname: integration-tests\ndescription: linked\n---\n")
	if err := os.MkdirAll(claude, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "..", "skills", "integration-tests"),
		filepath.Join(claude, "integration-tests")); err != nil {
		t.Fatal(err)
	}
	// A plain file next to the symlink must still be ignored.
	if err := os.WriteFile(filepath.Join(claude, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := Discover([]Source{{Label: "project", Dir: claude}})
	if len(got) != 1 {
		t.Fatalf("want 1 skill via symlink, got %d: %+v", len(got), got)
	}
	if got[0].Name != "integration-tests" || got[0].Description != "linked" {
		t.Errorf("symlinked skill parsed wrong: %+v", got[0])
	}
}

func TestDiscoverSkipsMissingDirs(t *testing.T) {
	got := Discover([]Source{{Label: "x", Dir: ""}, {Label: "y", Dir: "/nonexistent/path/xyz"}})
	if len(got) != 0 {
		t.Errorf("missing dirs should yield nothing, got %+v", got)
	}
}
