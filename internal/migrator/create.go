package migrator

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var namePartRe = regexp.MustCompile(`[^a-z0-9_]+`)

// Create writes a pair of empty NNNN_name.up.sql / NNNN_name.down.sql files
// into dir, picking the next sequential version number.
func Create(dir, name string) (string, error) {
	name = namePartRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "_")
	if name == "" {
		return "", fmt.Errorf("empty migration name")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("failed to read migrations dir: %w", err)
	}

	maxVersion := 0
	for _, e := range entries {
		match := migrationFileRe.FindStringSubmatch(e.Name())
		if match == nil {
			continue
		}
		v, err := strconv.Atoi(match[1])
		if err != nil {
			continue
		}
		if v > maxVersion {
			maxVersion = v
		}
	}

	base := fmt.Sprintf("%04d_%s", maxVersion+1, name)
	for _, suffix := range []string{".up.sql", ".down.sql"} {
		p := filepath.Join(dir, base+suffix)
		if err := os.WriteFile(p, []byte("-- "+base+suffix+"\n"), 0o644); err != nil {
			return "", fmt.Errorf("failed to create %s: %w", p, err)
		}
	}

	return base, nil
}
