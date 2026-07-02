package gitlab

import (
	"context"
	"errors"
)

// ErrNotConfigured is returned by the unconfigured client until a GitLab host
// and token are set. It lets the web UI start and render before setup.
var ErrNotConfigured = errors.New("GitLab is not configured: set gitlab.host and the token env var")

type unconfigured struct{}

// Unconfigured returns an API whose every call fails with ErrNotConfigured.
func Unconfigured() API { return unconfigured{} }

func (unconfigured) CurrentUser(context.Context) (*User, error) { return nil, ErrNotConfigured }
func (unconfigured) ListReviewerMRs(context.Context) ([]MergeRequest, error) {
	return nil, ErrNotConfigured
}
func (unconfigured) GetProject(context.Context, string) (*Project, error) {
	return nil, ErrNotConfigured
}
func (unconfigured) GetMR(context.Context, string, int64) (*MergeRequest, error) {
	return nil, ErrNotConfigured
}
func (unconfigured) ListMRDiffs(context.Context, string, int64) ([]MergeRequestDiff, error) {
	return nil, ErrNotConfigured
}
func (unconfigured) ListMRVersions(context.Context, string, int64) ([]MergeRequestVersion, error) {
	return nil, ErrNotConfigured
}
func (unconfigured) ListMRDiscussions(context.Context, string, int64) ([]Discussion, error) {
	return nil, ErrNotConfigured
}
func (unconfigured) ListMRPipelines(context.Context, string, int64) ([]Pipeline, error) {
	return nil, ErrNotConfigured
}
func (unconfigured) ListMRCommits(context.Context, string, int64) ([]Commit, error) {
	return nil, ErrNotConfigured
}
func (unconfigured) GetRawFile(context.Context, string, string, string) ([]byte, error) {
	return nil, ErrNotConfigured
}
func (unconfigured) CreateDraftNote(context.Context, string, int64, string, *Position) (*DraftNote, error) {
	return nil, ErrNotConfigured
}
func (unconfigured) ListDraftNotes(context.Context, string, int64) ([]DraftNote, error) {
	return nil, ErrNotConfigured
}
func (unconfigured) PublishDraftNote(context.Context, string, int64, int64) error {
	return ErrNotConfigured
}
func (unconfigured) BulkPublishDraftNotes(context.Context, string, int64) error {
	return ErrNotConfigured
}
func (unconfigured) CreateMRNote(context.Context, string, int64, string) (*Note, error) {
	return nil, ErrNotConfigured
}
func (unconfigured) CreateDiscussion(context.Context, string, int64, string, *Position) (*Discussion, error) {
	return nil, ErrNotConfigured
}

var _ API = unconfigured{}
