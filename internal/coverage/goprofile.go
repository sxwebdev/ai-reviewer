package coverage

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ParseGoCoverProfile parses Go cover profile lines of the form
//
//	import/path/file.go:startLine.startCol,endLine.endCol numStmt count
//
// expanding each block into per-line Hits entries (max of overlapping
// counts). File keys are import paths; modulePath maps them to root-relative
// paths. Files outside the module (nested modules, stdlib) are skipped.
func ParseGoCoverProfile(r io.Reader, modulePath string) (Profile, error) {
	p := Profile{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "mode:") {
			continue
		}
		colon := strings.LastIndexByte(line, ':')
		if colon < 0 {
			continue
		}
		file := line[:colon]
		rel, ok := strings.CutPrefix(file, modulePath+"/")
		if !ok {
			continue // outside this module
		}
		var sl, sc2, el, ec, stmts, count int
		if _, err := fmt.Sscanf(line[colon+1:], "%d.%d,%d.%d %d %d", &sl, &sc2, &el, &ec, &stmts, &count); err != nil {
			continue // tolerate malformed lines
		}
		fp := p[rel]
		if fp == nil {
			fp = &FileProfile{Hits: map[int]int{}}
			p[rel] = fp
		}
		for l := sl; l <= el; l++ {
			if cur, seen := fp.Hits[l]; !seen || count > cur {
				fp.Hits[l] = count
			}
		}
	}
	return p, sc.Err()
}

// moduleImportPath reads the module line from root/go.mod.
func moduleImportPath(root string) (string, error) {
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if mod, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.TrimSpace(mod), nil
		}
	}
	return "", fmt.Errorf("no module line in go.mod")
}
