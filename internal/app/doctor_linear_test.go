package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sxwebdev/ai-reviewer/internal/config"
	"github.com/sxwebdev/ai-reviewer/internal/linear"
)

type fakeLinearDoctor struct {
	viewer    *linear.User
	viewerErr error
	teams     map[string]*linear.Team
	teamErr   error

	// entered/release turn GetTeam into a barrier: every call announces itself on
	// entered and then parks until release is closed. A sequential implementation
	// can only ever announce once, so the test's second read is what proves the
	// probes overlap.
	entered chan string
	release chan struct{}

	// deadlines, when set, receives the deadline Viewer was called with.
	deadlines chan time.Time
}

func (f fakeLinearDoctor) Viewer(ctx context.Context) (*linear.User, error) {
	if f.deadlines != nil {
		d, ok := ctx.Deadline()
		if !ok {
			close(f.deadlines) // a zero-value read tells the test there was none
		} else {
			f.deadlines <- d
		}
	}
	if f.viewerErr != nil {
		return nil, f.viewerErr
	}
	return f.viewer, nil
}

func (f fakeLinearDoctor) GetTeam(ctx context.Context, id string) (*linear.Team, error) {
	if f.entered != nil {
		f.entered <- id
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-f.release:
		}
	}
	if f.teamErr != nil {
		return nil, f.teamErr
	}
	team, ok := f.teams[id]
	if !ok {
		return nil, errors.New("not found")
	}
	return team, nil
}

func linearDoctorConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := testConfig(t)
	cfg.Teams[0].LinearTeamIDs = []string{"linear-team-1"}
	return cfg
}

func findLinearCheck(t *testing.T, checks []DoctorCheck, name string) DoctorCheck {
	t.Helper()
	for _, check := range checks {
		if check.Name == name {
			return check
		}
	}
	t.Fatalf("check %q not found: %+v", name, checks)
	return DoctorCheck{}
}

func TestCheckLinearAPISuccess(t *testing.T) {
	t.Parallel()
	cfg := linearDoctorConfig(t)
	api := fakeLinearDoctor{
		viewer: &linear.User{ID: "u1", Name: "Digest Bot"},
		teams: map[string]*linear.Team{
			"linear-team-1": {
				ID: "linear-team-1", Key: "PAY", Name: "Payments",
				States: []linear.WorkflowState{{ID: "s1", Name: " in review "}},
			},
		},
	}
	col := &checkCollector{}
	checkLinearAPI(t.Context(), col, api, cfg)
	if check := findLinearCheck(t, col.checks, "linear"); check.Status != StatusOK || !strings.Contains(check.Detail, "Digest Bot") {
		t.Errorf("linear check = %+v", check)
	}
	if check := findLinearCheck(t, col.checks, "linear teams"); check.Status != StatusOK || !strings.Contains(check.Detail, "Payments → Payments (PAY)") {
		t.Errorf("linear teams check = %+v", check)
	}
}

func TestCheckLinearAPIRejectsMissingOrAmbiguousState(t *testing.T) {
	t.Parallel()
	for _, states := range [][]linear.WorkflowState{
		{{ID: "s1", Name: "Started"}},
		{{ID: "s1", Name: "In Review"}, {ID: "s2", Name: "IN REVIEW"}},
	} {
		states := states
		t.Run(states[0].Name, func(t *testing.T) {
			t.Parallel()
			cfg := linearDoctorConfig(t)
			api := fakeLinearDoctor{
				viewer: &linear.User{ID: "u1", Name: "Bot"},
				teams: map[string]*linear.Team{
					"linear-team-1": {ID: "linear-team-1", Key: "PAY", Name: "Payments", States: states},
				},
			}
			col := &checkCollector{}
			checkLinearAPI(t.Context(), col, api, cfg)
			if check := findLinearCheck(t, col.checks, "linear teams"); check.Status != StatusFail {
				t.Errorf("check = %+v, want failure", check)
			}
		})
	}
}

// Every mapping is probed. Returning on the first bad UUID showed one problem
// per run, so a config with several broken teams took several runs to fix.
func TestCheckLinearAPIReportsEveryBadTeam(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	cfg.Teams[0].LinearTeamIDs = []string{"missing-team", "no-in-review", "linear-team-ok"}
	api := fakeLinearDoctor{
		viewer: &linear.User{ID: "u1", Name: "Bot"},
		teams: map[string]*linear.Team{
			"no-in-review": {
				ID: "no-in-review", Key: "OPS", Name: "Operations",
				States: []linear.WorkflowState{{ID: "s1", Name: "Started"}},
			},
			"linear-team-ok": {
				ID: "linear-team-ok", Key: "PAY", Name: "Payments",
				States: []linear.WorkflowState{{ID: "s1", Name: "In Review"}},
			},
		},
	}
	col := &checkCollector{}
	checkLinearAPI(t.Context(), col, api, cfg)

	check := findLinearCheck(t, col.checks, "linear teams")
	if check.Status != StatusFail {
		t.Fatalf("check = %+v, want failure", check)
	}
	// Both failures, and the count that says one of the three was fine.
	for _, want := range []string{"missing-team", "Operations", "2 of 3 unusable"} {
		if !strings.Contains(check.Detail, want) {
			t.Errorf("detail is missing %q: %s", want, check.Detail)
		}
	}
}

func TestCheckLinearAPIStopsAfterAuthenticationFailure(t *testing.T) {
	t.Parallel()
	col := &checkCollector{}
	checkLinearAPI(t.Context(), col, fakeLinearDoctor{viewerErr: errors.New("unauthorized")}, linearDoctorConfig(t))
	if len(col.checks) != 1 || col.checks[0].Status != StatusFail {
		t.Fatalf("checks = %+v", col.checks)
	}
}

// The team lookups run concurrently. Sequentially, a workspace with a dozen
// UUIDs spends the whole shared doctor budget here and the checks behind it —
// Slack, workdir — inherit a cancelled context and report failures of their own.
func TestCheckLinearAPIProbesTeamsConcurrently(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	cfg.Teams[0].LinearTeamIDs = []string{"t1", "t2", "t3", "t4"}
	api := fakeLinearDoctor{
		viewer:  &linear.User{ID: "u1", Name: "Bot"},
		entered: make(chan string, 4),
		release: make(chan struct{}),
		teams: map[string]*linear.Team{
			"t1": linearTeam("t1"), "t2": linearTeam("t2"),
			"t3": linearTeam("t3"), "t4": linearTeam("t4"),
		},
	}

	col := &checkCollector{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		checkLinearAPI(t.Context(), col, api, cfg)
	}()

	// Two probes inside GetTeam at once is the whole claim; a sequential loop
	// cannot produce the second announcement while the first is parked.
	for i := range 2 {
		select {
		case <-api.entered:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d probe(s) started; the lookups are still sequential", i)
		}
	}
	close(api.release)
	<-done

	if check := findLinearCheck(t, col.checks, "linear teams"); check.Status != StatusOK {
		t.Errorf("check = %+v, want all four teams resolved", check)
	}
}

// The Linear probes get a deadline of their own, strictly inside the shared
// service budget: the client retries with backoff, so one unreachable instance
// would otherwise spend every second the checks behind it need and `doctor`
// would blame Slack for Linear being down.
func TestCheckLinearAPIBoundsItsOwnProbeBudget(t *testing.T) {
	t.Parallel()
	if linearProbeTimeout >= serviceProbeTimeout {
		t.Fatalf("linearProbeTimeout %s leaves nothing of serviceProbeTimeout %s for the checks behind it",
			linearProbeTimeout, serviceProbeTimeout)
	}

	deadlines := make(chan time.Time, 1)
	api := fakeLinearDoctor{
		viewer:    &linear.User{ID: "u1", Name: "Bot"},
		teams:     map[string]*linear.Team{"linear-team-1": linearTeam("linear-team-1")},
		deadlines: deadlines,
	}
	// t.Context() carries no deadline of its own, so any deadline the probe sees
	// is one this check applied.
	checkLinearAPI(t.Context(), &checkCollector{}, api, linearDoctorConfig(t))

	select {
	case got := <-deadlines:
		if got.IsZero() {
			t.Fatal("the Linear probe context carries no deadline")
		}
		if budget := time.Until(got); budget > linearProbeTimeout {
			t.Errorf("probe budget %s exceeds linearProbeTimeout %s", budget, linearProbeTimeout)
		}
	default:
		t.Fatal("Viewer was never called")
	}
}

func linearTeam(id string) *linear.Team {
	return &linear.Team{
		ID: id, Key: strings.ToUpper(id), Name: "Team " + id,
		States: []linear.WorkflowState{{ID: "s1", Name: linear.InReviewState}},
	}
}

func TestCheckLinearSkipsWhenUnused(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	for i := range cfg.Teams {
		cfg.Teams[i].LinearTeamIDs = nil
	}
	a := &App{Config: cfg, Log: quietLogger()}
	col := &checkCollector{}
	a.checkLinear(t.Context(), col)
	if len(col.checks) != 1 || col.checks[0].Status != StatusOK || !strings.Contains(col.checks[0].Detail, "skipped") {
		t.Fatalf("checks = %+v", col.checks)
	}
}
