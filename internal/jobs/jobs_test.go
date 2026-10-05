package jobs_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/sxwebdev/ai-reviewer/internal/jobs"
)

// jobSpec is one row of the plan's §6.2 table.
type jobSpec struct {
	name        string
	args        river.JobArgs
	kind        string
	queue       string
	maxAttempts int
	byArgs      bool
}

func specs() []jobSpec {
	return []jobSpec{
		{"scan", jobs.ScanArgs{}, "scan", "default", 3, false},
		{"scan_repo", jobs.ScanRepoArgs{}, "scan_repo", "default", 3, true},
		{"review", jobs.ReviewArgs{}, "review", "review", 1, true},
		{"publish_review", jobs.PublishReviewArgs{}, "publish_review", "publish", 10, true},
		{"digest", jobs.DigestArgs{}, "digest", "default", 3, true},
		{"slack_send", jobs.SlackSendArgs{}, "slack_send", "slack", 10, true},
		{"cleanup", jobs.CleanupArgs{}, "cleanup", "default", 1, false},
	}
}

// insertOpts is the JobArgsWithInsertOpts half of the contract; every kind here
// declares its own defaults so a plain Insert cannot land in the wrong queue.
func insertOpts(t *testing.T, args river.JobArgs) river.InsertOpts {
	t.Helper()
	withOpts, ok := args.(river.JobArgsWithInsertOpts)
	if !ok {
		t.Fatalf("%T does not declare InsertOpts", args)
	}
	return withOpts.InsertOpts()
}

// TestJobTableMatchesPlan pins §6.2: kind, queue and attempt budget per kind.
// The attempt budgets are the load-bearing part — review must be 1 (a failed
// review already burned tokens) while the two delivery kinds must be 10.
func TestJobTableMatchesPlan(t *testing.T) {
	t.Parallel()
	for _, s := range specs() {
		t.Run(s.name, func(t *testing.T) {
			t.Parallel()
			if got := s.args.Kind(); got != s.kind {
				t.Errorf("Kind() = %q, want %q", got, s.kind)
			}
			opts := insertOpts(t, s.args)
			if opts.Queue != s.queue {
				t.Errorf("queue = %q, want %q", opts.Queue, s.queue)
			}
			if opts.MaxAttempts != s.maxAttempts {
				t.Errorf("MaxAttempts = %d, want %d", opts.MaxAttempts, s.maxAttempts)
			}
			if opts.UniqueOpts.ByArgs != s.byArgs {
				t.Errorf("UniqueOpts.ByArgs = %v, want %v", opts.UniqueOpts.ByArgs, s.byArgs)
			}
		})
	}
}

// TestUniqueStatesAreInFlightOnly is the mutation check for the ByState list.
//
// River's default includes Completed, and with it a re-insert after a
// successful run would be silently skipped until the job cleaner removed the
// row: re-driving publish_review after its attempts ran out would be
// impossible, and a re-review of a SHA whose previous review completed would
// vanish. Both failures are silent, which is why this is asserted per kind
// rather than trusted to one shared helper.
func TestUniqueStatesAreInFlightOnly(t *testing.T) {
	t.Parallel()
	want := []rivertype.JobState{
		rivertype.JobStateAvailable,
		rivertype.JobStatePending,
		rivertype.JobStateRunning,
		rivertype.JobStateRetryable,
		rivertype.JobStateScheduled,
	}
	forbidden := []rivertype.JobState{
		rivertype.JobStateCompleted,
		rivertype.JobStateCancelled,
		rivertype.JobStateDiscarded,
	}

	for _, s := range specs() {
		t.Run(s.name, func(t *testing.T) {
			t.Parallel()
			got := insertOpts(t, s.args).UniqueOpts.ByState
			if len(got) == 0 {
				t.Fatal("uniqueness is not configured: two replicas could run this job at once")
			}
			for _, f := range forbidden {
				if slices.Contains(got, f) {
					t.Errorf("ByState contains %s: a finished job would block a re-insert", f)
				}
			}
			if !slices.Equal(got, want) {
				t.Errorf("ByState = %v, want %v", got, want)
			}
		})
	}
}

// TestReviewArgsUniqueTags pins which fields key the review's uniqueness.
// Publish must stay out of it: including it would let a manual publishing run
// and a scanner-inserted dry run coexist for the same SHA, and both publishers
// would see note_id IS NULL and post every finding twice.
func TestReviewArgsUniqueTags(t *testing.T) {
	t.Parallel()
	got := uniqueTaggedFields(t, jobs.ReviewArgs{})
	want := []string{"head_sha", "mr_iid", "project_id"}
	if !slices.Equal(got, want) {
		t.Errorf("review unique fields = %v, want %v", got, want)
	}
}

// TestUniqueTaggedFieldsPerKind pins the rest of §6.2's uniqueness column.
func TestUniqueTaggedFieldsPerKind(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		args river.JobArgs
		want []string
	}{
		// Kind-only uniqueness: no tagged fields, and ByArgs is off, so the kind
		// alone is the key.
		"scan":           {jobs.ScanArgs{}, nil},
		"cleanup":        {jobs.CleanupArgs{}, nil},
		"scan_repo":      {jobs.ScanRepoArgs{}, []string{"repository"}},
		"publish_review": {jobs.PublishReviewArgs{}, []string{"review_id"}},
		"digest":         {jobs.DigestArgs{}, []string{"attempt", "run_date", "slot", "team"}},
		"slack_send":     {jobs.SlackSendArgs{}, []string{"message_id"}},
		"slack_command":  {jobs.SlackCommandArgs{}, []string{"channel_id", "scope", "slack_user_id", "team"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := uniqueTaggedFields(t, tc.args); !slices.Equal(got, tc.want) {
				t.Errorf("unique fields = %v, want %v", got, tc.want)
			}
		})
	}
}

// uniqueTaggedFields reports the JSON names of the fields River will hash,
// sorted. River reads the `river:"unique"` struct tag; this mirrors that rule
// so a tag removed by accident is visible without a database.
func uniqueTaggedFields(t *testing.T, args river.JobArgs) []string {
	t.Helper()
	var out []string
	rt := reflect.TypeOf(args)
	for f := range rt.Fields() {
		f := f
		if f.Tag.Get("river") != "unique" {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" {
			name = f.Name
		}
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// TestSlackSendTeamIsNotPartOfTheKey guards a specific mistake: labelling the
// delivery metrics from the args is fine, but if Team were tagged unique, a
// message re-enqueued with a different team label would be delivered twice.
func TestSlackSendTeamIsNotPartOfTheKey(t *testing.T) {
	t.Parallel()
	if slices.Contains(uniqueTaggedFields(t, jobs.SlackSendArgs{}), "team") {
		t.Error("slack_send uniqueness includes team; one message id must map to one delivery")
	}
}

func TestIsTerminal(t *testing.T) {
	t.Parallel()
	cases := map[rivertype.JobState]bool{
		rivertype.JobStateCompleted: true,
		rivertype.JobStateCancelled: true,
		rivertype.JobStateDiscarded: true,
		rivertype.JobStateAvailable: false,
		rivertype.JobStatePending:   false,
		rivertype.JobStateRunning:   false,
		rivertype.JobStateRetryable: false,
		rivertype.JobStateScheduled: false,
	}
	for state, want := range cases {
		if got := jobs.IsTerminal(state); got != want {
			t.Errorf("IsTerminal(%s) = %v, want %v", state, got, want)
		}
	}
}

func TestDecodeArgs(t *testing.T) {
	t.Parallel()
	publish := true
	encoded, err := json.Marshal(jobs.ReviewArgs{ProjectID: 7, MRIID: 12, HeadSHA: "abc", Publish: &publish})
	if err != nil {
		t.Fatal(err)
	}

	got, err := jobs.DecodeArgs[jobs.ReviewArgs](&rivertype.JobRow{Kind: "review", EncodedArgs: encoded})
	if err != nil {
		t.Fatalf("DecodeArgs: %v", err)
	}
	if got.Publish == nil || !*got.Publish {
		t.Errorf("Publish did not survive the round trip: %+v", got.Publish)
	}
	if got.ProjectID != 7 || got.MRIID != 12 || got.HeadSHA != "abc" {
		t.Errorf("args = %+v", got)
	}

	if _, err := jobs.DecodeArgs[jobs.ReviewArgs](nil); err == nil {
		t.Error("a nil row must be an error, not a zero value")
	}
	if _, err := jobs.DecodeArgs[jobs.ReviewArgs](&rivertype.JobRow{Kind: "review", EncodedArgs: []byte("{")}); err == nil {
		t.Error("malformed args must be an error")
	}
}

// TestDecodeArgsOmitsPublishWhenUnset proves the scanner's "decide at run time"
// encoding: no publish key at all, which is what lets the CLI tell a job that
// will publish from one that will not.
func TestDecodeArgsOmitsPublishWhenUnset(t *testing.T) {
	t.Parallel()
	encoded, err := json.Marshal(jobs.ReviewArgs{ProjectID: 1, MRIID: 2, HeadSHA: "s"})
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(encoded, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["publish"]; ok {
		t.Errorf("an unset Publish must not be encoded: %s", encoded)
	}
}

func TestLocalLockKey(t *testing.T) {
	t.Parallel()
	got := jobs.LocalLockKey(jobs.ReviewArgs{ProjectID: 42, MRIID: 7, HeadSHA: "deadbeef"})
	if got != "review:42:7:deadbeef" {
		t.Errorf("LocalLockKey = %q", got)
	}

	// The key must track the same three fields as the uniqueness key, or
	// --local would exclude the wrong runs.
	other := jobs.LocalLockKey(jobs.ReviewArgs{ProjectID: 42, MRIID: 7, HeadSHA: "cafe"})
	if got == other {
		t.Error("a different head SHA must produce a different lock key")
	}
	publish := true
	withPublish := jobs.LocalLockKey(jobs.ReviewArgs{ProjectID: 42, MRIID: 7, HeadSHA: "deadbeef", Publish: &publish})
	if withPublish != got {
		t.Error("--publish must not change the lock key, just as it does not change the unique key")
	}
}

// TestPublishReviewArgsCarryTheReviewID is a guard on the one field that makes
// publication idempotent.
func TestPublishReviewArgsCarryTheReviewID(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	encoded, err := json.Marshal(jobs.PublishReviewArgs{ReviewID: id})
	if err != nil {
		t.Fatal(err)
	}
	got, err := jobs.DecodeArgs[jobs.PublishReviewArgs](&rivertype.JobRow{Kind: "publish_review", EncodedArgs: encoded})
	if err != nil {
		t.Fatal(err)
	}
	if got.ReviewID != id {
		t.Errorf("ReviewID = %s, want %s", got.ReviewID, id)
	}
}

// TestSlackCommandChannelIsPartOfTheKey guards the delivery, not the dedupe.
// The answer goes to the response URL of whichever invocation won the unique
// key, so a key without the channel folds two conversations together: the second
// caller is acknowledged and then answered somewhere else. It bites on a
// single-team deployment, where SlackCommandTeam resolves any channel — a DM to
// the app included — to the one team, so /all in the channel followed by /all in
// a DM is the same key with two different destinations.
func TestSlackCommandChannelIsPartOfTheKey(t *testing.T) {
	t.Parallel()
	if !slices.Contains(uniqueTaggedFields(t, jobs.SlackCommandArgs{}), "channel_id") {
		t.Error("slack_command uniqueness omits channel_id; the answer would be delivered to the wrong conversation")
	}
}
