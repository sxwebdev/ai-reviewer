package coverage

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sxwebdev/ai-reviewer/internal/security"
	"github.com/sxwebdev/ai-reviewer/internal/toolchain"
)

// nodeProvider measures JS/TS coverage via the repository's own test runner
// (vitest or jest) with an lcov reporter. It executes repository code
// (node_modules binaries, test files) — the whole coverage feature is opt-in
// for exactly this reason; dependency installation is a second opt-in.
type nodeProvider struct {
	run     Runner
	install bool
	log     *slog.Logger
}

// NewNodeProvider builds the node coverage provider.
func NewNodeProvider(run Runner, install bool, log *slog.Logger) Provider {
	return &nodeProvider{run: run, install: install, log: log}
}

func (p *nodeProvider) Name() string      { return "node" }
func (p *nodeProvider) Markers() []string { return toolchain.NodeMarkers }

// nodeSourceExts are the extensions the node test runners can instrument.
var nodeSourceExts = []string{".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs", ".mts", ".cts"}

func (p *nodeProvider) Covers(path string) bool {
	for _, ext := range nodeSourceExts {
		if strings.HasSuffix(path, ext) {
			return true
		}
	}
	return false
}

func (p *nodeProvider) Detect(root string) bool {
	return detectNodeRunner(root) != ""
}

// detectNodeRunner inspects package.json for vitest/jest in dependencies or
// devDependencies, falling back to a scripts.test mention. Returns
// "vitest" | "jest" | "".
func detectNodeRunner(root string) string {
	data, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		return ""
	}
	var pkg struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
		Scripts         map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return ""
	}
	for _, runner := range []string{"vitest", "jest"} {
		if _, ok := pkg.DevDependencies[runner]; ok {
			return runner
		}
		if _, ok := pkg.Dependencies[runner]; ok {
			return runner
		}
	}
	test := pkg.Scripts["test"]
	switch {
	case strings.Contains(test, "vitest"):
		return "vitest"
	case strings.Contains(test, "jest"):
		return "jest"
	}
	return ""
}

func (p *nodeProvider) Run(ctx context.Context, root string) (Profile, string, error) {
	runner := detectNodeRunner(root)
	if runner == "" {
		return nil, "", fmt.Errorf("no supported test runner (vitest/jest) detected")
	}

	if _, err := os.Stat(filepath.Join(root, "node_modules")); err != nil {
		if !p.install {
			return nil, "", fmt.Errorf("node_modules missing (set review.coverage.node.install: true or install manually)")
		}
		if err := p.installDeps(ctx, root); err != nil {
			return nil, "", err
		}
	}

	bin := filepath.Join(root, "node_modules", ".bin", runner)
	if _, err := os.Stat(bin); err != nil {
		return nil, "", fmt.Errorf("%s binary not found in node_modules/.bin", runner)
	}

	tmp, err := os.MkdirTemp("", "ai-reviewer-lcov-*")
	if err != nil {
		return nil, "", err
	}
	defer os.RemoveAll(tmp) //nolint:errcheck

	var args []string
	switch runner {
	case "vitest":
		args = []string{"run", "--coverage", "--coverage.reporter=lcov", "--coverage.reportsDirectory=" + tmp}
	case "jest":
		args = []string{"--coverage", "--coverageReporters=lcov", "--coverageDirectory=" + tmp, "--watchman=false", "--ci"}
	}
	out, runErr := p.run(ctx, root, []string{"CI=1"}, bin, args...)

	f, err := os.Open(filepath.Join(tmp, "lcov.info"))
	if err != nil {
		if runErr != nil {
			// Masked: persisted/UI-visible, and test output may carry secrets.
			return nil, "", fmt.Errorf("%s failed: %s", runner, firstLines(security.Mask(string(out)), 3))
		}
		return nil, "", fmt.Errorf("no lcov.info produced")
	}
	defer f.Close() //nolint:errcheck

	profile, err := ParseLCOV(f, root)
	if err != nil {
		return nil, "", err
	}
	note := ""
	if runErr != nil {
		note = "some tests failed; partial coverage profile used"
	}
	return profile, note, nil
}

// installDeps installs dependencies by lockfile (frozen, no updates). Requires
// the matching package manager on PATH.
func (p *nodeProvider) installDeps(ctx context.Context, root string) error {
	var name string
	var args []string
	switch {
	case exists(filepath.Join(root, "pnpm-lock.yaml")):
		name, args = "pnpm", []string{"install", "--frozen-lockfile"}
	case exists(filepath.Join(root, "yarn.lock")):
		name, args = "yarn", []string{"install", "--frozen-lockfile"}
	default:
		name, args = "npm", []string{"ci"}
	}
	if _, err := exec.LookPath(name); err != nil {
		return fmt.Errorf("%s not on PATH; cannot install dependencies", name)
	}
	p.log.Info("coverage: installing node dependencies", "root", root, "manager", name)
	if out, err := p.run(ctx, root, []string{"CI=1"}, name, args...); err != nil {
		return fmt.Errorf("%s install failed: %s", name, firstLines(security.Mask(string(out)), 3))
	}
	return nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
