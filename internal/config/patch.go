package config

import (
	"bytes"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
)

// PatchFile applies string overrides addressed by dotted keys (for example
// "gitlab.host") to the YAML config at path, preserving comments, key order,
// and unrelated keys. If the file does not exist it is created from the
// documented default template first, so a user who only ever configures via
// the web UI still ends up with a fully commented config. The write is atomic
// (temp file in the same directory + rename) and the file stays 0600.
//
// Values may be secrets: they must never appear in returned errors.
func PatchFile(path string, values map[string]string) error {
	return patchFile(path, values, nil)
}

// PatchFileMixed is like PatchFile but writes the keys named in rawKeys as bare
// YAML scalars (no surrounding quotes), for non-string fields such as booleans
// and numbers — a quoted "true" fails to unmarshal into a Go bool field. All
// other keys are quoted as usual. Doing both in one pass keeps the write atomic
// (a single read-modify-rename). Raw values must be plain literals and are never
// secrets.
//
// List-valued fields are also written via rawKeys: format them with
// FormatYAMLList (a YAML flow sequence such as ["a", "b"]) and mark the key raw
// so the sequence is emitted verbatim, replacing any existing block sequence.
func PatchFileMixed(path string, values map[string]string, rawKeys map[string]bool) error {
	return patchFile(path, values, rawKeys)
}

// FormatYAMLList renders items as a YAML flow sequence, e.g. `["a", "b"]`; an
// empty list renders as `[]`. Each item is double-quoted. Pass the result as a
// rawKeys value to PatchFileMixed to overwrite a list-valued config field.
func FormatYAMLList(items []string) string {
	if len(items) == 0 {
		return "[]"
	}
	parts := make([]string, len(items))
	for i, s := range items {
		parts[i] = strconv.Quote(s)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func patchFile(path string, values map[string]string, rawKeys map[string]bool) error {
	if len(values) == 0 {
		return nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if werr := WriteDefaultFile(path); werr != nil {
			return werr
		}
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return fmt.Errorf("read config %s: %w", path, err)
	}
	// An existing-but-empty file has no document node to patch into; seed it
	// with the commented template just like the missing-file case.
	if len(bytes.TrimSpace(data)) == 0 {
		data = []byte(defaultYAML)
	}

	file, err := parser.ParseBytes(data, parser.ParseComments)
	if err != nil {
		return fmt.Errorf("parse config %s: %w", path, err)
	}

	// Deterministic order so created-key output is reproducible.
	for _, key := range slices.Sorted(maps.Keys(values)) {
		if err := setYAMLValue(file, key, values[key], rawKeys[key]); err != nil {
			return fmt.Errorf("set %s in config %s: %w", key, path, err)
		}
	}

	out := []byte(file.String())
	if !bytes.HasSuffix(out, []byte("\n")) {
		out = append(out, '\n')
	}
	return writeFileAtomic(path, out, 0o600)
}

// setYAMLValue sets dottedKey to a double-quoted string scalar. Existing nodes
// are replaced in place; missing keys are merged into the deepest existing
// ancestor mapping (ReplaceWithReader silently no-ops on missing paths, so
// existence is checked explicitly).
func setYAMLValue(file *ast.File, dottedKey, value string, raw bool) error {
	quoted := strconv.Quote(value)
	if raw {
		quoted = value
	}
	full, err := yaml.PathString("$." + dottedKey)
	if err != nil {
		return fmt.Errorf("bad key: %w", err)
	}
	if _, err := full.FilterFile(file); err == nil {
		return full.ReplaceWithReader(file, strings.NewReader(quoted))
	} else if !yaml.IsNotFoundNodeError(err) {
		return err
	}

	parts := strings.Split(dottedKey, ".")
	for i := len(parts) - 1; i >= 0; i-- {
		ancestor := "$"
		if i > 0 {
			ancestor = "$." + strings.Join(parts[:i], ".")
		}
		p, err := yaml.PathString(ancestor)
		if err != nil {
			return fmt.Errorf("bad key: %w", err)
		}
		if _, err := p.FilterFile(file); err != nil {
			if yaml.IsNotFoundNodeError(err) {
				continue
			}
			return err
		}
		snippet := yamlSnippet(parts[i:], quoted)
		return p.MergeFromReader(file, strings.NewReader(snippet))
	}
	return fmt.Errorf("no mapping to merge into")
}

// yamlSnippet renders nested keys as a small YAML document, e.g.
// keys=[pipeline mode] value=`"deep"` → "pipeline:\n  mode: \"deep\"".
func yamlSnippet(keys []string, quotedValue string) string {
	var b strings.Builder
	for i, k := range keys {
		indent := strings.Repeat("  ", i)
		if i == len(keys)-1 {
			fmt.Fprintf(&b, "%s%s: %s\n", indent, k, quotedValue)
		} else {
			fmt.Fprintf(&b, "%s%s:\n", indent, k)
		}
	}
	return b.String()
}

// writeFileAtomic writes data to path via a temp file + rename so a crash
// mid-write never truncates the config.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
