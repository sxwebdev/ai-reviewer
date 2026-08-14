package gitlab

import (
	"context"
	"fmt"
	"net/url"
)

// API is the subset of GitLab operations the app depends on. *Client and the
// test fake both implement it — adding a method here means adding it in both.
type API interface {
	CurrentUser(ctx context.Context) (*User, error)
	ListOpenMRs(ctx context.Context, projectKey string) ([]MergeRequest, error)
	GetProject(ctx context.Context, projectKey string) (*Project, error)
	GetMR(ctx context.Context, projectKey string, iid int64) (*MergeRequest, error)
	ListMRDiffs(ctx context.Context, projectKey string, iid int64) ([]MergeRequestDiff, error)
	ListMRVersions(ctx context.Context, projectKey string, iid int64) ([]MergeRequestVersion, error)
	ListMRDiscussions(ctx context.Context, projectKey string, iid int64) ([]Discussion, error)
	ListMRCommits(ctx context.Context, projectKey string, iid int64) ([]Commit, error)
	ListMRPipelines(ctx context.Context, projectKey string, iid int64) ([]Pipeline, error)
	GetMRApprovals(ctx context.Context, projectKey string, iid int64) (*Approvals, error)
	GetRawFile(ctx context.Context, projectKey, filePath, ref string) ([]byte, error)
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
	if err := c.tr.do(ctx, "GET", "/user", nil, nil, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// ListOpenMRs lists every open MR of a project. This is the team-service scan
// entrypoint: the service watches whole repositories, not one account's review
// queue. The returned MRs come from the *list* endpoint, so mergeability and
// head_pipeline are absent — fetch details with GetMR for the candidates that
// survive the cheap filtering.
func (c *Client) ListOpenMRs(ctx context.Context, projectKey string) ([]MergeRequest, error) {
	q := url.Values{}
	q.Set("state", "opened")
	return getList[MergeRequest](ctx, c.tr, "/projects/"+projectKey+"/merge_requests", q)
}

// GetProject fetches a project by numeric id or url-encoded path.
func (c *Client) GetProject(ctx context.Context, projectKey string) (*Project, error) {
	var p Project
	if err := c.tr.do(ctx, "GET", "/projects/"+projectKey, nil, nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// GetMR fetches a single merge request (includes diff_refs, mergeability and
// head_pipeline).
func (c *Client) GetMR(ctx context.Context, projectKey string, iid int64) (*MergeRequest, error) {
	var m MergeRequest
	if err := c.tr.do(ctx, "GET", mrPath(projectKey, iid, ""), nil, nil, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// ListMRDiffs lists the changed files of an MR.
func (c *Client) ListMRDiffs(ctx context.Context, projectKey string, iid int64) ([]MergeRequestDiff, error) {
	return getList[MergeRequestDiff](ctx, c.tr, mrPath(projectKey, iid, "/diffs"), nil)
}

// ListMRVersions lists the diff versions (with base/head/start SHAs). The
// newest version's created_at is the MR's last push time.
func (c *Client) ListMRVersions(ctx context.Context, projectKey string, iid int64) ([]MergeRequestVersion, error) {
	return getList[MergeRequestVersion](ctx, c.tr, mrPath(projectKey, iid, "/versions"), nil)
}

// ListMRDiscussions lists existing discussions on an MR.
func (c *Client) ListMRDiscussions(ctx context.Context, projectKey string, iid int64) ([]Discussion, error) {
	return getList[Discussion](ctx, c.tr, mrPath(projectKey, iid, "/discussions"), nil)
}

// ListMRCommits lists the commits of an MR (newest first, per the API).
func (c *Client) ListMRCommits(ctx context.Context, projectKey string, iid int64) ([]Commit, error) {
	return getList[Commit](ctx, c.tr, mrPath(projectKey, iid, "/commits"), nil)
}

// ListMRPipelines lists pipelines for an MR.
func (c *Client) ListMRPipelines(ctx context.Context, projectKey string, iid int64) ([]Pipeline, error) {
	return getList[Pipeline](ctx, c.tr, mrPath(projectKey, iid, "/pipelines"), nil)
}

// GetMRApprovals fetches the approval state of an MR. Unlike approval *rules*,
// this endpoint is available on GitLab Free.
func (c *Client) GetMRApprovals(ctx context.Context, projectKey string, iid int64) (*Approvals, error) {
	var a Approvals
	if err := c.tr.do(ctx, "GET", mrPath(projectKey, iid, "/approvals"), nil, nil, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// GetRawFile fetches a repository file's raw content at ref.
func (c *Client) GetRawFile(ctx context.Context, projectKey, filePath, ref string) ([]byte, error) {
	q := url.Values{}
	q.Set("ref", ref)
	path := fmt.Sprintf("/projects/%s/repository/files/%s/raw", projectKey, url.PathEscape(filePath))
	resp, err := c.tr.doRaw(ctx, "GET", path, q, nil)
	if err != nil {
		return nil, err
	}
	return resp.body, nil
}
