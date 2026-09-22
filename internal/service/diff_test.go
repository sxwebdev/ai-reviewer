package service

import (
	"slices"
	"testing"

	"github.com/tkcrm/mx/logger"

	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
)

func TestIsVendored(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path string
		want bool
	}{
		{"internal/service/review.go", false},
		{"vendor/github.com/x/y.go", true},
		{"node_modules/left-pad/index.js", true},
		{"dist/app.js", true},
		{"build/out.go", true},
		{"third_party/lib.c", true},
		{"api/v1/service.pb.go", true},
		{"web/static/jquery.min.js", true},
		// The prefixes anchor at the start: a nested vendor directory belongs
		// to the repository's own tree and is reviewed.
		{"cmd/vendor/main.go", false},
		{"docs/node_modules.md", false},
		{"pkg/protobuf.go", false},
		{"", false},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			t.Parallel()
			if got := isVendored(c.path); got != c.want {
				t.Errorf("isVendored(%q) = %v, want %v", c.path, got, c.want)
			}
		})
	}
}

// TestParseDiffsDropsUnreviewableFiles pins the invariant that binary, vendored
// and generated files never reach the LLM. Every excluded kind is fed alongside
// one reviewable file so the test fails if a filter is dropped rather than
// simply returning nothing.
func TestParseDiffsDropsUnreviewableFiles(t *testing.T) {
	t.Parallel()
	log := logger.ForTests(t)

	diffs := []gitlab.MergeRequestDiff{
		{NewPath: "main.go", Diff: sampleDiff},
		{NewPath: "generated.go", Diff: sampleDiff, GeneratedFile: true},
		{NewPath: "vendor/dep/dep.go", Diff: sampleDiff},
		{NewPath: "api.pb.go", Diff: sampleDiff},
		{NewPath: "logo.png", Diff: "Binary files a/logo.png and b/logo.png differ\n"},
		{NewPath: "broken.go", Diff: "@@ this is not a hunk header @@\n"},
		{NewPath: "renamed.go", OldPath: "old.go", RenamedFile: true, Diff: ""},
	}

	files := parseDiffs(diffs, nil, log)
	if len(files) != 1 {
		var got []string
		for _, f := range files {
			got = append(got, f.Path())
		}
		t.Fatalf("parseDiffs returned %d files (%v), want only main.go", len(files), got)
	}
	if files[0].Path() != "main.go" {
		t.Errorf("kept %q, want main.go", files[0].Path())
	}
	if len(files[0].Hunks) != 1 {
		t.Errorf("main.go has %d hunks, want 1", len(files[0].Hunks))
	}
}

func TestParseDiffsUsesOldPathWhenFileIsDeleted(t *testing.T) {
	t.Parallel()
	// A deleted vendored file has no new_path, so the vendor filter has to look
	// at old_path or it would send vendored code to the model.
	files := parseDiffs([]gitlab.MergeRequestDiff{
		{OldPath: "vendor/dep/dep.go", DeletedFile: true, Diff: sampleDiff},
	}, nil, logger.ForTests(t))
	if len(files) != 0 {
		t.Fatalf("parseDiffs kept %d files, want a deleted vendored file to be dropped", len(files))
	}
}

func TestParseDiffsEmptyInput(t *testing.T) {
	t.Parallel()
	if got := parseDiffs(nil, nil, logger.ForTests(t)); got != nil {
		t.Errorf("parseDiffs(nil) = %v, want nil", got)
	}
}

// TestParseDiffsHonoursIgnoreGlobs pins review.ignore_globs as the containment
// control README.md and CLAUDE.md say it is.
//
// It was plumbed config → app → service.Config and read nowhere, and the reason
// nobody noticed is that the hardcoded vendor list happens to overlap the
// default value. An operator who reacts to an incident by adding
// `infra/secrets/**` gets a setting the service accepts, doctor approves, and
// nothing enforces — worse than not offering the knob.
func TestParseDiffsHonoursIgnoreGlobs(t *testing.T) {
	t.Parallel()
	globs := []string{"infra/secrets/**", "**/*.pem", "db/*.sql"}

	files := parseDiffs([]gitlab.MergeRequestDiff{
		{NewPath: "main.go", Diff: sampleDiff},
		{NewPath: "infra/secrets/prod.env", Diff: sampleDiff},
		{NewPath: "deploy/tls/server.pem", Diff: sampleDiff},
		{NewPath: "db/schema.sql", Diff: sampleDiff},
		// A rename OUT of an excluded directory must not launder the file: the
		// old path is checked too.
		{OldPath: "infra/secrets/old.env", NewPath: "config/old.env", RenamedFile: true, Diff: sampleDiff},
		// …while db/ is only excluded at its own level, so a nested file stays.
		{NewPath: "db/migrations/0001.sql", Diff: sampleDiff},
	}, globs, logger.ForTests(t))

	var got []string
	for _, f := range files {
		got = append(got, f.Path())
	}
	want := []string{"main.go", "db/migrations/0001.sql"}
	if !slices.Equal(got, want) {
		t.Errorf("parseDiffs kept %v, want %v", got, want)
	}
}
