package llm

import (
	"encoding/json"
	"errors"
)

// ErrNoJSON is returned when no balanced, valid JSON object can be found.
var ErrNoJSON = errors.New("no JSON object found in output")

// ExtractJSONObject returns the first balanced, VALID top-level {...} object in
// s. It tries each '{' as a candidate start (skipping braces inside string
// literals when balancing) and returns the first candidate that is valid JSON.
// This is the belt-and-suspenders fallback for when structured output is
// unavailable and the model wraps its JSON in prose, code fences, or text that
// itself contains stray braces (e.g. "use {curly}" before the real object).
func ExtractJSONObject(s string) (string, error) {
	for i := 0; i < len(s); i++ {
		if s[i] != '{' {
			continue
		}
		if cand, ok := balancedObject(s, i); ok && json.Valid([]byte(cand)) {
			return cand, nil
		}
	}
	return "", ErrNoJSON
}

// balancedObject returns the substring from start to the matching closing brace,
// correctly skipping braces inside string literals. ok is false if the braces
// never balance.
func balancedObject(s string, start int) (string, bool) {
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1], true
			}
		}
	}
	return "", false
}
