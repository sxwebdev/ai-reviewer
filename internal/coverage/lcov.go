package coverage

import (
	"bufio"
	"io"
	"path/filepath"
	"strconv"
	"strings"
)

// ParseLCOV parses an lcov.info stream (SF:/DA:/end_of_record records) into a
// Profile keyed by root-relative paths. SF paths may be absolute or relative
// to root; files outside root (e.g. node_modules resolved elsewhere) are
// skipped. DA records form the line universe directly.
func ParseLCOV(r io.Reader, root string) (Profile, error) {
	p := Profile{}
	var cur *FileProfile

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "SF:"):
			cur = nil
			rel, ok := lcovRelPath(strings.TrimPrefix(line, "SF:"), root)
			if !ok {
				continue
			}
			cur = p[rel]
			if cur == nil {
				cur = &FileProfile{Hits: map[int]int{}}
				p[rel] = cur
			}
		case strings.HasPrefix(line, "DA:") && cur != nil:
			parts := strings.SplitN(strings.TrimPrefix(line, "DA:"), ",", 3)
			if len(parts) < 2 {
				continue
			}
			ln, err1 := strconv.Atoi(parts[0])
			hits, err2 := strconv.Atoi(parts[1])
			if err1 != nil || err2 != nil {
				continue
			}
			if prev, seen := cur.Hits[ln]; !seen || hits > prev {
				cur.Hits[ln] = hits
			}
		case line == "end_of_record":
			cur = nil
		}
	}
	return p, sc.Err()
}

// lcovRelPath normalizes an SF path (absolute or relative) to root-relative,
// rejecting paths outside root.
func lcovRelPath(sf, root string) (string, bool) {
	sf = filepath.FromSlash(strings.TrimSpace(sf))
	if !filepath.IsAbs(sf) {
		clean := filepath.ToSlash(filepath.Clean(sf))
		if strings.HasPrefix(clean, "../") {
			return "", false
		}
		return clean, true
	}
	rel, err := filepath.Rel(root, sf)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", false
	}
	return filepath.ToSlash(rel), true
}
