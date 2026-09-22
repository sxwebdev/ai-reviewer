package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/sxwebdev/ai-reviewer/internal/config"
	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/linear"
	"github.com/sxwebdev/ai-reviewer/internal/match"
	"github.com/sxwebdev/ai-reviewer/internal/security"
	"github.com/sxwebdev/ai-reviewer/internal/slack"
	"github.com/sxwebdev/ai-reviewer/sql"
)

// serviceProbeTimeout bounds the whole service-layer half of the doctor. It is
// larger than probeTimeout because it covers a fan-out over every configured
// repository, but it must still be short enough that `doctor` stays a thing you
// run interactively.
const serviceProbeTimeout = 60 * time.Second

// repoProbeConcurrency bounds the parallel repository probes. A team service
// watches dozens of repositories, and firing all of them at once at a GitLab
// instance that is already unhealthy is how a diagnostic becomes an outage.
const repoProbeConcurrency = 8

// linearProbeTimeout is the Linear check's own slice of serviceProbeTimeout.
//
// The Linear client retries with backoff, so one unhealthy instance answering
// 503 can spend the *entire* service budget before returning — and the checks
// that run after it inherit an already-cancelled context, so `doctor` reported a
// perfectly healthy Slack workspace as `context deadline exceeded`. Diagnosing
// Slack because Linear is down is the exact opposite of the complete picture
// ServiceChecks promises, and the bound is what keeps one source's outage from
// spreading to every check behind it.
const linearProbeTimeout = 20 * time.Second

// linearTeamProbeConcurrency bounds the parallel team lookups, same reasoning as
// repoProbeConcurrency.
const linearTeamProbeConcurrency = 4

// checkCollector accumulates results in call order.
type checkCollector struct {
	mu     sync.Mutex
	checks []DoctorCheck
}

func (c *checkCollector) add(name string, status CheckStatus, format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checks = append(c.checks, DoctorCheck{
		Name:   name,
		Status: status,
		// Every detail goes through the redactor: a failed probe's error text is
		// the most likely place for a token to surface.
		Detail: security.Mask(fmt.Sprintf(format, args...)),
	})
}

// ServiceChecks runs the probes that need the service layer's collaborators:
// Postgres, GitLab, the configured repositories, pipeline visibility, Slack and
// the work directory (§15).
//
// It never returns an error. Each probe reports its own verdict, because the
// point of `doctor` is a complete picture: an operator with a broken GitLab
// token still needs to know whether Slack and Postgres are fine.
func (a *App) ServiceChecks(ctx context.Context) []DoctorCheck {
	ctx, cancel := context.WithTimeout(ctx, serviceProbeTimeout)
	defer cancel()

	col := &checkCollector{}

	a.checkPostgres(ctx, col)
	gl := a.checkGitLab(ctx, col)
	if gl != nil {
		a.checkRepositories(ctx, col, gl)
	}
	a.checkLinear(ctx, col)
	a.checkSlack(ctx, col)
	a.checkWorkdir(col)

	return col.checks
}

type linearDoctorAPI interface {
	Viewer(context.Context) (*linear.User, error)
	GetTeam(context.Context, string) (*linear.Team, error)
}

func (a *App) checkLinear(ctx context.Context, col *checkCollector) {
	if !usesLinear(a.Config) {
		col.add("linear", StatusOK, "not configured; skipped")
		return
	}
	client, err := a.linearClient()
	if err != nil {
		col.add("linear", StatusFail, "%s", err)
		return
	}
	checkLinearAPI(ctx, col, client, a.Config)
}

// linearTeamProbe is one configured UUID's verdict. Like repoProbe, the probes
// run in parallel and write into a pre-sized slice, so the summary reads in
// config order rather than completion order.
type linearTeamProbe struct {
	mapping string
	problem string
	// gate is the team's column order as the readiness gate reads it. Printed
	// separately from mapping because with a per-team gate "why did my merge
	// request not reach a reviewer" has no other answer available anywhere: the
	// configuration names only In Review, and which columns count as before it is
	// decided by the team's own board.
	gate string
	// gateWarn names states whose order could not be established. Those cards fail
	// open — reviewers are notified as before — which is safe but silent, and a
	// column Linear reports with a type this build does not know will stay that way
	// until somebody is told.
	gateWarn string
}

func checkLinearAPI(ctx context.Context, col *checkCollector, api linearDoctorAPI, cfg *config.Config) {
	// The budget is taken here rather than at the caller so it covers every
	// request this check makes and is exercised by the tests that drive it.
	ctx, cancel := context.WithTimeout(ctx, linearProbeTimeout)
	defer cancel()

	viewer, err := api.Viewer(ctx)
	if err != nil {
		col.add("linear", StatusFail, "authentication failed: %s", err)
		return
	}
	name := strings.TrimSpace(viewer.Name)
	if name == "" {
		name = viewer.ID
	}
	col.add("linear", StatusOK, "authenticated as %s", name)

	type target struct {
		team string
		id   string
	}
	var targets []target
	for _, appTeam := range cfg.Teams {
		for _, id := range appTeam.LinearTeamIDs {
			targets = append(targets, target{team: appTeam.Name, id: id})
		}
	}

	// Every mapping is probed, exactly like checkRepositories: returning on the
	// first bad UUID reported one broken team while hiding all the others, so an
	// operator fixing a five-team config learned about one problem per run — and
	// the mappings collected before it were thrown away, leaving no OK line at
	// all for the teams that were fine.
	probes := make([]linearTeamProbe, len(targets))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(linearTeamProbeConcurrency)
	for i, tg := range targets {
		g.Go(func() error {
			probes[i] = probeLinearTeam(gctx, api, tg.team, tg.id)
			return nil
		})
	}
	_ = g.Wait() // every probe records its own verdict; none returns an error

	var mappings, problems []string
	for _, p := range probes {
		if p.problem != "" {
			problems = append(problems, p.problem)
			continue
		}
		mappings = append(mappings, p.mapping)
	}
	// The gate line is emitted before any early return and carries the splits *and*
	// the warnings together. Suppressing the splits whenever something else was
	// broken made the line absent in exactly the situation an operator opens doctor
	// for — "why did my merge request not reach a reviewer" — and there is no other
	// place that answers it, because only the team's own board decides which columns
	// count as before In Review.
	var gates, gateWarns []string
	for _, p := range probes {
		if p.gate != "" {
			gates = append(gates, p.gate)
		}
		if p.gateWarn != "" {
			gateWarns = append(gateWarns, p.gateWarn)
		}
	}
	if len(gates) > 0 || len(gateWarns) > 0 {
		status := StatusOK
		detail := strings.Join(gates, "; ")
		if len(gateWarns) > 0 {
			status = StatusWarn
			if detail != "" {
				detail += "; "
			}
			detail += strings.Join(gateWarns, "; ")
		}
		col.add("linear review gate", status, "%s", detail)
	}

	if len(problems) > 0 {
		// The working mappings are named alongside, same rule as checkUserMap: a
		// summary that prints only what is broken leaves an operator unable to tell
		// a team that passed from one that was never probed.
		detail := fmt.Sprintf("%d of %d unusable: %s", len(problems), len(targets), strings.Join(problems, "; "))
		if len(mappings) > 0 {
			detail += "; resolved: " + strings.Join(mappings, ", ")
		}
		col.add("linear teams", StatusFail, "%s", detail)
		return
	}
	col.add("linear teams", StatusOK, "%s", strings.Join(mappings, ", "))
}

func probeLinearTeam(ctx context.Context, api linearDoctorAPI, appTeam, id string) linearTeamProbe {
	team, err := api.GetTeam(ctx, id)
	if err != nil {
		return linearTeamProbe{problem: fmt.Sprintf("%s → %s: %s", appTeam, id, security.Mask(err.Error()))}
	}
	// linear.NewWorkflow rather than a local scan for the In Review state: the
	// digest decides "before review" with this exact constructor, and a doctor that
	// re-implemented the check would pass on a board the digest cannot order.
	wf, err := linear.NewWorkflow(team.States)
	if err != nil {
		return linearTeamProbe{problem: fmt.Sprintf("%s → %s: %s", appTeam, team.Label(), err)}
	}

	probe := linearTeamProbe{mapping: fmt.Sprintf("%s → %s", appTeam, team.Label())}
	states := slices.Clone(team.States)
	// linear.CompareStates, not a Position sort: Position only ranks columns that
	// share a type, so sorting on it alone interleaves a backlog column with a
	// started one and the printed board stops matching the team's own.
	slices.SortStableFunc(states, linear.CompareStates)
	var before, atOrAfter, unknown []string
	for _, st := range states {
		name := strings.TrimSpace(st.Name)
		switch wf.Stage(st) {
		case linear.StageBeforeReview:
			before = append(before, name)
		case linear.StageReviewOrLater:
			atOrAfter = append(atOrAfter, name)
		case linear.StageUnknown:
			unknown = append(unknown, name)
		}
	}
	// The board is named as well as the service team: a service team may map several
	// linear_team_ids, and two segments both prefixed "payments" cannot be
	// attributed to a board — which is the one question this line exists to answer.
	label := fmt.Sprintf("%s → %s", appTeam, team.Label())
	probe.gate = fmt.Sprintf("%s: reviewed from [%s], parked with the author before [%s]",
		label, strings.Join(atOrAfter, ", "), strings.Join(before, ", "))
	if len(unknown) > 0 {
		probe.gateWarn = fmt.Sprintf("%s: cannot order [%s] against %q, so merge requests on those statuses keep notifying reviewers",
			label, strings.Join(unknown, ", "), linear.InReviewState)
	}
	return probe
}

// checkPostgres verifies the connection, the application's migration state and
// River's own tables. All three fail differently and are worth separating: a
// pod that connects but finds no river_job table starts, elects a leader and
// then fails every insert.
func (a *App) checkPostgres(ctx context.Context, col *checkCollector) {
	pg, err := a.OpenPostgres(ctx)
	if err != nil {
		col.add("postgres", StatusFail, "%s", err)
		return
	}
	defer func() { _ = pg.Stop(ctx) }()

	// Host and port only. The database name is deliberately left out: it
	// frequently equals the username, which is a registered secret, so printing
	// it renders as [REDACTED] and reads like a bug rather than a connection.
	col.add("postgres", StatusOK, "connected to %s:%s", a.Config.Postgres.Host, a.Config.Postgres.Port)

	pending, err := pendingMigrations(ctx, pg.Pool)
	switch {
	case err != nil:
		col.add("migrations", StatusFail, "%s", err)
	case len(pending) > 0:
		col.add("migrations", StatusFail, "%d pending: %s", len(pending), strings.Join(pending, ", "))
	default:
		col.add("migrations", StatusOK, "up to date")
	}

	if err := riverTablesPresent(ctx, pg.Pool); err != nil {
		col.add("river schema", StatusFail, "%s — run `ai-reviewer migrations up`", err)
	} else {
		col.add("river schema", StatusOK, "river_job is present")
	}
}

var migrationFileRe = regexp.MustCompile(`^(\d+)_([\w-]+)\.(up|down)\.sql$`)

// pendingMigrations names the migrations present in the binary but absent from
// schema_migrations.
func pendingMigrations(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	entries, err := fs.ReadDir(sql.MigrationsFS, sql.MigrationsPath)
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}

	known := map[int]string{}
	for _, e := range entries {
		m := migrationFileRe.FindStringSubmatch(e.Name())
		if m == nil || m[3] != "up" {
			continue
		}
		v, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, fmt.Errorf("invalid migration version in %q: %w", e.Name(), err)
		}
		known[v] = m[1] + "_" + m[2]
	}
	applied := map[int]struct{}{}
	rows, err := pool.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		// The table itself is created by the first migration run, so its absence
		// means "nothing applied", not a broken database.
		return sortedNames(known), nil //nolint:nilerr // reported as pending, not as an error
	}
	defer rows.Close()
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		applied[v] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for v := range applied {
		delete(known, v)
	}
	return sortedNames(known), nil
}

func sortedNames(m map[int]string) []string {
	versions := make([]int, 0, len(m))
	for v := range m {
		versions = append(versions, v)
	}
	sort.Ints(versions)
	out := make([]string, 0, len(versions))
	for _, v := range versions {
		out = append(out, m[v])
	}
	return out
}

func riverTablesPresent(ctx context.Context, pool *pgxpool.Pool) error {
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('river_job') IS NOT NULL`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return errors.New("river_job table is missing")
	}
	return nil
}

// checkGitLab verifies authentication and GraphQL availability, and returns the
// client so the repository probes can reuse it.
func (a *App) checkGitLab(ctx context.Context, col *checkCollector) gitlab.API {
	if a.Config.GitLab.BaseURL == "" || !a.Config.GitLab.Token.IsSet() {
		col.add("gitlab", StatusFail, "gitlab.base_url and gitlab.token are required")
		return nil
	}
	gl, err := a.gitlabClient()
	if err != nil {
		col.add("gitlab", StatusFail, "%s", err)
		return nil
	}

	user, err := gl.CurrentUser(ctx)
	if err != nil {
		col.add("gitlab", StatusFail, "GET /user: %s", err)
		return nil
	}
	col.add("gitlab", StatusOK, "authenticated as @%s (%s)", user.Username, a.Config.GitLab.BaseURL)

	a.checkGraphQL(ctx, col, gl)
	return gl
}

// checkGraphQL probes the reviewState query §9.2 depends on. A self-managed
// GitLab too old to answer it is a warning, not a failure: the service falls
// back to the REST heuristic, which is less precise but works.
func (a *App) checkGraphQL(ctx context.Context, col *checkCollector, gl *gitlab.Client) {
	if !a.Config.GitLab.GraphQLEnabled {
		col.add("gitlab graphql", StatusWarn, "disabled in config; reviewer states come from the REST heuristic")
		return
	}
	repo := firstRepository(a)
	if repo == "" {
		col.add("gitlab graphql", StatusWarn, "no repositories configured, so availability was not probed")
		return
	}

	// An empty iids list makes no request, so the probe needs a real MR to be
	// meaningful. Ask for iid 1: an unsupported instance errors regardless of
	// whether that MR exists, and a supported one answers with an empty map.
	if _, err := gl.GraphQL().ReviewStates(ctx, repo, []int64{1}); err != nil {
		if errors.Is(err, gitlab.ErrGraphQLUnsupported) {
			col.add("gitlab graphql", StatusWarn,
				"reviewState is unavailable on this instance; falling back to the REST heuristic (%s)", err)
			return
		}
		col.add("gitlab graphql", StatusWarn, "probe failed: %s", err)
		return
	}
	col.add("gitlab graphql", StatusOK, "reviewState is available")
}

func firstRepository(a *App) string {
	for _, t := range a.Config.Teams {
		if len(t.Repositories) > 0 {
			return t.Repositories[0]
		}
	}
	return ""
}

// repoProbe is one repository's verdict. The probes run in parallel but write
// into a pre-sized slice, so the summary reports in config order rather than in
// completion order.
type repoProbe struct {
	name      string
	status    CheckStatus
	detail    string
	pipeline  pipelineVisibility
	approvals approvalsVisibility
	// unlistable records that the merge-request listing itself was refused, which
	// is *not* the same as "this repository has no open merge requests" — and both
	// per-merge-request verdicts collapsed into that claim, so a 403 on the listing
	// printed "no open merge request to check /approvals against" at a repository
	// with forty of them.
	unlistable bool
}

type pipelineVisibility int

const (
	pipelineUnknown pipelineVisibility = iota // no open MR to look at, or the probe could not tell
	pipelineVisible
	pipelineHidden
	// pipelineNone — the merge request genuinely has no pipeline. GitLab omits
	// head_pipeline for that too, and reporting it as a permissions problem
	// tells operators of repositories with no CI to go change project
	// membership to fix something that is not broken.
	pipelineNone
)

// approvalsVisibility mirrors pipelineVisibility for GET /approvals, and exists
// for the same reason: the endpoint failing is invisible in normal operation but
// disables a whole digest feature. Every merge request then looks unapproved,
// which makes both Linear completion rules inert — reviewers keep being nudged
// after approving, and no author is ever asked to advance their card.
//
// `GET /projects/:id/merge_requests/:iid/approvals` is available on **every**
// GitLab tier; only approval *rules* are Premium (internal/gitlab/endpoints.go
// says so at the call site). So a refusal here is a **permissions** problem with
// the same remedy as hidden pipelines — at least Reporter — and reporting it as a
// licensing limit sends the operator to buy a tier they already have while the
// real fix is one membership change.
//
// `approvalsError` is separate from `approvalsUnknown` because collapsing them
// made a 5xx print "no open merge request to check /approvals against" at a
// repository with forty of them, and made an inconclusive repository disappear
// entirely as soon as another one answered — the silent gap this check exists to
// close.
type approvalsVisibility int

const (
	approvalsUnknown approvalsVisibility = iota // nothing to look at: no open MR
	approvalsVisible
	approvalsHidden // 401/403: the service account may not read approvals
	approvalsError  // asked, and the answer settled nothing
)

// checkRepositories resolves every configured repository in parallel and, for
// each, checks whether head_pipeline comes back (§20.2).
//
// The pipeline check is the non-obvious one: without at least Reporter on a
// private project, GitLab simply omits head_pipeline. Nothing errors, nothing
// retries — the "pipeline failed" line just never appears in a digest. That is
// exactly the kind of silent gap a diagnostic exists to find.
func (a *App) checkRepositories(ctx context.Context, col *checkCollector, gl gitlab.API) {
	type target struct {
		team string
		repo string
	}
	var targets []target
	for _, t := range a.Config.Teams {
		for _, r := range t.Repositories {
			targets = append(targets, target{team: t.Name, repo: r})
		}
	}
	if len(targets) == 0 {
		col.add("repositories", StatusFail, "no repositories are configured")
		return
	}

	probes := make([]repoProbe, len(targets))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(repoProbeConcurrency)

	for i, tg := range targets {
		g.Go(func() error {
			probes[i] = probeRepository(gctx, gl, tg.team, tg.repo)
			return nil
		})
	}
	_ = g.Wait() // every probe records its own verdict; none returns an error

	var unresolved []string
	var hidden []string
	var noPipeline []string
	var visible int
	var approvalsBlind []string
	var approvalsBroken []string
	var approvalsOK int
	var unlistable []string
	for _, p := range probes {
		if p.status == StatusFail {
			unresolved = append(unresolved, p.name+" ("+p.detail+")")
		}
		switch p.pipeline {
		case pipelineVisible:
			visible++
		case pipelineHidden:
			hidden = append(hidden, p.name)
		case pipelineNone:
			noPipeline = append(noPipeline, p.name)
		case pipelineUnknown:
			// No open merge request to inspect — nothing can be concluded.
		}
		if p.unlistable {
			unlistable = append(unlistable, p.name)
		}
		switch p.approvals {
		case approvalsVisible:
			approvalsOK++
		case approvalsHidden:
			approvalsBlind = append(approvalsBlind, p.name)
		case approvalsError:
			approvalsBroken = append(approvalsBroken, p.name)
		case approvalsUnknown:
			// No open merge request to inspect — nothing can be concluded.
		}
	}

	if len(unresolved) > 0 {
		col.add("repositories", StatusFail, "%d of %d unresolved: %s",
			len(unresolved), len(targets), strings.Join(unresolved, "; "))
	} else {
		col.add("repositories", StatusOK, "all %d resolved", len(targets))
	}

	switch {
	case len(hidden) > 0:
		col.add("pipeline visibility", StatusWarn,
			"pipelines are not readable in %s — the service account needs at least Reporter, or \"pipeline failed\" will never appear in a digest",
			strings.Join(hidden, ", "))
	case visible > 0 && len(noPipeline) > 0:
		col.add("pipeline visibility", StatusOK,
			"head_pipeline is visible in %d repository/-ies; %s simply has/have no pipeline on the merge request checked",
			visible, strings.Join(noPipeline, ", "))
	case visible > 0:
		col.add("pipeline visibility", StatusOK, "head_pipeline is visible in %d repository/-ies", visible)
	case len(noPipeline) > 0:
		// Readable, just idle. Not a warning: a repository without CI is a
		// legitimate configuration, and telling its operator to change project
		// membership sends them after a problem that does not exist.
		col.add("pipeline visibility", StatusOK,
			"no pipeline has run for the merge request checked in %s; nothing is being hidden",
			strings.Join(noPipeline, ", "))
	case len(unlistable) > 0:
		// Said instead of "no open merge request": the listing was refused, so
		// nothing is known about this repository's merge requests either way.
		col.add("pipeline visibility", StatusWarn,
			"the merge request listing was refused in %s, so head_pipeline could not be checked anywhere",
			strings.Join(unlistable, ", "))
	default:
		col.add("pipeline visibility", StatusWarn, "no open merge request to check head_pipeline against")
	}

	// The consequence clause is conditional on Linear actually being configured:
	// without it `linked` is always false, so neither completion rule can fire and
	// promising an operator that "no author is asked to advance their Linear card"
	// describes a feature they do not run.
	consequence := "every merge request reads as unapproved, so reviewers keep being nudged after approving"
	if usesLinear(a.Config) {
		consequence += " and no author is asked to advance their Linear card"
	}
	var approvalProblems []string
	if len(approvalsBlind) > 0 {
		approvalProblems = append(approvalProblems, fmt.Sprintf(
			"refused in %s — the service account needs at least Reporter (approvals are available on every GitLab tier, so this is access, not licensing); while it is refused, %s",
			strings.Join(approvalsBlind, ", "), consequence))
	}
	if len(approvalsBroken) > 0 {
		approvalProblems = append(approvalProblems, fmt.Sprintf(
			"could not be checked in %s — the request answered, but settled nothing",
			strings.Join(approvalsBroken, ", ")))
	}
	if len(unlistable) > 0 {
		approvalProblems = append(approvalProblems, fmt.Sprintf(
			"not reached in %s — the merge request listing was refused there",
			strings.Join(unlistable, ", ")))
	}
	switch {
	case len(approvalProblems) > 0:
		detail := strings.Join(approvalProblems, "; ")
		if approvalsOK > 0 {
			// Named alongside, same rule as checkUserMap and the Linear teams check: a
			// summary that prints only the bad half leaves an operator unable to tell a
			// repository that passed from one that was never asked.
			detail += fmt.Sprintf("; readable in %d other repository/-ies", approvalsOK)
		}
		col.add("approvals visibility", StatusWarn, "%s", detail)
	case approvalsOK > 0:
		col.add("approvals visibility", StatusOK, "approvals are readable in %d repository/-ies", approvalsOK)
	default:
		col.add("approvals visibility", StatusWarn, "no open merge request to check /approvals against")
	}
}

func probeRepository(ctx context.Context, gl gitlab.API, team, repo string) repoProbe {
	p := repoProbe{name: repo}

	key := projectKey(repo)
	proj, err := gl.GetProject(ctx, key)
	if err != nil {
		p.status = StatusFail
		p.detail = security.Mask(err.Error())
		return p
	}
	p.status = StatusOK
	p.detail = fmt.Sprintf("team %s, id %d", team, proj.ID)

	// head_pipeline only comes back from the MR *detail* endpoint, never the
	// list, so the probe needs two calls: one to find an open MR and one to
	// load it.
	open, err := gl.ListOpenMRs(ctx, key)
	if err != nil {
		p.pipeline, p.unlistable = pipelineUnknown, true
		return p
	}
	if len(open) == 0 {
		p.pipeline = pipelineUnknown
		return p
	}
	// Asked before the pipeline branches below return, so the answer is recorded
	// for every repository with an open merge request rather than only for the
	// ones whose pipeline story is inconclusive.
	switch _, err := gl.GetMRApprovals(ctx, key, open[0].IID); {
	case err == nil:
		p.approvals = approvalsVisible
	case isForbidden(err):
		p.approvals = approvalsHidden
	default:
		p.approvals = approvalsError
	}

	mr, err := gl.GetMR(ctx, key, open[0].IID)
	if err != nil {
		p.pipeline = pipelineUnknown
		return p
	}
	if mr.HeadPipeline != nil {
		p.pipeline = pipelineVisible
		return p
	}

	// head_pipeline: null answers two different questions with one silence —
	// "you cannot see pipelines" and "no pipeline exists for this commit". The
	// second is an ordinary repository without CI, and warning about it sends
	// its operator off to change project membership for nothing.
	//
	// /merge_requests/:iid/pipelines separates them: a token that is allowed to
	// read pipelines gets a list (possibly empty), one that is not gets 401/403.
	// Any other error leaves the question open rather than accusing anyone.
	p.pipeline = pipelineUnknown
	switch _, err := gl.ListMRPipelines(ctx, key, open[0].IID); {
	case err == nil:
		p.pipeline = pipelineNone
	case isForbidden(err):
		p.pipeline = pipelineHidden
	}
	return p
}

// isForbidden reports whether err is GitLab refusing on authentication or
// authorization grounds, as opposed to being unable to answer.
func isForbidden(err error) bool {
	var ae *gitlab.APIError
	return errors.As(err, &ae) &&
		(ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden)
}

// projectKey turns a configured repository into an API path segment: a numeric
// id passes through, a path is URL-escaped.
func projectKey(repo string) string {
	if _, err := strconv.ParseInt(repo, 10, 64); err == nil {
		return repo
	}
	return gitlab.MRRef{ProjectPath: repo}.ProjectKey()
}

// checkSlack verifies the token and that the bot is actually in every
// configured channel. Membership is the failure operators hit most: the token
// is valid, auth.test passes, and every post returns not_in_channel.
func (a *App) checkSlack(ctx context.Context, col *checkCollector) {
	if !a.Config.Slack.Token.IsSet() {
		status := StatusWarn
		detail := "no token configured; digests can be built but not delivered"
		if a.Config.Service.SlackSendEnabled {
			// Delivery is switched on and cannot possibly work.
			status = StatusFail
			detail = "service.slack_send_enabled is on but slack.token is empty"
		}
		col.add("slack", status, "%s", detail)
		return
	}

	client, err := slack.New(slack.Config{
		Token:    a.Config.Slack.Token.Unmask(),
		AppToken: a.Config.Slack.AppToken.Unmask(),
	})
	if err != nil {
		col.add("slack", StatusFail, "%s", err)
		return
	}
	info, err := client.AuthTest(ctx)
	if err != nil {
		col.add("slack", StatusFail, "auth.test: %s", err)
		return
	}
	col.add("slack", StatusOK, "authenticated as %s in %s", info.User, info.Team)

	var problems []string
	for _, team := range a.Config.Teams {
		conv, err := client.ConversationInfo(ctx, team.SlackChannel)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s → %s: %s", team.Name, team.SlackChannel, security.Mask(err.Error())))
			continue
		}
		if !conv.IsMember {
			problems = append(problems, fmt.Sprintf("%s → #%s: the bot is not a member", team.Name, conv.Name))
		}
	}
	if len(problems) > 0 {
		col.add("slack channels", StatusFail, "%s", strings.Join(problems, "; "))
	} else {
		col.add("slack channels", StatusOK, "the bot is a member of all %d channel(s)", len(a.Config.Teams))
	}

	checkSlackDirectory(ctx, col, client, a.Config.Slack.UserMap)
	a.checkSlackCommands(ctx, col, client)
}

// checkSlackCommands probes the in-chat commands: the app-level token, and the
// names the workspace has to have registered.
//
// Both halves are invisible until somebody types a command and nothing happens.
// The token is checked by opening a Socket Mode connection URL and not dialling
// it — apps.connections.open is the only method that accepts an app-level token,
// so it is also the only way to tell a valid one from a typo. The ticket it hands
// back expires unused, which costs nothing.
//
// The command names cannot be verified from here at all: Slack has no API that
// lists an app's slash commands, and they are registered in the app
// configuration rather than by this service. So they are printed — that is what
// makes "we typed /digest" answerable against "this deployment answers /all".
func (a *App) checkSlackCommands(ctx context.Context, col *checkCollector, client *slack.Client) {
	if !a.Config.Slack.AppToken.IsSet() {
		col.add("slack commands", StatusWarn,
			"off: slack.app_token is empty, so no Socket Mode connection is opened and %s / %s answer nothing",
			a.Config.Slack.Commands.Team, a.Config.Slack.Commands.Mine)
		return
	}
	if _, err := client.OpenSocketURL(ctx); err != nil {
		var apiErr *slack.APIError
		if errors.As(err, &apiErr) && apiErr.Code == "not_allowed_token_type" {
			// The single most likely mistake: a bot token pasted into app_token.
			// Both tokens are needed and only one of them opens a socket.
			col.add("slack commands", StatusFail,
				"slack.app_token is not an app-level token — Socket Mode needs the xapp-… token "+
					"from the app's Basic Information page (scope connections:write), not the bot token")
			return
		}
		col.add("slack commands", StatusFail, "apps.connections.open: %s", security.Mask(err.Error()))
		return
	}
	col.add("slack commands", StatusOK,
		"Socket Mode is reachable; this deployment answers %s (team digest, posted to the channel) "+
			"and %s (the caller's own rows, shown only to them)",
		a.Config.Slack.Commands.Team, a.Config.Slack.Commands.Mine)
}

// checkSlackDirectory reads the workspace directory and resolves every user_map
// entry against it.
//
// It exists because `doctor` used to stop at auth.test and channel membership,
// and passed green while every mention in the digest was broken: the token was
// missing the users:read scope, so users.list failed at the first digest slot and
// each person was named without a ping. A diagnostic that cannot see the most common Slack
// misconfiguration is not doing its job.
//
// Split out from checkSlack and taking slack.UserLister (the interface
// slack.Directory already defines) so it can be driven by a fake — checkSlack
// itself builds a client against the real API.
func checkSlackDirectory(ctx context.Context, col *checkCollector, lister slack.UserLister, userMap map[string]string) {
	// The same Directory the digest uses, so the indexes checked here are the
	// indexes matching will actually consult — including the deactivated-and-bot
	// filtering, which is why a resolved entry proves the account is live.
	snap, err := slack.NewDirectory(lister, slack.DirectoryConfig{}).Snapshot(ctx)
	if err != nil {
		var apiErr *slack.APIError
		if errors.As(err, &apiErr) && apiErr.Needed != "" {
			col.add("slack directory", StatusFail,
				"users.list needs the %s scope (token has: %s) — without it every digest names people without mentioning them",
				apiErr.Needed, apiErr.Provided)
			return
		}
		col.add("slack directory", StatusFail, "users.list: %s", err)
		return
	}

	withEmail := 0
	for _, u := range snap.Users {
		if strings.TrimSpace(u.Profile.Email) != "" {
			withEmail++
		}
	}
	switch {
	case len(snap.Users) == 0:
		col.add("slack directory", StatusFail, "users.list returned no active members")
	case withEmail == 0:
		// Slack answers 200 with the field simply absent when users:read.email is
		// missing, so this is the only way to tell the two scopes apart. Email is
		// the matcher's one exact identifier; without it matching falls back to
		// name comparison and quietly gets worse.
		col.add("slack directory", StatusWarn,
			"%d active members, but not one exposes an email: add the users:read.email scope, "+
				"or matching falls back to comparing names", len(snap.Users))
	default:
		col.add("slack directory", StatusOK, "%d active members, %d with an email", len(snap.Users), withEmail)
	}

	checkUserMap(col, snap, userMap)
}

// errUserMapEntryIgnored marks a value the *runtime* throws away rather than
// chokes on, which is why it may not fail the check: match.New drops a blank
// override, the matcher falls through to the ordinary probe ladder and the person
// is matched normally. Failing the run for it made `doctor` exit non-zero — and
// blocked every deploy gate reading that exit code — over a no-op. It is still
// reported, as a warning: somebody meant to type an id there.
//
// Same rule as the workdir/agent-mode verdict: doctor's exit code may only
// diverge from what `start` will actually do when the deployment is broken.
var errUserMapEntryIgnored = errors.New("blank value: the entry is ignored, and the user is matched the ordinary way")

// checkUserMap resolves slack.user_map exactly the way the matcher does, and
// prints what each entry became.
//
// The map is the escape hatch for people the directory cannot match, so a broken
// entry is doubly invisible: the digest names the person without a ping, which is
// precisely what the entry was added to prevent, and looks identical to having no
// entry at all.
func checkUserMap(col *checkCollector, snap *slack.Snapshot, userMap map[string]string) {
	if len(userMap) == 0 {
		return
	}
	names := slices.Sorted(maps.Keys(userMap))

	var resolved, ignored, problems []string
	for _, gitlabUser := range names {
		value := strings.TrimSpace(userMap[gitlabUser])
		u, err := resolveUserMapValue(snap, value)
		switch {
		case errors.Is(err, errUserMapEntryIgnored):
			// The value is blank, so printing it would render as "jsmith → :".
			ignored = append(ignored, fmt.Sprintf("%s: %s", gitlabUser, err))
		case err != nil:
			problems = append(problems, fmt.Sprintf("%s → %s: %s", gitlabUser, value, err))
		default:
			resolved = append(resolved, fmt.Sprintf("%s → %s (%s)", gitlabUser, u.ID, u.DisplayName()))
		}
	}

	summary := "no entries resolved"
	if len(resolved) > 0 {
		summary = fmt.Sprintf("%d entr%s resolved: %s",
			len(resolved), map[bool]string{true: "y", false: "ies"}[len(resolved) == 1], strings.Join(resolved, ", "))
	}

	switch {
	case len(problems) > 0:
		// The ignored entries are named alongside, or a warning-worthy entry stays
		// hidden until the failing one is fixed and the check is run again.
		col.add("slack user_map", StatusFail, "%s", strings.Join(slices.Concat(problems, ignored), "; "))
	case len(ignored) > 0:
		col.add("slack user_map", StatusWarn, "%s; %s", summary, strings.Join(ignored, "; "))
	default:
		col.add("slack user_map", StatusOK, "%s", summary)
	}
}

// resolveUserMapValue resolves one user_map value against the directory, with
// match.ParseOverride — the matcher's own grammar, not a copy of it — deciding
// what the value is.
//
// The classification used to be reimplemented here: the @-stripping, the "an @
// elsewhere means email" rule, the ambiguity policy, all of it, sharing only the
// id predicate with the matcher. That is a drift waiting to happen in the one
// place that cannot afford it, because verifying this grammar is the entire
// purpose of the check: a doctor that classifies a value differently from the
// matcher either blesses a map that mentions nobody or fails one that works.
// Only the *lookup* is doctor's own, and only because it reads a
// slack.Snapshot where the matcher reads a match.Directory.
//
// Two forms make the two sides differ legitimately, both in the safe direction.
// An id: the matcher takes it at face value so an override survives users.list
// being unavailable, which leaves doctor as the only side that ever checks one
// exists. And a blank value: the runtime discards it, so doctor may only warn —
// see errUserMapEntryIgnored.
func resolveUserMapValue(snap *slack.Snapshot, value string) (slack.User, error) {
	o := match.ParseOverride(value)
	switch o.Form {
	case match.OverrideID:
		u, ok := snap.ByID(o.Key)
		if !ok {
			return slack.User{}, errors.New("no active Slack account has this user id")
		}
		return u, nil
	case match.OverrideEmail:
		u, ok := snap.ByEmail(o.Key)
		if !ok {
			return slack.User{}, errors.New("no active Slack account has this email (users:read.email may be missing)")
		}
		return u, nil
	case match.OverrideHandle:
		switch found := snap.ByHandle(o.Key); len(found) {
		case 1:
			return found[0], nil
		case 0:
			return slack.User{}, errors.New("no active Slack account has this handle")
		default:
			// Never resolved by picking one, same rule as the matcher.
			return slack.User{}, fmt.Errorf("%d accounts share this handle; use the Slack user id", len(found))
		}
	case match.OverrideEmpty:
		return slack.User{}, errUserMapEntryIgnored
	default:
		return slack.User{}, fmt.Errorf("unclassifiable value (%s)", o.Form)
	}
}

// checkWorkdir verifies review.workdir is writable. It is where mirrors and
// worktrees live, so a read-only mount fails every review at clone time.
//
// The probe itself lives in workdir.go, shared with the `start` gate: doctor
// reporting one verdict while start enforces another is the failure this
// diagnostic exists to prevent. So is the *severity*, which is why agent mode is
// read here at all: checkAgentWorkdir refuses to start only when
// llm.claude.agent_mode is on, because with it off nothing is cloned and no
// worktree is created, and this check used to fail regardless. A deliberately
// diff-only deployment on a read-only mount therefore ran perfectly while
// `doctor` exited non-zero — and every deploy gate built on that exit code
// blocked it.
func (a *App) checkWorkdir(col *checkCollector) {
	dir := a.Config.Review.WorkDir
	if err := ensureWorkdirWritable(dir); err != nil {
		if !a.Config.LLM.Claude.AgentMode {
			// Exactly what start concludes: nothing here is used, so nothing is
			// broken. Still said out loud, because the reason it is survivable is
			// one config flag away from no longer being true — and an operator who
			// reads only "not writable" goes off to fix a mount that is fine.
			col.add("workdir", StatusWarn,
				"%s — but llm.claude.agent_mode is off, so nothing is cloned or checked out here and "+
					"reviews read the diff only; a writable path is required before turning agent mode on", err)
			return
		}
		col.add("workdir", StatusFail, "%s", err)
		return
	}
	if !a.Config.LLM.Claude.AgentMode {
		// Not a failure — with agent mode off nothing writes here — but worth
		// saying, because it also means reviews see only the diff.
		col.add("workdir", StatusWarn, "%s is writable, but llm.claude.agent_mode is off: reviews read the diff only",
			filepath.Clean(dir))
		return
	}
	col.add("workdir", StatusOK, "%s is writable", filepath.Clean(dir))
}
