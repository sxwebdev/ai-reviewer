// Package security provides secret redaction for logs, job output, and any
// text that might reach the user or the LLM. Nothing in this codebase should
// log a token, and no private snippet should reach an external service without
// an explicit debug opt-in — this package is the single choke point for that.
package security

import (
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"
)

// placeholder is substituted for any detected secret.
const placeholder = "[REDACTED]"

// minLiteralLen guards against registering trivially short "secrets" (e.g. "")
// that would corrupt unrelated text.
const minLiteralLen = 6

// builtinPatterns match well-known secret shapes regardless of registration.
var builtinPatterns = []*regexp.Regexp{
	regexp.MustCompile(`glpat-[A-Za-z0-9_\-]{16,}`),                        // GitLab PAT
	regexp.MustCompile(`gldt-[A-Za-z0-9_\-]{16,}`),                         // GitLab deploy token
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{16,}`),                       // Anthropic API key
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]{12,}`),                // Bearer header
	regexp.MustCompile(`(?i)private-token:\s*[A-Za-z0-9._\-]{8,}`),         // GitLab header
	regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`), // email
}

// Redactor masks registered literal secrets and built-in secret patterns.
// It is safe for concurrent use.
type Redactor struct {
	mu       sync.RWMutex
	literals []string
	patterns []*regexp.Regexp
}

// NewRedactor returns a Redactor seeded with the built-in patterns.
func NewRedactor() *Redactor {
	return &Redactor{patterns: builtinPatterns}
}

// AddSecret registers an exact secret value (e.g. a resolved token) to mask.
// Values shorter than minLiteralLen are ignored to avoid corrupting text.
func (r *Redactor) AddSecret(secret string) {
	secret = strings.TrimSpace(secret)
	if len(secret) < minLiteralLen {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.literals {
		if existing == secret {
			return
		}
	}
	r.literals = append(r.literals, secret)
}

// Mask returns s with all known secrets replaced by the placeholder.
func (r *Redactor) Mask(s string) string {
	if s == "" {
		return s
	}
	r.mu.RLock()
	literals := r.literals
	patterns := r.patterns
	r.mu.RUnlock()

	for _, lit := range literals {
		if lit != "" {
			s = strings.ReplaceAll(s, lit, placeholder)
		}
	}
	for _, p := range patterns {
		s = p.ReplaceAllString(s, placeholder)
	}
	return s
}

// defaultRedactor is the process-wide instance used by the logging handler and
// helpers. Register secrets on it via RegisterSecret.
var defaultRedactor = NewRedactor()

// RegisterSecret adds a secret to the process-wide redactor.
func RegisterSecret(secret string) { defaultRedactor.AddSecret(secret) }

// Mask masks secrets using the process-wide redactor.
func Mask(s string) string { return defaultRedactor.Mask(s) }

// Default returns the process-wide redactor.
func Default() *Redactor { return defaultRedactor }

// Truncate shortens s to at most max bytes on a UTF-8 rune boundary,
// appending an ellipsis. The single home for output-hygiene truncation
// (comment bodies, prompt sections, subprocess error excerpts).
func Truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
