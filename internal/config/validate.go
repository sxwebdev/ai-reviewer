package config

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sxwebdev/ai-reviewer/internal/llm"
	"github.com/sxwebdev/ai-reviewer/internal/scheduler"
)

// repoPathRe matches a GitLab project path: two or more slash-separated
// segments of [A-Za-z0-9_.-]. A bare numeric id is accepted separately.
var repoPathRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*(/[A-Za-z0-9][A-Za-z0-9_.-]*)+$`)

// Validate runs the domain checks that struct tags cannot express (§7.3).
//
// Unlike the tag validator it does not stop at the first problem: an operator
// fixing a config one error per restart is the worst possible feedback loop, so
// every failure is collected and reported as a list.
//
// # Secrets are validated here, never by a `validate:"required"` tag
//
// A required secret is the field an operator is most likely to get wrong — it
// is the one thing config.example.yaml deliberately ships empty — and the tag
// validator answers with go-playground's internal rendering:
//
//	Key: 'Config.Postgres.Username' Error:Field validation for 'Username' failed on the 'required' tag
//
// which names a Go field path rather than a config key and, crucially, cannot
// name the environment variable or the Vault key, which are the two ways the
// value is actually supplied. It also short-circuits: xconfig runs this
// function before the tag validator, so a tag failure is reported alone instead
// of joining the numbered list above. TestSecretsAreNotValidatedByTag pins the
// rule, and every check here names the key, the env variable and the Vault key.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	// --- postgres -----------------------------------------------------------
	// Note there is deliberately no check on Postgres.Password: local
	// development and IAM/peer authentication legitimately run without one, and
	// DSN() omits it rather than emitting a stray ":@".
	if !c.Postgres.Username.IsSet() {
		add("postgres.username is empty (set %s_POSTGRES_USERNAME, or the Vault key %s_POSTGRES_USERNAME)", EnvPrefix, EnvPrefix)
	}

	// --- gitlab -------------------------------------------------------------
	if c.GitLab.BaseURL == "" {
		add("gitlab.base_url is empty")
	} else if u, err := url.Parse(c.GitLab.BaseURL); err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		add("gitlab.base_url must be an absolute http(s) URL, got %q", c.GitLab.BaseURL)
	}
	if !c.GitLab.Token.IsSet() {
		// The Vault key is the prefixed env name: xconfigvault looks a field up by
		// its Meta["env"], which already carries EnvPrefix. Naming the bare key
		// here would send an operator to store the secret where nothing reads it.
		add("gitlab.token is empty (set %s_GITLAB_TOKEN, or the Vault key %s_GITLAB_TOKEN)", EnvPrefix, EnvPrefix)
	}

	// --- linear -------------------------------------------------------------
	if c.hasLinearTeams() {
		if !c.Linear.APIKey.IsSet() {
			add("linear.api_key is empty but teams declare linear_team_ids (set %s_LINEAR_API_KEY, or the Vault key %s_LINEAR_API_KEY)", EnvPrefix, EnvPrefix)
		}
		if u, err := url.Parse(c.Linear.Endpoint); err != nil || !u.IsAbs() || u.Scheme != "https" || u.Host == "" {
			add("linear.endpoint must be an absolute HTTPS URL, got %q", c.Linear.Endpoint)
		}
	}
	if c.Linear.Timeout <= 0 {
		add("linear.timeout must be positive, got %s", c.Linear.Timeout)
	}
	if c.Linear.MaxAttempts < 1 {
		add("linear.max_attempts must be at least 1, got %d", c.Linear.MaxAttempts)
	}
	if c.Linear.MaxRetryAfter <= 0 {
		add("linear.max_retry_after must be positive, got %s", c.Linear.MaxRetryAfter)
	}

	// --- llm ----------------------------------------------------------------
	// NewClaudeAuth owns the mode/secret matrix; reusing it here means the
	// doctor's verdict and the subprocess's behaviour can never disagree.
	if !c.LLM.Claude.Auth.Mode.Valid() {
		add("llm.claude.auth.mode %q is not one of %v", c.LLM.Claude.Auth.Mode, llm.AuthModes)
	} else if _, err := llm.NewClaudeAuth(c.ClaudeAuthConfig()); err != nil {
		add("%s", err)
	}

	// --- slack --------------------------------------------------------------
	// A configured channel with no token is the silent-failure case: the digest
	// is built, the send fails, and nobody notices until someone asks why the
	// channel is quiet.
	if !c.Slack.Token.IsSet() && (c.Service.SlackSendEnabled || c.hasSlackChannel()) {
		add("slack.token is empty but %s", c.slackRequiredReason())
	}

	// --- review -------------------------------------------------------------
	if c.Review.ScanInterval < time.Minute {
		add("review.scan_interval must be at least 1m, got %s", c.Review.ScanInterval)
	}
	if c.Review.MaxParallel < 1 {
		add("review.max_parallel must be at least 1, got %d", c.Review.MaxParallel)
	}
	if c.Review.MaxComments < 1 {
		add("review.max_comments must be at least 1, got %d", c.Review.MaxComments)
	}
	if c.Review.Pipeline.Mode == "custom" && len(c.Review.Pipeline.Passes) == 0 {
		add("review.pipeline.mode=custom requires a non-empty review.pipeline.passes list")
	}
	for _, p := range c.Review.Pipeline.Passes {
		if !validPasses[p] {
			add("review.pipeline.passes: unknown pass %q", p)
		}
	}
	for _, v := range c.Review.Pipeline.Verifiers {
		if !validVerifiers[v] {
			add("review.pipeline.verifiers: unknown verifier %q", v)
		}
	}
	for _, p := range c.Review.Coverage.Providers {
		if p != "go" && p != "node" {
			add("review.coverage.providers: unknown provider %q", p)
		}
	}

	// --- digest ------------------------------------------------------------
	// The global block is checked on its own even when every team overrides it:
	// an unused default that cannot be parsed is a trap for the next team added.
	// Everything goes through the scheduler's own parser and loader, so a schedule
	// this service accepts is exactly one it can run and name. Rejecting it here
	// rather than at the first firing is the point — a typo'd time is otherwise a
	// service that starts clean and simply never sends a digest.
	if _, err := scheduler.ParseClocks(c.Digest.Slots); err != nil {
		add("digest.slots: %s", err)
	}
	if _, err := scheduler.LoadLocation(c.Digest.Timezone); err != nil {
		add("digest.timezone: %s", err)
	}
	for _, t := range c.Teams {
		slots, tz := c.DigestScheduleFor(t)
		// Reported against the team, not against the key that happens to hold the
		// value: an operator reading "teams[payments].digest" knows which digest
		// is broken whether the offending value is the team's or the inherited one.
		if _, err := scheduler.ParseClocks(slots); err != nil {
			add("teams[%s].digest.slots: %s", t.Name, err)
		}
		if _, err := scheduler.LoadLocation(tz); err != nil {
			add("teams[%s].digest.timezone: %s", t.Name, err)
		}
	}

	// --- teams --------------------------------------------------------------
	errs = append(errs, c.validateTeams()...)

	return joinConfigErrors(errs)
}

// validateTeams checks the team block: non-empty, unique names, every team wired
// to a channel and at least one repository, and no repository claimed twice.
func (c *Config) validateTeams() []error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if len(c.Teams) == 0 {
		add("teams is empty — the service has nothing to scan")
		return errs
	}

	seenName := map[string]int{}          // lower(name) -> index
	repoOwner := map[string]string{}      // repository -> owning team name
	linearOwner := map[uuid.UUID]string{} // Linear team UUID -> owning team name
	for i, t := range c.Teams {
		label := fmt.Sprintf("teams[%d]", i)
		if t.Name != "" {
			label = fmt.Sprintf("teams[%d] (%s)", i, t.Name)
		}

		switch {
		case strings.TrimSpace(t.Name) == "":
			add("%s: name is empty", label)
		default:
			key := strings.ToLower(strings.TrimSpace(t.Name))
			if prev, dup := seenName[key]; dup {
				add("%s: duplicate team name (also teams[%d]); names are compared case-insensitively", label, prev)
			} else {
				seenName[key] = i
			}
		}

		if !validSlackChannel(t.SlackChannel) {
			add("%s: slack_channel %q is not a Slack channel id (expected e.g. C012345678)", label, t.SlackChannel)
		}

		if len(t.Repositories) == 0 {
			add("%s: repositories is empty", label)
		}
		for _, r := range t.Repositories {
			r = strings.TrimSpace(r)
			if !validRepository(r) {
				add("%s: repository %q is neither a GitLab path (group/sub/repo) nor a numeric id", label, r)
				continue
			}
			// Two teams scanning one repository would review the same MR twice
			// and put it in two digests, so this is an error rather than a warning.
			if owner, dup := repoOwner[r]; dup {
				add("repository %q is claimed by both team %q and team %q", r, owner, t.Name)
				continue
			}
			repoOwner[r] = t.Name
		}
		// Canonicalised in place, and this is the only place that can do it: the
		// form is already proven here and nowhere else.
		//
		// uuid.Parse accepts uppercase, undashed, braced and `urn:uuid:` spellings,
		// and Linear answers with exactly one — lowercase dashed. The client filters
		// returned issues by comparing team ids as strings (its fail-closed team
		// boundary), so a config written as
		// `9CFB482A-81E3-4154-B5B9-2C805E70A02D` used to drop *every* issue: the
		// board read as empty, both gates went inert, and nothing reported it — no
		// warning, no partial status, `Linear · In Review: 0` in the digest and a
		// green doctor line printing a healthy column split for a gate that could
		// never fire. Comparing case-insensitively would fix only one of the four
		// spellings, so the raw string is replaced with the canonical one instead.
		for j, rawID := range t.LinearTeamIDs {
			id, err := uuid.Parse(strings.TrimSpace(rawID))
			if err != nil {
				add("%s: linear_team_id %q is not a UUID", label, rawID)
				continue
			}
			if owner, dup := linearOwner[id]; dup {
				add("Linear team %q is claimed by both team %q and team %q", id, owner, t.Name)
				continue
			}
			linearOwner[id] = t.Name
			c.Teams[i].LinearTeamIDs[j] = id.String()
		}
	}
	return errs
}

// ClaudeAuthConfig converts the config block into the llm package's input. It is
// the one place secrets are unmasked for the auth layer.
func (c *Config) ClaudeAuthConfig() llm.AuthConfig {
	return llm.AuthConfig{
		Mode:           c.LLM.Claude.Auth.Mode,
		OAuthToken:     c.LLM.Claude.Auth.OAuthToken.Unmask(),
		APIKey:         c.LLM.Claude.Auth.APIKey.Unmask(),
		ExtraEnv:       c.LLM.Claude.ExtraEnv,
		PassthroughEnv: c.LLM.Claude.PassthroughEnv,
	}
}

func (c *Config) hasSlackChannel() bool {
	for _, t := range c.Teams {
		if strings.TrimSpace(t.SlackChannel) != "" {
			return true
		}
	}
	return false
}

func (c *Config) hasLinearTeams() bool {
	for _, t := range c.Teams {
		if len(t.LinearTeamIDs) > 0 {
			return true
		}
	}
	return false
}

func (c *Config) slackRequiredReason() string {
	if c.Service.SlackSendEnabled {
		return "service.slack_send_enabled is on"
	}
	return "teams declare slack channels"
}

// validSlackChannel accepts Slack's channel-id shape (C/G/D followed by
// alphanumerics). A channel *name* like "#payments" is rejected: chat.postMessage
// accepts it, but the bot-membership check and the digest's per-channel state
// key both need a stable id.
func validSlackChannel(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 9 {
		return false
	}
	switch s[0] {
	case 'C', 'G', 'D':
	default:
		return false
	}
	for _, r := range s[1:] {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// validRepository accepts a GitLab project path or a numeric project id — the
// two forms the API's :id parameter takes.
func validRepository(s string) bool {
	if s == "" {
		return false
	}
	if id, err := strconv.ParseInt(s, 10, 64); err == nil {
		return id > 0
	}
	return repoPathRe.MatchString(s)
}

var validPasses = map[string]bool{
	"general": true, "correctness": true, "concurrency": true,
	"security": true, "contracts": true,
}

var validVerifiers = map[string]bool{
	"go_build": true, "go_vet": true, "py_syntax": true,
	"tsc": true, "go_test": true,
}

// joinConfigErrors renders the collected problems as a numbered list under one
// heading. Reporting them all at once is the point: fixing a config one error
// per restart is the worst possible feedback loop.
func joinConfigErrors(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "invalid configuration (%d problem(s)):", len(errs))
	for i, err := range errs {
		fmt.Fprintf(&b, "\n  %d. %s", i+1, err)
	}
	return errors.New(b.String())
}
