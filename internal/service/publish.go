package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/models"
	"github.com/sxwebdev/ai-reviewer/internal/security"
)

// PublishReview puts a persisted review into GitLab.
//
// It is idempotent by construction, which is what lets the publish_review job
// retry ten times without ever duplicating a comment (§10.4):
//
//   - only findings with note_id IS NULL are posted, and note_id/published_at
//     are written immediately after each successful POST, not batched at the
//     end — so a crash costs at most a resumed loop, never a duplicate;
//   - a finding whose fingerprint marker is already on the MR is adopted rather
//     than reposted, which is what makes a restored-from-backup database safe;
//   - the summary note carrying the review marker goes LAST, and status is set
//     to 'succeeded' only after it lands AND after the database agrees that
//     nothing is left unposted, so 'succeeded' can never be a false success;
//   - a summary marker already present for this head SHA is not posted twice.
//
// It returns the number of findings actually posted in this call.
func (s *Service) PublishReview(ctx context.Context, reviewID uuid.UUID) (int, error) {
	rev, err := s.st.Review().GetByID(ctx, reviewID)
	if err != nil {
		return 0, fmt.Errorf("load review %s: %w", reviewID, err)
	}
	switch rev.Status {
	case StatusSucceeded:
		// Already published in full — the retry-safe no-op.
		return 0, nil
	case StatusDryRun, StatusFailed, StatusAbandoned:
		// A dry run must never reach GitLab, a failed run has nothing to say, and
		// an abandoned one has already been given up on. All three are successful
		// no-ops so the job does not retry forever.
		s.log.Infow("publish skipped", "review_id", reviewID, "status", rev.Status)
		return 0, nil
	}

	pk := strconv.FormatInt(rev.ProjectID, 10)

	// One discussions read answers both idempotency questions: which findings
	// are already on the MR, and whether the summary for this head SHA is.
	discussions, err := s.gl.ListMRDiscussions(ctx, pk, rev.MrIid)
	if err != nil {
		return 0, fmt.Errorf("list discussions for %d!%d: %w", rev.ProjectID, rev.MrIid, err)
	}
	postedFindings := findingNotesByFingerprint(discussions)
	summaryPosted := reviewMarkerPosted(discussions, rev.HeadSha)

	pending, err := s.st.Finding().ListUnpublishedByReview(ctx, reviewID)
	if err != nil {
		return 0, fmt.Errorf("list unpublished findings: %w", err)
	}

	published := 0
	for _, f := range pending {
		if noteID, ok := postedFindings[normalizeFingerprint(f.Fingerprint)]; ok {
			// The comment is on the MR but this database does not know it.
			// Adopt the existing note instead of posting a second one.
			if err := s.markPublished(ctx, f.ID, noteID); err != nil {
				return published, err
			}
			continue
		}
		noteID, err := s.postFinding(ctx, pk, rev.MrIid, f)
		if err != nil {
			// Stop at the first failure: the remaining findings keep note_id
			// NULL, so the retry resumes exactly here. Continuing would risk
			// posting the summary marker while findings are still missing.
			if s.abandonIfPermanent(ctx, rev, err) {
				return published, nil
			}
			return published, fmt.Errorf("publish finding %s: %w", f.ID, err)
		}
		if err := s.markPublished(ctx, f.ID, noteID); err != nil {
			return published, err
		}
		published++
	}

	// §10.4's "no false success", asserted rather than assumed. The set of
	// findings owned by this review is re-read because 'succeeded' is a claim
	// about GitLab's state, and the only evidence for it is that every row this
	// review owns carries a note id. A row still NULL here means either a
	// markPublished that did not land or a re-attachment that moved a finding
	// under us — both of which would otherwise produce a review marked succeeded
	// with findings on no merge request.
	left, err := s.st.Finding().ListUnpublishedByReview(ctx, reviewID)
	if err != nil {
		return published, fmt.Errorf("verify published findings: %w", err)
	}
	if len(left) > 0 {
		return published, fmt.Errorf("review %s still has %d unpublished finding(s); not posting the summary", reviewID, len(left))
	}

	if !summaryPosted {
		if _, err := s.gl.CreateMRNote(ctx, pk, rev.MrIid, s.renderSummary(rev)); err != nil {
			if s.abandonIfPermanent(ctx, rev, err) {
				return published, nil
			}
			return published, fmt.Errorf("publish summary note: %w", err)
		}
	}

	// Detached: the comments are live on GitLab now, and a shutdown between the
	// last POST and this UPDATE would otherwise leave the review 'reviewed'
	// forever, re-published on every safety-net pass.
	markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := s.st.Review().MarkSucceeded(markCtx, reviewID); err != nil {
		return published, fmt.Errorf("mark review succeeded: %w", err)
	}

	s.log.Infow("review published", "review_id", reviewID, "project_id", rev.ProjectID,
		"iid", rev.MrIid, "head_sha", rev.HeadSha, "published", published)
	return published, nil
}

// postFinding publishes one finding and returns the id of the note it created.
//
// An inline position that GitLab rejects falls back to an unpositioned
// discussion. GitLab refuses a position it cannot resolve in the current diff,
// and that refusal is deterministic: without the fallback such a finding would
// exhaust the job's ten attempts, then be re-enqueued by the §6.3 safety net,
// forever. Losing the line anchor is a much smaller loss than losing the
// finding and looping.
func (s *Service) postFinding(ctx context.Context, pk string, iid int64, f *models.MrFinding) (int64, error) {
	body := renderFinding(f)
	pos := decodePosition(f.PositionJson, s.log)

	d, err := s.gl.CreateDiscussion(ctx, pk, iid, body, pos)
	if err != nil && pos != nil && positionRejected(err) {
		s.log.Warnw("gitlab rejected the stored position; posting the finding without a line anchor",
			"finding", f.ID, "file", f.FilePath, "err", err)
		d, err = s.gl.CreateDiscussion(ctx, pk, iid, body, nil)
	}
	if err != nil {
		return 0, err
	}
	if len(d.Notes) == 0 {
		// GitLab always echoes the created note; without it we cannot record
		// what we posted, and recording nothing would repost on the next retry.
		return 0, errors.New("gitlab returned a discussion with no notes")
	}
	return d.Notes[0].ID, nil
}

// markPublished records a note id against a finding on a detached context: the
// comment already exists on GitLab, so losing this write means posting it
// twice.
//
// The error stops publication rather than being logged and stepped over. A row
// left with note_id NULL is invisible to ListPublishedFingerprints, so the next
// review re-derives the finding and posts a comment that is already on the MR —
// and if the loop continued, MarkSucceeded would run and short-circuit every
// later PublishReview, making the NULL permanent. Aborting is safe precisely
// because the note carries a fingerprint marker: the retry finds it in the
// discussions and adopts it instead of posting a second one.
func (s *Service) markPublished(ctx context.Context, findingID uuid.UUID, noteID int64) error {
	markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := s.st.Finding().MarkPublished(markCtx, noteID, findingID); err != nil {
		s.log.Errorw("published to GitLab but failed to record the note id; the retry will adopt the existing note",
			"finding", findingID, "note_id", noteID, "err", err)
		return fmt.Errorf("record note id for finding %s: %w", findingID, err)
	}
	return nil
}

// abandonIfPermanent retires a review whose publication GitLab has refused for a
// reason no retry can change, and reports whether it did.
//
// Publication is retried ten times and then re-enqueued by the §6.3 sweep on
// every scan. For a merge request that was deleted, a project that was archived
// or a token that lost its api scope, that loop never terminates: it runs
// forever through a one- or two-worker queue and starves the publications that
// could still succeed. A deterministic 4xx is GitLab's final answer, so the
// review is marked terminal here instead of waiting for the sweep's attempt
// ceiling to notice the same thing an hour later.
//
// Returning true makes the job succeed: there is nothing left for it to do, and
// failing would only spend the remaining attempts on the same 4xx.
func (s *Service) abandonIfPermanent(ctx context.Context, rev *models.MrReview, cause error) bool {
	if !positionRejected(cause) {
		return false
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	stored := security.Truncate(security.Mask(cause.Error()), maxStoredErrorLen)
	n, err := s.st.Review().Abandon(writeCtx, stored, rev.ID)
	if err != nil {
		s.log.Errorw("abandoning the unpublishable review failed; the sweep will keep retrying it",
			"review_id", rev.ID, "err", err)
		return false
	}
	if n == 0 {
		return false
	}
	s.log.Errorw("giving up on publishing this review: GitLab refused deterministically",
		"review_id", rev.ID, "project_id", rev.ProjectID, "iid", rev.MrIid,
		"head_sha", rev.HeadSha, "err", cause)
	return true
}

// renderFinding is the published comment: a severity/category header, the
// stored body, and the fingerprint marker that makes the comment recognisable
// on a later run even without the database.
func renderFinding(f *models.MrFinding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**[%s/%s] %s**\n\n", f.Severity, f.Category, strings.TrimSpace(f.Title))
	b.WriteString(strings.TrimSpace(f.Body))
	if marker := gitlab.RenderFindingMarker(f.Fingerprint); marker != "" {
		b.WriteString("\n\n")
		b.WriteString(marker)
	}
	return b.String()
}

// renderSummary is the overview note published last. Its marker is what says
// "the whole review landed", which is why nothing else may be posted after it.
func (s *Service) renderSummary(rev *models.MrReview) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🤖 **AI review** — %d finding(s)", rev.FindingsCount)
	if rev.RiskLevel != "" {
		fmt.Fprintf(&b, ", risk: %s", rev.RiskLevel)
	}
	b.WriteString("\n")
	if summary := strings.TrimSpace(rev.Summary); summary != "" {
		b.WriteString("\n")
		b.WriteString(summary)
		b.WriteString("\n")
	}
	marker := gitlab.ReviewMarker{
		ProjectID:  rev.ProjectID,
		MRIID:      rev.MrIid,
		HeadSHA:    rev.HeadSha,
		ReviewedAt: s.now(),
		Findings:   int(rev.FindingsCount),
		Pipeline:   s.cfg.PipelineName,
		Tool:       s.cfg.ToolVersion,
	}.Render()
	if marker != "" {
		b.WriteString("\n")
		b.WriteString(marker)
	}
	return b.String()
}

// findingNotesByFingerprint indexes the notes already on the MR by the
// fingerprint marker they carry, so a finding we lost track of can be adopted
// instead of reposted.
func findingNotesByFingerprint(discussions []gitlab.Discussion) map[string]int64 {
	out := map[string]int64{}
	for _, d := range discussions {
		for _, n := range d.Notes {
			for _, fp := range gitlab.ParseFindingMarkers(n.Body) {
				if fp = normalizeFingerprint(fp); fp != "" {
					// First wins: GitLab returns notes oldest-first, and the
					// original posting is the one to adopt.
					if _, seen := out[fp]; !seen {
						out[fp] = n.ID
					}
				}
			}
		}
	}
	return out
}

// reviewMarkerPosted reports whether a review marker for this exact head SHA is
// already on the MR. Matching on the SHA is what lets a re-review post its own
// summary while a retry of the same review does not post a second one.
func reviewMarkerPosted(discussions []gitlab.Discussion, headSHA string) bool {
	if headSHA == "" {
		return false
	}
	for _, d := range discussions {
		for _, n := range d.Notes {
			if m, ok := gitlab.ParseReviewMarker(n.Body); ok && m.HeadSHA == headSHA {
				return true
			}
		}
	}
	return false
}

// decodePosition restores the GitLab position stored with a finding. An empty
// or unreadable payload yields nil, i.e. an overview comment — the same
// degradation the engine's own mapping ladder ends in.
func decodePosition(raw []byte, log loggerWarn) *gitlab.Position {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "{}" || trimmed == "null" {
		return nil
	}
	var pos gitlab.Position
	if err := json.Unmarshal(raw, &pos); err != nil {
		log.Warnw("stored position is unreadable; posting the finding as an overview comment", "err", err)
		return nil
	}
	if pos.NewPath == "" && pos.OldPath == "" {
		return nil
	}
	return &pos
}

// loggerWarn is the sliver of the logger decodePosition needs, so the helper
// stays callable without a Service.
type loggerWarn interface {
	Warnw(msg string, keysAndValues ...any)
}

// positionRejected reports whether GitLab refused the request because of the
// position rather than because of a transient problem. 4xx other than 429 are
// deterministic: retrying them changes nothing.
func positionRejected(err error) bool {
	var apiErr *gitlab.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.Status >= http.StatusBadRequest &&
		apiErr.Status < http.StatusInternalServerError &&
		apiErr.Status != http.StatusTooManyRequests
}
