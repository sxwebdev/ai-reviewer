package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/sxwebdev/ai-reviewer/internal/git"
	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/index"
	"github.com/sxwebdev/ai-reviewer/internal/review"
	"github.com/sxwebdev/ai-reviewer/internal/state"
)

// ReviewConfig carries the scalar settings a review needs.
type ReviewConfig struct {
	Host             string
	ReviewerUsername string
	Model            string
	LLMProvider      string
	AgentMode        bool
	AllowedTools     []string
	Profile          *review.Profile
	Token            string
	CacheDir         string
	IgnoreGlobs      []string
}

// ReviewService drives a full review: fetch MR context from GitLab, run the
// engine, and persist the review + findings to state.
type ReviewService struct {
	gl      gitlab.API
	db      *state.DB
	eng     *review.Engine
	cfg     ReviewConfig
	log     *slog.Logger
	cache   *git.Cache
	indexer *index.Indexer
}

// NewReviewService constructs a ReviewService. When agent mode and a cache dir
// are configured it also prepares the git cache/indexer for repo worktrees.
func NewReviewService(gl gitlab.API, db *state.DB, eng *review.Engine, cfg ReviewConfig, log *slog.Logger) *ReviewService {
	if cfg.Profile == nil {
		cfg.Profile = review.DefaultProfile()
	}
	s := &ReviewService{gl: gl, db: db, eng: eng, cfg: cfg, log: log}
	if cfg.AgentMode && cfg.CacheDir != "" {
		s.cache = git.NewCache(cfg.CacheDir, log)
		s.indexer = index.NewIndexer(db, log)
	}
	return s
}

// RunReview reviews the MR identified by ref and returns the new review id.
func (s *ReviewService) RunReview(ctx context.Context, ref gitlab.MRRef) (string, error) {
	projectKey := ref.ProjectKey()

	mr, err := s.gl.GetMR(ctx, projectKey, ref.IID)
	if err != nil {
		return "", fmt.Errorf("get MR: %w", err)
	}
	proj, err := s.gl.GetProject(ctx, projectKey)
	if err != nil {
		return "", fmt.Errorf("get project: %w", err)
	}

	mrID, err := s.upsertMR(ctx, proj, mr)
	if err != nil {
		return "", err
	}

	diffs, err := s.gl.ListMRDiffs(ctx, projectKey, ref.IID)
	if err != nil {
		return "", fmt.Errorf("list diffs: %w", err)
	}
	files, rawDiffs := parseDiffs(diffs, s.log)
	if len(files) == 0 {
		return "", fmt.Errorf("no reviewable changed files (binary/generated/vendored excluded)")
	}

	discussions, _ := s.gl.ListMRDiscussions(ctx, projectKey, ref.IID)
	pipelineStatus := s.latestPipelineStatus(ctx, projectKey, ref.IID)
	memory := s.loadMemory(ctx)
	existing := s.existingFingerprints(ctx, mrID)

	// Agent mode: prepare a read-only worktree at head sha so the LLM can
	// inspect the repository. Best-effort — any failure falls back to
	// diff-only review.
	workDir, agentMode, cleanup := s.prepareWorktree(ctx, proj, mr)
	defer cleanup()

	in := review.ReviewInput{
		ProjectPath:          proj.PathWithNamespace,
		ProjectID:            proj.ID,
		MRIID:                mr.IID,
		Title:                mr.Title,
		Description:          mr.Description,
		AuthorUsername:       mr.Author.Username,
		ReviewerUsername:     s.cfg.ReviewerUsername,
		SourceBranch:         mr.SourceBranch,
		TargetBranch:         mr.TargetBranch,
		Files:                files,
		RawDiffs:             rawDiffs,
		Refs:                 mr.DiffRefs,
		Memory:               memory,
		Profile:              s.cfg.Profile,
		ExistingFingerprints: existing,
		PipelineStatus:       pipelineStatus,
		ExistingDiscussions:  len(discussions),
		WorkDir:              workDir,
		AgentMode:            agentMode,
		AllowedTools:         s.cfg.AllowedTools,
		Model:                s.cfg.Model,
	}

	result, err := s.eng.Review(ctx, in)
	if err != nil {
		return "", err
	}
	return s.persist(ctx, mrID, mr, result)
}

func (s *ReviewService) upsertMR(ctx context.Context, proj *gitlab.Project, mr *gitlab.MergeRequest) (int64, error) {
	if _, err := s.db.UpsertProject(ctx, &state.Project{
		GitLabHost: s.cfg.Host, ProjectID: proj.ID, PathWithNamespace: proj.PathWithNamespace,
		DefaultBranch: proj.DefaultBranch, CloneURLHTTP: proj.HTTPURLToRepo, WebURL: proj.WebURL,
	}); err != nil {
		return 0, err
	}
	return s.db.UpsertMergeRequest(ctx, &state.MergeRequest{
		GitLabHost: s.cfg.Host, ProjectID: proj.ID, IID: mr.IID, WebURL: mr.WebURL,
		Title: mr.Title, Description: mr.Description, AuthorUsername: mr.Author.Username,
		SourceBranch: mr.SourceBranch, TargetBranch: mr.TargetBranch, State: mr.State,
		Draft: mr.IsDraft(), HeadSHA: mr.SHA, BaseSHA: mr.DiffRefs.BaseSHA, StartSHA: mr.DiffRefs.StartSHA,
		UpdatedAt: parseTime(mr.UpdatedAt),
	})
}

// prepareWorktree clones/fetches the project mirror, checks out a worktree at
// the MR head sha, and indexes it. It returns the worktree dir, whether agent
// mode is active, and a cleanup func (always non-nil). On any failure it
// degrades to diff-only review.
func (s *ReviewService) prepareWorktree(ctx context.Context, proj *gitlab.Project, mr *gitlab.MergeRequest) (string, bool, func()) {
	noop := func() {}
	if s.cache == nil || proj.HTTPURLToRepo == "" || mr.SHA == "" {
		return "", false, noop
	}
	if _, err := s.cache.EnsureMirror(ctx, proj.HTTPURLToRepo, s.cfg.Host, proj.PathWithNamespace, s.cfg.Token); err != nil {
		s.log.Warn("agent mode: mirror failed, falling back to diff-only", "err", err)
		return "", false, noop
	}
	wt, cleanup, err := s.cache.AddWorktree(ctx, s.cfg.Host, proj.PathWithNamespace, mr.SHA)
	if err != nil {
		s.log.Warn("agent mode: worktree failed, falling back to diff-only", "err", err)
		return "", false, noop
	}
	if s.indexer != nil {
		if n, err := s.indexer.IndexWorktree(ctx, proj.ID, mr.SHA, wt, s.cfg.IgnoreGlobs); err != nil {
			s.log.Warn("index worktree failed", "err", err)
		} else {
			s.log.Info("indexed worktree", "files", n, "sha", mr.SHA)
		}
	}
	return wt, true, cleanup
}

func (s *ReviewService) latestPipelineStatus(ctx context.Context, projectKey string, iid int64) string {
	pipelines, err := s.gl.ListMRPipelines(ctx, projectKey, iid)
	if err != nil || len(pipelines) == 0 {
		return ""
	}
	return pipelines[0].Status
}

func (s *ReviewService) loadMemory(ctx context.Context) []review.MemoryRule {
	items, err := s.db.ListReviewMemory(ctx)
	if err != nil {
		s.log.Warn("load memory failed", "err", err)
		return nil
	}
	var out []review.MemoryRule
	for _, m := range items {
		if !m.Enabled {
			continue
		}
		out = append(out, review.MemoryRule{Type: m.Type, Title: m.Title, Body: m.Body})
		if len(out) >= 20 {
			break
		}
	}
	return out
}

func (s *ReviewService) existingFingerprints(ctx context.Context, mrID int64) map[string]bool {
	prior, err := s.db.ListFindingsByMR(ctx, mrID)
	if err != nil {
		return nil
	}
	set := map[string]bool{}
	for _, f := range prior {
		if f.Fingerprint != "" {
			set[f.Fingerprint] = true
		}
	}
	return set
}

func (s *ReviewService) persist(ctx context.Context, mrID int64, mr *gitlab.MergeRequest, result *review.Result) (string, error) {
	reviewID := uuid.NewString()
	rv := &state.Review{
		ID: reviewID, MRID: mrID, ProjectID: mr.ProjectID, MRIID: mr.IID,
		HeadSHA: mr.DiffRefs.HeadSHA, BaseSHA: mr.DiffRefs.BaseSHA, StartSHA: mr.DiffRefs.StartSHA,
		Mode: "full", Status: state.ReviewReady, RiskLevel: result.RiskLevel,
		OverallRecommendation: result.Recommendation, LLMProvider: s.cfg.LLMProvider,
		LLMModel: s.cfg.Model, ReviewerProfileID: s.cfg.Profile.Name, Summary: result.Summary,
		RawReportJSON: result.Raw, CostUSD: result.CostUSD,
	}
	if err := s.db.CreateReview(ctx, rv); err != nil {
		return "", fmt.Errorf("create review: %w", err)
	}
	for _, vf := range result.Findings {
		if err := s.db.InsertFinding(ctx, toStateFinding(reviewID, mrID, mr.DiffRefs.HeadSHA, vf)); err != nil {
			s.log.Warn("insert finding failed", "title", vf.Title, "err", err)
		}
	}
	return reviewID, nil
}

// toStateFinding maps an engine ValidatedFinding to a persisted row.
func toStateFinding(reviewID string, mrID int64, headSHA string, vf review.ValidatedFinding) *state.Finding {
	f := &state.Finding{
		ID: uuid.NewString(), ReviewID: reviewID, MRID: mrID, HeadSHA: headSHA,
		Severity: vf.Severity, Category: vf.Category, FilePath: vf.FilePath,
		LineKind: vf.Source.LineKind, Title: vf.Title, Body: vf.Body, Suggestion: vf.Suggestion,
		Confidence: vf.Confidence, Fingerprint: vf.Fingerprint, Status: state.FindingProposed,
		ValidationError: vf.ValidationError,
	}
	if ev, err := json.Marshal(vf.Source.Evidence); err == nil {
		f.EvidenceJSON = string(ev)
	}
	if vf.Position != nil {
		f.OldPath = vf.Position.OldPath
		f.NewPath = vf.Position.NewPath
		f.OldLine = intPtrToInt64(vf.Position.OldLine)
		f.NewLine = intPtrToInt64(vf.Position.NewLine)
		if pj, err := json.Marshal(vf.Position); err == nil {
			f.GitLabPositionJSON = string(pj)
		}
	}
	return f
}

func intPtrToInt64(p *int) *int64 {
	if p == nil {
		return nil
	}
	v := int64(*p)
	return &v
}

// parseDiffs converts GitLab diffs into engine FileDiffs, excluding binary,
// generated, and vendored files (never sent to the LLM).
func parseDiffs(diffs []gitlab.MergeRequestDiff, log *slog.Logger) ([]*review.FileDiff, map[string]string) {
	var files []*review.FileDiff
	raw := map[string]string{}
	for _, d := range diffs {
		path := d.NewPath
		if path == "" {
			path = d.OldPath
		}
		if d.GeneratedFile || review.IsBinaryDiff(d.Diff) || isVendored(path) {
			continue
		}
		hunks, err := review.ParseHunks(d.Diff)
		if err != nil {
			log.Warn("parse diff failed", "path", path, "err", err)
			continue
		}
		if len(hunks) == 0 {
			continue
		}
		files = append(files, &review.FileDiff{
			OldPath: d.OldPath, NewPath: d.NewPath, NewFile: d.NewFile,
			Renamed: d.RenamedFile, Deleted: d.DeletedFile, Hunks: hunks,
		})
		raw[path] = d.Diff
	}
	return files, raw
}

var vendorPrefixes = []string{"vendor/", "node_modules/", "dist/", "build/", "third_party/"}

func isVendored(path string) bool {
	for _, p := range vendorPrefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return strings.HasSuffix(path, ".pb.go") || strings.HasSuffix(path, ".min.js")
}
