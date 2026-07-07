package toolchain

import (
	"os"
	"path/filepath"
	"testing"
)

func tree(t *testing.T, files ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, rel := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestNearestRootMonorepo(t *testing.T) {
	wd := tree(t,
		"package.json",
		"apps/web/package.json",
		"apps/web/src/App.tsx",
		"services/api/go.mod",
		"services/api/internal/handler.go",
		"scripts/deploy.sh",
	)

	cases := []struct {
		file    string
		markers []string
		want    string
		ok      bool
	}{
		{"apps/web/src/App.tsx", NodeMarkers, "apps/web", true}, // nearest wins over root
		{"scripts/deploy.sh", NodeMarkers, ".", true},           // falls to top-level package.json
		{"services/api/internal/handler.go", GoMarkers, "services/api", true},
		{"apps/web/src/App.tsx", GoMarkers, "", false},
	}
	for _, c := range cases {
		got, ok := NearestRoot(wd, c.file, c.markers)
		if got != c.want || ok != c.ok {
			t.Errorf("NearestRoot(%q, %v) = (%q, %v), want (%q, %v)", c.file, c.markers, got, ok, c.want, c.ok)
		}
	}
}

func TestGroupByRoot(t *testing.T) {
	wd := tree(t, "go.mod", "sub/go.mod", "sub/a.go", "b.go")
	got := GroupByRoot(wd, []string{"sub/a.go", "b.go", "nomatch.py"}, GoMarkers)
	if len(got["sub"]) != 1 || got["sub"][0] != "sub/a.go" {
		t.Errorf("sub bucket wrong: %+v", got)
	}
	if len(got["."]) != 2 { // b.go and nomatch.py both fall to the top go.mod... nomatch.py too (markers only decide root)
		t.Errorf("top bucket wrong: %+v", got)
	}
}

func TestIsTestPath(t *testing.T) {
	yes := []string{"a/b_test.go", "src/App.test.tsx", "src/x.spec.ts", "pkg/test_util.py", "src/__tests__/x.js"}
	no := []string{"a/b.go", "src/App.tsx", "tests.go", "contest_go.py.txt"}
	for _, p := range yes {
		if !IsTestPath(p) {
			t.Errorf("IsTestPath(%q) = false, want true", p)
		}
	}
	for _, p := range no {
		if IsTestPath(p) {
			t.Errorf("IsTestPath(%q) = true, want false", p)
		}
	}
}

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		rel   string
		globs []string
		want  bool
	}{
		{"internal/auth/token.go", []string{"**/auth/**"}, true},
		{"auth/token.go", []string{"**/auth/**"}, true},
		{"internal/author/x.go", []string{"**/auth/**"}, false},
		{"db/migrations/0001.sql", []string{"**/migrations/**"}, true},
		{"schema.sql", []string{"**/*.sql"}, true},
		{"go.mod", []string{"go.mod"}, true},
		{"Dockerfile.dev", []string{"Dockerfile*"}, true},
		{"yarn.lock", []string{"yarn.lock"}, true},
		// Explicit lockfile names, not "*lock*": ordinary files whose basename
		// merely contains "lock" must not count as sensitive.
		{"pkg/blocks/block.go", []string{"package-lock.json", "yarn.lock", "pnpm-lock.yaml"}, false},
		{"src/clock.ts", []string{"package-lock.json", "yarn.lock"}, false},
		{".github/workflows/ci.yml", []string{".github/**"}, true},
		{"main.go", []string{"**/auth/**", "*.sql"}, false},
		// "**/" means any depth INCLUDING zero — patterns with a slash in the
		// remainder must match at the root, one level deep, and deeper.
		{"gen/schema.sql", []string{"**/gen/*.sql"}, true},
		{"pkg/gen/schema.sql", []string{"**/gen/*.sql"}, true},
		{"a/b/gen/schema.sql", []string{"**/gen/*.sql"}, true},
		{"pkg/gens/schema.sql", []string{"**/gen/*.sql"}, false},
		{"a/testdata/big.json", []string{"**/testdata/*.json"}, true},
		{"deep/nested/x.sql", []string{"**/*.sql"}, true},
	}
	for _, c := range cases {
		if got := MatchGlob(c.rel, c.globs); got != c.want {
			t.Errorf("MatchGlob(%q, %v) = %v, want %v", c.rel, c.globs, got, c.want)
		}
	}
}

func TestIsSourceFile(t *testing.T) {
	cases := []struct {
		rel  string
		want bool
	}{
		{"cmd/main.go", true},
		{"web/src/App.tsx", true},
		{"scripts/migrate.py", true},
		{"db/0001_init.sql", true},
		{"api/v1/service.proto", true},
		{"deploy.sh", true},
		{"Handler.KT", true}, // extension match is deliberately case-insensitive
		{"README.md", false},
		{"config.yaml", false},
		{"package.json", false},
		{"LICENSE", false},
	}
	for _, c := range cases {
		if got := IsSourceFile(c.rel); got != c.want {
			t.Errorf("IsSourceFile(%q) = %v, want %v", c.rel, got, c.want)
		}
	}
}
