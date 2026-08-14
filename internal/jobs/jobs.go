// Package jobs runs every unit of deferred work on River, a Postgres-backed
// durable queue.
//
// The rule the plan (§6.2) states and this package enforces: anything that can
// fail on the network and must be retried is a job — publication, Slack
// delivery, cleanup. Nothing background runs "just in a goroutine", because a
// goroutine dies with its replica and leaves no trace; a River job survives the
// crash, is retried, and is visible in `river_job` and in the river_* metrics.
//
// Seven kinds form two chains:
//
//	periodic scan ──► scan_repo (one per repository)
//	                    ├──► review ──► publish_review (findings + summary marker)
//	                    └──► publish_review (sweep of reviews stuck at 'reviewed')
//
//	periodic digest ──► slack_send (one per digest part)
//	periodic cleanup
//
// Two properties of that shape are load-bearing and easy to undo by accident:
//
//   - review has MaxAttempts = 1 (a failed review already burned tokens) while
//     publish_review and slack_send get 10 — they are pure network deliveries
//     whose retry is cheap. This is why publication is a separate job at all.
//
//   - uniqueness is always restricted to an explicit in-flight state list. See
//     uniqueInFlightStates.
package jobs

import (
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// Queue names. `review` is sized by review.max_parallel; `publish` and `slack`
// stay at 1–2 workers because serial delivery is far more predictable against
// GitLab's and Slack's rate limits than a burst that earns a 429.
const (
	QueueDefault = river.QueueDefault
	QueueReview  = "review"
	QueuePublish = "publish"
	QueueSlack   = "slack"
)

// Job kinds. Exported because the CLI prints them and the metrics label on
// them; a literal repeated in three packages is a literal that drifts.
const (
	KindScan          = "scan"
	KindScanRepo      = "scan_repo"
	KindReview        = "review"
	KindPublishReview = "publish_review"
	KindDigest        = "digest"
	KindSlackSend     = "slack_send"
	KindCleanup       = "cleanup"
)

// Attempt budgets (§6.2).
const (
	scanMaxAttempts     = 3
	scanRepoMaxAttempts = 3
	// reviewMaxAttempts is deliberately 1. A review that failed has already paid
	// for its LLM calls; retrying a deterministic failure triples the spend on
	// the same error. The scanner re-enqueues on the next pass under the §6.5
	// backoff, and the backoff is what stops a poison MR from burning money.
	reviewMaxAttempts = 1
	// publishMaxAttempts / slackSendMaxAttempts are high because these jobs are
	// network deliveries: a transient 502 on the third of five findings must not
	// cost a whole LLM run.
	publishMaxAttempts   = 10
	digestMaxAttempts    = 3
	slackSendMaxAttempts = 10
	cleanupMaxAttempts   = 1
)

// Per-kind timeouts (§6.2). River's default is one minute, which every job here
// except the dispatchers would exceed.
const (
	scanTimeout      = 2 * time.Minute
	scanRepoTimeout  = 10 * time.Minute
	reviewTimeout    = 30 * time.Minute
	publishTimeout   = 5 * time.Minute
	digestTimeout    = 10 * time.Minute
	slackSendTimeout = 2 * time.Minute
	cleanupTimeout   = 5 * time.Minute
)

// uniqueInFlightStates is the unique-state set every kind in this package uses.
//
// It must stay explicit. River's default set includes Completed, so a second
// insert after a successful run would be silently skipped until the job cleaner
// removed the row — for publish_review that means a review whose publication
// exhausted its attempts could never be re-driven, and for review it means a
// re-review of the same head SHA (after the previous one completed) would
// vanish. Restricting uniqueness to in-flight states makes "already running" a
// no-op and "ran and finished" a fresh job.
//
// Available, Pending, Running and Scheduled are mandatory in a custom set;
// Retryable is the only one River allows dropping, and dropping it would let a
// duplicate slip in while the original waits out its backoff.
func uniqueInFlightStates() []rivertype.JobState {
	return []rivertype.JobState{
		rivertype.JobStateAvailable,
		rivertype.JobStatePending,
		rivertype.JobStateRunning,
		rivertype.JobStateRetryable,
		rivertype.JobStateScheduled,
	}
}

// ScanArgs dispatches a scan pass: it reads the configured teams and enqueues
// one scan_repo per repository. Deliberately cheap — no network calls — so the
// 2m timeout is generous.
//
// Uniqueness is by kind only (ByArgs is off), matching §6.2. The consequence is
// worth knowing: `ai-reviewer scan --team payments` collapses into an already
// in-flight full pass rather than running a narrower one. The CLI reports that
// instead of exiting silently successful.
type ScanArgs struct {
	// Team, when set, limits the pass to one team. Empty means every team.
	Team string `json:"team,omitempty"`
}

func (ScanArgs) Kind() string { return KindScan }

func (ScanArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       QueueDefault,
		MaxAttempts: scanMaxAttempts,
		UniqueOpts:  river.UniqueOpts{ByState: uniqueInFlightStates()},
	}
}

// ScanRepoArgs inspects exactly one repository: list open MRs, classify them,
// enqueue reviews for the candidates and publication for reviews left stranded.
//
// One job per repository rather than one job for everything is §6.3's central
// decision: a monolithic pass over 40 repositories does not fit a timeout when
// GitLab degrades, gets cut mid-way, and after the retry repeats the same work
// from the start — so the tail of the list is never scanned at all. Split, the
// repositories also run in parallel and one failure does not touch the others.
type ScanRepoArgs struct {
	Team string `json:"team"`
	// Repository is the GitLab full path (or numeric id) exactly as configured.
	// It alone keys uniqueness: a repository belongs to one team by config
	// validation, so including Team would only add a way for the key to drift.
	Repository string `json:"repository" river:"unique"`
}

func (ScanRepoArgs) Kind() string { return KindScanRepo }

func (ScanRepoArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       QueueDefault,
		MaxAttempts: scanRepoMaxAttempts,
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByState: uniqueInFlightStates(),
		},
	}
}

// ReviewArgs reviews one merge request at one head SHA.
//
// Uniqueness is (project_id, mr_iid, head_sha): two replicas physically cannot
// review the same SHA at the same time, and a re-enqueue while one is in flight
// is a no-op.
type ReviewArgs struct {
	Team        string `json:"team"`
	ProjectPath string `json:"project_path"`
	ProjectID   int64  `json:"project_id" river:"unique"`
	MRIID       int64  `json:"mr_iid" river:"unique"`
	HeadSHA     string `json:"head_sha" river:"unique"`

	// Publish is the per-job override of service.ai_review_publish_enabled, and
	// it is deliberately OUTSIDE the uniqueness key (§15).
	//
	// It lives in the args, not in process config, because otherwise `--publish`
	// could not reach the worker at all. It is excluded from the key because
	// including it would let a manual publishing run and a scanner-inserted
	// dry run coexist for the same SHA — two LLM runs and two publishers, both
	// seeing note_id IS NULL, posting every finding twice.
	//
	// nil means "use the configured default", which is what the scanner inserts:
	// the decision is then made when the job runs, not when it was queued.
	Publish *bool `json:"publish,omitempty"`
}

func (ReviewArgs) Kind() string { return KindReview }

func (ReviewArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       QueueReview,
		MaxAttempts: reviewMaxAttempts,
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByState: uniqueInFlightStates(),
		},
	}
}

// PublishReviewArgs delivers a finished review to GitLab: the findings whose
// note_id is still NULL, then the summary marker.
//
// It is normally inserted by the review job inside the same transaction that
// writes mr_reviews and mr_findings (§10.4). Without that atomicity, a crash
// between the commit and the insert would strand the review forever: the next
// scan sees the head SHA as already reviewed and enqueues neither job.
type PublishReviewArgs struct {
	ReviewID uuid.UUID `json:"review_id" river:"unique"`
}

func (PublishReviewArgs) Kind() string { return KindPublishReview }

func (PublishReviewArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       QueuePublish,
		MaxAttempts: publishMaxAttempts,
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByState: uniqueInFlightStates(),
		},
	}
}

// DigestArgs builds one team's digest for one schedule slot and persists it as
// digest_runs + digest_messages. It does not talk to Slack; slack_send does.
type DigestArgs struct {
	Team string `json:"team" river:"unique"`
	// Slot is scheduler.Daily.Slot's value ("09:00" / "16:30").
	Slot string `json:"slot" river:"unique"`
	// RunDate is a plain YYYY-MM-DD string in the schedule's own zone, not a
	// time.Time: the uniqueness hash is computed over the encoded args, and a
	// time.Time carries a zone and sub-second precision that would differ
	// between two replicas inserting the same slot.
	RunDate string `json:"run_date" river:"unique"`
	// Attempt is 0 for every scheduled run, so two replicas cannot double-send a
	// slot. `digest --force` takes the next attempt, which is both a new unique
	// key and a new digest_runs row — an honest journal of manual repeats.
	Attempt int `json:"attempt" river:"unique"`
}

func (DigestArgs) Kind() string { return KindDigest }

func (DigestArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       QueueDefault,
		MaxAttempts: digestMaxAttempts,
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByState: uniqueInFlightStates(),
		},
	}
}

// SlackSendArgs delivers exactly one digest_messages row.
//
// Separate from digest because the two have wildly different retry costs:
// rebuilding a digest is dozens of GitLab requests plus user matching, while
// re-POSTing one message is one call. A `ratelimited` must not cost the former.
type SlackSendArgs struct {
	MessageID uuid.UUID `json:"message_id" river:"unique"`
	// Team labels the delivery metrics. It is not part of the key: the message
	// id already identifies the row uniquely.
	Team string `json:"team,omitempty"`
}

func (SlackSendArgs) Kind() string { return KindSlackSend }

func (SlackSendArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       QueueSlack,
		MaxAttempts: slackSendMaxAttempts,
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByState: uniqueInFlightStates(),
		},
	}
}

// CleanupArgs removes orphaned worktrees and stale mirrors under review.workdir.
// Periodic, leader-only, one attempt: a failed sweep is retried an hour later
// anyway and nothing depends on it having run.
type CleanupArgs struct{}

func (CleanupArgs) Kind() string { return KindCleanup }

func (CleanupArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       QueueDefault,
		MaxAttempts: cleanupMaxAttempts,
		UniqueOpts:  river.UniqueOpts{ByState: uniqueInFlightStates()},
	}
}
