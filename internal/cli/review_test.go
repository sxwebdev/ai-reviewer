package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/riverqueue/river/rivertype"
	"github.com/urfave/cli/v3"

	"github.com/sxwebdev/ai-reviewer/internal/domain"
	"github.com/sxwebdev/ai-reviewer/internal/jobs"
)

func encodeArgs(t *testing.T, args jobs.ReviewArgs) []byte {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func insertResult(t *testing.T, id int64, args jobs.ReviewArgs) jobs.InsertResult {
	t.Helper()
	return jobs.InsertResult{
		Job: &rivertype.JobRow{
			ID: id, Kind: jobs.KindReview, EncodedArgs: encodeArgs(t, args),
		},
		Deduplicated: true,
	}
}

// TestReportCollapseWarnsWhenPublishWasDropped is §15's explicit requirement.
//
// --publish is outside the uniqueness key, so a manual publishing request can
// collapse into a scanner-inserted job that will finish as a dry run. The
// command must not exit successfully there: nothing will publish, and the user
// asked for exactly that.
func TestReportCollapseWarnsWhenPublishWasDropped(t *testing.T) {
	t.Parallel()
	yes, no := true, false

	cases := []struct {
		name string
		// existing is the Publish value of the job already in flight.
		existing  *bool
		requested bool
		wantExit  bool
	}{
		{"--publish collapses into a dry run", nil, true, true},
		{"--publish collapses into an explicit non-publishing run", &no, true, true},
		{"--publish collapses into a run that already publishes", &yes, true, false},
		{"no --publish, nothing was lost", nil, false, false},
		{"no --publish, the running job publishes anyway", &yes, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := insertResult(t, 4242, jobs.ReviewArgs{
				ProjectID: 1, MRIID: 2, HeadSHA: "abc", Publish: tc.existing,
			})

			err := reportCollapse(res, tc.requested)
			if !tc.wantExit {
				if err != nil {
					t.Fatalf("unexpected failure: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("the command exited successfully while silently not publishing")
			}
			var coder cli.ExitCoder
			if !asExitCoder(err, &coder) || coder.ExitCode() == 0 {
				t.Fatalf("err = %v, want a non-zero exit code", err)
			}
			// The message has to be actionable: it must name the job that
			// survived and both documented ways out.
			msg := err.Error()
			for _, want := range []string{"4242", "--wait", "--local --publish"} {
				if !strings.Contains(msg, want) {
					t.Errorf("message does not mention %q:\n%s", want, msg)
				}
			}
		})
	}
}

// asExitCoder is errors.As specialised to the interface urfave/cli returns.
func asExitCoder(err error, target *cli.ExitCoder) bool {
	c, ok := err.(cli.ExitCoder) //nolint:errorlint // cli.Exit returns the value directly, unwrapped
	if ok {
		*target = c
	}
	return ok
}

// TestReportCollapseWithUnreadableArgs: a job row whose args cannot be decoded
// must still report the collapse. Hiding it would be the exact silent success
// this function exists to prevent.
func TestReportCollapseWithUnreadableArgs(t *testing.T) {
	t.Parallel()
	res := jobs.InsertResult{
		Job:          &rivertype.JobRow{ID: 7, Kind: jobs.KindReview, EncodedArgs: []byte("{")},
		Deduplicated: true,
	}
	if err := reportCollapse(res, true); err != nil {
		t.Fatalf("undecodable args must not fail the command: %v", err)
	}
}

func TestShortSHA(t *testing.T) {
	t.Parallel()
	if got := shortSHA("0123456789abcdef"); got != "01234567" {
		t.Errorf("shortSHA = %q", got)
	}
	if got := shortSHA("abc"); got != "abc" {
		t.Errorf("a short SHA must pass through, got %q", got)
	}
}

// TestExplainSkip covers D1's CLI half: a review whose head SHA moved is a
// skip, not a failure and not a success — nothing was reviewed, nothing was
// persisted, and no §6.5 strike was recorded. "skipped: head_moved" alone reads
// like something went wrong, so the command says what happened and what fixes
// it.
func TestExplainSkip(t *testing.T) {
	t.Parallel()

	// Every reason must stay greppable: the raw value is what an operator finds
	// in the logs and in ai_reviews_skipped_total.
	for _, r := range domain.SkipReasons {
		got := explainSkip(r)
		if !strings.HasPrefix(got, string(r)) {
			t.Errorf("explainSkip(%s) = %q, want it to lead with the reason itself", r, got)
		}
	}

	head := explainSkip(domain.ReasonHeadMoved)
	if !strings.Contains(head, "pushed") || !strings.Contains(head, "Re-run") {
		t.Errorf("head_moved does not explain itself or the way out: %q", head)
	}
	if !strings.Contains(head, "nothing was reviewed") {
		t.Errorf("head_moved does not say the review did not happen: %q", head)
	}

	// An unknown reason must still print something rather than an empty line.
	if got := explainSkip("something_new"); got != "something_new" {
		t.Errorf("an unmapped reason = %q", got)
	}
}
