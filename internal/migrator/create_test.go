package migrator_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sxwebdev/ai-reviewer/internal/migrator"
)

func TestCreateNumbersFromTheHighestExistingVersion(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	write(t, dir, "0000_uuidv7.up.sql")
	write(t, dir, "0000_uuidv7.down.sql")
	write(t, dir, "0002_digest.up.sql")
	write(t, dir, "0002_digest.down.sql")
	// Not a migration; it must not influence the numbering.
	write(t, dir, "README.md")

	base, err := migrator.Create(dir, "add teams")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if base != "0003_add_teams" {
		t.Fatalf("got %q, want 0003_add_teams", base)
	}
	for _, suffix := range []string{".up.sql", ".down.sql"} {
		if _, err := os.Stat(filepath.Join(dir, base+suffix)); err != nil {
			t.Errorf("missing %s: %v", base+suffix, err)
		}
	}
}

func TestCreateOnAnEmptyDirStartsAtOne(t *testing.T) {
	t.Parallel()
	base, err := migrator.Create(t.TempDir(), "initial")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if base != "0001_initial" {
		t.Errorf("got %q, want 0001_initial", base)
	}
}

func TestCreateSanitizesNames(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "spaces", in: "add teams table", want: "0001_add_teams_table"},
		{name: "case", in: "AddTeams", want: "0001_addteams"},
		{name: "punctuation", in: "add-teams!", want: "0001_add_teams_"},
		{name: "surrounding space", in: "  teams  ", want: "0001_teams"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := migrator.Create(t.TempDir(), tt.in)
			if err != nil {
				t.Fatalf("Create(%q): %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCreateRejectsAnEmptyName(t *testing.T) {
	t.Parallel()
	if _, err := migrator.Create(t.TempDir(), "  "); err == nil {
		t.Fatal("Create with a blank name succeeded, want an error")
	}
}

func TestCreateRejectsAMissingDir(t *testing.T) {
	t.Parallel()
	if _, err := migrator.Create(filepath.Join(t.TempDir(), "nope"), "x"); err == nil {
		t.Fatal("Create in a missing directory succeeded, want an error")
	}
}

func write(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("-- x\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
