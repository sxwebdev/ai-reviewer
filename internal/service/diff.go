package service

import (
	"strings"

	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/review"
	"github.com/sxwebdev/ai-reviewer/internal/toolchain"
	"github.com/tkcrm/mx/logger"
)

// parseDiffs converts GitLab's changed-file list into engine FileDiffs.
//
// This is the gate that enforces "binary, vendored and generated files never
// reach the LLM, and neither do paths matching review.ignore_globs". Everything
// it drops is dropped before any prompt is built:
//
//   - generated_file: GitLab's own verdict (.gitattributes linguist-generated);
//   - a binary diff, which carries no text to reason about;
//   - vendored paths (see isVendored) — third-party code we do not review;
//   - paths the operator excluded through review.ignore_globs;
//   - files whose hunks do not parse, and files with no hunks at all (a pure
//     mode or rename change has nothing to comment on).
//
// ignoreGlobs is enforced here rather than at the prompt builders because this
// is the only chokepoint that also removes the file from the finding-eligible
// set: an excluded path must be unable to receive a published comment, not
// merely be absent from one prompt section. Both sides of a rename are checked —
// moving a file out of an excluded directory in the same MR that changes it
// would otherwise sidestep the rule.
//
// An empty result is the caller's error to report: a review with no reviewable
// files must not silently produce an empty run.
func parseDiffs(diffs []gitlab.MergeRequestDiff, ignoreGlobs []string, log logger.Logger) []*review.FileDiff {
	var files []*review.FileDiff
	for _, d := range diffs {
		path := d.NewPath
		if path == "" {
			path = d.OldPath
		}
		if d.GeneratedFile || review.IsBinaryDiff(d.Diff) || isVendored(path) {
			continue
		}
		if ignored(path, ignoreGlobs) || (d.OldPath != path && ignored(d.OldPath, ignoreGlobs)) {
			log.Debugw("file excluded by review.ignore_globs", "path", path)
			continue
		}
		hunks, err := review.ParseHunks(d.Diff)
		if err != nil {
			log.Warnw("parse diff failed", "path", path, "err", err)
			continue
		}
		if len(hunks) == 0 {
			continue
		}
		files = append(files, &review.FileDiff{
			OldPath: d.OldPath, NewPath: d.NewPath, NewFile: d.NewFile,
			Renamed: d.RenamedFile, Deleted: d.DeletedFile, Hunks: hunks,
		})
	}
	return files
}

// ignored reports whether a path is excluded by review.ignore_globs. An empty
// path never matches: a rename with no old side is one path, not two.
func ignored(path string, globs []string) bool {
	if path == "" || len(globs) == 0 {
		return false
	}
	return toolchain.MatchGlob(path, globs)
}

// vendorPrefixes are directory roots whose contents are somebody else's code.
var vendorPrefixes = []string{"vendor/", "node_modules/", "dist/", "build/", "third_party/"}

// isVendored reports whether a path holds vendored or machine-written code that
// must never be sent to the model. The suffix rules catch the two generated
// artefacts that routinely live outside a vendor directory.
func isVendored(path string) bool {
	for _, p := range vendorPrefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return strings.HasSuffix(path, ".pb.go") || strings.HasSuffix(path, ".min.js")
}
