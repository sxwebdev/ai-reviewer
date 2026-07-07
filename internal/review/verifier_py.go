package review

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const pyVerifyTimeout = 15 * time.Second

// pySyntaxFailurePhrases mark a finding as asserting a Python syntax/parse
// failure. Bilingual.
var pySyntaxFailurePhrases = []string{
	"syntax error", "syntaxerror", "invalid syntax", "does not parse",
	"will not parse", "won't parse", "parse error", "indentationerror",
	"is not valid python", "invalid python",
	"синтаксическая ошибка", "не парсится", "невалидный python", "не валидный python",
}

func claimsPySyntaxFailure(f ValidatedFinding) bool {
	hay := strings.ToLower(f.Title + "\n" + f.Body)
	for _, p := range pySyntaxFailurePhrases {
		if strings.Contains(hay, p) {
			return true
		}
	}
	return false
}

// pySyntaxVerifier refutes false "syntax error" claims about Python files via
// ast.parse — a pure parser that executes nothing from the repository and
// writes nothing into the read-only worktree (deliberately NOT py_compile,
// which creates __pycache__). Safe as a default-on verifier.
type pySyntaxVerifier struct {
	log     *slog.Logger
	results map[string]int // file -> 0 parses, 1 syntax error, review lifetime
}

func newPySyntaxVerifier(log *slog.Logger) *pySyntaxVerifier {
	return &pySyntaxVerifier{log: log, results: map[string]int{}}
}

func (v *pySyntaxVerifier) Name() string { return "py_syntax" }

func (v *pySyntaxVerifier) Applies(f ValidatedFinding) bool {
	return strings.HasSuffix(strings.ToLower(f.FilePath), ".py") && claimsPySyntaxFailure(f)
}

const pyParseSnippet = `import ast, sys
src = open(sys.argv[1], "rb").read()
try:
    ast.parse(src, sys.argv[1])
except SyntaxError:
    sys.exit(3)
`

func (v *pySyntaxVerifier) Verify(ctx context.Context, workDir string, f ValidatedFinding) VerifierResult {
	verdict, cached := v.results[f.FilePath]
	if !cached {
		verdict = runPyParse(ctx, workDir, f.FilePath)
		v.results[f.FilePath] = verdict
	}
	switch verdict {
	case 0:
		return VerifierResult{Verdict: VerdictDrop,
			Note: "file parses cleanly under python3, contradicting the syntax-error claim"}
	case 1:
		return VerifierResult{Verdict: VerdictAnnotate, Note: "python3 confirms the file does not parse"}
	default:
		return VerifierResult{Verdict: VerdictKeep} // environmental
	}
}

// runPyParse returns 0 (parses), 1 (syntax error), or -1 (cannot judge).
func runPyParse(ctx context.Context, workDir, filePath string) int {
	pyBin, err := exec.LookPath("python3")
	if err != nil {
		if pyBin, err = exec.LookPath("python"); err != nil {
			return -1
		}
	}
	abs := filepath.Join(workDir, filepath.FromSlash(filePath))
	if info, err := os.Stat(abs); err != nil || info.IsDir() {
		return -1
	}
	ctx, cancel := context.WithTimeout(ctx, pyVerifyTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, pyBin, "-c", pyParseSnippet, abs)
	cmd.Env = os.Environ()
	err = cmd.Run()
	if ctx.Err() != nil {
		return -1
	}
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 3 {
		return 1
	}
	return -1
}
