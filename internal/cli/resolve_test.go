package cli

import (
	"strings"
	"testing"

	"github.com/sxwebdev/ai-reviewer/internal/app"
	"github.com/sxwebdev/ai-reviewer/internal/config"
	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/service"
	"github.com/tkcrm/mx/logger"
)

func refConfig() *config.Config {
	cfg := config.DefaultConfig()
	cfg.GitLab.BaseURL = "https://gitlab.example.com"
	cfg.Teams = []config.TeamConfig{{
		Name: "payments", SlackChannel: "C1",
		AIReview:     config.TeamAIReviewConfig{Enabled: true},
		Repositories: []string{"backend/payments"},
	}}
	return cfg
}

func refApp() *app.App {
	return &app.App{
		Config: refConfig(),
		Log: logger.NewExtended(logger.WithConfig(logger.Config{
			Level: logger.LogLevelFatal, Format: logger.LoggerFormatJSON,
		})),
	}
}

func refFake(path string, id int64, sha string) *gitlab.FakeClient {
	f := gitlab.NewFake()
	key := gitlab.MRRef{ProjectPath: path}.ProjectKey()
	f.Projects[key] = &gitlab.Project{ID: id, PathWithNamespace: path}
	f.MRs[key+"/12"] = &gitlab.MergeRequest{IID: 12, SHA: sha}
	return f
}

// TestResolveRefFillsTheUniquenessKey is why the CLI talks to GitLab before it
// enqueues anything: uniqueness is keyed on (project_id, mr_iid, head_sha), so
// a job inserted from a bare path would hash differently from the scanner's and
// produce a second, parallel review of the same merge request.
func TestResolveRefFillsTheUniquenessKey(t *testing.T) {
	t.Parallel()
	const sha = "deadbeefdeadbeefdeadbeefdeadbeef"

	for _, ref := range []string{
		"backend/payments!12",
		"42:12",
		"https://gitlab.example.com/backend/payments/-/merge_requests/12",
	} {
		t.Run(ref, func(t *testing.T) {
			t.Parallel()
			// The numeric form addresses the project by id, so register that key
			// too — the fake indexes by whatever the caller passed.
			fake := refFake("backend/payments", 42, sha)
			fake.Projects["42"] = &gitlab.Project{ID: 42, PathWithNamespace: "backend/payments"}
			fake.MRs["42/12"] = &gitlab.MergeRequest{IID: 12, SHA: sha}

			got, err := resolveRef(t.Context(), refApp(), fake, ref)
			if err != nil {
				t.Fatalf("resolveRef: %v", err)
			}
			if got.ProjectID != 42 || got.MRIID != 12 || got.HeadSHA != sha {
				t.Errorf("args = %+v, want the full uniqueness key", got)
			}
			// The team labels the metrics and selects the review settings.
			if got.Team != "payments" {
				t.Errorf("team = %q, want the configured owner", got.Team)
			}
			if got.ProjectPath != "backend/payments" {
				t.Errorf("project path = %q", got.ProjectPath)
			}
			// Publish is never inferred here: the flag is the only source.
			if got.Publish != nil {
				t.Errorf("resolveRef decided publication: %v", *got.Publish)
			}
		})
	}
}

// TestResolveRefUsesTheSharedHeadSHA is D2: the uniqueness key is
// (project_id, mr_iid, head_sha), so every producer of a review job must derive
// the SHA the same way. The scanner goes through service.HeadSHA, which prefers
// diff_refs.head_sha; a CLI reading mr.SHA would insert a second job — under a
// different key, so River cannot dedupe it — for work already queued, inside
// exactly the window right after a push where the two fields disagree.
func TestResolveRefUsesTheSharedHeadSHA(t *testing.T) {
	t.Parallel()
	const (
		diffRefsHead = "1111111111111111111111111111111111111111"
		mrSHA        = "2222222222222222222222222222222222222222"
	)

	f := refFake("backend/payments", 42, mrSHA)
	// The window the two fields disagree in: positions are anchored to
	// diff_refs, so that is the SHA the whole system keys off.
	f.MRs[gitlab.MRRef{ProjectPath: "backend/payments"}.ProjectKey()+"/12"] = &gitlab.MergeRequest{
		IID:      12,
		SHA:      mrSHA,
		DiffRefs: gitlab.DiffRefs{HeadSHA: diffRefsHead},
	}

	got, err := resolveRef(t.Context(), refApp(), f, "backend/payments!12")
	if err != nil {
		t.Fatalf("resolveRef: %v", err)
	}
	if got.HeadSHA != diffRefsHead {
		t.Errorf("head SHA = %s, want diff_refs.head_sha %s — the key must match the scanner's",
			got.HeadSHA, diffRefsHead)
	}
	// And it must agree with the helper itself, not merely with this literal.
	if want := service.HeadSHA(f.MRs[gitlab.MRRef{ProjectPath: "backend/payments"}.ProjectKey()+"/12"]); got.HeadSHA != want {
		t.Errorf("head SHA = %s, want service.HeadSHA's %s", got.HeadSHA, want)
	}
}

// TestResolveRefWithoutAHeadSHA: a merge request with no head SHA cannot be
// keyed, so enqueueing it would create a job that never dedupes.
func TestResolveRefWithoutAHeadSHA(t *testing.T) {
	t.Parallel()
	f := refFake("backend/payments", 42, "")
	_, err := resolveRef(t.Context(), refApp(), f, "backend/payments!12")
	if err == nil {
		t.Fatal("a merge request with no head SHA must be rejected")
	}
	if !strings.Contains(err.Error(), "head SHA") {
		t.Errorf("error %q does not explain what is missing", err)
	}
}

// TestResolveRefOutsideAnyTeam: reviewing an unlisted merge request on request
// is legitimate, so it proceeds with an empty team rather than being refused.
func TestResolveRefOutsideAnyTeam(t *testing.T) {
	t.Parallel()
	f := refFake("other/repo", 9, "abc")
	got, err := resolveRef(t.Context(), refApp(), f, "other/repo!12")
	if err != nil {
		t.Fatalf("resolveRef: %v", err)
	}
	if got.Team != "" {
		t.Errorf("team = %q, want empty for an unconfigured repository", got.Team)
	}
	if got.ProjectID != 9 {
		t.Errorf("args = %+v", got)
	}
}

func TestResolveRefRejectsAMalformedReference(t *testing.T) {
	t.Parallel()
	if _, err := resolveRef(t.Context(), refApp(), gitlab.NewFake(), "not a reference"); err == nil {
		t.Fatal("a malformed reference must be rejected before any API call")
	}
}

func TestResolveRefReportsAnUnknownProject(t *testing.T) {
	t.Parallel()
	if _, err := resolveRef(t.Context(), refApp(), gitlab.NewFake(), "backend/payments!12"); err == nil {
		t.Fatal("an unresolvable project must be an error")
	}
}

func TestDigestTargets(t *testing.T) {
	t.Parallel()
	a := refApp()

	all, err := digestTargets(a, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Name != "payments" {
		t.Errorf("targets = %+v", all)
	}

	// Case-insensitive, matching every other team lookup.
	one, err := digestTargets(a, "PAYMENTS")
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 || one[0].Name != "payments" {
		t.Errorf("targets = %+v", one)
	}

	if _, err := digestTargets(a, "retired"); err == nil {
		t.Error("an unknown team must be reported, not silently produce no digests")
	}

	empty := &app.App{Config: config.DefaultConfig(), Log: a.Log}
	if _, err := digestTargets(empty, ""); err == nil {
		t.Error("a config with no teams must be reported")
	}
}

func TestSymbolsAreDistinct(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, s := range []app.CheckStatus{app.StatusOK, app.StatusWarn, app.StatusFail} {
		sym := symbol(s)
		if sym == "" || seen[sym] {
			t.Fatalf("symbol(%v) = %q is empty or a duplicate", s, sym)
		}
		seen[sym] = true
	}
}

// TestCommandTreeCoversThePlan pins §15's command set: a command silently
// dropped from the tree is a feature nobody can reach.
func TestCommandTreeCoversThePlan(t *testing.T) {
	t.Parallel()
	root := NewApp(refApp().Log)

	want := map[string]bool{
		"start": false, "scan": false, "review": false,
		"digest": false, "doctor": false, "migrations": false,
	}
	for _, c := range root.Commands {
		if _, ok := want[c.Name]; ok {
			want[c.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("command %q is missing from the tree", name)
		}
	}

	// The removed personal-mode commands must stay removed (§15). `start` was
	// on that list too — it was the personal tool's alias for the local
	// watch-daemon — and is deliberately off it now: the name was reclaimed for
	// the production process (the plan's §15 correction note records this). The
	// two below have no successor and must not come back.
	for _, c := range root.Commands {
		switch c.Name {
		case "sync", "daemon":
			t.Errorf("%q survived the rewrite", c.Name)
		}
	}
}
