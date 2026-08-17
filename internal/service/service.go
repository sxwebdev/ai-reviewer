// Package service orchestrates one operation of the team service end to end.
//
// It is the only layer that talks to GitLab, Slack, the review engine and
// Postgres in the same breath: the jobs layer above it owns queueing, retries
// and schedules; the packages below it (domain, gitlab, slack, match, review,
// store) each do one thing and know nothing of each other.
//
// Four entry points, one per River job:
//
//	ScanRepository — inspect one repository, classify its open MRs, decide
//	                 which need an AI review (§6.3, §6.5)
//	RunReview      — review one MR and persist the result (§10)
//	PublishReview  — put a persisted review into GitLab, idempotently (§10.4)
//	BuildDigest    — assemble a team's Slack digest and persist it (§13)
//	SendMessage    — deliver one persisted digest part (§6.4)
//
// # Two rules that shape everything here
//
// One MR is loaded once per run (§9.4). Every entry point creates a
// snapshotLoader whose cache is keyed by project+iid; the expensive calls
// (diffs, commits, worktree) happen only after the cheap classification has
// said a review is needed.
//
// Nothing reaches GitLab or Slack outside PublishReview and SendMessage. A
// review that must not be published is still persisted (status='dry_run'), which
// is what stops the scanner from re-reviewing the same head SHA every five
// minutes.
package service

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sxwebdev/ai-reviewer/internal/coverage"
	"github.com/sxwebdev/ai-reviewer/internal/dbtypes"
	"github.com/sxwebdev/ai-reviewer/internal/domain"
	"github.com/sxwebdev/ai-reviewer/internal/git"
	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/match"
	"github.com/sxwebdev/ai-reviewer/internal/review"
	"github.com/sxwebdev/ai-reviewer/internal/slack"
	"github.com/sxwebdev/ai-reviewer/internal/store"
	"github.com/tkcrm/mx/logger"
)

// Status values used across this package. The closed sets, their meanings and
// their validation live in internal/dbtypes — the columns have no CHECK, so
// there must be exactly one place that decides what a status may be. These are
// plain string aliases of those constants so the rest of the package can keep
// comparing against the `string` fields the generated models carry; adding a
// value here without adding it there produces a compile error, which is the
// point.
const (
	// StatusReviewed means the review is persisted and publication is pending.
	StatusReviewed = string(dbtypes.ReviewReviewed)
	// StatusSucceeded is set last, after the summary note carrying the review
	// marker has been posted — so it can never be a false success.
	StatusSucceeded = string(dbtypes.ReviewSucceeded)
	// StatusDryRun means the review ran and was persisted but must never reach
	// GitLab.
	StatusDryRun = string(dbtypes.ReviewDryRun)
	// StatusFailed records an attempt that did not produce a result. Failed rows
	// are what the §6.5 backoff ladder counts.
	StatusFailed = string(dbtypes.ReviewFailed)
	// StatusAbandoned is the terminal state of a review that was computed and
	// persisted but can never be published — the MR was deleted, the project
	// archived, the token lost its scope. It keeps the SHA out of re-review (the
	// unique index excludes only 'failed') without keeping the row in the §6.3
	// sweep, and it is not a §6.5 strike: the review worked, GitLab did not.
	StatusAbandoned = string(dbtypes.ReviewAbandoned)
)

// Digest message statuses. See dbtypes.MessageStatus for the set and why it
// being closed is load-bearing for §6.4.
const (
	MessagePending = string(dbtypes.MessagePending)
	MessageSending = string(dbtypes.MessageSending)
	MessageSent    = string(dbtypes.MessageSent)
	MessageFailed  = string(dbtypes.MessageFailed)
	MessageDryRun  = string(dbtypes.MessageDryRun)
)

// Digest run statuses. See dbtypes.DigestRunStatus.
const (
	DigestBuilt   = string(dbtypes.DigestBuilt)
	DigestSent    = string(dbtypes.DigestSent)
	DigestPartial = string(dbtypes.DigestPartial)
	DigestDryRun  = string(dbtypes.DigestDryRun)
	DigestFailed  = string(dbtypes.DigestFailed)
)

// defaultStalePublishAfter is how long a review may sit in status='reviewed'
// before scan_repo re-enqueues its publication (§6.3 safety net).
const defaultStalePublishAfter = 15 * time.Minute

// defaultReviewGrace is the fallback for Config.ReviewGrace: comfortably longer
// than any review the queue permits, so a `review --local` run outside the queue
// is not mistaken for a crashed one either. The real value comes from
// jobs.ReviewTimeout via the composition root.
const defaultReviewGrace = 45 * time.Minute

// SlackAPI is the Slack surface this package needs. It is declared here rather
// than imported as *slack.Client so a test can drive delivery through an
// httptest server or a stub without the digest builder depending on either.
type SlackAPI interface {
	PostMessage(ctx context.Context, req slack.PostMessageRequest) (*slack.PostMessageResult, error)
}

// RiskSettings configures the deterministic risk score fed to the engine.
type RiskSettings struct {
	Enabled        bool
	HistoryCommits int
	SensitiveGlobs []string
}

// CoverageSettings configures opt-in changed-line coverage measurement. Running
// it executes the reviewed repository's test code, so it stays off by default.
type CoverageSettings struct {
	Enabled   bool
	Providers []string
	Options   coverage.Options
}

// Config carries the scalar settings the service needs. Everything here comes
// from internal/config; the service never reads configuration itself.
type Config struct {
	// Host is the GitLab base URL. It keys the git mirror cache and is not used
	// to build API URLs (the gitlab client owns those).
	Host string
	// Token is the GitLab PAT, used only to authenticate `git fetch` for the
	// mirror. API calls carry it inside the gitlab client.
	Token string
	// ReviewerUsername is this service's own GitLab account. It marks our own
	// notes in prompt context so a re-review can see what it said last time.
	ReviewerUsername string
	// ToolVersion and PipelineName are recorded in the review marker, which is
	// how a human reading the MR can tell which build and preset produced it.
	ToolVersion  string
	PipelineName string

	// Teams is the configured team list. It answers two questions: whether a
	// team has AI review switched on, and which Slack channel its digest goes to.
	Teams []domain.Team

	// SlackSendEnabled is service.slack_send_enabled (§13.5). When false the
	// digest is built and persisted with status='dry_run' and no delivery job
	// may be created.
	//
	// There is deliberately no AIReviewPublishEnabled twin: the effective
	// publish decision for one review is ReviewRequest.Publish, because job
	// args must win over config (§15) — a CLI `--publish` has to be able to
	// override a globally-off flag.
	SlackSendEnabled bool

	Profile  *review.Profile
	Pipeline review.PipelineConfig
	Context  review.ContextBudget
	Risk     RiskSettings
	Coverage CoverageSettings

	// AgentMode grants the model read-only repository access from a worktree.
	AgentMode    bool
	AllowedTools []string
	Model        string
	IgnoreGlobs  []string

	// StalePublishAfter overrides the 15m threshold of the §6.3 safety net.
	StalePublishAfter time.Duration

	// ReviewGrace is how long an attempt row written by startAttempt is believed
	// to belong to a review that is still running rather than to one that died
	// without a word. It must exceed the review job's own timeout, so the
	// composition root derives it from jobs.ReviewTimeout — this layer cannot
	// import internal/jobs, and duplicating the number as a local constant is how
	// the two silently drift apart.
	//
	// Zero falls back to defaultReviewGrace.
	ReviewGrace time.Duration

	// Now is injectable so backoff and digest timing are testable without
	// sleeping. Nil means time.Now.
	Now func() time.Time
}

// Deps are the collaborators the service orchestrates. They are grouped in a
// struct rather than spelled out as constructor arguments so that adding one
// does not break the jobs layer.
type Deps struct {
	// GitLab is required.
	GitLab gitlab.API
	// GraphQL is optional: nil (or gitlab.graphql_enabled=false) means every
	// reviewer keeps ReviewStateUnknown and the REST heuristic classifies them.
	GraphQL gitlab.GraphQLAPI
	// Slack and Matcher are required for the digest path only.
	Slack   SlackAPI
	Matcher match.UserMatcher
	// Store is required.
	Store *store.Store
	// Engine is required for RunReview.
	Engine *review.Engine
	// Cache is optional: without it there is no worktree (so no agent mode, no
	// skeptic pass, no coverage) and no interdiff on re-review.
	Cache *git.Cache
	Log   logger.Logger
}

// Service is the orchestration layer. It is safe for concurrent use: every
// entry point builds its own per-run state.
type Service struct {
	gl      gitlab.API
	gql     gitlab.GraphQLAPI
	slack   SlackAPI
	matcher match.UserMatcher
	st      *store.Store
	eng     *review.Engine
	cache   *git.Cache
	log     logger.Logger
	cfg     Config

	// teams indexes cfg.Teams by lower-cased name. Team names are validated
	// unique case-insensitively by the config layer.
	teams map[string]domain.Team
}

// New builds a Service. It fails fast on the dependencies that no entry point
// can work without; the optional ones degrade at the call site instead, so a
// review-only deployment does not need Slack and a Slack-only one does not need
// the engine.
func New(deps Deps, cfg Config) (*Service, error) {
	if deps.GitLab == nil {
		return nil, fmt.Errorf("service: GitLab client is required")
	}
	if deps.Store == nil {
		return nil, fmt.Errorf("service: store is required")
	}
	return newService(deps, cfg), nil
}

// newService applies the defaults and assembles the struct. It is separate from
// New so tests that exercise only the I/O-free collaborators (the snapshot
// loader) can build a Service without a database.
func newService(deps Deps, cfg Config) *Service {
	if cfg.Profile == nil {
		cfg.Profile = review.DefaultProfile()
	}
	if cfg.StalePublishAfter <= 0 {
		cfg.StalePublishAfter = defaultStalePublishAfter
	}
	if cfg.ReviewGrace <= 0 {
		cfg.ReviewGrace = defaultReviewGrace
	}
	log := deps.Log
	if log == nil {
		log = logger.Default()
	}

	teams := make(map[string]domain.Team, len(cfg.Teams))
	for _, t := range cfg.Teams {
		teams[strings.ToLower(t.Name)] = t
	}

	return &Service{
		gl:      deps.GitLab,
		gql:     deps.GraphQL,
		slack:   deps.Slack,
		matcher: deps.Matcher,
		st:      deps.Store,
		eng:     deps.Engine,
		cache:   deps.Cache,
		log:     log,
		cfg:     cfg,
		teams:   teams,
	}
}

func (s *Service) now() time.Time {
	if s.cfg.Now != nil {
		return s.cfg.Now()
	}
	return time.Now()
}

// team returns the configured team by name.
func (s *Service) team(name string) (domain.Team, bool) {
	t, ok := s.teams[strings.ToLower(strings.TrimSpace(name))]
	return t, ok
}

// aiReviewEnabled reports whether automated review is on for a team.
//
// An unknown team counts as enabled: RunReview is also reachable from the CLI
// for an arbitrary MR, and refusing an explicitly requested review because its
// repository is not in any team would be surprising. The scan path never hits
// this case — it always passes a configured team.
func (s *Service) aiReviewEnabled(team string) bool {
	t, ok := s.team(team)
	if !ok {
		// Said out loud, because from the queue's side this is how a review runs
		// for a team whose switch is off: a job enqueued before the team was
		// renamed or removed still carries the old name, finds no team, and is
		// treated as an explicit request. Rare, and impossible to work out from a
		// log that stays silent about it.
		s.log.Warnw("review requested for a team that is not configured; treating ai_review as enabled",
			"team", team)
		return true
	}
	return t.AIReview
}

// projectKey renders a repository reference as the GitLab API path segment:
// the numeric id when known, the URL-encoded full path otherwise.
func projectKey(id int64, path string) string {
	if id != 0 {
		return strconv.FormatInt(id, 10)
	}
	return url.PathEscape(strings.Trim(path, "/"))
}
