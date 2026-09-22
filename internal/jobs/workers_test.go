package jobs_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/riverqueue/river"

	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/jobs"
	"github.com/sxwebdev/ai-reviewer/internal/metrics"
	"github.com/sxwebdev/ai-reviewer/internal/slack"
)

// TestScanDispatchesOneJobPerRepository is §6.3's split: the scan itself makes
// no network calls, it only fans out. One job per repository is what keeps a
// slow repository from starving the tail of the list on every retry.
func TestScanDispatchesOneJobPerRepository(t *testing.T) {
	f := newFixture(t)
	svc := f.newService(t, jobs.Config{}, newDeps(nil, nil, nil))

	if err := jobs.WorkScan(t.Context(), svc, jobs.ScanArgs{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	// Two payments repositories plus one platform repository.
	if got := f.count(t, jobs.KindScanRepo, "team", f.team(0).Name); got != 2 {
		t.Errorf("scan_repo jobs for %s = %d, want 2", f.team(0).Name, got)
	}
	if got := f.count(t, jobs.KindScanRepo, "team", f.team(1).Name); got != 1 {
		t.Errorf("scan_repo jobs for %s = %d, want 1", f.team(1).Name, got)
	}

	// A second pass while the first batch is still in flight adds nothing.
	if err := jobs.WorkScan(t.Context(), svc, jobs.ScanArgs{}); err != nil {
		t.Fatalf("second scan: %v", err)
	}
	if got := f.count(t, jobs.KindScanRepo, "repository", f.repo(0)); got != 1 {
		t.Errorf("scan_repo jobs after a second pass = %d, want 1", got)
	}
}

func TestScanTeamFilter(t *testing.T) {
	f := newFixture(t)
	svc := f.newService(t, jobs.Config{}, newDeps(nil, nil, nil))

	// Case-insensitive, matching how teams are looked up everywhere else.
	if err := jobs.WorkScan(t.Context(), svc, jobs.ScanArgs{Team: strings.ToUpper(f.team(1).Name)}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got := f.count(t, jobs.KindScanRepo, "team", f.team(1).Name); got != 1 {
		t.Errorf("scan_repo jobs = %d, want platform's single repository", got)
	}
	if got := f.count(t, jobs.KindScanRepo, "team", f.team(0).Name); got != 0 {
		t.Errorf("the filter leaked: %d payments jobs were queued", got)
	}
}

// TestScanUnknownTeamFails: a typo'd or removed team must be reported, not
// quietly treated as "nothing to do".
func TestScanUnknownTeamFails(t *testing.T) {
	f := newFixture(t)
	svc := f.newService(t, jobs.Config{}, newDeps(nil, nil, nil))

	err := jobs.WorkScan(t.Context(), svc, jobs.ScanArgs{Team: "does-not-exist-" + f.token})
	if err == nil {
		t.Fatal("an unknown team must fail the job")
	}
	if got := f.count(t, jobs.KindScanRepo, "team", f.team(0).Name); got != 0 {
		t.Errorf("scan_repo jobs = %d, want 0", got)
	}
}

// TestScanRepoEnqueuesCandidates: the scanner leaves Publish unset so the
// decision is taken from config when the review runs — and so a later
// --publish insert is recognisably different from this one.
func TestScanRepoEnqueuesCandidates(t *testing.T) {
	f := newFixture(t)
	shaA, shaB := f.sha("1"), f.sha("2")
	sc := &fakeScanner{results: map[string]*jobs.ScanResult{
		f.repo(0): {Candidates: []jobs.ReviewRequest{
			{Team: f.team(0).Name, ProjectPath: f.repo(0), ProjectID: 1, MRIID: 5, HeadSHA: shaA},
			{Team: f.team(0).Name, ProjectPath: f.repo(0), ProjectID: 1, MRIID: 6, HeadSHA: shaB},
		}},
	}}
	rev := &fakeReviewer{}
	svc := f.newService(t, jobs.Config{PublishEnabled: true}, newDeps(rev, sc, nil))

	if err := jobs.WorkScanRepo(t.Context(), svc, jobs.ScanRepoArgs{Team: f.team(0).Name, Repository: f.repo(0)}); err != nil {
		t.Fatalf("scan_repo: %v", err)
	}
	for _, sha := range []string{shaA, shaB} {
		if got := f.count(t, jobs.KindReview, "head_sha", sha); got != 1 {
			t.Errorf("review jobs for %s = %d, want 1", sha, got)
		}
	}
	// Scanning must never run a review inline — that is what the review queue
	// and its concurrency limit are for.
	if reqs, _ := rev.snapshot(); len(reqs) != 0 {
		t.Errorf("the scan ran %d reviews itself", len(reqs))
	}

	// Publish must be absent from the encoded args, even though publication is
	// enabled in config: pinning it here would make the scanner's job
	// indistinguishable from an explicit `--publish` one.
	raw := string(f.rawArgs(t, jobs.KindReview, "head_sha", shaA))
	if strings.Contains(raw, `"publish"`) {
		t.Errorf("the scanner pinned a publish decision into the args: %s", raw)
	}
}

// TestScanRepoRequeuesStalePublications is §6.3's safety net: a review that was
// persisted but never delivered gets its publication re-driven, and the
// expensive half is not repeated.
func TestScanRepoRequeuesStalePublications(t *testing.T) {
	f := newFixture(t)
	stale := []uuid.UUID{uuid.New(), uuid.New()}
	sc := &fakeScanner{results: map[string]*jobs.ScanResult{
		f.repo(0): {StalePublish: stale},
	}}
	rev := &fakeReviewer{}
	svc := f.newService(t, jobs.Config{}, newDeps(rev, sc, nil))

	if err := jobs.WorkScanRepo(t.Context(), svc, jobs.ScanRepoArgs{Team: f.team(0).Name, Repository: f.repo(0)}); err != nil {
		t.Fatalf("scan_repo: %v", err)
	}
	for _, id := range stale {
		if got := f.count(t, jobs.KindPublishReview, "review_id", id.String()); got != 1 {
			t.Errorf("publish_review jobs for %s = %d, want 1", id, got)
		}
	}
	if got := f.countByPrefix(t, jobs.KindReview, "head_sha", f.token); got != 0 { //nolint:staticcheck // token prefix is this fixture
		t.Errorf("review jobs = %d, want 0 — the LLM must not run again", got)
	}
	if reqs, _ := rev.snapshot(); len(reqs) != 0 {
		t.Errorf("the sweep ran %d reviews", len(reqs))
	}
}

// TestScanRepoUnknownTeamIsANoOp: a team removed from the config between the
// dispatch and the pass cannot be fixed by retrying, so the job succeeds having
// done nothing rather than burning three attempts.
func TestScanRepoUnknownTeamIsANoOp(t *testing.T) {
	f := newFixture(t)
	sc := &fakeScanner{}
	svc := f.newService(t, jobs.Config{}, newDeps(nil, sc, nil))

	if err := jobs.WorkScanRepo(t.Context(), svc, jobs.ScanRepoArgs{Team: "retired-" + f.token, Repository: "x/y"}); err != nil {
		t.Fatalf("a removed team must not fail the job: %v", err)
	}
	if len(sc.inspects) != 0 {
		t.Errorf("the repository was inspected anyway: %v", sc.inspects)
	}
}

func TestScanRepoPropagatesFailure(t *testing.T) {
	f := newFixture(t)
	want := errors.New("gitlab 503")
	sc := &fakeScanner{err: want}
	svc := f.newService(t, jobs.Config{}, newDeps(nil, sc, nil))

	err := jobs.WorkScanRepo(t.Context(), svc, jobs.ScanRepoArgs{Team: f.team(0).Name, Repository: f.repo(0)})
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want the scanner's error so River retries", err)
	}
}

// TestScanRepoDoesNotEmitTheScanMetrics pins ownership of
// ai_reviewer_scans_total / _duration_seconds: service.ScanRepository emits
// them, this worker wraps exactly one such call, and a copy here doubled every
// sample — every rate panel and alert threshold on those names read 2× the
// truth. The fake scanner emits nothing, so any observation seen here is the
// worker's own.
//
// Deltas rather than absolutes, so the test survives `-count=2` against the
// process-global promauto collectors.
func TestScanRepoDoesNotEmitTheScanMetrics(t *testing.T) {
	f := newFixture(t)
	sc := &fakeScanner{results: map[string]*jobs.ScanResult{
		f.repo(0): {Failed: false},
		f.repo(1): {Failed: true}, // the partial branch
	}}
	svc := f.newService(t, jobs.Config{}, newDeps(nil, sc, nil))

	// ObserveScan is the only writer of either collector and updates both at
	// once, so the counter alone answers "did this layer record a scan?".
	before := map[string]float64{}
	for _, r := range []string{metrics.ResultOK, metrics.ResultPartial, metrics.ResultError} {
		before[r] = testutil.ToFloat64(metrics.ScansTotal.WithLabelValues(r))
	}

	for _, repo := range []string{f.repo(0), f.repo(1)} {
		if err := jobs.WorkScanRepo(t.Context(), svc, jobs.ScanRepoArgs{Team: f.team(0).Name, Repository: repo}); err != nil {
			t.Fatalf("scan_repo %s: %v", repo, err)
		}
	}
	// And the error path, which had its own duplicate observation.
	failing := f.newService(t, jobs.Config{}, newDeps(nil, &fakeScanner{err: errors.New("gitlab 503")}, nil))
	if err := jobs.WorkScanRepo(t.Context(), failing, jobs.ScanRepoArgs{Team: f.team(0).Name, Repository: f.repo(0)}); err == nil {
		t.Fatal("the failing scanner must fail the job")
	}

	for _, r := range []string{metrics.ResultOK, metrics.ResultPartial, metrics.ResultError} {
		if got := testutil.ToFloat64(metrics.ScansTotal.WithLabelValues(r)); got != before[r] {
			t.Errorf("the worker emitted ai_reviewer_scans_total{result=%q}: %v → %v; the service owns it",
				r, before[r], got)
		}
	}
}

// TestDigestDryRunQueuesNothing pins §17's dry-run bullet: the digest is built
// and persisted in full, and no slack_send job exists.
func TestDigestDryRunQueuesNothing(t *testing.T) {
	t.Run("switched off in config", func(t *testing.T) {
		f := newFixture(t)
		messages := []uuid.UUID{uuid.New(), uuid.New()}
		dg := &fakeDigester{outcome: &jobs.DigestOutcome{RunID: uuid.New(), Messages: messages, Status: "built"}}
		svc := f.newService(t, jobs.Config{SlackSendEnabled: false}, newDeps(nil, nil, dg))

		if err := jobs.WorkDigest(t.Context(), svc, f.digestArgs()); err != nil {
			t.Fatalf("digest: %v", err)
		}
		if len(dg.builds) != 1 {
			t.Errorf("the digest was not built: %v", dg.builds)
		}
		for _, id := range messages {
			if got := f.count(t, jobs.KindSlackSend, "message_id", id.String()); got != 0 {
				t.Errorf("slack_send jobs = %d, want 0", got)
			}
		}
	})

	t.Run("reported as dry_run by the service", func(t *testing.T) {
		// Belt and braces: even with the switch on, a service that persisted the
		// parts as dry_run must not have them delivered.
		f := newFixture(t)
		messages := []uuid.UUID{uuid.New()}
		dg := &fakeDigester{outcome: &jobs.DigestOutcome{RunID: uuid.New(), Messages: messages, Status: jobs.StatusDryRun}}
		svc := f.newService(t, jobs.Config{SlackSendEnabled: true}, newDeps(nil, nil, dg))

		if err := jobs.WorkDigest(t.Context(), svc, f.digestArgs()); err != nil {
			t.Fatalf("digest: %v", err)
		}
		if got := f.count(t, jobs.KindSlackSend, "message_id", messages[0].String()); got != 0 {
			t.Errorf("slack_send jobs = %d, want 0", got)
		}
	})
}

// TestDigestQueuesOneJobPerPart: each digest_messages row is one delivery job,
// so a failure of part 2 never re-sends part 1.
func TestDigestQueuesOneJobPerPart(t *testing.T) {
	f := newFixture(t)
	messages := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	dg := &fakeDigester{outcome: &jobs.DigestOutcome{RunID: uuid.New(), Messages: messages, Status: "built", MRCount: 9}}
	svc := f.newService(t, jobs.Config{SlackSendEnabled: true}, newDeps(nil, nil, dg))

	if err := jobs.WorkDigest(t.Context(), svc, f.digestArgs()); err != nil {
		t.Fatalf("digest: %v", err)
	}
	for _, id := range messages {
		if got := f.count(t, jobs.KindSlackSend, "message_id", id.String()); got != 1 {
			t.Errorf("slack_send jobs for %s = %d, want 1", id, got)
		}
	}
	// The build must have received the slot exactly as scheduled — a digest
	// filed under the wrong slot would let the real one be sent twice.
	if len(dg.builds) != 1 || !strings.HasPrefix(dg.builds[0], f.team(0).Name+"|09:00|") {
		t.Errorf("builds = %v", dg.builds)
	}
}

func TestDigestUnknownTeamIsANoOp(t *testing.T) {
	f := newFixture(t)
	dg := &fakeDigester{outcome: &jobs.DigestOutcome{Status: "built"}}
	svc := f.newService(t, jobs.Config{SlackSendEnabled: true}, newDeps(nil, nil, dg))

	args := f.digestArgs()
	args.Team = "retired-" + f.token
	if err := jobs.WorkDigest(t.Context(), svc, args); err != nil {
		t.Fatalf("a removed team must not fail the job: %v", err)
	}
	if len(dg.builds) != 0 {
		t.Errorf("the digest was built for a team that is gone: %v", dg.builds)
	}
}

func TestDigestRejectsAMalformedRunDate(t *testing.T) {
	f := newFixture(t)
	dg := &fakeDigester{outcome: &jobs.DigestOutcome{Status: "built"}}
	svc := f.newService(t, jobs.Config{}, newDeps(nil, nil, dg))

	args := f.digestArgs()
	args.RunDate = "13.08.2026"
	if err := jobs.WorkDigest(t.Context(), svc, args); err == nil {
		t.Fatal("a run date the service cannot file must fail loudly")
	}
	if len(dg.builds) != 0 {
		t.Errorf("the digest was built from an unparsable date: %v", dg.builds)
	}
}

func TestSlackSendDelegatesTheClaimProtocol(t *testing.T) {
	f := newFixture(t)
	id := uuid.New()

	dg := &fakeDigester{}
	svc := f.newService(t, jobs.Config{}, newDeps(nil, nil, dg))
	if err := jobs.WorkSlackSend(t.Context(), svc, jobs.SlackSendArgs{MessageID: id, Team: f.team(0).Name}); err != nil {
		t.Fatalf("slack_send: %v", err)
	}
	if len(dg.sent) != 1 || dg.sent[0] != id {
		t.Errorf("sent = %v, want exactly [%s]", dg.sent, id)
	}

	// A delivery failure must reach River so the job is retried; the service's
	// no-op branches (already sent, failed, dry-run) return nil and are its own
	// business, not this worker's.
	want := errors.New("ratelimited")
	failing := &fakeDigester{sendErr: want}
	svc2 := f.newService(t, jobs.Config{}, newDeps(nil, nil, failing))
	if err := jobs.WorkSlackSend(t.Context(), svc2, jobs.SlackSendArgs{MessageID: id}); !errors.Is(err, want) {
		t.Fatalf("err = %v, want the delivery error", err)
	}
}

func TestPublishReviewDelegatesToTheService(t *testing.T) {
	f := newFixture(t)
	id := uuid.New()
	rev := &fakeReviewer{}
	svc := f.newService(t, jobs.Config{}, newDeps(rev, nil, nil))

	if err := jobs.WorkPublishReview(t.Context(), svc, jobs.PublishReviewArgs{ReviewID: id}); err != nil {
		t.Fatalf("publish_review: %v", err)
	}
	if len(rev.publishedID) != 1 || rev.publishedID[0] != id {
		t.Errorf("published = %v, want [%s]", rev.publishedID, id)
	}

	want := errors.New("gitlab 502")
	failing := &fakeReviewer{publishErr: want}
	svc2 := f.newService(t, jobs.Config{}, newDeps(failing, nil, nil))
	if err := jobs.WorkPublishReview(t.Context(), svc2, jobs.PublishReviewArgs{ReviewID: id}); !errors.Is(err, want) {
		t.Fatalf("err = %v, want the publication error so the ten attempts are used", err)
	}
}

func TestCleanupWithoutAWorkdirIsANoOp(t *testing.T) {
	f := newFixture(t)
	svc := f.newService(t, jobs.Config{WorkDir: ""}, newDeps(nil, nil, nil))
	if err := jobs.WorkCleanup(t.Context(), svc); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}

func TestCleanupSweepsTheConfiguredWorkdir(t *testing.T) {
	f := newFixture(t)
	svc := f.newService(t, jobs.Config{WorkDir: t.TempDir()}, newDeps(nil, nil, nil))
	if err := jobs.WorkCleanup(t.Context(), svc); err != nil {
		t.Fatalf("cleanup on an empty workdir: %v", err)
	}
}

// TestJobErrorClassification is §17's "fatal vs retryable" at the job layer.
//
// Without it a revoked token makes every scan_repo in every repository burn all
// three attempts, and the periodic scan repeats that every interval, forever.
// river.JobCancel ends the job at this attempt however many remain; the
// retryable cases must stay plain errors so the attempt budget is still spent
// on the failures a retry can actually fix.
func TestJobErrorClassification(t *testing.T) {
	f := newFixture(t)

	cases := []struct {
		name       string
		err        error
		wantCancel bool
	}{
		{"a revoked token", &gitlab.APIError{Status: 401, Method: "GET", Path: "/projects/x"}, true},
		{"a missing scope or role", &gitlab.APIError{Status: 403, Method: "GET", Path: "/projects/x"}, true},
		{"a repository that is gone", &gitlab.APIError{Status: 404, Method: "GET", Path: "/projects/x"}, true},
		{"gitlab is down", &gitlab.APIError{Status: 503, Method: "GET", Path: "/projects/x"}, false},
		{"gitlab is rate limiting", &gitlab.APIError{Status: 429, Method: "GET", Path: "/projects/x"}, false},
		{"a transport failure", errors.New("dial tcp: i/o timeout"), false},
		{"a cancelled context on shutdown", context.Canceled, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Wrapped the way a service returns it, so the classifier has to look
			// through the wrap rather than compare the top-level error.
			sc := &fakeScanner{err: fmt.Errorf("get project: %w", tc.err)}
			svc := f.newService(t, jobs.Config{}, newDeps(nil, sc, nil))

			err := jobs.WorkScanRepo(t.Context(), svc, jobs.ScanRepoArgs{Team: f.team(0).Name, Repository: f.repo(0)})
			if err == nil {
				t.Fatal("the failure must reach River")
			}
			if !errors.Is(err, tc.err) {
				t.Errorf("the cause was lost: %v", err)
			}

			var cancel *river.JobCancelError
			if got := errors.As(err, &cancel); got != tc.wantCancel {
				t.Errorf("cancelled = %v, want %v (err %v)", got, tc.wantCancel, err)
			}
		})
	}
}

// TestSlackSendErrorClassification: Slack answers most failures with HTTP 200
// and a code, so the code is what decides. A rate limit or a Slack outage must
// keep its ten attempts — cancelling one of those throws away a digest a retry
// would have delivered.
func TestSlackSendErrorClassification(t *testing.T) {
	f := newFixture(t)

	cases := []struct {
		name       string
		code       string
		wantCancel bool
	}{
		{"the token is invalid", "invalid_auth", true},
		{"the bot was never invited", "not_in_channel", true},
		{"the channel does not exist", "channel_not_found", true},
		{"a scope is missing", "missing_scope", true},
		{"rate limited", "ratelimited", false},
		{"slack's own outage", "service_unavailable", false},
		{"an http failure behind the client", "http_error", false},
		{"a code nobody has seen before", "some_new_code", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cause := &slack.APIError{Method: "chat.postMessage", Status: 200, Code: tc.code}
			dg := &fakeDigester{sendErr: fmt.Errorf("post digest part 1/2: %w", cause)}
			svc := f.newService(t, jobs.Config{}, newDeps(nil, nil, dg))

			err := jobs.WorkSlackSend(t.Context(), svc, jobs.SlackSendArgs{MessageID: uuid.New(), Team: f.team(0).Name})
			if err == nil {
				t.Fatal("the failure must reach River")
			}
			var cancel *river.JobCancelError
			if got := errors.As(err, &cancel); got != tc.wantCancel {
				t.Errorf("cancelled = %v, want %v (err %v)", got, tc.wantCancel, err)
			}
		})
	}
}

// TestSlackCommandDelegatesToTheService: the worker resolves the team from the
// name in its args and forwards everything else untouched — the digest, the
// filtering and the delivery are the service's.
func TestSlackCommandDelegatesToTheService(t *testing.T) {
	f := newFixture(t)
	cmd := &fakeCommander{}
	deps := newDeps(nil, nil, nil)
	deps.Commander = cmd
	svc := f.newService(t, jobs.Config{}, deps)

	args := jobs.SlackCommandArgs{
		Scope: "mine", Team: f.team(0).Name, SlackUserID: "U42",
		ChannelID: "C1", ResponseURL: "https://hooks.slack.com/commands/T1/B2/C3",
	}
	if err := jobs.WorkSlackCommand(t.Context(), svc, args); err != nil {
		t.Fatalf("slack_command: %v", err)
	}
	got := cmd.requests()
	if len(got) != 1 {
		t.Fatalf("requests = %d, want 1", len(got))
	}
	if got[0].Team.Name != f.team(0).Name || got[0].Scope != "mine" || got[0].SlackUserID != "U42" {
		t.Errorf("request = %+v, want the args resolved to the configured team", got[0])
	}
	if got[0].ResponseURL != args.ResponseURL {
		t.Error("the response URL must survive the hop; without it the answer has nowhere to go")
	}
}

// TestSlackCommandForAnUnknownTeamIsANoOp: the team was renamed or removed
// between the command and its turn on the queue. There is nothing to answer with
// and nothing a retry would fix.
func TestSlackCommandForAnUnknownTeamIsANoOp(t *testing.T) {
	f := newFixture(t)
	cmd := &fakeCommander{}
	deps := newDeps(nil, nil, nil)
	deps.Commander = cmd
	svc := f.newService(t, jobs.Config{}, deps)

	err := jobs.WorkSlackCommand(t.Context(), svc, jobs.SlackCommandArgs{
		Scope: "team", Team: "retired", SlackUserID: "U42",
		ResponseURL: "https://hooks.slack.com/commands/T1/B2/C3",
	})
	if err != nil {
		t.Fatalf("an unknown team must not fail the job: %v", err)
	}
	if n := len(cmd.requests()); n != 0 {
		t.Errorf("service called %d time(s) for a team that is gone", n)
	}
}

// TestSlackCommandFailurePropagates: the answer has a deadline (the response URL
// lives 30 minutes), so a failure has to reach River while a retry can still
// land inside it.
func TestSlackCommandFailurePropagates(t *testing.T) {
	f := newFixture(t)
	want := errors.New("gitlab is down")
	cmd := &fakeCommander{err: want}
	deps := newDeps(nil, nil, nil)
	deps.Commander = cmd
	svc := f.newService(t, jobs.Config{}, deps)

	err := jobs.WorkSlackCommand(t.Context(), svc, jobs.SlackCommandArgs{
		Scope: "team", Team: f.team(0).Name, ResponseURL: "https://hooks.slack.com/commands/T1/B2/C3",
	})
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want the service's failure", err)
	}
}
