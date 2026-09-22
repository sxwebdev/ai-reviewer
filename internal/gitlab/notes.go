package gitlab

import "context"

// Direct note publication. The team service is automation-first: it posts
// findings as inline discussions and the run summary as an overview note, both
// immediately — there is no human sitting in front of a draft queue.

// noteRequest is the POST body for a plain MR note.
type noteRequest struct {
	Body string `json:"body"`
}

// CreateMRNote posts a non-positioned overview note. This carries the run
// summary and the review marker (see RenderReviewMarker) and is published last,
// after every finding, so that the marker's presence means "the whole review
// landed".
func (c *Client) CreateMRNote(ctx context.Context, projectKey string, iid int64, body string) (*Note, error) {
	var n Note
	if err := c.tr.do(ctx, "POST", mrPath(projectKey, iid, "/notes"), nil, noteRequest{Body: body}, &n); err != nil {
		return nil, err
	}
	return &n, nil
}

// discussionRequest is the POST body for a positioned discussion.
type discussionRequest struct {
	Body     string    `json:"body"`
	Position *Position `json:"position,omitempty"`
}

// CreateDiscussion posts a discussion thread, inline when pos is non-nil. One
// call per published finding; the body carries the finding marker.
func (c *Client) CreateDiscussion(ctx context.Context, projectKey string, iid int64, body string, pos *Position) (*Discussion, error) {
	var d Discussion
	if err := c.tr.do(ctx, "POST", mrPath(projectKey, iid, "/discussions"), nil, discussionRequest{Body: body, Position: pos}, &d); err != nil {
		return nil, err
	}
	return &d, nil
}
