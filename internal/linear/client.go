// Package linear implements the read-only subset of Linear's GraphQL API used
// by the Slack digest and doctor diagnostics.
package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sxwebdev/xutils/retry"
)

const (
	// DefaultEndpoint is Linear's public GraphQL endpoint.
	DefaultEndpoint = "https://api.linear.app/graphql"
	// InReviewState is the product invariant for issues included in a digest.
	InReviewState = "In Review"
	maxBodyBytes  = 8 << 20
)

// Config configures the Linear client.
type Config struct {
	Endpoint      string
	APIKey        string
	Timeout       time.Duration
	MaxAttempts   int
	MaxRetryAfter time.Duration
	HTTPClient    *http.Client
	// Observer is called once for each completed HTTP attempt. Operation is a
	// fixed low-cardinality name (viewer, team, issues_in_review).
	Observer func(operation string, status int, duration time.Duration, err error)
}

func (c Config) normalized() Config {
	if c.Endpoint == "" {
		c.Endpoint = DefaultEndpoint
	}
	if c.Timeout <= 0 {
		c.Timeout = 15 * time.Second
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 4
	}
	if c.MaxRetryAfter <= 0 {
		c.MaxRetryAfter = time.Minute
	}
	return c
}

// Client is safe for concurrent use.
type Client struct {
	cfg  Config
	http *http.Client
}

// User is the subset of a Linear user needed by diagnostics and rendering.
type User struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// WorkflowState is a Linear team's workflow status.
//
// Type and Position are what make the state *orderable*, and the digest needs
// the order rather than the name: "before In Review" has to be answerable for
// states this service has never heard of ("Ready", "Blocked", "QA"), because the
// only name in the configuration is In Review itself and every team invents the
// rest. See Workflow.
type WorkflowState struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Type is Linear's own coarse category — triage, backlog, unstarted,
	// started, completed, canceled — whose relative order is fixed by Linear and
	// not by the team.
	Type string `json:"type"`
	// Position orders states *within* one type, and only within one type: it is
	// a float Linear reshuffles as the team drags columns around, so it is
	// meaningless across types and never compared across teams. Use
	// CompareStates for board order rather than this field alone.
	//
	// Only the copy that arrives with a *team's board* is ever ordered. The copy
	// riding along on an issue is not: a bare float64 cannot tell `null` or an
	// absent key from a legitimate 0, so trusting it there fails closed. See
	// Workflow.
	Position float64 `json:"position"`
}

// Team is a Linear team and the workflow states visible to the API key.
type Team struct {
	ID     string          `json:"id"`
	Key    string          `json:"key"`
	Name   string          `json:"name"`
	States []WorkflowState `json:"-"`
}

// Label names the team for a diagnostic. The key is parenthesised only when the
// API actually returned one: GetTeam requires an id and nothing else, so a team
// with no key rendered as "Chain ()" everywhere the two were concatenated by hand.
func (t Team) Label() string {
	name := strings.TrimSpace(t.Name)
	if name == "" {
		name = strings.TrimSpace(t.ID)
	}
	if key := strings.TrimSpace(t.Key); key != "" {
		return name + " (" + key + ")"
	}
	return name
}

// Issue is the subset of a Linear issue the digest actually needs: the
// identifier it links MRs by, the URL it links to, and the state and team the
// client re-checks as a fail-closed boundary.
//
// Every field here is selected by every query in this file, and that is the
// invariant to keep. It previously also carried Title, UpdatedAt and Assignee,
// which no query ever asked for — they decoded to the zero value on every path
// while the test fixtures dutifully supplied them, so a renderer reaching for
// issue.Title would have passed its tests and shipped an empty string. The
// digest renders no per-issue row (§4), so the honest fix is for the field not
// to exist rather than for the query to fetch something nobody reads: Linear is
// the source of truth about issues and this struct is not a mirror of it.
type Issue struct {
	ID         string        `json:"id"`
	Identifier string        `json:"identifier"`
	Number     int           `json:"number"`
	URL        string        `json:"url"`
	State      WorkflowState `json:"state"`
	Team       Team          `json:"team"`
}

const issuesByNumbersQuery = `query IssuesByNumbers($teamIds: [ID!]!, $numbers: [Float!]!, $after: String) {
  issues(
    first: 50
    after: $after
    filter: {
      team: { id: { in: $teamIds } }
      number: { in: $numbers }
    }
    orderBy: updatedAt
  ) {
    nodes {
      id identifier number url
      state { id name type position }
      team { id key name }
    }
    pageInfo { hasNextPage endCursor }
  }
}`

// issueNumberBatch bounds one `number: { in: … }` filter.
//
// The caller's list is whatever the open merge requests happen to mention, and
// the extraction is deliberately permissive — it yields candidates, not
// identifiers, so another tracker's key or a token like sha-1 contributes a
// number too. Those cost nothing in correctness (the team filter and the caller's
// identifier re-match both reject them) but they do inflate the request, and a
// team with hundreds of open MRs would otherwise send one unbounded filter that
// Linear is free to reject outright. Chunking makes the request size a property
// of this constant rather than of the repositories.
const issueNumberBatch = 100

// ListIssuesByNumbers resolves the candidate issue numbers extracted from MR
// titles and source branches. Issue numbers are only unique inside a Linear
// team, so the query and the response boundary both restrict results to the
// configured team UUIDs. Callers match the returned full identifiers.
func (c *Client) ListIssuesByNumbers(ctx context.Context, teamIDs []string, numbers []int) ([]Issue, error) {
	if len(teamIDs) == 0 || len(numbers) == 0 {
		return []Issue{}, nil
	}
	issues := make([]Issue, 0, len(numbers))
	// listIssues dedupes within one call; two chunks can still return the same
	// issue only if the caller passed a duplicate number, but the merge is what
	// makes that impossible to observe.
	seen := make(map[string]struct{}, len(numbers))
	for start := 0; start < len(numbers); start += issueNumberBatch {
		batch := numbers[start:min(start+issueNumberBatch, len(numbers))]
		page, err := c.listIssues(ctx, "issues_by_number", issuesByNumbersQuery, map[string]any{
			"teamIds": teamIDs,
			"numbers": batch,
		}, teamIDs, nil)
		if err != nil {
			return nil, err
		}
		for _, issue := range page {
			if _, duplicate := seen[issue.ID]; duplicate {
				continue
			}
			seen[issue.ID] = struct{}{}
			issues = append(issues, issue)
		}
	}
	return issues, nil
}

// APIError describes an HTTP or GraphQL failure. It deliberately never embeds
// request headers or the raw response body, either of which can echo a key.
type APIError struct {
	Operation string
	Status    int
	Code      string
	Message   string
}

func (e *APIError) Error() string {
	parts := []string{"linear " + e.Operation}
	if e.Status != 0 {
		parts = append(parts, "status "+strconv.Itoa(e.Status))
	}
	if e.Code != "" {
		parts = append(parts, "code "+e.Code)
	}
	if e.Message != "" {
		parts = append(parts, e.Message)
	}
	return strings.Join(parts, ": ")
}

// New builds a Linear client.
func New(cfg Config) (*Client, error) {
	cfg = cfg.normalized()
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("linear endpoint must be an absolute http(s) URL: %q", cfg.Endpoint)
	}
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("linear api key is required")
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: cfg.Timeout}
	}
	return &Client{cfg: cfg, http: hc}, nil
}

type graphQLRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

type graphQLError struct {
	Message    string `json:"message"`
	Extensions struct {
		Code string `json:"code"`
	} `json:"extensions"`
}

type graphQLResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []graphQLError  `json:"errors"`
}

const viewerQuery = `query Viewer { viewer { id name email } }`

// Viewer returns the account represented by the configured key.
func (c *Client) Viewer(ctx context.Context) (*User, error) {
	var data struct {
		Viewer User `json:"viewer"`
	}
	if err := c.do(ctx, "viewer", viewerQuery, nil, &data); err != nil {
		return nil, err
	}
	// Same fail-closed boundary GetTeam applies. `{"data":{"viewer":null}}` is a
	// well-formed 200 that decodes into a zero User, and doctor's only use of the
	// call is to print who the key belongs to — so without this it reported
	// "linear OK: authenticated as " and passed green on a key that authenticates
	// as nobody.
	if strings.TrimSpace(data.Viewer.ID) == "" {
		return nil, &APIError{Operation: "viewer", Message: "response has no viewer"}
	}
	return &data.Viewer, nil
}

// teamQuery is built from statesPageSize rather than repeating it: NewWorkflow's
// "the board may be truncated" hint is only correct while the two agree, and
// nothing else would notice them drifting apart.
var teamQuery = fmt.Sprintf(`query Team($id: String!) {
  team(id: $id) {
    id key name
    states(first: %d) { nodes { id name type position } }
  }
}`, statesPageSize)

// GetTeam returns a configured team and its visible workflow states.
func (c *Client) GetTeam(ctx context.Context, id string) (*Team, error) {
	var data struct {
		Team struct {
			ID     string `json:"id"`
			Key    string `json:"key"`
			Name   string `json:"name"`
			States struct {
				Nodes []WorkflowState `json:"nodes"`
			} `json:"states"`
		} `json:"team"`
	}
	if err := c.do(ctx, "team", teamQuery, map[string]any{"id": id}, &data); err != nil {
		return nil, err
	}
	if data.Team.ID == "" {
		return nil, &APIError{Operation: "team", Message: "response has no team"}
	}
	return &Team{ID: data.Team.ID, Key: data.Team.Key, Name: data.Team.Name, States: data.Team.States.Nodes}, nil
}

const issuesInReviewQuery = `query IssuesInReview($teamIds: [ID!]!, $stateName: String!, $after: String) {
  issues(
    first: 50
    after: $after
    filter: {
      team: { id: { in: $teamIds } }
      state: { name: { eqIgnoreCase: $stateName } }
    }
    orderBy: updatedAt
  ) {
    nodes {
      id identifier number url
      state { id name type position }
      team { id key name }
    }
    pageInfo { hasNextPage endCursor }
  }
}`

// ListIssuesInReview returns every non-archived issue currently in the exact
// In Review state for the supplied teams. Linear filters server-side; the
// client repeats the state check as a fail-closed rendering boundary.
func (c *Client) ListIssuesInReview(ctx context.Context, teamIDs []string) ([]Issue, error) {
	if len(teamIDs) == 0 {
		return []Issue{}, nil
	}
	return c.listIssues(ctx, "issues_in_review", issuesInReviewQuery, map[string]any{
		"teamIds":   teamIDs,
		"stateName": InReviewState,
	}, teamIDs, func(issue Issue) bool { return isInReview(issue.State.Name) })
}

func (c *Client) listIssues(
	ctx context.Context,
	operation string,
	query string,
	variables map[string]any,
	teamIDs []string,
	accept func(Issue) bool,
) ([]Issue, error) {
	seen := make(map[string]struct{})
	allowedTeams := make(map[string]struct{}, len(teamIDs))
	for _, id := range teamIDs {
		allowedTeams[strings.TrimSpace(id)] = struct{}{}
	}
	issues := make([]Issue, 0)
	var after any
	for {
		var data struct {
			Issues struct {
				Nodes    []Issue `json:"nodes"`
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
			} `json:"issues"`
		}
		pageVariables := make(map[string]any, len(variables)+1)
		for key, value := range variables {
			pageVariables[key] = value
		}
		pageVariables["after"] = after
		if err := c.do(ctx, operation, query, pageVariables, &data); err != nil {
			return nil, err
		}
		for _, issue := range data.Issues.Nodes {
			if accept != nil && !accept(issue) {
				continue
			}
			if _, allowed := allowedTeams[strings.TrimSpace(issue.Team.ID)]; !allowed {
				continue
			}
			if strings.TrimSpace(issue.ID) == "" {
				return nil, &APIError{Operation: operation, Message: "issue has no id"}
			}
			if _, duplicate := seen[issue.ID]; duplicate {
				continue
			}
			seen[issue.ID] = struct{}{}
			issues = append(issues, issue)
		}
		if !data.Issues.PageInfo.HasNextPage {
			return issues, nil
		}
		if data.Issues.PageInfo.EndCursor == "" {
			return nil, &APIError{Operation: operation, Message: "pagination has next page but no end cursor"}
		}
		after = data.Issues.PageInfo.EndCursor
	}
}

func isInReview(state string) bool {
	return strings.EqualFold(strings.TrimSpace(state), InReviewState)
}

func (c *Client) do(ctx context.Context, operation, query string, variables map[string]any, out any) error {
	payload, err := json.Marshal(graphQLRequest{Query: query, Variables: variables})
	if err != nil {
		return fmt.Errorf("encode linear %s request: %w", operation, err)
	}

	r := retry.New(
		retry.WithMaxAttempts(c.cfg.MaxAttempts),
		retry.WithPolicy(retry.PolicyBackoff),
		retry.WithDelay(500*time.Millisecond),
		retry.WithMaxDelay(10*time.Second),
		retry.WithContext(ctx),
	)
	err = r.Do(func() error {
		started := time.Now()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.Endpoint, bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("%w: build request: %w", retry.ErrExit, err)
		}
		req.Header.Set("Authorization", c.cfg.APIKey)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")

		resp, err := c.http.Do(req)
		if err != nil {
			wrapped := fmt.Errorf("linear %s request: %w", operation, err)
			c.observe(operation, 0, time.Since(started), wrapped)
			return wrapped
		}
		defer resp.Body.Close() //nolint:errcheck
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
		if readErr != nil {
			wrapped := fmt.Errorf("linear %s response: %w", operation, readErr)
			c.observe(operation, resp.StatusCode, time.Since(started), wrapped)
			return wrapped
		}
		if len(body) > maxBodyBytes {
			apiErr := &APIError{Operation: operation, Status: resp.StatusCode, Message: "response exceeds size limit"}
			c.observe(operation, resp.StatusCode, time.Since(started), apiErr)
			return fmt.Errorf("%w: %w", retry.ErrExit, apiErr)
		}

		response := graphQLResponse{}
		decodeErr := json.Unmarshal(body, &response)
		if decodeErr == nil && len(response.Errors) > 0 {
			gql := response.Errors[0]
			apiErr := &APIError{
				Operation: operation, Status: resp.StatusCode, Code: gql.Extensions.Code,
				Message: c.safeMessage(gql.Message),
			}
			c.observe(operation, resp.StatusCode, time.Since(started), apiErr)
			if strings.EqualFold(gql.Extensions.Code, "RATELIMITED") {
				if err := c.waitRetryAfter(ctx, resp.Header); err != nil {
					return fmt.Errorf("%w: %w", retry.ErrExit, err)
				}
				return apiErr
			}
			return fmt.Errorf("%w: %w", retry.ErrExit, apiErr)
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			apiErr := &APIError{Operation: operation, Status: resp.StatusCode}
			c.observe(operation, resp.StatusCode, time.Since(started), apiErr)
			switch {
			case resp.StatusCode == http.StatusTooManyRequests:
				if err := c.waitRetryAfter(ctx, resp.Header); err != nil {
					return fmt.Errorf("%w: %w", retry.ErrExit, err)
				}
				return apiErr
			case resp.StatusCode >= 500:
				return apiErr
			default:
				return fmt.Errorf("%w: %w", retry.ErrExit, apiErr)
			}
		}
		if decodeErr != nil {
			apiErr := &APIError{Operation: operation, Status: resp.StatusCode, Message: "invalid JSON response"}
			c.observe(operation, resp.StatusCode, time.Since(started), apiErr)
			return fmt.Errorf("%w: %w", retry.ErrExit, apiErr)
		}
		if out != nil {
			if len(response.Data) == 0 || string(response.Data) == "null" {
				apiErr := &APIError{Operation: operation, Status: resp.StatusCode, Message: "response has no data"}
				c.observe(operation, resp.StatusCode, time.Since(started), apiErr)
				return fmt.Errorf("%w: %w", retry.ErrExit, apiErr)
			}
			if err := json.Unmarshal(response.Data, out); err != nil {
				apiErr := &APIError{Operation: operation, Status: resp.StatusCode, Message: "invalid data payload"}
				c.observe(operation, resp.StatusCode, time.Since(started), apiErr)
				return fmt.Errorf("%w: %w", retry.ErrExit, apiErr)
			}
		}
		c.observe(operation, resp.StatusCode, time.Since(started), nil)
		return nil
	})
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			return apiErr
		}
		return err
	}
	return nil
}

func (c *Client) observe(operation string, status int, d time.Duration, err error) {
	if c.cfg.Observer != nil {
		c.cfg.Observer(operation, status, d, err)
	}
}

func (c *Client) safeMessage(message string) string {
	message = strings.ReplaceAll(message, c.cfg.APIKey, "[REDACTED]")
	const maxRunes = 512
	runes := []rune(message)
	if len(runes) > maxRunes {
		message = string(runes[:maxRunes-1]) + "…"
	}
	return message
}

func (c *Client) waitRetryAfter(ctx context.Context, header http.Header) error {
	d, ok := retryAfterDelay(header.Get("Retry-After"), time.Now())
	if !ok {
		return nil
	}
	timer := time.NewTimer(min(d, c.cfg.MaxRetryAfter))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func retryAfterDelay(header string, now time.Time) (time.Duration, bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0, false
	}
	if seconds, err := strconv.Atoi(header); err == nil {
		if seconds <= 0 {
			return 0, false
		}
		return time.Duration(seconds) * time.Second, true
	}
	ts, err := http.ParseTime(header)
	if err != nil || !ts.After(now) {
		return 0, false
	}
	return ts.Sub(now), true
}
