package review

import (
	"regexp"
	"sort"
	"strings"
)

// identifierRe matches language-agnostic identifier tokens worth searching
// for: at least 4 chars so short keywords and noise words drop out.
var identifierRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]{3,}`)

// identifierStopwords are keywords and ubiquitous tokens across common
// languages that carry no search signal.
var identifierStopwords = map[string]bool{
	// Go / general keywords
	"func": true, "return": true, "package": true, "import": true, "type": true,
	"struct": true, "interface": true, "range": true, "defer": true, "select": true,
	"const": true, "continue": true, "break": true, "switch": true, "case": true,
	"default": true, "else": true, "goto": true, "chan": true, "false": true,
	"true": true, "nil": true, "null": true, "none": true, "self": true, "this": true,
	// other languages
	"function": true, "class": true, "public": true, "private": true, "protected": true,
	"static": true, "void": true, "string": true, "number": true, "boolean": true,
	"async": true, "await": true, "yield": true, "lambda": true,
	"elif": true, "except": true, "finally": true, "raise": true, "pass": true,
	"catch": true, "throw": true, "throws": true, "final": true, "extends": true,
	"implements": true, "instanceof": true, "typeof": true, "undefined": true,
	// ubiquitous nouns/idents
	"error": true, "value": true, "result": true, "data": true, "name": true,
	"count": true, "index": true, "item": true, "items": true, "list": true,
	"context": true, "test": true, "tests": true, "main": true, "print": true,
	"println": true, "printf": true, "sprintf": true, "errorf": true, "format": true,
	"append": true, "make": true, "delete": true, "copy": true,
}

// ExtractIdentifiers pulls the most promising identifiers from the changed
// (added/removed) lines of the diffs, ranked by frequency, capped at max.
// Language-agnostic by design: regex tokens minus a keyword stoplist.
func ExtractIdentifiers(files []*FileDiff, max int) []string {
	freq := map[string]int{}
	for _, f := range files {
		for _, h := range f.Hunks {
			for _, l := range h.Lines {
				if l.Kind == LineContext {
					continue
				}
				for _, tok := range identifierRe.FindAllString(l.Content, -1) {
					if identifierStopwords[strings.ToLower(tok)] {
						continue
					}
					freq[tok]++
				}
			}
		}
	}
	out := make([]string, 0, len(freq))
	for tok := range freq {
		out = append(out, tok)
	}
	sort.Slice(out, func(i, j int) bool {
		if freq[out[i]] != freq[out[j]] {
			return freq[out[i]] > freq[out[j]]
		}
		return out[i] < out[j]
	})
	if max > 0 && len(out) > max {
		out = out[:max]
	}
	return out
}
