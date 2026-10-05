package gitlab

import (
	"strings"
	"testing"
	"time"
)

func TestReviewMarkerRoundTrip(t *testing.T) {
	t.Parallel()
	m := ReviewMarker{
		V:          MarkerVersion,
		ProjectID:  123,
		MRIID:      456,
		HeadSHA:    "abcdef0123456789abcdef0123456789abcdef01",
		ReviewedAt: time.Date(2026, 8, 13, 9, 12, 44, 0, time.UTC),
		Findings:   3,
		Pipeline:   "standard",
		Tool:       "1.4.0",
	}

	rendered := m.Render()
	want := `<!-- ai-reviewer:review:v1 {"v":1,"project_id":123,"mr_iid":456,` +
		`"head_sha":"abcdef0123456789abcdef0123456789abcdef01",` +
		`"reviewed_at":"2026-08-13T09:12:44Z","findings":3,"pipeline":"standard","tool":"1.4.0"} -->`
	if rendered != want {
		t.Errorf("render mismatch\n got: %s\nwant: %s", rendered, want)
	}

	body := "🤖 **AI review** — 3 finding(s), risk: medium\n\n" + rendered
	got, ok := ParseReviewMarker(body)
	if !ok {
		t.Fatal("marker not found in a rendered note body")
	}
	if got != m {
		t.Errorf("round trip changed the payload:\n got %+v\nwant %+v", got, m)
	}
}

// Render must be deterministic: the same review always yields the same bytes,
// whatever timezone or sub-second precision the clock hands us.
func TestReviewMarkerRenderNormalizesTime(t *testing.T) {
	t.Parallel()
	msk := time.FixedZone("MSK", 3*60*60)
	a := ReviewMarker{ReviewedAt: time.Date(2026, 8, 13, 12, 12, 44, 987_000_000, msk)}
	b := ReviewMarker{ReviewedAt: time.Date(2026, 8, 13, 9, 12, 44, 0, time.UTC)}
	if a.Render() != b.Render() {
		t.Errorf("not deterministic:\n%s\n%s", a.Render(), b.Render())
	}
	if !strings.Contains(a.Render(), `"reviewed_at":"2026-08-13T09:12:44Z"`) {
		t.Errorf("timestamp not normalized to UTC seconds: %s", a.Render())
	}
	// The version is stamped by Render, never taken from the caller.
	if !strings.Contains(ReviewMarker{V: 7}.Render(), `"v":1`) {
		t.Error("Render must force the current marker version")
	}
}

func TestParseReviewMarkerIgnoresJunk(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
	}{
		{name: "empty body", body: ""},
		{name: "no marker", body: "just a human comment about ai-reviewer"},
		{name: "unknown tag version", body: `<!-- ai-reviewer:review:v2 {"v":2,"project_id":1} -->`},
		{name: "unknown payload version", body: `<!-- ai-reviewer:review:v1 {"v":2,"project_id":1} -->`},
		{name: "malformed json", body: `<!-- ai-reviewer:review:v1 {"v":1,"project_id": -->`},
		{name: "truncated json", body: `<!-- ai-reviewer:review:v1 {"v":1,"mr_iid":} -->`},
		{name: "wrong payload types", body: `<!-- ai-reviewer:review:v1 {"v":1,"project_id":"not-a-number"} -->`},
		{name: "not our marker", body: `<!-- some-other-bot:review:v1 {"v":1} -->`},
		{name: "no payload", body: `<!-- ai-reviewer:review:v1 -->`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if m, ok := ParseReviewMarker(tc.body); ok {
				t.Errorf("parsed %+v, want ignored", m)
			}
		})
	}
}

// A malformed marker must not shadow a good one in the same body.
func TestParseReviewMarkerSkipsBrokenNeighbour(t *testing.T) {
	t.Parallel()
	good := ReviewMarker{V: 1, ProjectID: 1, MRIID: 2, HeadSHA: "aaa", Findings: 1}
	body := `<!-- ai-reviewer:review:v1 {"v":1,"broken -->` + "\n" + good.Render() +
		"\n" + `<!-- ai-reviewer:review:v9 {"v":9} -->`
	got, ok := ParseReviewMarker(body)
	if !ok {
		t.Fatal("valid marker not found")
	}
	if got.HeadSHA != "aaa" {
		t.Errorf("got %+v", got)
	}
}

func TestFindingMarkerRoundTrip(t *testing.T) {
	t.Parallel()
	const fp = "9f2c1ab34de55701aa"
	rendered := RenderFindingMarker(fp)
	if rendered != "<!-- ai-reviewer:finding:v1 fp=9f2c1ab34de55701aa -->" {
		t.Fatalf("render = %q", rendered)
	}

	body := "**[high/correctness] Possible connection leak**\n\nbody\n\n" + rendered
	got := ParseFindingMarkers(body)
	if len(got) != 1 || got[0] != fp {
		t.Errorf("parsed %v, want [%s]", got, fp)
	}

	if RenderFindingMarker("  ") != "" {
		t.Error("an empty fingerprint must render nothing")
	}
	// Fingerprints are lowercase hex on the wire and in the database; parsing
	// normalizes so a hand-edited marker still dedupes.
	if got := ParseFindingMarkers(`<!-- ai-reviewer:finding:v1 fp=ABCDEF01 -->`); len(got) != 1 || got[0] != "abcdef01" {
		t.Errorf("parsed %v, want [abcdef01]", got)
	}
	for _, body := range []string{
		`<!-- ai-reviewer:finding:v2 fp=abcdef01 -->`,
		`<!-- ai-reviewer:finding:v1 fp=zzzz -->`,
		`<!-- ai-reviewer:finding:v1 -->`,
	} {
		if got := ParseFindingMarkers(body); len(got) != 0 {
			t.Errorf("body %q parsed as %v, want ignored", body, got)
		}
	}
}

// The realistic shape: our inline threads, our summary notes, human replies and
// GitLab system notes all mixed together.
func TestScanDiscussions(t *testing.T) {
	t.Parallel()
	older := ReviewMarker{
		V: MarkerVersion, ProjectID: 123, MRIID: 456, HeadSHA: "old111",
		ReviewedAt: time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC), Findings: 1,
		Pipeline: "standard", Tool: "1.3.0",
	}
	newer := ReviewMarker{
		V: MarkerVersion, ProjectID: 123, MRIID: 456, HeadSHA: "new222",
		ReviewedAt: time.Date(2026, 8, 13, 9, 12, 44, 0, time.UTC), Findings: 2,
		Pipeline: "deep", Tool: "1.4.0",
	}

	discussions := []Discussion{
		{ID: "d0", IndividualNote: true, Notes: []Note{
			{ID: 1, System: true, Body: "assigned to @bob"},
		}},
		{ID: "d1", Notes: []Note{
			{ID: 2, Body: "**[high/correctness] Leak**\n\n" + RenderFindingMarker("aaa111"), Resolvable: true},
			{ID: 3, Body: "fixed, thanks", Author: User{Username: "author"}},
		}},
		{ID: "d2", Notes: []Note{
			{ID: 4, Body: "**[medium/security] Missing check**\n\n" + RenderFindingMarker("bbb222"), Resolvable: true},
		}},
		// The newest summary appears before the older one in document order, so
		// this only passes if reviewed_at decides.
		{ID: "d3", IndividualNote: true, Notes: []Note{
			{ID: 5, Body: "🤖 **AI review** — 2 finding(s)\n\n" + newer.Render()},
		}},
		{ID: "d4", IndividualNote: true, Notes: []Note{
			{ID: 6, Body: "🤖 **AI review** — 1 finding(s)\n\n" + older.Render()},
		}},
		// A third-party bot's marker-looking comment must not confuse us.
		{ID: "d5", IndividualNote: true, Notes: []Note{
			{ID: 7, Body: `<!-- other-bot:finding:v1 fp=ccc333 -->`},
		}},
	}

	scan := ScanDiscussions(discussions)
	if scan.Review == nil {
		t.Fatal("no review marker found")
	}
	if *scan.Review != newer {
		t.Errorf("newest marker not selected:\n got %+v\nwant %+v", *scan.Review, newer)
	}
	if len(scan.Fingerprints) != 2 {
		t.Fatalf("fingerprints = %v, want exactly the two published findings", scan.Fingerprints)
	}
	if !scan.HasFingerprint("aaa111") || !scan.HasFingerprint("BBB222") {
		t.Errorf("fingerprints = %v", scan.Fingerprints)
	}
	if scan.HasFingerprint("ccc333") {
		t.Error("another bot's marker must not count as ours")
	}
	if scan.HasFingerprint("dddd44") {
		t.Error("unknown fingerprint reported as published")
	}
}

// Same-second markers are a real possibility; the result must still be stable.
func TestScanDiscussionsTieBreaksOnDocumentOrder(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 8, 13, 9, 12, 44, 0, time.UTC)
	first := ReviewMarker{V: MarkerVersion, HeadSHA: "first", ReviewedAt: at}
	second := ReviewMarker{V: MarkerVersion, HeadSHA: "second", ReviewedAt: at}

	scan := ScanDiscussions([]Discussion{
		{ID: "d1", Notes: []Note{{ID: 1, Body: first.Render()}, {ID: 2, Body: second.Render()}}},
	})
	if scan.Review == nil || scan.Review.HeadSHA != "second" {
		t.Errorf("review = %+v, want the last note to win the tie", scan.Review)
	}
}

func TestScanDiscussionsEmpty(t *testing.T) {
	t.Parallel()
	scan := ScanDiscussions(nil)
	if scan.Review != nil {
		t.Errorf("review = %+v, want nil", scan.Review)
	}
	if scan.Fingerprints == nil {
		t.Error("fingerprint set must be usable without a nil check")
	}
	if scan.HasFingerprint("aaa") {
		t.Error("empty scan reported a fingerprint")
	}
}
