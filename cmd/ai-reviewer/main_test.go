package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/sxwebdev/ai-reviewer/internal/security"
)

// reportError prints the terminal error.
//
// It goes through the redactor explicitly: this is the one process-exit path
// that does not pass through the zap core, and every command's failure funnels
// into it. The realistic leaks are narrow — config errors carry no values, pgx
// redacts DSNs, doctor masks its own details — but "the only output with no
// choke point" is not a property worth keeping.
func reportError(w io.Writer, err error) {
	_, _ = fmt.Fprintln(w, "error:", security.Mask(err.Error()))
}

// TestReportErrorRedacts covers the one output path outside the zap core. Every
// command's failure exits through it, so an error text carrying a token would
// land on the terminal — and from there in a ticket — unmasked.
func TestReportErrorRedacts(t *testing.T) {
	const token = "glpat-abcdefghijklmnopqrst"
	security.RegisterSecret("s3cr3t-registered-value")

	cases := []struct {
		name string
		err  error
		// leak is the substring that must not appear.
		leak string
	}{
		{"a pattern the redactor knows", errors.New("GET /user failed with " + token), token},
		{"a registered secret", fmt.Errorf("connect: password %s rejected", "s3cr3t-registered-value"), "s3cr3t-registered-value"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf strings.Builder
			reportError(&buf, tc.err)

			if strings.Contains(buf.String(), tc.leak) {
				t.Fatalf("the secret reached stderr: %s", buf.String())
			}
			// The message must still be readable, or masking it would just be a
			// different way of losing the error.
			if !strings.HasPrefix(buf.String(), "error: ") || len(buf.String()) < len("error: ")+8 {
				t.Errorf("the error was lost rather than masked: %q", buf.String())
			}
		})
	}
}
