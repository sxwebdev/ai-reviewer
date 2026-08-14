package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sxwebdev/ai-reviewer/internal/config"
	"github.com/sxwebdev/ai-reviewer/internal/coverage"
	"github.com/sxwebdev/ai-reviewer/internal/domain"
	"github.com/sxwebdev/ai-reviewer/internal/git"
	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/jobs"
	"github.com/sxwebdev/ai-reviewer/internal/llm"
	"github.com/sxwebdev/ai-reviewer/internal/match"
	"github.com/sxwebdev/ai-reviewer/internal/metrics"
	"github.com/sxwebdev/ai-reviewer/internal/migrator"
	"github.com/sxwebdev/ai-reviewer/internal/postgres"
	"github.com/sxwebdev/ai-reviewer/internal/review"
	"github.com/sxwebdev/ai-reviewer/internal/service"
	"github.com/sxwebdev/ai-reviewer/internal/slack"
	"github.com/sxwebdev/ai-reviewer/internal/store"
	"github.com/sxwebdev/ai-reviewer/internal/version"
	"github.com/sxwebdev/ai-reviewer/sql"
)

// Runtime is the wired process: the Postgres pool, the store, the service layer
// and the River jobs service. `start` registers its pieces with the launcher;
// `review --local` uses the same objects in-process.
type Runtime struct {
	Postgres *postgres.Postgres
	Store    *store.Store
	GitLab   *gitlab.Client
	Service  *service.Service
	Jobs     *jobs.Service
}

// Close releases the runtime's resources. The launcher stops the Postgres
// service itself; this is for the CLI paths that never build a launcher.
func (r *Runtime) Close(ctx context.Context) {
	if r == nil || r.Postgres == nil {
		return
	}
	_ = r.Postgres.Stop(ctx)
}

// OpenPostgres connects the pool. It is separated from the rest of the runtime
// because three CLI commands (`scan`, `digest`, a queued `review`) need only a
// database and a River client — they insert a job and exit, and building a
// GitLab client or an LLM subprocess wrapper for that would be waste.
func (a *App) OpenPostgres(ctx context.Context) (*postgres.Postgres, error) {
	pg, err := postgres.New(ctx, a.Config.Postgres.DSN(), postgres.WithMaxConns(a.poolSize()))
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	return pg, nil
}

// Pool sizing bounds.
const (
	// basePoolSize is postgres.New's own default. The pool never goes below it,
	// so the CLI paths — which need two connections at most — are unaffected.
	basePoolSize = 20
	// maxPoolSize stops a mis-set worker count from asking for more connections
	// than a stock PostgreSQL (max_connections = 100, shared with the other
	// replica) can give. Being capped and queuing beats being refused at connect
	// time.
	maxPoolSize = 200
)

// poolSize covers every connection the process can hold at once.
//
// The rule postgres.WithMaxConns states, made real: the four River worker pools
// plus River's own notifier and leader-election connections, plus a margin. A
// review counts three times, because two of its connections are pinned for the
// duration rather than borrowed per query:
//
//   - one does the work;
//   - one holds the review advisory lock, which keeps a queued review and a
//     `--local` one off the same head SHA, for the whole run;
//   - one holds the git mirror lock while the mirror is fetched or a worktree
//     is added (see gitCache) — shorter, but overlapping the other two.
func (a *App) poolSize() int32 {
	cfg := a.Config
	n := 3*cfg.Review.MaxParallel +
		cfg.Jobs.Queues.Default + cfg.Jobs.Queues.Publish + cfg.Jobs.Queues.Slack + 4
	return int32(min(max(n, basePoolSize), maxPoolSize)) //nolint:gosec // clamped to [20,200] on the line above
}

// migrateLock identifies the session advisory lock that serializes the whole
// migration sequence across replicas.
//
// It is deliberately a different key from the one internal/migrator takes for
// its own apply loop: this one has to stay held across *both* migrators, and
// re-taking the migrator's key from a second connection would self-deadlock.
// The nesting is safe because the order is fixed — nothing else ever takes this
// key, and every path that takes it does so before the migrator's — so no
// inversion is possible.
const (
	migrateLockClass int32 = 0x4149 // "AI"
	migrateLockObj   int32 = 0x5550 // "UP"
)

// Migrate applies the application migrations and then River's own, with one
// advisory lock held across both.
//
// They are two independent migrators over one pool: River versions its schema
// separately from ours, so `migrations up` must run both or the queue tables
// silently go missing.
//
// The lock spans both because rivermigrate takes none of its own: it reads the
// applied set and then applies each pending migration in its own transaction
// (verified in river@v0.40.0/rivermigrate). Two replicas starting together —
// which is exactly what `replicas: 2` plus a per-pod init container produces on
// a fresh install — would both read an empty river_migration and both run
// `CREATE TABLE river_job`, and the loser aborts. Kubernetes hides that by
// restarting the init container; a `migrations up` run by hand does not.
func (a *App) Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	// Session-scoped, so it must be held on one dedicated connection for the
	// whole sequence. If this process dies, Postgres releases it and the next
	// waiter proceeds.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration lock connection: %w", err)
	}
	defer conn.Release()
	// Deferred BEFORE the lock is taken, not after: a cancelled wait can leave
	// the lock granted even though the statement returned an error, and putting
	// a connection back in the pool while it still holds a session lock leaks
	// that lock until the connection is recycled — after which every startup
	// blocks here on something no code believes it holds. Unlocking a lock we
	// never got is a no-op, so the unconditional order is the safe one.
	//
	// Detached from ctx for the same reason the migrator does it: a cancelled
	// startup must still release, or the next pod waits.
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1, $2)`, migrateLockClass, migrateLockObj)
	}()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1, $2)`, migrateLockClass, migrateLockObj); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}

	m := migrator.New(a.Log, sql.MigrationsFS, sql.MigrationsPath, migrator.DataMigrations{})
	if err := m.MigrateUpAll(ctx, pool); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	if err := jobs.MigrateUp(ctx, pool); err != nil {
		return err
	}
	return nil
}

// Queue opens Postgres and an insert-only River client.
func (a *App) Queue(ctx context.Context) (*postgres.Postgres, *jobs.Queue, error) {
	pg, err := a.OpenPostgres(ctx)
	if err != nil {
		return nil, nil, err
	}
	if a.Config.Postgres.MigrateOnStart {
		if err := a.Migrate(ctx, pg.Pool); err != nil {
			_ = pg.Stop(ctx)
			return nil, nil, err
		}
	}
	q, err := jobs.NewQueue(pg.Pool)
	if err != nil {
		_ = pg.Stop(ctx)
		return nil, nil, err
	}
	return pg, q, nil
}

// Runtime assembles everything: Postgres, the store, the GitLab and Slack
// clients, the review engine, the git cache, the service layer and the jobs
// service. On any failure it releases what it already opened.
func (a *App) Runtime(ctx context.Context) (*Runtime, error) {
	cfg := a.Config

	pg, err := a.OpenPostgres(ctx)
	if err != nil {
		return nil, err
	}
	rt := &Runtime{Postgres: pg}
	fail := func(err error) (*Runtime, error) {
		rt.Close(ctx)
		return nil, err
	}

	if cfg.Postgres.MigrateOnStart {
		if err := a.Migrate(ctx, pg.Pool); err != nil {
			return fail(err)
		}
	}

	st, err := store.New(pg.Pool)
	if err != nil {
		return fail(fmt.Errorf("build store: %w", err))
	}
	rt.Store = st

	gl, err := a.gitlabClient()
	if err != nil {
		return fail(err)
	}
	rt.GitLab = gl

	// GraphQL is optional. With it off (or unsupported by the instance) every
	// reviewer keeps ReviewStateUnknown and the REST heuristic classifies them,
	// which is a degradation, not a failure.
	var gql gitlab.GraphQLAPI
	if cfg.GitLab.GraphQLEnabled {
		gql = gl.GraphQL()
	}

	slackAPI, matcher := a.slack()

	engine := review.NewEngine(a.llmClient(), a.Log)
	cache := a.gitCache(pg.Pool)

	svc, err := service.New(service.Deps{
		GitLab:  gl,
		GraphQL: gql,
		Slack:   slackAPI,
		Matcher: matcher,
		Store:   st,
		Engine:  engine,
		Cache:   cache,
		Log:     a.Log,
	}, a.serviceConfig(ctx, gl))
	if err != nil {
		return fail(err)
	}
	rt.Service = svc

	jobsSvc, err := jobs.NewService(a.Log, a.jobsConfig(), pg.Pool, jobs.Deps{
		Reviewer: jobsAdapter{svc: svc},
		Scanner:  jobsAdapter{svc: svc},
		Digester: jobsAdapter{svc: svc},
	})
	if err != nil {
		return fail(err)
	}
	rt.Jobs = jobsSvc

	return rt, nil
}

// gitCache builds the repository cache with cross-process exclusion wired in.
//
// internal/git serializes work on one mirror within its own process, and takes
// an injected Locker for everything beyond it — the package stays database-free
// that way, and this is the one place that can supply the implementation. It is
// not optional in a deployed service: `scan_repo` enqueues one review per
// candidate MR of a repository, so N reviews of the same project reach for the
// same bare clone within milliseconds, and concurrent `git fetch` on one mirror
// does not merely duplicate work — it fails with
// `cannot lock ref 'refs/heads/…'` (measured: 5 of 6 concurrent fetches lose).
// The losers degrade silently to a diff-only review: no agent mode, no
// coverage, no interdiff, skeptic downgraded to reflect.
//
// The per-pod emptyDir in the reference manifest hides that today, because the
// in-process half is then enough. It stops hiding it the moment review.workdir
// is a shared volume or two replicas land on one node with a hostPath — and the
// symptom is a Warn line, not a failure, so nobody would notice.
func (a *App) gitCache(pool *pgxpool.Pool) *git.Cache {
	return git.NewCache(a.Config.Review.WorkDir, a.Log, git.WithLocker(jobs.NewLocker(pool)))
}

// GitLabClient builds a standalone REST client. The CLI uses it to resolve a
// merge request reference into a project id and a head SHA before enqueueing a
// review — without those two the job's uniqueness key would not match the
// scanner's, and the same MR would be reviewed twice in parallel.
func (a *App) GitLabClient() (*gitlab.Client, error) { return a.gitlabClient() }

// gitlabClient builds the REST client with the metrics observer attached.
func (a *App) gitlabClient() (*gitlab.Client, error) {
	cfg := a.Config.GitLab
	c, err := gitlab.New(gitlab.Config{
		Host:               cfg.BaseURL,
		Token:              cfg.Token.Unmask(),
		Timeout:            cfg.Timeout,
		InsecureSkipVerify: cfg.InsecureSkipVerify,
		CACertPath:         cfg.CACertPath,
		MaxAttempts:        cfg.MaxAttempts,
		MaxRetryAfter:      cfg.MaxRetryAfter,
		// The client hands over a templated path ("/projects/:key/merge_requests/:iid"),
		// never a concrete one, so the endpoint label stays low-cardinality. That
		// is the whole reason the seam is a callback here rather than a counter
		// inside internal/gitlab.
		Observer: func(endpoint, method string, status int, err error) {
			metrics.GitLabRequest(endpoint, method)
			if err != nil || status >= 400 {
				metrics.GitLabRequestError(endpoint, status)
			}
		},
	})
	if err != nil {
		return nil, fmt.Errorf("build gitlab client: %w", err)
	}
	return c, nil
}

// slack builds the Slack client, the workspace directory and the user matcher.
// All three are optional: without a token the digest still builds and persists,
// it simply cannot be delivered, and every mention degrades to a plain name.
func (a *App) slack() (service.SlackAPI, match.UserMatcher) {
	cfg := a.Config.Slack
	if !cfg.Token.IsSet() {
		return nil, nil
	}
	client, err := slack.New(slack.Config{Token: cfg.Token.Unmask()})
	if err != nil {
		// Only a missing token can fail here, and that is already excluded.
		a.Log.Warnw("slack client not built", "error", err)
		return nil, nil
	}
	// One directory instance per process: its singleflight is what stops two
	// teams' digests both calling users.list on the same cold cache.
	dir := slack.NewDirectory(client, slack.DirectoryConfig{TTL: cfg.DirectoryTTL})
	return client, match.New(slack.MatchDirectory(dir), cfg.UserMap)
}

// llmClient builds the Claude CLI wrapper under the configured auth mode.
func (a *App) llmClient() llm.Client {
	cfg := a.Config
	auth, err := llm.NewClaudeAuth(cfg.ClaudeAuthConfig())
	if err != nil {
		// Config validation (§7.3) rejects this before startup; if it somehow
		// gets here, fall back to the mode that adds no credentials at all
		// rather than running with a half-built environment.
		a.Log.Errorw("claude auth is not usable, falling back to the existing login", "error", err)
		auth = llm.ExistingLogin()
	}
	// Logged once, at startup, never per call: it names the mode and whether a
	// secret is present, never the secret.
	a.Log.Infow("claude authentication", "mode", auth.Describe())

	return llm.NewClaudeCLI(llm.ClaudeOptions{
		Bin:            cfg.LLM.Claude.Bin,
		Model:          cfg.LLM.Claude.Model,
		PermissionMode: cfg.LLM.Claude.PermissionMode,
		Timeout:        cfg.LLM.Timeout,
		ExtraArgs:      cfg.LLM.Claude.ExtraArgs,
		Auth:           auth,
	}, a.Log)
}

// serviceConfig maps the application config onto the service layer's.
func (a *App) serviceConfig(ctx context.Context, gl gitlab.API) service.Config {
	cfg := a.Config
	return service.Config{
		Host:             cfg.GitLab.BaseURL,
		Token:            cfg.GitLab.Token.Unmask(),
		ReviewerUsername: a.reviewerUsername(ctx, gl),
		ToolVersion:      version.Version,
		PipelineName:     cfg.Review.Pipeline.Mode,
		Teams:            Teams(cfg),
		SlackSendEnabled: cfg.Service.SlackSendEnabled,
		Profile:          profileFromConfig(cfg.Review),
		Pipeline:         pipelineFromConfig(cfg.Review),
		Context:          contextBudgetFromConfig(cfg.Review),
		Risk:             riskSettingsFromConfig(cfg.Review),
		Coverage:         coverageSettingsFromConfig(cfg.Review),
		AgentMode:        cfg.LLM.Claude.AgentMode,
		AllowedTools:     cfg.LLM.Claude.AllowedTools,
		Model:            cfg.LLM.Claude.Model,
		IgnoreGlobs:      cfg.Review.IgnoreGlobs,
	}
}

// reviewerUsername resolves the service account's own GitLab username, which
// marks our notes in prompt context so a re-review can see what it said last
// time.
//
// Best-effort by design: it is one call, bounded tightly, and a failure only
// costs that continuity. Making startup depend on GitLab being reachable would
// trade a degraded review for a pod that will not start.
func (a *App) reviewerUsername(ctx context.Context, gl gitlab.API) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	u, err := gl.CurrentUser(ctx)
	if err != nil {
		a.Log.Warnw("could not resolve the reviewer account; prior-review continuity is degraded",
			"error", err)
		return ""
	}
	return u.Username
}

// jobsConfig maps the application config onto the queue runtime's.
func (a *App) jobsConfig() jobs.Config {
	cfg := a.Config
	return jobs.Config{
		Teams:           Teams(cfg),
		ScanInterval:    cfg.Review.ScanInterval,
		CleanupInterval: cfg.Jobs.CleanupInterval,
		DrainTimeout:    cfg.Jobs.DrainTimeout,
		// The review queue is sized by review.max_parallel, not by a jobs.queues
		// entry: one knob for review concurrency rather than two that drift.
		ReviewWorkers:    cfg.Review.MaxParallel,
		DefaultWorkers:   cfg.Jobs.Queues.Default,
		PublishWorkers:   cfg.Jobs.Queues.Publish,
		SlackWorkers:     cfg.Jobs.Queues.Slack,
		PublishEnabled:   cfg.Service.AIReviewPublishEnabled,
		SlackSendEnabled: cfg.Service.SlackSendEnabled,
		WorkDir:          cfg.Review.WorkDir,
	}
}

// Teams maps the configured teams onto the pure domain type.
func Teams(cfg *config.Config) []domain.Team {
	out := make([]domain.Team, 0, len(cfg.Teams))
	for _, t := range cfg.Teams {
		out = append(out, domain.Team{
			Name:         t.Name,
			SlackChannel: t.SlackChannel,
			AIReview:     t.AIReview.Enabled,
			Repositories: t.Repositories,
		})
	}
	return out
}

// TeamByName finds a configured team, case-insensitively. Config validation
// guarantees names are unique under that comparison.
func TeamByName(cfg *config.Config, name string) (domain.Team, bool) {
	for _, t := range Teams(cfg) {
		if strings.EqualFold(t.Name, name) {
			return t, true
		}
	}
	return domain.Team{}, false
}

// TeamForRepository finds the team that owns a repository. Config validation
// guarantees a repository belongs to at most one team.
func TeamForRepository(cfg *config.Config, repository string) (domain.Team, bool) {
	for _, t := range Teams(cfg) {
		for _, r := range t.Repositories {
			if strings.EqualFold(r, repository) {
				return t, true
			}
		}
	}
	return domain.Team{}, false
}

// ErrNoTeams is returned by commands that need at least one configured team.
var ErrNoTeams = errors.New("no teams are configured")

// --- config → collaborator mapping -----------------------------------------

// profileFromConfig builds the reviewer profile from config, overriding the
// built-in defaults with the review.* settings an operator controls: comment
// language, max comments and severity threshold. Category enablement stays at
// the profile default — config's flags do not map onto the profile's category
// list one-to-one, and wiring them naively would drop whole categories.
func profileFromConfig(rc config.ReviewConfig) *review.Profile {
	p := review.DefaultProfile()
	if rc.PreferredCommentLanguage != "" {
		p.Language = rc.PreferredCommentLanguage
	}
	if rc.MaxComments > 0 {
		p.MaxComments = rc.MaxComments
	}
	if rc.SeverityThreshold != "" {
		p.SeverityThreshold = rc.SeverityThreshold
	}
	return p
}

// pipelineFromConfig resolves review.pipeline (including the mode presets) into
// the engine's pipeline config.
func pipelineFromConfig(rc config.ReviewConfig) review.PipelineConfig {
	p := rc.Pipeline
	out := review.PipelineConfig{
		MaxParallel:       p.MaxParallel,
		VerifyMode:        p.VerifyMode,
		VerifyMaxFindings: p.VerifyMaxFindings,
		Verifiers:         p.Verifiers,
	}
	// Tri-state completeness: an explicit "on" survives cheap mode (and, in the
	// engine, bypasses the intent-text gate); "auto" follows the preset.
	switch p.Completeness {
	case "on":
		out.Completeness = review.CompletenessOn
	case "off":
		out.Completeness = review.CompletenessOff
	default: // auto
		if p.Mode != "cheap" {
			out.Completeness = review.CompletenessAuto
		} else {
			out.Completeness = review.CompletenessOff
		}
	}
	switch p.Mode {
	case "cheap":
		out.Passes = []string{review.PassGeneral}
		out.VerifyMode = review.VerifyOff
	case "deep":
		out.Passes = []string{
			review.PassGeneral, review.PassCorrectness, review.PassConcurrency,
			review.PassSecurity, review.PassContracts,
		}
	case "custom":
		out.Passes = p.Passes
	default: // standard
		out.Passes = []string{review.PassGeneral, review.PassCorrectness}
	}
	return out
}

func contextBudgetFromConfig(rc config.ReviewConfig) review.ContextBudget {
	return review.ContextBudget{
		IncludeFullFiles:   rc.Context.IncludeFullFiles,
		MaxFileLines:       rc.Context.MaxFileLines,
		HunkWindowLines:    rc.Context.HunkWindowLines,
		MaxTotalBytes:      rc.Context.MaxTotalKB << 10,
		IncludeCommits:     rc.Context.IncludeCommits,
		IncludeDiscussions: rc.Context.IncludeDiscussions,
		MaxDiscussionBytes: rc.Context.MaxDiscussionKB << 10,
		IncludePriorReview: rc.Context.PriorReview,
		MaxInterdiffBytes:  rc.Context.InterdiffMaxKB << 10,
	}
}

func riskSettingsFromConfig(rc config.ReviewConfig) service.RiskSettings {
	return service.RiskSettings{
		Enabled:        rc.Risk.Enabled,
		HistoryCommits: rc.Risk.HistoryCommits,
		SensitiveGlobs: rc.Risk.SensitiveGlobs,
	}
}

func coverageSettingsFromConfig(rc config.ReviewConfig) service.CoverageSettings {
	return service.CoverageSettings{
		Enabled:   rc.Coverage.Enabled,
		Providers: rc.Coverage.Providers,
		Options: coverage.Options{
			Timeout:     rc.Coverage.Timeout,
			NodeInstall: rc.Coverage.Node.Install,
		},
	}
}
