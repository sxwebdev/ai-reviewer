package app

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/sxwebdev/ai-reviewer/internal/config"
	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
)

// TestProbeRepositoryPipelineVisibility covers §20.2, the failure that breaks
// nothing and reports nothing: without at least Reporter on a private project
// GitLab simply omits head_pipeline, so the "pipeline failed" line never
// appears in a digest and no error is ever raised. Doctor is the only place
// that can catch it.
func TestProbeRepositoryPipelineVisibility(t *testing.T) {
	t.Parallel()
	const repo = "backend/payments"
	key := projectKey(repo)

	newFake := func() *gitlab.FakeClient {
		f := gitlab.NewFake()
		f.Projects[key] = &gitlab.Project{ID: 42, PathWithNamespace: repo}
		return f
	}

	t.Run("visible", func(t *testing.T) {
		t.Parallel()
		f := newFake()
		f.OpenMRs[key] = []gitlab.MergeRequest{{IID: 7}}
		f.MRs[key+"/7"] = &gitlab.MergeRequest{IID: 7, HeadPipeline: &gitlab.Pipeline{ID: 1, Status: "failed"}}

		got := probeRepository(t.Context(), f, "payments", repo)
		if got.status != StatusOK {
			t.Errorf("status = %v (%s)", got.status, got.detail)
		}
		if got.pipeline != pipelineVisible {
			t.Errorf("pipeline = %v, want visible", got.pipeline)
		}
		if !strings.Contains(got.detail, "42") {
			t.Errorf("detail %q does not report the resolved project id", got.detail)
		}
	})

	t.Run("hidden", func(t *testing.T) {
		t.Parallel()
		f := newFake()
		f.OpenMRs[key] = []gitlab.MergeRequest{{IID: 7}}
		// The detail endpoint answers, but without head_pipeline, and the
		// pipelines endpoint refuses — exactly what a token below Reporter sees.
		f.MRs[key+"/7"] = &gitlab.MergeRequest{IID: 7}

		got := probeRepository(t.Context(), refusingPipelines{f, 403}, "payments", repo)
		if got.pipeline != pipelineHidden {
			t.Errorf("pipeline = %v, want hidden", got.pipeline)
		}
	})

	// The finding this disambiguation exists for: head_pipeline is also absent
	// for a repository that simply has no CI, and reporting that as a
	// permissions problem sends its operator off to change project membership
	// to fix something that is not broken.
	t.Run("no pipeline has ever run", func(t *testing.T) {
		t.Parallel()
		f := newFake()
		f.OpenMRs[key] = []gitlab.MergeRequest{{IID: 7}}
		f.MRs[key+"/7"] = &gitlab.MergeRequest{IID: 7}
		// The pipelines endpoint answers (so pipelines ARE readable) with nothing.

		got := probeRepository(t.Context(), f, "payments", repo)
		if got.pipeline != pipelineNone {
			t.Errorf("pipeline = %v, want none — a repository without CI is not a permissions problem", got.pipeline)
		}
	})

	// A 500 or a timeout on the pipelines endpoint answers nothing, so the
	// probe must stay silent rather than accuse the token.
	t.Run("an unrelated failure concludes nothing", func(t *testing.T) {
		t.Parallel()
		f := newFake()
		f.OpenMRs[key] = []gitlab.MergeRequest{{IID: 7}}
		f.MRs[key+"/7"] = &gitlab.MergeRequest{IID: 7}

		got := probeRepository(t.Context(), refusingPipelines{f, 503}, "payments", repo)
		if got.pipeline != pipelineUnknown {
			t.Errorf("pipeline = %v, want unknown", got.pipeline)
		}
	})

	t.Run("no open merge request", func(t *testing.T) {
		t.Parallel()
		f := newFake()
		// Nothing to inspect: the probe must say "unknown", not "hidden", or
		// every quiet repository would raise a permissions warning.
		got := probeRepository(t.Context(), f, "payments", repo)
		if got.status != StatusOK {
			t.Errorf("status = %v", got.status)
		}
		if got.pipeline != pipelineUnknown {
			t.Errorf("pipeline = %v, want unknown", got.pipeline)
		}
	})

	t.Run("unresolvable", func(t *testing.T) {
		t.Parallel()
		f := gitlab.NewFake() // no project registered
		got := probeRepository(t.Context(), f, "payments", repo)
		if got.status != StatusFail {
			t.Errorf("status = %v, want fail", got.status)
		}
		if got.name != repo {
			t.Errorf("name = %q, want the configured repository so the report can name it", got.name)
		}
	})
}

// refusingPipelines is the fake with one endpoint answering a status. It is how
// the "the token cannot read pipelines" case is reproduced: the fake's own
// ListMRPipelines always succeeds.
type refusingPipelines struct {
	*gitlab.FakeClient
	status int
}

func (r refusingPipelines) ListMRPipelines(context.Context, string, int64) ([]gitlab.Pipeline, error) {
	return nil, &gitlab.APIError{Status: r.status, Method: "GET", Path: "/pipelines"}
}

// refusingApprovals is the same trick for GET /approvals. The endpoint exists on
// every GitLab tier, so a refusal is an access problem — the same one that hides
// pipelines — and it lasts exactly as long as the missing permission.
type refusingApprovals struct {
	*gitlab.FakeClient
	status int
}

func (r refusingApprovals) GetMRApprovals(context.Context, string, int64) (*gitlab.Approvals, error) {
	return nil, &gitlab.APIError{Status: r.status, Method: "GET", Path: "/approvals"}
}

// The approvals endpoint failing costs the digest both Linear completion rules
// and raises nothing anywhere, so doctor is the only place an operator can learn
// about it — the same argument as pipeline visibility above.
func TestProbeRepositoryApprovalsVisibility(t *testing.T) {
	t.Parallel()
	const repo = "backend/payments"
	key := projectKey(repo)

	newFake := func() *gitlab.FakeClient {
		f := gitlab.NewFake()
		f.Projects[key] = &gitlab.Project{ID: 42, PathWithNamespace: repo}
		f.OpenMRs[key] = []gitlab.MergeRequest{{IID: 7}}
		f.MRs[key+"/7"] = &gitlab.MergeRequest{IID: 7, HeadPipeline: &gitlab.Pipeline{ID: 1}}
		return f
	}

	t.Run("readable", func(t *testing.T) {
		t.Parallel()
		if got := probeRepository(t.Context(), newFake(), "payments", repo); got.approvals != approvalsVisible {
			t.Errorf("approvals = %v, want visible", got.approvals)
		}
	})

	for _, status := range []int{401, 403} {
		t.Run("refused "+strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			got := probeRepository(t.Context(), refusingApprovals{newFake(), status}, "payments", repo)
			if got.approvals != approvalsHidden {
				t.Errorf("approvals = %v, want hidden", got.approvals)
			}
		})
	}

	// A 500 concludes nothing about access — but it is not "nothing to check"
	// either, and collapsing the two made a 502 at a repository with forty open
	// merge requests print "no open merge request to check /approvals against".
	t.Run("an unrelated failure is reported, not silently unknown", func(t *testing.T) {
		t.Parallel()
		got := probeRepository(t.Context(), refusingApprovals{newFake(), 503}, "payments", repo)
		if got.approvals != approvalsError {
			t.Errorf("approvals = %v, want error", got.approvals)
		}
	})

	t.Run("no open merge request", func(t *testing.T) {
		t.Parallel()
		f := gitlab.NewFake()
		f.Projects[key] = &gitlab.Project{ID: 42, PathWithNamespace: repo}
		if got := probeRepository(t.Context(), f, "payments", repo); got.approvals != approvalsUnknown {
			t.Errorf("approvals = %v, want unknown", got.approvals)
		}
	})

	// The pipeline story and the approvals story are independent, and the probe
	// answers both: an early return on one used to leave the other unasked.
	t.Run("both are reported from one probe", func(t *testing.T) {
		t.Parallel()
		f := newFake()
		f.MRs[key+"/7"] = &gitlab.MergeRequest{IID: 7} // head_pipeline absent
		got := probeRepository(t.Context(), refusingPipelines{f, 403}, "payments", repo)
		if got.pipeline != pipelineHidden || got.approvals != approvalsVisible {
			t.Errorf("probe = pipeline %v, approvals %v; want hidden and visible", got.pipeline, got.approvals)
		}
	})
}

// The aggregate line has to name the consequence, not the endpoint: "GET
// /approvals is refused" alone reads like a cosmetic gap.
func TestCheckRepositoriesReportsBlindApprovals(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	a := &App{Config: cfg, Log: quietLogger()}

	f := gitlab.NewFake()
	for _, repo := range []string{"backend/payments", "platform/auth", "platform/gateway"} {
		key := projectKey(repo)
		f.Projects[key] = &gitlab.Project{ID: 1, PathWithNamespace: repo}
		f.OpenMRs[key] = []gitlab.MergeRequest{{IID: 1}}
		f.MRs[key+"/1"] = &gitlab.MergeRequest{IID: 1, HeadPipeline: &gitlab.Pipeline{ID: 1}}
	}

	col := &checkCollector{}
	a.checkRepositories(t.Context(), col, refusingApprovals{f, 403})

	var check DoctorCheck
	for _, c := range col.checks {
		if c.Name == "approvals visibility" {
			check = c
		}
	}
	if check.Status != StatusWarn {
		t.Fatalf("approvals visibility = %+v, want a warning", check)
	}
	for _, want := range []string{"backend/payments", "nudged after approving", "at least Reporter"} {
		if !strings.Contains(check.Detail, want) {
			t.Errorf("detail is missing %q: %s", want, check.Detail)
		}
	}
	// The endpoint is available on every GitLab tier, so the remedy is a membership
	// change. Naming licensing sends the operator to buy a tier they already have
	// while three reviewers keep getting nudged.
	for _, unwanted := range []string{"paid", "Premium", "licensing limit"} {
		if strings.Contains(check.Detail, unwanted) {
			t.Errorf("detail blames %q, but approvals are a Free-tier endpoint: %s", unwanted, check.Detail)
		}
	}
	// No team declares linear_team_ids here, so `linked` is always false and neither
	// completion rule can fire: promising the operator a Linear consequence would
	// describe a feature they do not run.
	if strings.Contains(check.Detail, "Linear") {
		t.Errorf("detail promises a Linear consequence on a deployment without Linear: %s", check.Detail)
	}
}

// ...and names it when Linear *is* configured, since that is the half that costs
// the team its author nudges.
func TestCheckRepositoriesNamesTheLinearConsequenceWhenLinearIsConfigured(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	cfg.Teams[0].LinearTeamIDs = []string{"3f1b4a9e-1c2d-4e5f-8a9b-0c1d2e3f4a5b"}
	a := &App{Config: cfg, Log: quietLogger()}

	f := gitlab.NewFake()
	key := projectKey("backend/payments")
	f.Projects[key] = &gitlab.Project{ID: 1, PathWithNamespace: "backend/payments"}
	f.OpenMRs[key] = []gitlab.MergeRequest{{IID: 1}}
	f.MRs[key+"/1"] = &gitlab.MergeRequest{IID: 1, HeadPipeline: &gitlab.Pipeline{ID: 1}}

	col := &checkCollector{}
	a.checkRepositories(t.Context(), col, refusingApprovals{f, 403})

	for _, c := range col.checks {
		if c.Name == "approvals visibility" && !strings.Contains(c.Detail, "Linear card") {
			t.Errorf("detail omits the Linear consequence: %s", c.Detail)
		}
	}
}

// refusingListing answers the merge-request listing with a status. A refused
// listing is not "this repository has no open merge requests", and reporting it as
// such is a claim the operator can see is false.
type refusingListing struct {
	*gitlab.FakeClient
	status int
}

func (r refusingListing) ListOpenMRs(context.Context, string) ([]gitlab.MergeRequest, error) {
	return nil, &gitlab.APIError{Status: r.status, Method: "GET", Path: "/merge_requests"}
}

func TestCheckRepositoriesDistinguishesARefusedListingFromNoMergeRequests(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	a := &App{Config: cfg, Log: quietLogger()}

	f := gitlab.NewFake()
	for _, repo := range []string{"backend/payments", "platform/auth", "platform/gateway"} {
		key := projectKey(repo)
		f.Projects[key] = &gitlab.Project{ID: 1, PathWithNamespace: repo}
	}

	col := &checkCollector{}
	a.checkRepositories(t.Context(), col, refusingListing{f, 403})

	byName := map[string]DoctorCheck{}
	for _, c := range col.checks {
		byName[c.Name] = c
	}
	for _, name := range []string{"pipeline visibility", "approvals visibility"} {
		check, ok := byName[name]
		if !ok {
			t.Fatalf("no %q check: %+v", name, col.checks)
		}
		if !strings.Contains(check.Detail, "listing was refused") {
			t.Errorf("%s does not name the refused listing: %s", name, check.Detail)
		}
		if strings.Contains(check.Detail, "no open merge request") {
			t.Errorf("%s claims there was nothing to check: %s", name, check.Detail)
		}
	}

	// A repository that genuinely has no open merge requests still reports that,
	// or every quiet repository would raise a permissions warning.
	quiet := &checkCollector{}
	a.checkRepositories(t.Context(), quiet, f)
	for _, c := range quiet.checks {
		if c.Name != "approvals visibility" {
			continue
		}
		if !strings.Contains(c.Detail, "no open merge request") {
			t.Errorf("a quiet repository was reported as refused: %s", c.Detail)
		}
	}
}

// An inconclusive repository must be named rather than absorbed by a healthy one —// An inconclusive repository must be named rather than absorbed by a healthy one —
// that silent gap is the class this whole check exists to close.
func TestCheckRepositoriesNamesInconclusiveApprovalProbes(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	a := &App{Config: cfg, Log: quietLogger()}

	f := gitlab.NewFake()
	for _, repo := range []string{"backend/payments", "platform/auth", "platform/gateway"} {
		key := projectKey(repo)
		f.Projects[key] = &gitlab.Project{ID: 1, PathWithNamespace: repo}
		f.OpenMRs[key] = []gitlab.MergeRequest{{IID: 1}}
		f.MRs[key+"/1"] = &gitlab.MergeRequest{IID: 1, HeadPipeline: &gitlab.Pipeline{ID: 1}}
	}
	// Every repository answers 502: nothing is concluded, and that has to be said.
	col := &checkCollector{}
	a.checkRepositories(t.Context(), col, refusingApprovals{f, 502})

	var check DoctorCheck
	for _, c := range col.checks {
		if c.Name == "approvals visibility" {
			check = c
		}
	}
	if check.Status != StatusWarn {
		t.Fatalf("approvals visibility = %+v, want a warning", check)
	}
	if !strings.Contains(check.Detail, "could not be checked in backend/payments") {
		t.Errorf("detail does not name the inconclusive repository: %s", check.Detail)
	}
	if strings.Contains(check.Detail, "no open merge request") {
		t.Errorf("detail claims there was nothing to check: %s", check.Detail)
	}
}

// TestCheckRepositoriesSummarises exercises the fan-out: unresolved
// repositories are named, and the pipeline verdict is aggregated across them.
func TestCheckRepositoriesSummarises(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	a := &App{Config: cfg, Log: quietLogger()}

	f := gitlab.NewFake()
	// Resolve two of the three configured repositories; one is left missing.
	for _, repo := range []string{"backend/payments", "platform/auth"} {
		key := projectKey(repo)
		f.Projects[key] = &gitlab.Project{ID: 1, PathWithNamespace: repo}
		f.OpenMRs[key] = []gitlab.MergeRequest{{IID: 1}}
		f.MRs[key+"/1"] = &gitlab.MergeRequest{IID: 1} // head_pipeline absent
	}

	col := &checkCollector{}
	// Pipelines refused as well, which is what makes the absence a permissions
	// problem rather than a repository without CI.
	a.checkRepositories(t.Context(), col, refusingPipelines{f, 403})

	byName := map[string]DoctorCheck{}
	for _, c := range col.checks {
		byName[c.Name] = c
	}

	repos, ok := byName["repositories"]
	if !ok {
		t.Fatalf("no repositories check: %+v", col.checks)
	}
	if repos.Status != StatusFail {
		t.Errorf("status = %v, want fail with one unresolved", repos.Status)
	}
	if !strings.Contains(repos.Detail, "backend/billing") {
		t.Errorf("the unresolved repository is not named: %s", repos.Detail)
	}

	pipe, ok := byName["pipeline visibility"]
	if !ok {
		t.Fatalf("no pipeline visibility check: %+v", col.checks)
	}
	if pipe.Status != StatusWarn {
		t.Errorf("status = %v, want a warning when head_pipeline is absent", pipe.Status)
	}
	// A warning that does not say what to do about it is noise.
	if !strings.Contains(pipe.Detail, "Reporter") {
		t.Errorf("the warning does not name the fix: %s", pipe.Detail)
	}
}

func TestCheckRepositoriesWithNoneConfigured(t *testing.T) {
	t.Parallel()
	a := &App{Config: defaultConfig(t), Log: quietLogger()}
	col := &checkCollector{}
	a.checkRepositories(t.Context(), col, gitlab.NewFake())

	if len(col.checks) != 1 || col.checks[0].Status != StatusFail {
		t.Fatalf("checks = %+v, want one failure", col.checks)
	}
}

// TestCheckCollectorRedacts: a probe's error text is the most likely place for
// a token to surface, so every detail goes through the redactor.
func TestCheckCollectorRedacts(t *testing.T) {
	col := &checkCollector{}
	col.add("gitlab", StatusFail, "%s", errors.New("failed with token glpat-abcdefghijklmnopqrst"))

	if strings.Contains(col.checks[0].Detail, "glpat-abcdefghijklmnopqrst") {
		t.Fatalf("a token reached the report: %s", col.checks[0].Detail)
	}
}

func TestFirstRepository(t *testing.T) {
	t.Parallel()
	if got := firstRepository(&App{Config: testConfig(t)}); got != "backend/payments" {
		t.Errorf("firstRepository = %q", got)
	}
	if got := firstRepository(&App{Config: defaultConfig(t)}); got != "" {
		t.Errorf("with no teams = %q, want empty", got)
	}
	// A team configured with no repositories must not shadow a later one that
	// has them.
	cfg := defaultConfig(t)
	cfg.Teams = []config.TeamConfig{
		{Name: "empty", SlackChannel: "C0"},
		{Name: "real", SlackChannel: "C1", Repositories: []string{"a/b"}},
	}
	if got := firstRepository(&App{Config: cfg}); got != "a/b" {
		t.Errorf("firstRepository = %q, want a/b", got)
	}
}

func TestSortedNames(t *testing.T) {
	t.Parallel()
	// Ordered by version, not by name: an operator reads this list as the order
	// the migrations will be applied in.
	got := sortedNames(map[int]string{10: "0010_b", 2: "0002_a", 33: "0033_c"})
	want := []string{"0002_a", "0010_b", "0033_c"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("names = %v, want %v", got, want)
		}
	}
	if len(sortedNames(nil)) != 0 {
		t.Error("no migrations must produce no names")
	}
}

func TestCheckSlackWithoutAToken(t *testing.T) {
	t.Parallel()

	// Delivery off: not having a token is a legitimate configuration.
	cfg := testConfig(t)
	a := &App{Config: cfg, Log: quietLogger()}
	col := &checkCollector{}
	a.checkSlack(t.Context(), col)
	if len(col.checks) != 1 || col.checks[0].Status != StatusWarn {
		t.Fatalf("checks = %+v, want one warning", col.checks)
	}

	// Delivery on with no token cannot possibly work, so it is a failure.
	cfg2 := testConfig(t)
	cfg2.Service.SlackSendEnabled = true
	col2 := &checkCollector{}
	(&App{Config: cfg2, Log: quietLogger()}).checkSlack(t.Context(), col2)
	if len(col2.checks) != 1 || col2.checks[0].Status != StatusFail {
		t.Fatalf("checks = %+v, want one failure", col2.checks)
	}
}

// TestCheckRepositoriesDoesNotWarnAboutRepositoriesWithoutCI is the aggregate
// half of the same finding: with pipelines readable everywhere and simply none
// running, the summary must not tell the operator to change project membership.
func TestCheckRepositoriesDoesNotWarnAboutRepositoriesWithoutCI(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	a := &App{Config: cfg, Log: quietLogger()}

	f := gitlab.NewFake()
	for _, repo := range []string{"backend/payments", "backend/billing", "platform/auth"} {
		key := projectKey(repo)
		f.Projects[key] = &gitlab.Project{ID: 1, PathWithNamespace: repo}
		f.OpenMRs[key] = []gitlab.MergeRequest{{IID: 1}}
		f.MRs[key+"/1"] = &gitlab.MergeRequest{IID: 1} // no CI ever ran
	}

	col := &checkCollector{}
	a.checkRepositories(t.Context(), col, f)

	for _, c := range col.checks {
		if c.Name != "pipeline visibility" {
			continue
		}
		if c.Status != StatusOK {
			t.Fatalf("pipeline visibility = %v (%s), want ok: no CI is not a permissions problem",
				c.Status, c.Detail)
		}
		if strings.Contains(c.Detail, "Reporter") {
			t.Errorf("the report still sends the operator after project membership: %s", c.Detail)
		}
		return
	}
	t.Fatalf("no pipeline visibility check: %+v", col.checks)
}
