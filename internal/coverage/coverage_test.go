package coverage

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sxwebdev/ai-reviewer/internal/security"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestIntersect(t *testing.T) {
	profile := Profile{
		"a.go": {Hits: map[int]int{10: 1, 11: 0, 12: 3, 13: 0}},
		"b.go": {Hits: map[int]int{5: 1}},
	}
	added := map[string][]int{
		"a.go": {10, 11, 12, 13, 14}, // 14 not in universe (comment/decl)
		"b.go": {99},                 // added line not executable
		"c.go": {1, 2},               // no profile at all → unmeasured, NOT 0%
	}
	r := Intersect(profile, added)

	if len(r.Files) != 1 || r.Files[0].Path != "a.go" {
		t.Fatalf("only a.go must be measured: %+v", r.Files)
	}
	f := r.Files[0]
	if f.Added != 4 || f.Covered != 2 {
		t.Errorf("a.go: want 2/4 (line 14 excluded from denominator), got %d/%d", f.Covered, f.Added)
	}
	if len(f.Uncovered) != 2 || f.Uncovered[0] != 11 || f.Uncovered[1] != 13 {
		t.Errorf("uncovered lines wrong: %v", f.Uncovered)
	}
	if r.Pct != 50 {
		t.Errorf("pct = %v, want 50", r.Pct)
	}
}

func TestIntersectEmpty(t *testing.T) {
	r := Intersect(Profile{}, map[string][]int{"a.go": {1}})
	if len(r.Files) != 0 || r.TotalAdded != 0 || r.Pct != 0 {
		t.Errorf("no profiles must measure nothing: %+v", r)
	}
}

func TestParseGoCoverProfile(t *testing.T) {
	const prof = `mode: set
example.com/mod/pkg/a.go:5.13,7.2 2 1
example.com/mod/pkg/a.go:9.1,9.20 1 0
example.com/mod/pkg/a.go:6.1,6.5 1 3
other.module/x.go:1.1,2.2 1 1
`
	p, err := ParseGoCoverProfile(strings.NewReader(prof), "example.com/mod")
	if err != nil {
		t.Fatal(err)
	}
	fp := p["pkg/a.go"]
	if fp == nil {
		t.Fatalf("pkg/a.go missing: %+v", p)
	}
	// Lines 5-7 covered (block count 1); line 6 overlaps a hotter block (3) → max wins.
	if fp.Hits[5] != 1 || fp.Hits[6] != 3 || fp.Hits[7] != 1 {
		t.Errorf("block expansion wrong: %+v", fp.Hits)
	}
	if fp.Hits[9] != 0 {
		t.Errorf("uncovered line must be in universe with 0 hits: %+v", fp.Hits)
	}
	if _, ok := fp.Hits[8]; ok {
		t.Error("line 8 is outside all blocks and must not be in the universe")
	}
	if _, ok := p["x.go"]; ok {
		t.Error("files outside the module must be skipped")
	}
}

func TestParseLCOV(t *testing.T) {
	root := t.TempDir()
	lcov := `TN:
SF:src/App.tsx
DA:3,1
DA:4,0
DA:5,2
end_of_record
SF:` + filepath.Join(root, "src", "util.ts") + `
DA:1,0
end_of_record
SF:/somewhere/else/node_modules/x.js
DA:1,1
end_of_record
`
	p, err := ParseLCOV(strings.NewReader(lcov), root)
	if err != nil {
		t.Fatal(err)
	}
	app := p["src/App.tsx"]
	if app == nil || app.Hits[3] != 1 || app.Hits[4] != 0 || app.Hits[5] != 2 {
		t.Errorf("relative SF parsing wrong: %+v", app)
	}
	util := p["src/util.ts"]
	if util == nil || util.Hits[1] != 0 {
		t.Errorf("absolute SF must be made root-relative: %+v", p)
	}
	if len(p) != 2 {
		t.Errorf("files outside root must be skipped: %+v", p)
	}
}

// scriptedProvider lets Collect tests run without real toolchains.
type scriptedProvider struct {
	name    string
	markers []string
	detect  bool
	exts    []string // Covers suffixes; empty = covers everything
	run     func(ctx context.Context, root string) (Profile, string, error)
}

func (s scriptedProvider) Name() string       { return s.name }
func (s scriptedProvider) Markers() []string  { return s.markers }
func (s scriptedProvider) Detect(string) bool { return s.detect }
func (s scriptedProvider) Covers(path string) bool {
	if len(s.exts) == 0 {
		return true
	}
	for _, e := range s.exts {
		if strings.HasSuffix(path, e) {
			return true
		}
	}
	return false
}

func (s scriptedProvider) Run(ctx context.Context, root string) (Profile, string, error) {
	return s.run(ctx, root)
}

func TestCollectTimeoutIsolation(t *testing.T) {
	wd := t.TempDir()
	for _, marker := range []string{"fast/go.mod", "slow/go.mod"} {
		p := filepath.Join(wd, filepath.FromSlash(marker))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("module m\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	prov := scriptedProvider{
		name: "go", markers: []string{"go.mod"}, detect: true,
		run: func(ctx context.Context, root string) (Profile, string, error) {
			if strings.HasSuffix(root, "slow") {
				<-ctx.Done() // blocks until the per-root timeout fires
				return nil, "", ctx.Err()
			}
			return Profile{"a.go": {Hits: map[int]int{1: 1}}}, "", nil
		},
	}
	profile, skips, _ := Collect(t.Context(), wd,
		[]string{"fast/a.go", "slow/b.go"},
		[]Provider{prov}, Options{Timeout: 50 * time.Millisecond}, discardLog())

	if profile["fast/a.go"] == nil {
		t.Errorf("fast root must survive the slow root's timeout: %+v", profile)
	}
	found := false
	for _, s := range skips {
		if s.Root == "slow" && strings.Contains(s.Reason, "timed out") {
			found = true
		}
	}
	if !found {
		t.Errorf("slow root must be skipped with a timeout reason: %+v", skips)
	}
}

func TestCollectUncoverableFileUnderRootIsSkippedNote(t *testing.T) {
	wd := t.TempDir()
	if err := os.WriteFile(filepath.Join(wd, "go.mod"), []byte("module m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ran := false
	prov := scriptedProvider{
		name: "go", markers: []string{"go.mod"}, detect: true, exts: []string{".go"},
		run: func(context.Context, string) (Profile, string, error) {
			ran = true
			return Profile{"a.go": {Hits: map[int]int{1: 1}}}, "", nil
		},
	}
	// app.ts lives under the go.mod root but the go provider can't measure it.
	profile, skips, _ := Collect(t.Context(), wd, []string{"a.go", "app.ts"},
		[]Provider{prov}, Options{}, discardLog())
	if !ran || profile["a.go"] == nil {
		t.Fatalf("go file must still be measured: %+v", profile)
	}
	found := false
	for _, s := range skips {
		if strings.Contains(s.Reason, "no coverage provider") && strings.Contains(s.Reason, "app.ts") {
			found = true
		}
	}
	if !found {
		t.Errorf("uncoverable .ts under go root must surface as unmeasured: %+v", skips)
	}
}

func TestCollectSkipsRootWithNoMeasurableChanges(t *testing.T) {
	wd := t.TempDir()
	if err := os.WriteFile(filepath.Join(wd, "go.mod"), []byte("module m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prov := scriptedProvider{
		name: "go", markers: []string{"go.mod"}, detect: true, exts: []string{".go"},
		run: func(context.Context, string) (Profile, string, error) {
			t.Error("Run must not be called when no measurable file changed under the root")
			return nil, "", nil
		},
	}
	// Only docs/sql changed — no reason to run the module's whole test suite.
	Collect(t.Context(), wd, []string{"README.md", "schema.sql"}, []Provider{prov}, Options{}, discardLog())
}

func TestCollectUnclaimedFilesSkipped(t *testing.T) {
	wd := t.TempDir()
	profile, skips, _ := Collect(t.Context(), wd, []string{"script.sh"},
		[]Provider{scriptedProvider{name: "go", markers: []string{"go.mod"}, detect: true,
			run: func(context.Context, string) (Profile, string, error) { return nil, "", errors.New("unreachable") }}},
		Options{}, discardLog())
	if len(profile) != 0 || len(skips) != 1 || !strings.Contains(skips[0].Reason, "no coverage provider") {
		t.Errorf("unclaimed file must be reported: %+v %+v", profile, skips)
	}
}

func TestCollectAggregatesUnclaimedFilesIntoOneNote(t *testing.T) {
	wd := t.TempDir()
	files := []string{"a.md", "b.md", "c.yaml", "d.yaml", "e.txt", "f.txt", "g.json"}
	_, skips, _ := Collect(t.Context(), wd, files, nil, Options{}, discardLog())
	if len(skips) != 1 {
		t.Fatalf("unclaimed files must aggregate into one note, got %d: %+v", len(skips), skips)
	}
	if !strings.Contains(skips[0].Reason, "7 file(s)") || !strings.Contains(skips[0].Reason, "and 2 more") {
		t.Errorf("aggregated note must carry the count and a capped list: %q", skips[0].Reason)
	}
}

func TestGoProviderWithScriptedRunner(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/m\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var gotArgs []string
	run := func(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
		gotArgs = append([]string{name}, args...)
		// Find the -coverprofile=path arg and write a fixture profile there.
		for _, a := range args {
			if path, ok := strings.CutPrefix(a, "-coverprofile="); ok {
				return nil, os.WriteFile(path, []byte("mode: set\nexample.com/m/a.go:1.1,2.2 1 1\n"), 0o644)
			}
		}
		return nil, errors.New("no coverprofile arg")
	}
	prov := NewGoProvider(run, discardLog())
	profile, note, err := prov.Run(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if note != "" {
		t.Errorf("clean run must have no note: %q", note)
	}
	if profile["a.go"] == nil || profile["a.go"].Hits[1] != 1 {
		t.Errorf("profile wrong: %+v", profile)
	}
	if gotArgs[0] != "go" || gotArgs[1] != "test" || gotArgs[len(gotArgs)-1] != "./..." {
		t.Errorf("go test argv wrong: %v", gotArgs)
	}
}

func TestGoProviderMasksSecretsInFailureReason(t *testing.T) {
	security.RegisterSecret("s3cret-token-value")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
		return []byte("FAIL: env dump GITLAB_TOKEN=s3cret-token-value\n"), errors.New("exit 1")
	}
	_, _, err := NewGoProvider(run, discardLog()).Run(t.Context(), root)
	if err == nil {
		t.Fatal("failed run without profile must error")
	}
	if strings.Contains(err.Error(), "s3cret-token-value") {
		t.Errorf("registered secret leaked into skip reason: %v", err)
	}
	if !strings.Contains(err.Error(), "go test failed") {
		t.Errorf("reason must still explain the failure: %v", err)
	}
}

func TestGoProviderTestFailureWithPartialProfile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
		for _, a := range args {
			if path, ok := strings.CutPrefix(a, "-coverprofile="); ok {
				_ = os.WriteFile(path, []byte("mode: set\nexample.com/m/a.go:1.1,2.2 1 0\n"), 0o644)
			}
		}
		return []byte("--- FAIL: TestX"), errors.New("exit 1")
	}
	profile, note, err := NewGoProvider(run, discardLog()).Run(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note, "tests failed") {
		t.Errorf("partial profile must carry a note: %q", note)
	}
	if profile["a.go"] == nil {
		t.Errorf("partial profile must still parse: %+v", profile)
	}
}

func TestDetectNodeRunner(t *testing.T) {
	cases := []struct {
		pkg  string
		want string
	}{
		{`{"devDependencies":{"vitest":"^1.0"}}`, "vitest"},
		{`{"dependencies":{"jest":"^29"}}`, "jest"},
		{`{"scripts":{"test":"vitest run"}}`, "vitest"},
		{`{"scripts":{"test":"jest --ci"}}`, "jest"},
		{`{"scripts":{"test":"mocha"}}`, ""},
		{`not json`, ""},
	}
	for _, c := range cases {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(c.pkg), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := detectNodeRunner(root); got != c.want {
			t.Errorf("detectNodeRunner(%s) = %q, want %q", c.pkg, got, c.want)
		}
	}
}

func TestNodeProviderSkipsWithoutNodeModules(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"devDependencies":{"vitest":"1"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := NewNodeProvider(nil, false, discardLog()).Run(t.Context(), root)
	if err == nil || !strings.Contains(err.Error(), "node_modules missing") {
		t.Errorf("missing node_modules must skip with a clear reason: %v", err)
	}
}

func TestNodeProviderRunsVitestWithScriptedRunner(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"package.json":             `{"devDependencies":{"vitest":"1"}}`,
		"node_modules/.bin/vitest": "#!/bin/sh\n",
	}
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	run := func(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
		if !strings.HasSuffix(name, "vitest") || args[0] != "run" {
			t.Errorf("unexpected command: %s %v", name, args)
		}
		for _, a := range args {
			if out, ok := strings.CutPrefix(a, "--coverage.reportsDirectory="); ok {
				return nil, os.WriteFile(filepath.Join(out, "lcov.info"),
					[]byte("SF:src/App.tsx\nDA:3,1\nDA:4,0\nend_of_record\n"), 0o644)
			}
		}
		return nil, errors.New("no reports dir arg")
	}
	profile, _, err := NewNodeProvider(run, false, discardLog()).Run(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	app := profile["src/App.tsx"]
	if app == nil || app.Hits[3] != 1 || app.Hits[4] != 0 {
		t.Errorf("lcov profile wrong: %+v", profile)
	}
}

func TestBuiltinProvidersUnknownSkipped(t *testing.T) {
	ps := BuiltinProviders([]string{"go", "nonsense", "node"}, nil, Options{}, discardLog())
	if len(ps) != 2 || ps[0].Name() != "go" || ps[1].Name() != "node" {
		t.Fatalf("unknown provider must be skipped: %v", ps)
	}
}

func TestProviderCovers(t *testing.T) {
	goP := NewGoProvider(nil, discardLog())
	nodeP := NewNodeProvider(nil, false, discardLog())
	cases := []struct {
		path       string
		goC, nodeC bool
	}{
		{"pkg/x.go", true, false},
		{"web/App.tsx", false, true},
		{"web/util.mjs", false, true},
		{"README.md", false, false},
		{"schema.sql", false, false},
	}
	for _, c := range cases {
		if got := goP.Covers(c.path); got != c.goC {
			t.Errorf("go.Covers(%q) = %v, want %v", c.path, got, c.goC)
		}
		if got := nodeP.Covers(c.path); got != c.nodeC {
			t.Errorf("node.Covers(%q) = %v, want %v", c.path, got, c.nodeC)
		}
	}
}
