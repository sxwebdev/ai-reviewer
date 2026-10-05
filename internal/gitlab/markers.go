package gitlab

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Machine-readable HTML-comment markers embedded in the notes we publish.
// GitLab does not render HTML comments, so they are invisible to humans while
// remaining greppable by us.
//
// They are the *secondary* source of review state (Postgres is primary, plan
// §10.1): they cost nothing — written into a note we publish anyway, read from
// discussions we load anyway — and they mean that losing or rebuilding the
// database never republishes findings that are already sitting in GitLab.

// MarkerVersion is the only marker schema version this build understands.
// Markers of any other version are ignored, never an error: a newer ai-reviewer
// must be able to write markers that an older one merely skips.
const MarkerVersion = 1

var (
	// The payload is emitted as one line of compact JSON, so `.` (which does
	// not match newlines) is the right thing to scan with, and non-greedy
	// matching keeps two markers on one line from merging into one match.
	reviewMarkerRe  = regexp.MustCompile(`<!--\s*ai-reviewer:review:v(\d+)\s+(\{.*?\})\s*-->`)
	findingMarkerRe = regexp.MustCompile(`<!--\s*ai-reviewer:finding:v(\d+)\s+fp=([0-9a-fA-F]+)\s*-->`)
)

// ReviewMarker is the payload of the summary note's marker. It records that a
// complete review of one head SHA landed in GitLab.
type ReviewMarker struct {
	V          int       `json:"v"`
	ProjectID  int64     `json:"project_id"`
	MRIID      int64     `json:"mr_iid"`
	HeadSHA    string    `json:"head_sha"`
	ReviewedAt time.Time `json:"reviewed_at"`
	Findings   int       `json:"findings"`
	Pipeline   string    `json:"pipeline"`
	Tool       string    `json:"tool"`
}

// Render returns the marker comment for m. Output is deterministic: the version
// is forced to MarkerVersion and the timestamp is normalized to whole seconds
// in UTC, so the same review always produces byte-identical text.
func (m ReviewMarker) Render() string {
	m.V = MarkerVersion
	m.ReviewedAt = m.ReviewedAt.UTC().Truncate(time.Second)
	payload, err := json.Marshal(m)
	if err != nil {
		// Unreachable: every field is a JSON-native scalar. Degrade to no
		// marker rather than blocking publication of a real review.
		return ""
	}
	return fmt.Sprintf("<!-- ai-reviewer:review:v%d %s -->", MarkerVersion, payload)
}

// RenderFindingMarker returns the marker comment identifying a published
// finding by its fingerprint (review.Fingerprint — head-SHA independent, which
// is what keeps a finding from being reposted after every push). An empty
// fingerprint renders nothing.
func RenderFindingMarker(fingerprint string) string {
	fp := strings.ToLower(strings.TrimSpace(fingerprint))
	if fp == "" {
		return ""
	}
	return fmt.Sprintf("<!-- ai-reviewer:finding:v%d fp=%s -->", MarkerVersion, fp)
}

// ParseReviewMarker extracts the last well-formed v1 review marker from a note
// body. Unknown versions, malformed JSON and payloads whose inner "v" disagrees
// with the tag are skipped silently — a marker we cannot read must never break
// a run.
func ParseReviewMarker(body string) (ReviewMarker, bool) {
	var (
		found bool
		out   ReviewMarker
	)
	for _, m := range reviewMarkerRe.FindAllStringSubmatch(body, -1) {
		v, err := strconv.Atoi(m[1])
		if err != nil || v != MarkerVersion {
			continue
		}
		var parsed ReviewMarker
		if err := json.Unmarshal([]byte(m[2]), &parsed); err != nil {
			continue
		}
		if parsed.V != MarkerVersion {
			continue
		}
		out, found = parsed, true
	}
	return out, found
}

// ParseFindingMarkers returns the fingerprints of every v1 finding marker in a
// note body, lowercased to match the hex produced by review.Fingerprint.
func ParseFindingMarkers(body string) []string {
	var out []string
	for _, m := range findingMarkerRe.FindAllStringSubmatch(body, -1) {
		v, err := strconv.Atoi(m[1])
		if err != nil || v != MarkerVersion {
			continue
		}
		out = append(out, strings.ToLower(m[2]))
	}
	return out
}

// MarkerScan is what one pass over an MR's discussions yields.
type MarkerScan struct {
	// Review is the most recent review marker, or nil if the MR has never been
	// reviewed by this tool (or only by an incompatible version).
	Review *ReviewMarker
	// Fingerprints are the findings already published to this MR. Union it
	// with the database's published fingerprints to build
	// review.ReviewInput.ExistingFingerprints.
	Fingerprints map[string]struct{}
}

// HasFingerprint reports whether a finding with this fingerprint is already
// posted on the MR.
func (s MarkerScan) HasFingerprint(fingerprint string) bool {
	_, ok := s.Fingerprints[strings.ToLower(strings.TrimSpace(fingerprint))]
	return ok
}

// ScanDiscussions collects our markers from an MR's discussions in a single
// pass — the discussions are already loaded for thread counting and prompt
// context, so this costs no extra request.
//
// "Newest" review marker means the greatest reviewed_at; ties fall back to
// document order (GitLab returns discussions and their notes oldest-first), so
// the result is deterministic even for markers written within the same second.
func ScanDiscussions(discussions []Discussion) MarkerScan {
	scan := MarkerScan{Fingerprints: map[string]struct{}{}}
	for _, d := range discussions {
		for _, n := range d.Notes {
			if n.Body == "" {
				continue
			}
			for _, fp := range ParseFindingMarkers(n.Body) {
				scan.Fingerprints[fp] = struct{}{}
			}
			if m, ok := ParseReviewMarker(n.Body); ok {
				if scan.Review == nil || !m.ReviewedAt.Before(scan.Review.ReviewedAt) {
					scan.Review = &m
				}
			}
		}
	}
	return scan
}
