// Package service holds the application service layer: UI-agnostic business
// logic that the web server and CLI both call. Services depend on the gitlab
// API interface and the state repositories, so they are unit-testable with
// fakes and a temp database.
package service

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/state"
)

// SyncResult summarizes a sync run.
type SyncResult struct {
	Total   int
	Tracked int
}

// SyncService pulls MRs assigned to the current user into local state.
type SyncService struct {
	gl   gitlab.API
	db   *state.DB
	host string
	log  *slog.Logger
}

// NewSyncService constructs a SyncService. host is the configured GitLab host,
// used as the local project/MR namespace key.
func NewSyncService(gl gitlab.API, db *state.DB, host string, log *slog.Logger) *SyncService {
	return &SyncService{gl: gl, db: db, host: host, log: log}
}

// SyncAssignedMRs fetches open MRs where the user is a reviewer and upserts them
// (and their projects) into the database.
func (s *SyncService) SyncAssignedMRs(ctx context.Context) (SyncResult, error) {
	mrs, err := s.gl.ListReviewerMRs(ctx)
	if err != nil {
		return SyncResult{}, err
	}

	projectPaths := map[int64]string{}
	res := SyncResult{Total: len(mrs)}
	for i := range mrs {
		mr := &mrs[i]
		if err := s.trackProject(ctx, mr.ProjectID, projectPaths); err != nil {
			s.log.Warn("track project failed", "project_id", mr.ProjectID, "err", err)
		}
		if err := s.trackMR(ctx, mr); err != nil {
			s.log.Warn("track MR failed", "iid", mr.IID, "err", err)
			continue
		}
		res.Tracked++
	}
	return res, nil
}

func (s *SyncService) trackProject(ctx context.Context, projectID int64, cache map[int64]string) error {
	if _, ok := cache[projectID]; ok {
		return nil
	}
	p, err := s.gl.GetProject(ctx, strconv.FormatInt(projectID, 10))
	if err != nil {
		return err
	}
	cache[projectID] = p.PathWithNamespace
	_, err = s.db.UpsertProject(ctx, &state.Project{
		GitLabHost:        s.host,
		ProjectID:         p.ID,
		PathWithNamespace: p.PathWithNamespace,
		DefaultBranch:     p.DefaultBranch,
		CloneURLHTTP:      p.HTTPURLToRepo,
		WebURL:            p.WebURL,
	})
	return err
}

func (s *SyncService) trackMR(ctx context.Context, mr *gitlab.MergeRequest) error {
	row := &state.MergeRequest{
		GitLabHost:     s.host,
		ProjectID:      mr.ProjectID,
		IID:            mr.IID,
		WebURL:         mr.WebURL,
		Title:          mr.Title,
		Description:    mr.Description,
		AuthorUsername: mr.Author.Username,
		SourceBranch:   mr.SourceBranch,
		TargetBranch:   mr.TargetBranch,
		State:          mr.State,
		Draft:          mr.IsDraft(),
		HeadSHA:        mr.SHA,
		BaseSHA:        mr.DiffRefs.BaseSHA,
		StartSHA:       mr.DiffRefs.StartSHA,
		UpdatedAt:      parseTime(mr.UpdatedAt),
	}
	_, err := s.db.UpsertMergeRequest(ctx, row)
	return err
}

// parseTime converts an RFC3339 timestamp to unix-millis (0 on failure).
func parseTime(s string) int64 {
	if s == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}
