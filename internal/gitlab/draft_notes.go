package gitlab

import (
	"context"
	"fmt"
)

// draftNoteRequest is the POST body for a draft note. A nil position creates an
// overview (non-positioned) draft note.
type draftNoteRequest struct {
	Note     string    `json:"note"`
	Position *Position `json:"position,omitempty"`
}

// CreateDraftNote creates an unpublished draft note, optionally inline.
func (c *Client) CreateDraftNote(ctx context.Context, projectKey string, iid int64, note string, pos *Position) (*DraftNote, error) {
	var dn DraftNote
	body := draftNoteRequest{Note: note, Position: pos}
	if err := c.do(ctx, "POST", mrPath(projectKey, iid, "/draft_notes"), nil, body, &dn); err != nil {
		return nil, err
	}
	return &dn, nil
}

// ListDraftNotes lists the current user's draft notes on an MR.
func (c *Client) ListDraftNotes(ctx context.Context, projectKey string, iid int64) ([]DraftNote, error) {
	return getList[DraftNote](ctx, c, mrPath(projectKey, iid, "/draft_notes"), nil)
}

// PublishDraftNote publishes a single draft note.
func (c *Client) PublishDraftNote(ctx context.Context, projectKey string, iid, draftID int64) error {
	path := mrPath(projectKey, iid, fmt.Sprintf("/draft_notes/%d/publish", draftID))
	return c.do(ctx, "PUT", path, nil, nil, nil)
}

// BulkPublishDraftNotes publishes all of the current user's draft notes on an MR.
func (c *Client) BulkPublishDraftNotes(ctx context.Context, projectKey string, iid int64) error {
	return c.do(ctx, "POST", mrPath(projectKey, iid, "/draft_notes/bulk_publish"), nil, nil, nil)
}

// noteRequest is the POST body for a plain MR note.
type noteRequest struct {
	Body string `json:"body"`
}

// CreateMRNote posts a non-positioned overview note (fallback path).
func (c *Client) CreateMRNote(ctx context.Context, projectKey string, iid int64, body string) (*Note, error) {
	var n Note
	if err := c.do(ctx, "POST", mrPath(projectKey, iid, "/notes"), nil, noteRequest{Body: body}, &n); err != nil {
		return nil, err
	}
	return &n, nil
}

// discussionRequest is the POST body for a positioned discussion (direct-post
// fallback when draft notes are not desired).
type discussionRequest struct {
	Body     string    `json:"body"`
	Position *Position `json:"position,omitempty"`
}

// CreateDiscussion posts a discussion, optionally inline.
func (c *Client) CreateDiscussion(ctx context.Context, projectKey string, iid int64, body string, pos *Position) (*Discussion, error) {
	var d Discussion
	if err := c.do(ctx, "POST", mrPath(projectKey, iid, "/discussions"), nil, discussionRequest{Body: body, Position: pos}, &d); err != nil {
		return nil, err
	}
	return &d, nil
}
