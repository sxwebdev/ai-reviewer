// Package domain holds the vocabulary of the team service — a merge request as
// the scanner sees it — plus the pure classifiers that decide what the service
// does with it: whether an AI review is due, which reviewers still owe an
// action, and which MRs need something from their author.
//
// The package is deliberately I/O-free and self-contained. It imports nothing
// from internal/gitlab, takes no context.Context, no client, no logger, and
// never calls time.Now(): every decision is a pure function of a
// MergeRequestSnapshot plus explicitly-passed inputs. That is what makes the
// rules table-testable without a GitLab instance, and what keeps a wire-format
// change in the API client from silently changing a classification.
//
// Mapping GitLab's wire structs (JSON tags, GitLab field names, RFC3339
// timestamp strings) onto these types is the service layer's job.
package domain

import "time"

// Team is one configured team: a Slack channel to notify and the repositories
// it owns. Repositories are GitLab full paths ("backend/payments").
type Team struct {
	Name          string
	SlackChannel  string
	AIReview      bool // team-level switch for automated review
	LinearTeamIDs []string
	Repositories  []string

	// DigestSlots and DigestTimezone are this team's digest schedule, already
	// resolved against the global default — never empty for a configured team.
	// They are carried here, rather than looked up from config where the schedule
	// is needed, because a digest belongs to a team: the CLI, the periodic job and
	// the worker all have to agree on which slots one team has, and passing them
	// with the team is what makes disagreeing impossible.
	DigestSlots    []string
	DigestTimezone string
	// DigestSkipWeekdays and DigestSkipDates are the days this team gets no
	// digest at all — weekends, holidays — resolved the same way and empty unless
	// configured. They travel with the team for the same reason the slots do: the
	// day a digest is skipped has to be the same day everywhere that asks.
	DigestSkipWeekdays []string
	DigestSkipDates    []string
}

// User is a GitLab account as the digest needs it. Email is frequently empty —
// GitLab only exposes it to admins on most endpoints — so the Slack matcher
// must be able to fall back to Username/Name.
type User struct {
	ID       int64
	Username string
	Name     string
	Email    string
}

// Project is a GitLab project. FullPath is path_with_namespace, the key used
// everywhere in config and in GraphQL queries.
type Project struct {
	ID            int64
	FullPath      string
	DefaultBranch string
	WebURL        string
}

// MergeRequest is the MR itself, without any per-scan state.
//
// It deliberately carries no SHA: the head SHA is a property of the snapshot
// (MergeRequestSnapshot.HeadSHA), so there is exactly one field that review
// dedupe, pipeline freshness and job uniqueness all key off. Two fields that
// can disagree would be a silent source of double reviews.
type MergeRequest struct {
	ID           int64
	IID          int64
	ProjectID    int64
	Title        string
	Description  string
	State        string // "opened" | "locked" | "merged" | "closed"
	Draft        bool
	WebURL       string
	Author       User
	SourceBranch string
	TargetBranch string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// IsOpen reports whether the MR is still open, i.e. still a candidate for
// review and for the digest. "locked" means the discussion is locked while the
// MR is open, so it counts as open. An empty state means the caller could not
// determine it; we treat that as open rather than silently dropping the MR.
func (m MergeRequest) IsOpen() bool {
	switch m.State {
	case "", "opened", "locked":
		return true
	default:
		return false
	}
}

// ReviewState is a reviewer's review state as reported by GitLab GraphQL
// (mergeRequestInteraction.reviewState). REST v4 does not expose it at all —
// reviewers[].state there is the *account* state (active/blocked) — which is
// why the GraphQL call exists.
type ReviewState string

const (
	// ReviewStateUnknown is the zero value: GraphQL is disabled, the query
	// failed, or GitLab returned an enum value this build does not know.
	// It routes the reviewer through the REST fallback heuristic instead of
	// guessing, so the service stays useful on instances without the field.
	ReviewStateUnknown ReviewState = ""

	ReviewStateUnreviewed       ReviewState = "UNREVIEWED"
	ReviewStateReviewed         ReviewState = "REVIEWED"
	ReviewStateRequestedChanges ReviewState = "REQUESTED_CHANGES"
	ReviewStateApproved         ReviewState = "APPROVED"
	// ReviewStateUnapproved is a reviewer who withdrew an approval, and
	// ReviewStateReviewStarted one who opened the MR without delivering a verdict.
	//
	// Both were missing here while gitlab.ReviewState already knew them, so both
	// collapsed into ReviewStateUnknown and took the REST fallback written for old
	// self-managed instances. Measured against one live gitlab.com project: 7 of 57
	// reviewer states, 12%. The fallback errs in the dangerous direction — it reads
	// "left a note after the last push" as done, so a reviewer who *withdrew* an
	// approval but had commented earlier silently left "Reviews needed" and the MR
	// stalled with nobody chasing it.
	ReviewStateUnapproved    ReviewState = "UNAPPROVED"
	ReviewStateReviewStarted ReviewState = "REVIEW_STARTED"
)

// Known reports whether the state carries usable information.
func (s ReviewState) Known() bool { return s != ReviewStateUnknown }

// ParseReviewState maps a GraphQL enum value onto a ReviewState.
//
// Anything unrecognised becomes ReviewStateUnknown on purpose: a future GitLab
// enum value must degrade into the REST fallback, never be assumed to mean
// "already reviewed" — that would silently drop a reviewer from the digest.
func ParseReviewState(v string) ReviewState {
	switch ReviewState(v) {
	case ReviewStateUnreviewed:
		return ReviewStateUnreviewed
	case ReviewStateReviewed:
		return ReviewStateReviewed
	case ReviewStateRequestedChanges:
		return ReviewStateRequestedChanges
	case ReviewStateApproved:
		return ReviewStateApproved
	case ReviewStateUnapproved:
		return ReviewStateUnapproved
	case ReviewStateReviewStarted:
		return ReviewStateReviewStarted
	default:
		return ReviewStateUnknown
	}
}

// Reviewer is an assigned reviewer together with their review state.
type Reviewer struct {
	User
	State ReviewState

	// LastActivityAt is when this reviewer last engaged with the MR: their most
	// recent ordinary note, or — only when they wrote none — their most recent
	// system note **predating LastPushAt**. The service layer computes it from the
	// discussions it already loads for every snapshot, so it costs no extra API
	// call.
	//
	// It exists because GitLab's reviewState (mergeRequestInteraction) carries
	// no timestamp of its own, and NeedsHumanReview has to know whether a
	// REQUESTED_CHANGES verdict predates the author's latest push. Zero means
	// "could not be determined", which now covers two situations: the discussions
	// were not loaded, or the only evidence of this reviewer is a system note the
	// author has not answered yet.
	//
	// The system-note fallback is what makes a verdict delivered through GitLab's
	// UI alone datable at all: a "Request changes" click with no comment leaves
	// nothing but a *system* note, so counting ordinary notes only pinned such a
	// reviewer at zero forever and no push could ever hand the MR back to them.
	// The clamp is what keeps that from losing the opposite way — a system note can
	// only ever prove a verdict is OLDER than the last push, never that the
	// reviewer acted after it, because GitLab credits label, assignee and commit
	// events to whoever made them too. service.lastActivityAt carries the full
	// argument. An ordinary note is unclamped and wins whenever there is one; an
	// approval would be dated the same way, which changes nothing, since
	// NeedsHumanReview answers APPROVED before it ever reads this field.
	LastActivityAt time.Time
}

// Note is one note inside a discussion. CreatedAt is parsed by the service from
// GitLab's RFC3339 string; the classifiers compare it against
// MergeRequestSnapshot.LastPushAt.
type Note struct {
	ID         int64
	Body       string
	Author     User
	System     bool // GitLab's own event notes ("added 3 commits") — never a human action
	Resolvable bool
	Resolved   bool
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Discussion is a thread of notes. IndividualNote is GitLab's
// individual_note: true for a standalone comment, false for a real thread.
type Discussion struct {
	ID             string
	IndividualNote bool
	Notes          []Note
}

// IsUnresolvedThread reports whether this discussion counts as an open thread
// the author has to deal with: a real thread (not a standalone comment) whose
// first note is resolvable and where no note has been resolved yet.
//
// Plain comments are excluded deliberately — they are conversation, not a
// blocking action item, and counting them would make the digest cry wolf.
func (d Discussion) IsUnresolvedThread() bool {
	if d.IndividualNote || len(d.Notes) == 0 {
		return false
	}
	// Resolvability is a property of the thread, and GitLab reports it on the
	// root note; a thread rooted in a non-resolvable note can never be closed
	// and so can never be "unresolved work".
	if !d.Notes[0].Resolvable {
		return false
	}
	// GitLab marks every note of a resolved thread as resolved, so a single
	// resolved note closes the thread.
	for _, n := range d.Notes {
		if n.Resolved {
			return false
		}
	}
	return true
}

// Mergeability is what GitLab tells us about whether the MR can merge.
//
// Known is set by the service when the API actually returned mergeability
// fields. It is separate from the values themselves because "false" and
// "not reported" must not look alike: reporting a conflict we are not sure
// about sends the author chasing a problem that does not exist.
type Mergeability struct {
	HasConflicts   bool
	DetailedStatus string // detailed_merge_status: "mergeable", "conflict", "checking", …
	Known          bool
}

// Pipeline is the MR's head pipeline.
//
// Known means the head_pipeline field was present in the API response — GitLab
// exposes it "only if the current user can view pipelines for this project", so
// its absence is a permissions signal, not "no pipeline". The service uses
// !Known to decide whether to fall back to GET /merge_requests/:iid/pipelines
// for that one MR. It is NOT the same thing as the `known` returned by
// FailedPipeline, which additionally requires the pipeline to be current and
// conclusive.
type Pipeline struct {
	ID     int64
	SHA    string
	Status string
	WebURL string
	Known  bool
}

// MergeRequestSnapshot is everything one scan pass learned about one MR. It is
// assembled once per run (see plan §9.4: one MR is fetched once) and then fed
// to the classifiers below; nothing in this package mutates it.
type MergeRequestSnapshot struct {
	Team       string
	Project    Project
	MR         MergeRequest
	HeadSHA    string
	Reviewers  []Reviewer
	ApprovedBy []User
	// ApprovalsKnown separates "nobody approved" from "we could not ask".
	//
	// It exists because an empty ApprovedBy used to mean both, and the second
	// meaning became load-bearing the moment Linear started using approvals to
	// decide a review was finished: where GET /approvals answers 401/403 — a
	// service account below Reporter, or a project that restricts merge-request
	// access — every merge request looks unapproved for as long as that lasts, so
	// the completion gate can never fire and the digest keeps nudging reviewers who
	// have already approved, with nothing anywhere saying why.
	// Consumers must treat false as "unknown" and fail open, never as zero.
	ApprovalsKnown bool
	Discussions    []Discussion
	Mergeability   Mergeability
	Pipeline       Pipeline
	LastPushAt     time.Time // created_at of the latest diff version
}
