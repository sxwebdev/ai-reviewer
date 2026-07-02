package coverage

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sxwebdev/ai-reviewer/internal/security"
	"github.com/sxwebdev/ai-reviewer/internal/toolchain"
)

// goProvider measures Go coverage: `go test -coverprofile ./...` at the module
// root. Scope is deliberately the whole module (not "affected packages"): a
// changed function may be exercised by another package's tests, and the
// per-run timeout is the cost bound.
type goProvider struct {
	run Runner
	log *slog.Logger
}

// NewGoProvider builds the Go coverage provider.
func NewGoProvider(run Runner, log *slog.Logger) Provider {
	return &goProvider{run: run, log: log}
}

func (p *goProvider) Name() string      { return "go" }
func (p *goProvider) Markers() []string { return toolchain.GoMarkers }

func (p *goProvider) Covers(path string) bool { return strings.HasSuffix(path, ".go") }

func (p *goProvider) Detect(root string) bool {
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		return false
	}
	_, err := exec.LookPath("go")
	return err == nil
}

func (p *goProvider) Run(ctx context.Context, root string) (Profile, string, error) {
	tmp, err := os.MkdirTemp("", "ai-reviewer-cover-*")
	if err != nil {
		return nil, "", err
	}
	defer os.RemoveAll(tmp) //nolint:errcheck
	profileFile := filepath.Join(tmp, "cover.out")

	out, runErr := p.run(ctx, root, []string{"GOFLAGS=-mod=readonly"},
		"go", "test", "-count=1", "-coverprofile="+profileFile, "./...")

	f, err := os.Open(profileFile)
	if err != nil {
		// No profile at all: surface the test failure as the reason. Masked:
		// this string is persisted (coverage_json) and shown in the UI, and
		// repo tests may print environment/secrets on failure.
		if runErr != nil {
			return nil, "", fmt.Errorf("go test failed: %s", firstLines(security.Mask(string(out)), 3))
		}
		return nil, "", fmt.Errorf("no coverage profile produced")
	}
	defer f.Close() //nolint:errcheck

	modulePath, err := moduleImportPath(root)
	if err != nil {
		return nil, "", err
	}
	profile, err := ParseGoCoverProfile(f, modulePath)
	if err != nil {
		return nil, "", err
	}
	note := ""
	if runErr != nil {
		note = "some tests failed; partial coverage profile used"
	}
	return profile, note, nil
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, " | ")
}
