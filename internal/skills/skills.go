// Package skills discovers Claude Code "skills" installed on the machine so the
// web UI can offer them per review. A skill is a directory containing a
// SKILL.md file with YAML frontmatter (name + description). Discovery is pure
// filesystem I/O with no dependency on the claude CLI, which keeps it
// unit-testable against a temp dir.
package skills

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Skill is one installed skill offered to the reviewer.
type Skill struct {
	Name        string // invocation name (frontmatter name, or the directory name)
	Description string // frontmatter description (may be empty)
	Source      string // label of the source root it was found under (e.g. "user", "project")
	Path        string // absolute path to the SKILL.md file
}

// Source is a directory to scan for skills, with a human label recorded on each
// discovered skill.
type Source struct {
	Label string
	Dir   string
}

// UserDir returns the per-user skills directory (~/.claude/skills). It returns
// "" when the home directory cannot be resolved.
func UserDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "skills")
}

// ProjectDir returns the project-local skills directory for a working copy
// (<workDir>/.claude/skills), or "" when workDir is empty.
func ProjectDir(workDir string) string {
	if workDir == "" {
		return ""
	}
	return filepath.Join(workDir, ".claude", "skills")
}

// Discover scans each source directory for immediate subdirectories containing a
// SKILL.md, returning the skills sorted by name. When the same skill name
// appears in more than one source, the first source listed wins (so callers
// should list higher-precedence sources first). Missing/unreadable directories
// are skipped silently — discovery is best-effort.
func Discover(sources []Source) []Skill {
	seen := map[string]bool{}
	var out []Skill
	for _, src := range sources {
		if src.Dir == "" {
			continue
		}
		entries, err := os.ReadDir(src.Dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			mdPath := filepath.Join(src.Dir, e.Name(), "SKILL.md")
			info, err := os.Stat(mdPath)
			if err != nil || info.IsDir() {
				continue
			}
			name, desc := parseFrontmatter(mdPath)
			if name == "" {
				name = e.Name()
			}
			if seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, Skill{Name: name, Description: desc, Source: src.Label, Path: mdPath})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// parseFrontmatter reads a SKILL.md's leading `---`-delimited YAML block and
// extracts the name and description keys. It intentionally does a minimal
// line-based parse (only the two scalar keys we need) rather than pulling in a
// YAML dependency; unknown or multi-line values are ignored.
func parseFrontmatter(path string) (name, description string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close() //nolint:errcheck

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	if !sc.Scan() || strings.TrimSpace(sc.Text()) != "---" {
		return "", "" // no frontmatter block
	}
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "---" {
			break
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "name":
			name = unquote(strings.TrimSpace(val))
		case "description":
			description = blockScalarSafe(unquote(strings.TrimSpace(val)))
		}
	}
	return name, description
}

// blockScalarSafe returns "" for a YAML block-scalar indicator ("|" or ">",
// optionally with a chomping/indent modifier). This minimal parser only reads
// single-line values, so it cannot capture the following indented lines of a
// block scalar; returning "" is better than surfacing the bare "|"/">" marker
// as the description.
func blockScalarSafe(s string) string {
	if s == "" {
		return ""
	}
	if s[0] == '|' || s[0] == '>' {
		return ""
	}
	return s
}

// unquote strips a single matching pair of surrounding quotes.
func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
