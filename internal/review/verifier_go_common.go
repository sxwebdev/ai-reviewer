package review

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/sxwebdev/ai-reviewer/internal/toolchain"
)

// goToolchainFor locates the go binary and the finding's NEAREST module root
// (monorepo-aware — the module may live in a subdirectory, not the worktree
// top). ok=false is the conservative Keep path for all Go verifiers.
func goToolchainFor(workDir, filePath string) (goBin, rootRel string, ok bool) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		return "", "", false
	}
	rootRel, found := toolchain.NearestRoot(workDir, filePath, toolchain.GoMarkers)
	if !found {
		return "", "", false
	}
	return goBin, rootRel, true
}

// goPkgDirWithin returns filePath's package directory relative to the module
// root ("./..."-style path used with cmd.Dir = the root).
func goPkgDirWithin(rootRel, filePath string) string {
	rel := filePath
	if rootRel != "." {
		rel = strings.TrimPrefix(filePath, rootRel+"/")
	}
	return path.Dir(rel)
}

// runGoCmd runs a go subcommand with cmd.Dir at the module root, bounded by
// timeout, returning combined output and success.
func runGoCmd(ctx context.Context, timeout time.Duration, goBin, workDir, rootRel string, args ...string) ([]byte, bool) {
	absRoot := filepath.Join(workDir, filepath.FromSlash(rootRel))
	if info, err := os.Stat(absRoot); err != nil || !info.IsDir() {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, goBin, args...)
	cmd.Dir = absRoot
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	return out, err == nil
}

// hasBuildConstraint reports whether the file's head carries a build
// constraint (//go:build or // +build). Such files may be excluded from the
// local build entirely, so a clean build proves nothing about them.
func hasBuildConstraint(workDir, filePath string) bool {
	f, err := os.Open(filepath.Join(workDir, filepath.FromSlash(filePath)))
	if err != nil {
		return false
	}
	defer f.Close() //nolint:errcheck
	head := make([]byte, 1024)
	n, _ := f.Read(head)
	for _, line := range bytes.Split(head[:n], []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if bytes.HasPrefix(trimmed, []byte("//go:build")) || bytes.HasPrefix(trimmed, []byte("// +build")) {
			return true
		}
		if bytes.HasPrefix(trimmed, []byte("package ")) {
			break // constraints must precede the package clause
		}
	}
	return false
}
