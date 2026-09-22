package app

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sxwebdev/ai-reviewer/internal/domain"
	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/jobs"
	"github.com/sxwebdev/ai-reviewer/internal/service"
	"github.com/sxwebdev/ai-reviewer/internal/slack"
	"github.com/sxwebdev/ai-reviewer/internal/store"
	"github.com/sxwebdev/ai-reviewer/internal/store/storetest"
)

// dispatcherFixture builds the dispatcher over this binary's private test
// database: what it does is insert a River job, so the assertion worth making is
// about a row, not about a mock.
func dispatcherFixture(t *testing.T, teams ...domain.Team) (*commandDispatcher, *pgxpool.Pool) {
	t.Helper()
	pool := storetest.Pool(t,
		storetest.WithSetup(jobs.MigrateUp),
		storetest.WithTruncate("river_job"),
	)
	queue, err := jobs.NewQueue(pool)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	st, err := store.New(pool)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	if len(teams) == 0 {
		teams = []domain.Team{
			{Name: "payments", SlackChannel: "C123"},
			{Name: "checkout", SlackChannel: "C999"},
		}
	}
	svc, err := service.New(
		service.Deps{GitLab: gitlab.NewFake(), Store: st, Log: quietLogger()},
		service.Config{Teams: teams},
	)
	if err != nil {
		t.Fatalf("service.New: %v", err)
	}
	d := newCommandDispatcher(quietLogger(), queue, svc, SlackCommandNames{Team: "/all", Mine: "/my"})
	return d, pool
}

func testSlashCommand(name, channel string) slack.SlashCommand {
	return slack.SlashCommand{
		Command: name, ChannelID: channel, UserID: "U42", UserName: "rita",
		ResponseURL: "https://hooks.slack.com/commands/T1/B2/C3",
	}
}

// queuedCommands reads back what the dispatcher inserted. The row is the
// assertion: a command that acknowledged in Slack and left no job is exactly the
// failure the queue exists to make impossible.
func queuedCommands(t *testing.T, pool *pgxpool.Pool) []jobs.SlackCommandArgs {
	t.Helper()
	rows, err := pool.Query(t.Context(),
		"SELECT args FROM river_job WHERE kind = $1 ORDER BY id", jobs.KindSlackCommand)
	if err != nil {
		t.Fatalf("query slack_command jobs: %v", err)
	}
	defer rows.Close()

	var out []jobs.SlackCommandArgs
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan args: %v", err)
		}
		var args jobs.SlackCommandArgs
		if err := json.Unmarshal(raw, &args); err != nil {
			t.Fatalf("decode args: %v", err)
		}
		out = append(out, args)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read args: %v", err)
	}
	return out
}

// TestDispatcherQueuesBothCommands is the whole of the socket's business logic:
// the command name decides the scope, the channel decides the team, and the work
// itself becomes a durable job rather than something done inside Slack's
// three-second window.
func TestDispatcherQueuesBothCommands(t *testing.T) {
	tests := []struct {
		name      string
		command   string
		channel   string
		wantScope string
		wantTeam  string
		wantAck   string
	}{
		{"team digest", "/all", "C123", "team", "payments", "Building the team digest"},
		{"personal digest", "/my", "C123", "mine", "payments", "Checking what is waiting on you"},
		{"the channel picks the team", "/all", "C999", "team", "checkout", "Building the team digest"},
		// Slack sends the command as registered; the config may name it in any
		// case, and a mismatch would present as a command that does nothing.
		{"case folds", "/ALL", "C123", "team", "payments", "Building the team digest"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, pool := dispatcherFixture(t)

			ack := d.Handle(t.Context(), testSlashCommand(tt.command, tt.channel))
			if !strings.Contains(ack.Text, tt.wantAck) {
				t.Errorf("ack = %q, want it to contain %q", ack.Text, tt.wantAck)
			}
			// Never in channel: an "on it" line everyone sees is noise, and the
			// digest that follows is the message worth reading.
			if ack.InChannel {
				t.Error("the acknowledgement must be ephemeral")
			}

			queued := queuedCommands(t, pool)
			if len(queued) != 1 {
				t.Fatalf("queued %d job(s), want 1", len(queued))
			}
			if queued[0].Scope != tt.wantScope || queued[0].Team != tt.wantTeam {
				t.Errorf("args = %+v, want scope %q team %q", queued[0], tt.wantScope, tt.wantTeam)
			}
			if queued[0].SlackUserID != "U42" {
				t.Errorf("caller = %q, want the Slack user who typed it", queued[0].SlackUserID)
			}
			if queued[0].ResponseURL == "" {
				t.Error("the response URL must travel with the job; without it the answer has nowhere to go")
			}
		})
	}
}

// TestDispatcherRefusesAChannelWithoutATeam: answering with some other team's
// digest would be worse than not answering, and "unknown channel" would leave
// the caller guessing which channel is the right one.
func TestDispatcherRefusesAChannelWithoutATeam(t *testing.T) {
	d, pool := dispatcherFixture(t)

	ack := d.Handle(t.Context(), testSlashCommand("/all", "C-unknown"))
	if !strings.Contains(ack.Text, "team's digest channel") {
		t.Errorf("ack = %q, want it to name the fix", ack.Text)
	}
	if n := len(queuedCommands(t, pool)); n != 0 {
		t.Errorf("queued %d job(s) for a channel with no team", n)
	}
}

// TestDispatcherAnswersAnUnknownCommand: Slack only delivers commands this app
// registered, so this is configuration drift — the workspace knows a name the
// config no longer does, and from the channel it looks like the service is
// ignoring people.
func TestDispatcherAnswersAnUnknownCommand(t *testing.T) {
	d, pool := dispatcherFixture(t)

	ack := d.Handle(t.Context(), testSlashCommand("/digest", "C123"))
	for _, want := range []string{"/digest", "/all", "/my"} {
		if !strings.Contains(ack.Text, want) {
			t.Errorf("ack = %q, want it to contain %q", ack.Text, want)
		}
	}
	if n := len(queuedCommands(t, pool)); n != 0 {
		t.Errorf("queued %d job(s) for an unknown command", n)
	}
}

// TestDispatcherFoldsARepeatedCommand: pressing it twice while the first answer
// is still being built must not start a second full GitLab pass, and the second
// press must still say something — silence reads as a command that did nothing.
func TestDispatcherFoldsARepeatedCommand(t *testing.T) {
	d, pool := dispatcherFixture(t)

	first := d.Handle(t.Context(), testSlashCommand("/my", "C123"))
	second := d.Handle(t.Context(), testSlashCommand("/my", "C123"))

	if strings.Contains(first.Text, "Already") {
		t.Errorf("the first press must not report a duplicate: %q", first.Text)
	}
	if !strings.Contains(second.Text, "Already working") {
		t.Errorf("second ack = %q, want it to say the answer is already coming", second.Text)
	}
	if n := len(queuedCommands(t, pool)); n != 1 {
		t.Errorf("queued %d job(s); the repeat must fold into the one in flight", n)
	}
}

// TestDispatcherSeparatesPeopleAndScopes: uniqueness folds a repeat of the same
// question, and nothing else. Two people asking for their own digests are two
// questions, and so are /all and /my from one person.
func TestDispatcherSeparatesPeopleAndScopes(t *testing.T) {
	d, pool := dispatcherFixture(t)

	mine := testSlashCommand("/my", "C123")
	other := testSlashCommand("/my", "C123")
	other.UserID = "U99"

	d.Handle(t.Context(), mine)
	d.Handle(t.Context(), other)
	d.Handle(t.Context(), testSlashCommand("/all", "C123"))

	if n := len(queuedCommands(t, pool)); n != 3 {
		t.Errorf("queued %d job(s), want 3 — different people and different scopes are different questions", n)
	}
}

// TestNormalizeCommand covers the config side: /All, all and " /all " are one
// intent typed three ways, and a mismatch is a command that silently does
// nothing.
func TestNormalizeCommand(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"/all": "/all", "All": "/all", " /ALL ": "/all", "my": "/my", "": "",
	}
	for in, want := range cases {
		if got := normalizeCommand(in); got != want {
			t.Errorf("normalizeCommand(%q) = %q, want %q", in, got, want)
		}
	}
}
