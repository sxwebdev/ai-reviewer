package gitlab

import (
	"context"
	"fmt"
	"net/url"
)

// API is the subset of GitLab operations the app depends on. Both *Client and
// the test fake implement it.
type API interface {
	CurrentUser(ctx context.Context) (*User, error)
	ListReviewerMRs(ctx context.Context) ([]MergeRequest, error)
	GetProject(ctx context.Context, projectKey string) (*Project, error)
	GetMR(ctx context.Context, projectKey string, iid int64) (*MergeRequest, error)
	ListMRDiffs(ctx context.Context, projectKey string, iid int64) ([]MergeRequestDiff, error)
	ListMRVersions(ctx context.Context, projectKey string, iid int64) ([]MergeRequestVersion, error)
	ListMRDiscussions(ctx context.Context, projectKey string, iid int64) ([]Discussion, error)
	ListMRPipelines(ctx context.Context, projectKey string, iid int64) ([]Pipeline, error)
	CreateDraftNote(ctx context.Context, projectKey string, iid int64, note string, pos *Position) (*DraftNote, error)
	ListDraftNotes(ctx context.Context, projectKey string, iid int64) ([]DraftNote, error)
	PublishDraftNote(ctx context.Context, projectKey string, iid, draftID int64) error
	BulkPublishDraftNotes(ctx context.Context, projectKey string, iid int64) error
	CreateMRNote(ctx context.Context, projectKey string, iid int64, body string) (*Note, error)
	CreateDiscussion(ctx context.Context, projectKey string, iid int64, body string, pos *Position) (*Discussion, error)
}

var _ API = (*Client)(nil)

func mrPath(projectKey string, iid int64, suffix string) string {
	return fmt.Sprintf("/projects/%s/merge_requests/%d%s", projectKey, iid, suffix)
}

// CurrentUser returns the authenticated user.
func (c *Client) CurrentUser(ctx context.Context) (*User, error) {
	var u User
	if err := c.do(ctx, "GET", "/user", nil, nil, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// ListReviewerMRs lists open MRs where the current user is a reviewer.
func (c *Client) ListReviewerMRs(ctx context.Context) ([]MergeRequest, error) {
	q := url.Values{}
	q.Set("scope", "reviews_for_me")
	q.Set("state", "opened")
	return getList[MergeRequest](ctx, c, "/merge_requests", q)
}

// GetProject fetches a project by numeric id or url-encoded path.
func (c *Client) GetProject(ctx context.Context, projectKey string) (*Project, error) {
	var p Project
	if err := c.do(ctx, "GET", "/projects/"+projectKey, nil, nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// GetMR fetches a single merge request (includes diff_refs).
func (c *Client) GetMR(ctx context.Context, projectKey string, iid int64) (*MergeRequest, error) {
	var m MergeRequest
	if err := c.do(ctx, "GET", mrPath(projectKey, iid, ""), nil, nil, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// ListMRDiffs lists the changed files of an MR.
func (c *Client) ListMRDiffs(ctx context.Context, projectKey string, iid int64) ([]MergeRequestDiff, error) {
	return getList[MergeRequestDiff](ctx, c, mrPath(projectKey, iid, "/diffs"), nil)
}

// ListMRVersions lists the diff versions (with base/head/start SHAs).
func (c *Client) ListMRVersions(ctx context.Context, projectKey string, iid int64) ([]MergeRequestVersion, error) {
	return getList[MergeRequestVersion](ctx, c, mrPath(projectKey, iid, "/versions"), nil)
}

// ListMRDiscussions lists existing discussions on an MR.
func (c *Client) ListMRDiscussions(ctx context.Context, projectKey string, iid int64) ([]Discussion, error) {
	return getList[Discussion](ctx, c, mrPath(projectKey, iid, "/discussions"), nil)
}

// ListMRPipelines lists pipelines for an MR.
func (c *Client) ListMRPipelines(ctx context.Context, projectKey string, iid int64) ([]Pipeline, error) {
	return getList[Pipeline](ctx, c, mrPath(projectKey, iid, "/pipelines"), nil)
}
