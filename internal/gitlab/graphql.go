package gitlab

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrGraphQLUnsupported is the fallback signal. Every error returned by
// GraphQLClient wraps it, so the caller has exactly one thing to check:
//
//	states, err := gql.ReviewStates(ctx, fullPath, iids)
//	if errors.Is(err, gitlab.ErrGraphQLUnsupported) {
//	    // warn ONCE per run, then classify reviewers with the REST heuristic
//	}
//
// It covers all three degradation modes: the instance has no /api/graphql or
// rejects the token, the schema has no mergeRequestInteraction.reviewState
// (older self-managed GitLab), or the field comes back null/empty. The service
// must stay useful on those instances — see plan §20.1.
var ErrGraphQLUnsupported = errors.New("gitlab graphql review state unavailable")

// ReviewState is a reviewer's per-MR review state. REST v4 cannot report it:
// its reviewers[].state is the *account* state (active/blocked), not the review
// state. Values are GitLab's MergeRequestReviewState enum.
type ReviewState string

const (
	ReviewStateUnreviewed       ReviewState = "UNREVIEWED"
	ReviewStateReviewed         ReviewState = "REVIEWED"
	ReviewStateRequestedChanges ReviewState = "REQUESTED_CHANGES"
	ReviewStateApproved         ReviewState = "APPROVED"
	ReviewStateUnapproved       ReviewState = "UNAPPROVED"
	ReviewStateReviewStarted    ReviewState = "REVIEW_STARTED"
)

// ReviewerState is one reviewer's state on one MR.
type ReviewerState struct {
	Username string
	State    ReviewState
	Approved bool
	Reviewed bool
}

// GraphQLAPI is the GraphQL surface the scanner depends on. Callers should
// depend on this rather than *GraphQLClient so FakeGraphQL can stand in.
type GraphQLAPI interface {
	ReviewStates(ctx context.Context, fullPath string, iids []int64) (map[int64][]ReviewerState, error)
}

// GraphQLClient talks to POST /api/graphql with the same host, token, timeout,
// TLS settings and retry ladder as the REST client.
type GraphQLClient struct {
	tr *transport
}

var _ GraphQLAPI = (*GraphQLClient)(nil)

// NewGraphQL builds a standalone GraphQL client. It shares the Config with the
// REST client but needs no *Client to exist.
func NewGraphQL(cfg Config) (*GraphQLClient, error) {
	tr, err := newTransport(cfg, "/api")
	if err != nil {
		return nil, err
	}
	return &GraphQLClient{tr: tr}, nil
}

// GraphQL returns a GraphQL client that shares this client's connection pool
// and configuration.
func (c *Client) GraphQL() *GraphQLClient {
	return &GraphQLClient{tr: c.tr.sibling("/api")}
}

// reviewStatesQuery asks for the review state of every reviewer of every listed
// MR of one project. Batched by design: one POST per project per run (plan §9.2).
const reviewStatesQuery = `query ($fullPath: ID!, $iids: [String!]) {
  project(fullPath: $fullPath) {
    mergeRequests(iids: $iids, state: opened) {
      nodes {
        iid
        reviewers {
          nodes {
            username
            mergeRequestInteraction {
              reviewState
              approved
              reviewed
            }
          }
        }
      }
    }
  }
}`

type graphQLRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

type graphQLError struct {
	Message string `json:"message"`
}

type graphQLInteraction struct {
	ReviewState string `json:"reviewState"`
	Approved    bool   `json:"approved"`
	Reviewed    bool   `json:"reviewed"`
}

type graphQLReviewer struct {
	Username    string              `json:"username"`
	Interaction *graphQLInteraction `json:"mergeRequestInteraction"`
}

type graphQLMR struct {
	// GitLab's GraphQL exposes iid as a String, not an Int.
	IID       string `json:"iid"`
	Reviewers struct {
		Nodes []graphQLReviewer `json:"nodes"`
	} `json:"reviewers"`
}

type graphQLReviewStatesResponse struct {
	Data struct {
		Project *struct {
			MergeRequests struct {
				Nodes []graphQLMR `json:"nodes"`
			} `json:"mergeRequests"`
		} `json:"project"`
	} `json:"data"`
	Errors []graphQLError `json:"errors"`
}

// ReviewStates returns the reviewer review states of the given MRs of one
// project, keyed by MR iid. An MR with no reviewers is simply absent from the
// map — that is not an error.
//
// Every returned error wraps ErrGraphQLUnsupported; see its doc for the
// fallback contract. In particular the caller must treat a non-nil error as
// "use the REST heuristic and log once", not as a fatal condition.
func (g *GraphQLClient) ReviewStates(ctx context.Context, fullPath string, iids []int64) (map[int64][]ReviewerState, error) {
	if fullPath == "" || len(iids) == 0 {
		return map[int64][]ReviewerState{}, nil
	}

	strIIDs := make([]string, 0, len(iids))
	for _, iid := range iids {
		strIIDs = append(strIIDs, strconv.FormatInt(iid, 10))
	}
	req := graphQLRequest{
		Query: reviewStatesQuery,
		Variables: map[string]any{
			"fullPath": fullPath,
			"iids":     strIIDs,
		},
	}

	var resp graphQLReviewStatesResponse
	if err := g.tr.do(ctx, "POST", "/graphql", nil, req, &resp); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrGraphQLUnsupported, err)
	}
	// GraphQL answers 200 even for schema errors, so errors[] is the real
	// status line: an instance without mergeRequestInteraction.reviewState
	// reports it here.
	if len(resp.Errors) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrGraphQLUnsupported, graphQLErrorText(resp.Errors))
	}
	if resp.Data.Project == nil {
		return nil, fmt.Errorf("%w: project %q not visible via graphql", ErrGraphQLUnsupported, fullPath)
	}

	out := make(map[int64][]ReviewerState, len(resp.Data.Project.MergeRequests.Nodes))
	var reviewers, withState int
	for _, node := range resp.Data.Project.MergeRequests.Nodes {
		iid, err := strconv.ParseInt(node.IID, 10, 64)
		if err != nil {
			continue // an iid we cannot key on is unusable; skip rather than fail the batch
		}
		states := make([]ReviewerState, 0, len(node.Reviewers.Nodes))
		for _, r := range node.Reviewers.Nodes {
			reviewers++
			rs := ReviewerState{Username: r.Username}
			if r.Interaction != nil {
				rs.State = ReviewState(strings.ToUpper(strings.TrimSpace(r.Interaction.ReviewState)))
				rs.Approved = r.Interaction.Approved
				rs.Reviewed = r.Interaction.Reviewed
			}
			if rs.State != "" {
				withState++
			}
			states = append(states, rs)
		}
		if len(states) > 0 {
			out[iid] = states
		}
	}
	// The whole point of the call is reviewState. If reviewers came back but
	// not one of them carried a state, the field is absent or nulled on this
	// instance and the answer is worthless — say so instead of reporting every
	// reviewer as UNREVIEWED, which would nag people who already reviewed.
	if reviewers > 0 && withState == 0 {
		return nil, fmt.Errorf("%w: no reviewState in response", ErrGraphQLUnsupported)
	}
	return out, nil
}

func graphQLErrorText(errs []graphQLError) string {
	msgs := make([]string, 0, len(errs))
	for _, e := range errs {
		msgs = append(msgs, e.Message)
	}
	return strings.Join(msgs, "; ")
}
