package app

import (
	"context"
	"errors"
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

// TestCheckRepositoriesSummarises exercises the fan-out: unresolved
// repositories are named, and the pipeline verdict is aggregated across them.
func TestCheckRepositoriesSummarises(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
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
	a := &App{Config: config.DefaultConfig(), Log: quietLogger()}
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
	if got := firstRepository(&App{Config: testConfig()}); got != "backend/payments" {
		t.Errorf("firstRepository = %q", got)
	}
	if got := firstRepository(&App{Config: config.DefaultConfig()}); got != "" {
		t.Errorf("with no teams = %q, want empty", got)
	}
	// A team configured with no repositories must not shadow a later one that
	// has them.
	cfg := config.DefaultConfig()
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
	cfg := testConfig()
	a := &App{Config: cfg, Log: quietLogger()}
	col := &checkCollector{}
	a.checkSlack(t.Context(), col)
	if len(col.checks) != 1 || col.checks[0].Status != StatusWarn {
		t.Fatalf("checks = %+v, want one warning", col.checks)
	}

	// Delivery on with no token cannot possibly work, so it is a failure.
	cfg2 := testConfig()
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
	cfg := testConfig()
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
