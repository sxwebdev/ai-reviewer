package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sxwebdev/ai-reviewer/internal/coverage"
	"github.com/sxwebdev/ai-reviewer/internal/jobs"
	"github.com/sxwebdev/ai-reviewer/internal/review"
	"github.com/sxwebdev/ai-reviewer/internal/service"
	"github.com/sxwebdev/ai-reviewer/internal/state"
)

type baseVM struct {
	Host      string
	Flash     string
	FlashKind string
	Active    string // nav highlight: dashboard|jobs|memory|settings
	PageClass string // extra class on <main> (e.g. "page--wide" for the MR workspace)
}

type dashboardVM struct {
	baseVM
	MRs []dashItem
}

// dashItem is a dashboard row plus live job state, with helpers that derive a
// clear review status for the table.
type dashItem struct {
	state.DashboardRow
	Running bool
	Failed  bool
}

func (d dashItem) Reviewed() bool { return d.ReviewHeadSHA != "" }
func (d dashItem) Current() bool  { return d.ReviewHeadSHA != "" && d.ReviewHeadSHA == d.HeadSHA }

// Status is a short label for the review state, shown as a badge.
func (d dashItem) Status() string {
	switch {
	case d.Running:
		return "reviewing"
	case !d.Reviewed() && d.Failed:
		return "failed"
	case !d.Reviewed():
		return "not reviewed"
	case d.Current():
		return "reviewed"
	default:
		return "head changed"
	}
}

// StatusClass maps Status to a CSS modifier.
func (d dashItem) StatusClass() string {
	switch d.Status() {
	case "reviewed":
		return "ok"
	case "reviewing":
		return "running"
	case "failed":
		return "failed"
	case "head changed":
		return "warn"
	default:
		return "none"
	}
}

type findingGroup struct {
	Severity string
	Items    []*state.Finding
}

// passReportVM is one pipeline pass for the review header (name, findings,
// cost, duration, error). Deserialized from reviews.pipeline_json.
type passReportVM struct {
	Name        string  `json:"name"`
	CostUSD     float64 `json:"cost_usd"`
	DurationMS  int64   `json:"duration_ms"`
	RawFindings int     `json:"raw_findings"`
	Err         string  `json:"error"`
}

// CostLabel formats the pass cost like the review-level label.
func (p passReportVM) CostLabel() string {
	if p.CostUSD <= 0 {
		return ""
	}
	return fmt.Sprintf("$%.4f", p.CostUSD)
}

// DurationLabel formats the pass duration in seconds.
func (p passReportVM) DurationLabel() string {
	if p.DurationMS <= 0 {
		return ""
	}
	return fmt.Sprintf("%.1fs", float64(p.DurationMS)/1000)
}

type mrVM struct {
	baseVM
	MR            *state.MergeRequest
	ProjectPath   string
	Running       bool
	Progress      string // "3/7" when a running review job reports progress
	JobStatus     string // latest review job status
	JobError      string // latest review job's error, if it failed
	Review        *state.Review
	Historical    bool // viewing a past review (not the latest) → actions hidden
	Groups        []findingGroup
	FindingCount  int
	ProposedCount int
	ApprovedCount int
	DraftedCount  int
	PublishPhrase string
	CostLabel     string
	PassReports   []passReportVM
	Risk          *review.RiskReport
	Completeness  *review.CompletenessReport
	Coverage      *coverage.Report
	ActionError   string
	PastReviews   []pastReviewVM
	Diff          diffVM
	findings      []*state.Finding // flat findings of the selected review, for the diff pane
}

// ---- diff viewer view models (assembled server-side; templates stay logic-free) ----

// diffVM is the right-hand diff pane for one review/head: the changed files with
// findings pinned to their mapped lines, plus any findings that did not resolve
// to a diff line (Unanchored). Captured is false when no diff was persisted for
// this head sha (e.g. an MR reviewed before diff persistence shipped).
type diffVM struct {
	Files       []diffFileVM
	Unanchored  []*state.Finding
	HasFindings bool
	Captured    bool
}

type diffFileVM struct {
	DisplayPath  string
	Kind         string // modified|new|deleted|renamed
	IsBinary     bool
	IsVendored   bool
	Expanded     bool
	FindingCount int
	Hunks        []diffHunkVM
}

type diffHunkVM struct {
	Header string
	Lines  []diffLineVM
}

type diffLineVM struct {
	Kind     string // add|del|ctx — CSS-ready
	OldLine  string // "" when the line has no old-side number
	NewLine  string // "" when the line has no new-side number
	Content  string
	Findings []*state.Finding // findings pinned to this exact line
}

type pastReviewVM struct {
	ID        string
	When      string
	HeadSHA   string
	RiskLevel string
	Status    string
	Findings  int
}

type jobsVM struct {
	baseVM
	Jobs []*state.Job
}

type memoryVM struct {
	baseVM
	Items []*state.ReviewMemory
}

type settingsVM struct {
	baseVM
	Cfg UIConfig
}

func (s *Server) base(w http.ResponseWriter, r *http.Request) baseVM {
	kind, msg := takeFlash(w, r)
	return baseVM{Host: s.host, Flash: msg, FlashKind: kind}
}

func (s *Server) baseActive(w http.ResponseWriter, r *http.Request, active string) baseVM {
	b := s.base(w, r)
	b.Active = active
	return b
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := s.svc.DB.DashboardRows(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	running, failed := s.reviewJobState(ctx)
	items := make([]dashItem, 0, len(rows))
	for _, row := range rows {
		k := mrKey{row.ProjectID, row.IID}
		items = append(items, dashItem{DashboardRow: row, Running: running[k], Failed: failed[k]})
	}
	s.render(w, "dashboard", dashboardVM{baseVM: s.baseActive(w, r, "dashboard"), MRs: items})
}

type mrKey struct{ projectID, iid int64 }

// reviewJobState returns, per MR, whether its latest review job is active
// (queued/running) and whether that latest job failed.
func (s *Server) reviewJobState(ctx context.Context) (running, failed map[mrKey]bool) {
	running, failed = map[mrKey]bool{}, map[mrKey]bool{}
	list, err := s.svc.DB.LatestReviewJobsPerMR(ctx) // one latest review job per MR
	if err != nil {
		return
	}
	for _, j := range list {
		if j.ProjectID == nil || j.MRIID == nil {
			continue
		}
		k := mrKey{*j.ProjectID, *j.MRIID}
		switch j.Status {
		case state.JobQueued, state.JobRunning:
			running[k] = true
		case state.JobFailed:
			failed[k] = true
		}
	}
	return
}

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	res, err := s.svc.Sync.SyncAssignedMRs(r.Context())
	if err != nil {
		setFlash(w, "err", "Sync failed: "+err.Error())
	} else {
		setFlash(w, "ok", "Synced "+strconv.Itoa(res.Tracked)+" merge request(s).")
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// buildMRVM assembles the MR detail view model (everything except baseVM, which
// the caller fills). It is shared by the full page, the htmx review-section
// fragment, and finding-action responses.
func (s *Server) buildMRVM(ctx context.Context, id int64, selectedReviewID string) (mrVM, error) {
	mr, err := s.svc.DB.GetMergeRequest(ctx, id)
	if err != nil {
		return mrVM{}, err
	}
	vm := mrVM{MR: mr}
	if p, err := s.svc.DB.GetProjectByGitLabID(ctx, mr.GitLabHost, mr.ProjectID); err == nil {
		vm.ProjectPath = p.PathWithNamespace
	}
	if active, _ := s.svc.DB.HasActiveJob(ctx, state.JobReview, mr.ProjectID, mr.IID); active {
		vm.Running = true
	}
	// Latest review job for this MR → status / error / progress. Targeted query,
	// correct regardless of overall job volume.
	if j, err := s.svc.DB.GetLatestReviewJob(ctx, mr.ProjectID, mr.IID); err == nil {
		vm.JobStatus = j.Status
		if j.Status == state.JobFailed {
			vm.JobError = j.Error
		}
		if j.Status == state.JobRunning && j.ProgressTotal > 0 {
			vm.Progress = fmt.Sprintf("%d/%d", j.ProgressCurrent, j.ProgressTotal)
		}
	}
	reviews, _ := s.svc.DB.ListReviewsByMR(ctx, id)
	if len(reviews) > 0 {
		sel := reviews[0] // newest by default
		if selectedReviewID != "" {
			for _, rv := range reviews {
				if rv.ID == selectedReviewID {
					sel = rv
					break
				}
			}
		}
		vm.Historical = sel.ID != reviews[0].ID
		vm.Review = sel
		findings, _ := s.svc.DB.ListFindingsByReview(ctx, sel.ID)
		vm.findings = findings
		vm.FindingCount = len(findings)
		for _, f := range findings {
			switch f.Status {
			case state.FindingProposed:
				vm.ProposedCount++
			case state.FindingApproved:
				vm.ApprovedCount++
			case state.FindingDrafted:
				vm.DraftedCount++
			}
		}
		vm.Groups = groupBySeverity(findings)
		vm.PublishPhrase = service.ConfirmPhrase(vm.DraftedCount)
		vm.CostLabel = s.costLabel(vm.Review.CostUSD)
		if sel.PipelineJSON != "" {
			var reports []passReportVM
			if err := json.Unmarshal([]byte(sel.PipelineJSON), &reports); err == nil && len(reports) > 1 {
				vm.PassReports = reports
			}
		}
		// Best-effort report columns: garbage JSON simply yields no section.
		if sel.RiskJSON != "" {
			var r review.RiskReport
			if err := json.Unmarshal([]byte(sel.RiskJSON), &r); err == nil && len(r.Factors) > 0 {
				vm.Risk = &r
			}
		}
		if sel.CompletenessJSON != "" {
			var c review.CompletenessReport
			if err := json.Unmarshal([]byte(sel.CompletenessJSON), &c); err == nil && (len(c.Criteria) > 0 || c.Note != "") {
				vm.Completeness = &c
			}
		}
		if sel.CoverageJSON != "" {
			var cov coverage.Report
			if err := json.Unmarshal([]byte(sel.CoverageJSON), &cov); err == nil && (len(cov.Files) > 0 || len(cov.Skipped) > 0) {
				vm.Coverage = &cov
			}
		}
		for _, rv := range reviews {
			if rv.ID == sel.ID {
				continue // the one being shown is not listed under history
			}
			fs, _ := s.svc.DB.ListFindingsByReview(ctx, rv.ID)
			vm.PastReviews = append(vm.PastReviews, pastReviewVM{
				ID:      rv.ID,
				When:    time.UnixMilli(rv.CreatedAt).Format("2006-01-02 15:04"),
				HeadSHA: rv.HeadSHA, RiskLevel: rv.RiskLevel, Status: rv.Status, Findings: len(fs),
			})
		}
	}
	return vm, nil
}

// buildDiffVM assembles the diff pane for an MR at headSHA, overlaying findings
// onto the exact hunk line each was mapped to at review time. Findings that do
// not resolve to a line land in Unanchored. Display-only: it reads persisted
// positions and never computes new ones (positions are owned by the engine).
func (s *Server) buildDiffVM(ctx context.Context, mrID int64, headSHA string, findings []*state.Finding) diffVM {
	rows, err := s.svc.DB.ListMRDiffFiles(ctx, mrID, headSHA)
	if err != nil {
		s.log.Warn("list diffs failed", "mr", mrID, "err", err)
		return diffVM{}
	}
	if len(rows) == 0 {
		return diffVM{} // Captured stays false → template shows a "not captured" note
	}
	vm := diffVM{Captured: true}

	// Index findings by the file they belong to (FilePath is always set).
	byFile := map[string][]*state.Finding{}
	for _, f := range findings {
		byFile[f.FilePath] = append(byFile[f.FilePath], f)
	}
	anchored := map[string]bool{} // finding IDs that resolved to a line

	for _, row := range rows {
		fv := diffFileVM{
			DisplayPath: diffDisplayPath(row),
			Kind:        diffKind(row),
			IsBinary:    row.IsBinary,
			IsVendored:  row.IsVendored,
		}
		fileFindings := append([]*state.Finding{}, byFile[row.NewPath]...)
		if row.OldPath != "" && row.OldPath != row.NewPath {
			fileFindings = append(fileFindings, byFile[row.OldPath]...)
		}
		if !row.IsBinary {
			hunks, err := review.ParseHunks(row.Diff)
			if err != nil {
				s.log.Warn("parse diff failed", "path", fv.DisplayPath, "err", err)
			}
			for _, h := range hunks {
				hv := diffHunkVM{Header: hunkHeader(h)}
				for _, dl := range h.Lines {
					lv := diffLineVM{Kind: lineKindClass(dl.Kind), Content: dl.Content}
					if dl.OldLine > 0 {
						lv.OldLine = strconv.Itoa(dl.OldLine)
					}
					if dl.NewLine > 0 {
						lv.NewLine = strconv.Itoa(dl.NewLine)
					}
					for _, f := range fileFindings {
						if !anchored[f.ID] && findingOnLine(f, dl) {
							lv.Findings = append(lv.Findings, f)
							anchored[f.ID] = true
							fv.FindingCount++
							vm.HasFindings = true
						}
					}
					hv.Lines = append(hv.Lines, lv)
				}
				fv.Hunks = append(fv.Hunks, hv)
			}
		}
		fv.Expanded = fv.FindingCount > 0 && !row.IsBinary && !row.IsVendored
		vm.Files = append(vm.Files, fv)
	}

	for _, f := range findings {
		if !anchored[f.ID] {
			vm.Unanchored = append(vm.Unanchored, f)
			vm.HasFindings = true
		}
	}
	return vm
}

func diffDisplayPath(d *state.MRDiff) string {
	switch {
	case d.Renamed && d.OldPath != "" && d.NewPath != "" && d.OldPath != d.NewPath:
		return d.OldPath + " → " + d.NewPath
	case d.NewPath != "":
		return d.NewPath
	default:
		return d.OldPath
	}
}

func diffKind(d *state.MRDiff) string {
	switch {
	case d.NewFile:
		return "new"
	case d.Deleted:
		return "deleted"
	case d.Renamed:
		return "renamed"
	default:
		return "modified"
	}
}

func lineKindClass(k review.LineKind) string {
	switch k {
	case review.LineAdded:
		return "add"
	case review.LineRemoved:
		return "del"
	default:
		return "ctx"
	}
}

func hunkHeader(h review.Hunk) string {
	return fmt.Sprintf("@@ -%d,%d +%d,%d @@", h.OldStart, h.OldCount, h.NewStart, h.NewCount)
}

// findingOnLine reports whether f was mapped to this diff line, using the
// Go-owned convention: new_line anchors added/context lines, old_line anchors
// removed/context lines. It is an exact match — no re-snapping.
func findingOnLine(f *state.Finding, dl review.DiffLine) bool {
	if f.NewLine != nil {
		return (dl.Kind == review.LineAdded || dl.Kind == review.LineContext) && int64(dl.NewLine) == *f.NewLine
	}
	if f.OldLine != nil {
		return (dl.Kind == review.LineRemoved || dl.Kind == review.LineContext) && int64(dl.OldLine) == *f.OldLine
	}
	return false
}

func (s *Server) handleMR(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	vm, err := s.buildMRVM(r.Context(), id, r.URL.Query().Get("review"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	vm.baseVM = s.base(w, r)
	vm.PageClass = "page--wide"
	// Diff pane: use the selected review's head so line numbers match its
	// findings; fall back to the MR head for a not-yet-reviewed MR.
	headSHA := vm.MR.HeadSHA
	if vm.Review != nil {
		headSHA = vm.Review.HeadSHA
	}
	vm.Diff = s.buildDiffVM(r.Context(), vm.MR.ID, headSHA, vm.findings)
	s.render(w, "mr", vm)
}

// handleReviewSection renders just the review card — used for htmx polling while
// a review runs and for swapping after finding actions.
func (s *Server) handleReviewSection(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	vm, err := s.buildMRVM(r.Context(), id, "")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	vm.baseVM = baseVM{Host: s.host}
	// The poll only fires while a review runs. When it comes back not-running,
	// the review just finished — reload the whole page so the sibling diff pane
	// rebuilds with findings pinned inline (it is not part of this fragment).
	if !vm.Running {
		w.Header().Set("HX-Refresh", "true")
	}
	s.renderPartial(w, "mr", "review-section", vm)
}

func (s *Server) handleRunReview(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	mr, err := s.svc.DB.GetMergeRequest(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	jobID, err := jobs.EnqueueReview(r.Context(), s.svc.DB, mr.ID, mr.ProjectID, mr.IID)
	switch {
	case err != nil:
		setFlash(w, "err", "Could not queue review: "+err.Error())
	case jobID == "":
		setFlash(w, "ok", "A review is already queued for this MR.")
	default:
		setFlash(w, "ok", "Review queued.")
	}
	http.Redirect(w, r, "/mr/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// ---- finding actions (htmx-aware) ----

func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	fid := r.PathValue("id")
	mrID, err := s.findingMRID(r.Context(), fid)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.finishFindingAction(w, r, mrID, s.svc.Finding.Approve(r.Context(), fid))
}

func (s *Server) handleReject(w http.ResponseWriter, r *http.Request) {
	fid := r.PathValue("id")
	mrID, err := s.findingMRID(r.Context(), fid)
	if err != nil {
		s.fail(w, err)
		return
	}
	reason := r.FormValue("reason")
	fp := r.FormValue("fp") != ""
	s.finishFindingAction(w, r, mrID, s.svc.Finding.Reject(r.Context(), fid, reason, fp))
}

func (s *Server) handleEdit(w http.ResponseWriter, r *http.Request) {
	fid := r.PathValue("id")
	mrID, err := s.findingMRID(r.Context(), fid)
	if err != nil {
		s.fail(w, err)
		return
	}
	body := strings.TrimSpace(r.FormValue("body"))
	if body == "" {
		// Never blank a finding — an empty body would post an empty note to GitLab.
		s.finishFindingAction(w, r, mrID, fmt.Errorf("comment cannot be empty"))
		return
	}
	s.finishFindingAction(w, r, mrID, s.svc.Finding.Edit(r.Context(), fid, body))
}

// handleApproveAll approves every still-proposed finding in a review (optionally
// filtered to one severity). Full-page action — not an htmx swap.
func (s *Server) handleApproveAll(w http.ResponseWriter, r *http.Request) {
	reviewID := r.PathValue("id")
	sev := r.FormValue("severity")
	findings, err := s.svc.DB.ListFindingsByReview(r.Context(), reviewID)
	if err != nil {
		s.fail(w, err)
		return
	}
	n := 0
	for _, f := range findings {
		if f.Status != state.FindingProposed {
			continue
		}
		if sev != "" && !strings.EqualFold(f.Severity, sev) {
			continue
		}
		if e := s.svc.Finding.Approve(r.Context(), f.ID); e != nil {
			err = e
		} else {
			n++
		}
	}
	if err != nil {
		setFlash(w, "err", err.Error())
	} else {
		setFlash(w, "ok", "Approved "+strconv.Itoa(n)+" finding(s).")
	}
	redirectBack(w, r)
}

func (s *Server) handleCreateDrafts(w http.ResponseWriter, r *http.Request) {
	reviewID := r.PathValue("id")
	n, err := s.svc.Publish.CreateDrafts(r.Context(), reviewID)
	if err != nil {
		setFlash(w, "err", "Create drafts failed: "+err.Error())
	} else {
		setFlash(w, "ok", "Created "+strconv.Itoa(n)+" GitLab draft note(s).")
	}
	redirectBack(w, r)
}

func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
	reviewID := r.PathValue("id")
	confirm := r.FormValue("confirm")
	n, err := s.svc.Publish.PublishDrafts(r.Context(), reviewID, confirm)
	if err != nil {
		setFlash(w, "err", err.Error())
	} else {
		setFlash(w, "ok", "Published "+strconv.Itoa(n)+" comment(s) to GitLab.")
	}
	redirectBack(w, r)
}

// ---- jobs ----

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	list, err := s.svc.DB.ListJobs(r.Context(), 100)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "jobs", jobsVM{baseVM: s.baseActive(w, r, "jobs"), Jobs: list})
}

func (s *Server) handleRetryJob(w http.ResponseWriter, r *http.Request) {
	ok, err := s.svc.DB.RequeueJob(r.Context(), r.PathValue("id"))
	switch {
	case err != nil:
		setFlash(w, "err", err.Error())
	case !ok:
		setFlash(w, "err", "Job is not in a retryable state.")
	default:
		setFlash(w, "ok", "Job requeued.")
	}
	http.Redirect(w, r, "/jobs", http.StatusSeeOther)
}

// ---- memory + settings ----

func (s *Server) handleMemory(w http.ResponseWriter, r *http.Request) {
	items, err := s.svc.DB.ListReviewMemory(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "memory", memoryVM{baseVM: s.baseActive(w, r, "memory"), Items: items})
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	s.render(w, "settings", settingsVM{baseVM: s.baseActive(w, r, "settings"), Cfg: s.cfg})
}

type healthVM struct{ Checks []HealthCheck }

// handleHealth runs the doctor checks and returns them as a fragment, loaded
// asynchronously by the Settings page so live checks (incl. a GitLab API call)
// never block the page.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	var checks []HealthCheck
	if s.health != nil {
		checks = s.health(r.Context())
	}
	s.renderPartial(w, "settings", "health-checks", healthVM{Checks: checks})
}

// ---- helpers ----

// finishFindingAction responds to a per-finding action: an htmx request gets the
// re-rendered review-section fragment (so counts stay in sync); a plain form
// submit is redirected back.
func (s *Server) finishFindingAction(w http.ResponseWriter, r *http.Request, mrID int64, actionErr error) {
	if !isHX(r) {
		// Full-page path: carry the error in a flash cookie, then redirect.
		if actionErr != nil {
			setFlash(w, "err", actionErr.Error())
		}
		redirectBack(w, r)
		return
	}
	// htmx path: render the error inline (ActionError) — do NOT set a flash
	// cookie, or it would re-appear on the next full page load.
	vm, err := s.buildMRVM(r.Context(), mrID, "")
	if err != nil {
		s.fail(w, err)
		return
	}
	vm.baseVM = baseVM{Host: s.host}
	if actionErr != nil {
		vm.ActionError = actionErr.Error()
	}
	s.renderPartial(w, "mr", "review-section", vm)
}

func (s *Server) findingMRID(ctx context.Context, fid string) (int64, error) {
	f, err := s.svc.DB.GetFinding(ctx, fid)
	if err != nil {
		return 0, err
	}
	return f.MRID, nil
}

func (s *Server) costLabel(cost float64) string {
	if cost <= 0 {
		return ""
	}
	if s.cfg.SubscriptionAuth {
		return fmt.Sprintf("≈$%.4f (covered by subscription)", cost)
	}
	return fmt.Sprintf("$%.4f", cost)
}

// groupBySeverity buckets findings by severity in a fixed order, with any
// unknown severities appended in first-seen order.
func groupBySeverity(findings []*state.Finding) []findingGroup {
	order := []string{"blocking", "critical", "high", "medium", "low", "nit"}
	buckets := map[string][]*state.Finding{}
	var firstSeen []string
	for _, f := range findings {
		sev := strings.ToLower(f.Severity)
		if _, ok := buckets[sev]; !ok {
			firstSeen = append(firstSeen, sev)
		}
		buckets[sev] = append(buckets[sev], f)
	}
	var groups []findingGroup
	emitted := map[string]bool{}
	for _, sev := range order {
		if items := buckets[sev]; len(items) > 0 {
			groups = append(groups, findingGroup{Severity: sev, Items: items})
			emitted[sev] = true
		}
	}
	for _, sev := range firstSeen {
		if !emitted[sev] {
			groups = append(groups, findingGroup{Severity: sev, Items: buckets[sev]})
			emitted[sev] = true
		}
	}
	return groups
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, state.ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	s.log.Error("handler error", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func pathID(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

func redirectBack(w http.ResponseWriter, r *http.Request) {
	dest := r.Referer()
	if dest == "" {
		dest = "/"
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

func setFlash(w http.ResponseWriter, kind, msg string) {
	http.SetCookie(w, &http.Cookie{
		Name: "flash", Value: kind + "|" + url.QueryEscape(msg), Path: "/", HttpOnly: true,
	})
}

func takeFlash(w http.ResponseWriter, r *http.Request) (kind, msg string) {
	c, err := r.Cookie("flash")
	if err != nil {
		return "", ""
	}
	http.SetCookie(w, &http.Cookie{Name: "flash", Value: "", Path: "/", MaxAge: -1})
	parts := strings.SplitN(c.Value, "|", 2)
	if len(parts) != 2 {
		return "", ""
	}
	m, _ := url.QueryUnescape(parts[1])
	return parts[0], m
}
