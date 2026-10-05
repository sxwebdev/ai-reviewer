package dbtypes_test

import (
	"database/sql/driver"
	"strings"
	"testing"

	"github.com/sxwebdev/ai-reviewer/internal/dbtypes"
)

// These types are the single source of truth for what a status column may hold —
// the schema has no CHECK. Everything else (the store wrappers, the SQL-literal
// guard, the service constants) reads the sets from here, so a hole here is a
// hole everywhere.

func TestStatusSetsAreAccepted(t *testing.T) {
	for _, s := range dbtypes.ReviewStatuses() {
		assertAccepted(t, "ReviewStatus", s)
	}
	for _, s := range dbtypes.DigestRunStatuses() {
		assertAccepted(t, "DigestRunStatus", s)
	}
	for _, s := range dbtypes.MessageStatuses() {
		assertAccepted(t, "MessageStatus", s)
	}
}

func TestStatusRejectsAnythingElse(t *testing.T) {
	// The realistic mistakes: a typo, the empty value a zero struct carries, and
	// a value that is valid for a *different* column — the one a CHECK per table
	// used to catch and a shared "is it a known status" check never would.
	cases := []struct {
		name string
		call func() error
		bad  string
	}{
		{"review typo", func() error { return valueErr(dbtypes.ReviewStatus("reviewd")) }, "reviewd"},
		{"review empty", func() error { return valueErr(dbtypes.ReviewStatus("")) }, ""},
		{"review borrows a message status", func() error { return valueErr(dbtypes.ReviewStatus("pending")) }, "pending"},
		{"digest run borrows a message status", func() error { return valueErr(dbtypes.DigestRunStatus("sending")) }, "sending"},
		{"message borrows a run status", func() error { return valueErr(dbtypes.MessageStatus("built")) }, "built"},
		{"message borrows a review status", func() error { return valueErr(dbtypes.MessageStatus("succeeded")) }, "succeeded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatalf("Value() accepted %q — nothing downstream will reject it either", tc.bad)
			}
			// The message has to name the column: "invalid status" alone leaves an
			// operator guessing which of three tables refused the write.
			if !strings.Contains(err.Error(), "status") || !strings.Contains(err.Error(), tc.bad) {
				t.Errorf("error should name the column and the value, got: %v", err)
			}
		})
	}
}

// Reads are deliberately permissive: a value already stored is a fact, and
// failing to scan it would turn one bad row into a broken sweep.
func TestStatusScanIsPermissive(t *testing.T) {
	var s dbtypes.MessageStatus
	for _, src := range []any{"sent", []byte("sending"), nil} {
		if err := s.Scan(src); err != nil {
			t.Fatalf("Scan(%v): %v", src, err)
		}
	}
	if err := s.Scan(42); err == nil {
		t.Error("Scan accepted an int; a non-text column would be a schema change worth failing on")
	}

	if s := dbtypes.MessageStatus("nonsense"); s.Valid() {
		t.Error("Valid() accepted a value Value() rejects — the two must agree")
	}
}

func assertAccepted[T interface {
	~string
	Valid() bool
	Value() (driver.Value, error)
}](t *testing.T, kind string, s T) {
	t.Helper()
	if !s.Valid() {
		t.Errorf("%s(%q) is declared but Valid() says no", kind, string(s))
	}
	if err := valueErr(s); err != nil {
		t.Errorf("%s(%q) is declared but Value() rejects it: %v", kind, string(s), err)
	}
}

func valueErr(v interface{ Value() (driver.Value, error) }) error {
	_, err := v.Value()
	return err
}
