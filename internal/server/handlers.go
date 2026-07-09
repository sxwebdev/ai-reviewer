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
	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/jobs"
	"github.com/sxwebdev/ai-reviewer/internal/review"
	"github.com/sxwebdev/ai-reviewer/internal/service"
	"github.com/sxwebdev/ai-reviewer/internal/skills"
	"github.com/sxwebdev/ai-reviewer/internal/state"
)

type baseVM struct {
	UI        UIConfig // header switches (pipeline mode / model) + display bits incl. Host
	Flash     string
	FlashKind string
	Active    string // nav highlight: dashboard|jobs|memory|settings
	PageClass string // extra class on <main> (e.g. "page--wide" for the MR workspace)
	Paused    bool   // global queue pause is active
}

type dashboardVM struct {
	baseVM
	MRs        []dashItem
	ShowClosed bool // true when the view includes merged/closed MRs (?show=all)
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

// Open reports whether the MR is still open (vs merged/closed). Merged/closed
// MRs are hidden from the dashboard by default; the "Show merged/closed" filter
// reveals them.
func (d dashItem) Open() bool { return gitlab.IsOpenState(d.State) }

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
		return "new commits"
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
	case "new commits":
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

// DurationLabel formats the pass duration.
func (p passReportVM) DurationLabel() string { return durationLabel(p.DurationMS) }

// durationLabel renders a millisecond duration as "8.4s" (sub-minute) or
// "1m 12s" (a minute or more), or "" when non-positive/unknown.
func durationLabel(ms int64) string {
	if ms <= 0 {
		return ""
	}
	sec := float64(ms) / 1000
	if sec < 60 {
		return fmt.Sprintf("%.1fs", sec)
	}
	return fmt.Sprintf("%dm %ds", int(sec)/60, int(sec)%60)
}

type mrVM struct {
	baseVM
	MR              *state.MergeRequest
	ProjectPath     string
	Running         bool
	JobID           string // latest review job id (for the Stop action while active)
	Progress        string // "3/7" when a running review job reports progress
	JobStatus       string // latest review job status
	JobError        string // latest review job's error, if it failed
	Review          *state.Review
	Historical      bool // viewing a past review (not the latest) → actions hidden
	Groups          []findingGroup
	FindingCount    int
	ProposedCount   int
	ApprovedCount   int
	DraftedCount    int
	PublishPhrase   string
	CostLabel       string
	PassReports     []passReportVM
	Risk            *review.RiskReport
	Completeness    *review.CompletenessReport
	Coverage        *coverage.Report
	Suppressed      []suppressedVM // findings the pipeline dropped, shown read-only
	ActionError     string
	PastReviews     []pastReviewVM
	Diff            diffVM
	AvailableSkills []skillOption    // skills offered in the run form
	AgentMode       bool             // agentic mode on → skills are usable
	UsedSkills      []string         // skills the shown review actually ran with
	NewCommits      []newCommitVM    // commits pushed after the shown review's head (capped for display)
	NewCommitsN     int              // full count of new commits (may exceed len(NewCommits))
	HeadAdvanced    bool             // head moved but the commit list couldn't be enumerated (fetch error)
	HistoryRewrote  bool             // reviewed head SHA absent from the commit list (force-push)
	findings        []*state.Finding // flat findings of the selected review, for the diff pane
}

// newCommitVM is one commit pushed after the reviewed head, shown in the
// "new commits since this review" banner on the MR detail page.
type newCommitVM struct {
	ShortSHA string
	Title    string
}

// suppressedVM is one finding the pipeline dropped, surfaced read-only in the
// "also considered" section. It embeds the engine type and adds display helpers.
type suppressedVM struct {
	review.SuppressedFinding
}

// StageLabel is the short human label for the drop stage badge.
func (s suppressedVM) StageLabel() string {
	switch s.Stage {
	case review.SuppressThreshold:
		return "below threshold"
	case review.SuppressDuplicate:
		return "already raised"
	case review.SuppressSkeptic:
		return "skeptic disputed"
	case review.SuppressVerifier:
		return "refuted by build/vet"
	default:
		return s.Stage
	}
}

// HeadChanged reports that the MR's current head has advanced past the head the
// shown review ran against — i.e. there are new commits to re-review. False when
// there is no review yet or the head still matches.
func (v mrVM) HeadChanged() bool {
	return v.Review != nil && v.MR != nil && v.MR.HeadSHA != "" &&
		v.Review.HeadSHA != v.MR.HeadSHA
}

// skillOption is one selectable skill in the run-review form.
type skillOption struct {
	Name        string
	Description string
}

// DurationLabel formats the shown review's wall-clock duration (e.g. "1m 12s",
// "8.4s"), or "" when unknown (older reviews predate duration tracking).
func (v mrVM) DurationLabel() string {
	if v.Review == nil {
		return ""
	}
	return durationLabel(v.Review.DurationMS)
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
	Settings SettingsView
}

// applyResultVM feeds the settings-apply-result fragment (htmx banner).
type applyResultVM struct {
	OK      bool
	Kind    string // ok|warn|err
	Message string
}

func (s *Server) base(w http.ResponseWriter, r *http.Request) baseVM {
	kind, msg := takeFlash(w, r)
	var paused bool
	// The settings/setup pages render before a service bundle exists (setup gate),
	// so query the pause flag only when the DB is wired.
	if s.deps.Bundle != nil {
		if b := s.deps.Bundle(); b != nil && b.DB != nil {
			paused, _ = b.DB.JobsPaused(r.Context())
		}
	}
	return baseVM{UI: s.ui(), Flash: msg, FlashKind: kind, Paused: paused}
}

func (s *Server) baseActive(w http.ResponseWriter, r *http.Request, active string) baseVM {
	b := s.base(w, r)
	b.Active = active
	return b
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	showClosed := r.URL.Query().Get("show") == "all"
	rows, err := s.svc().DB.DashboardRows(ctx, showClosed)
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
	s.render(w, "dashboard", dashboardVM{baseVM: s.baseActive(w, r, "dashboard"), MRs: items, ShowClosed: showClosed})
}

type mrKey struct{ projectID, iid int64 }

// reviewJobState returns, per MR, whether its latest review job is active
// (queued/running) and whether that latest job failed.
func (s *Server) reviewJobState(ctx context.Context) (running, failed map[mrKey]bool) {
	running, failed = map[mrKey]bool{}, map[mrKey]bool{}
	list, err := s.svc().DB.LatestReviewJobsPerMR(ctx) // one latest review job per MR
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
	res, err := s.svc().Sync.SyncAssignedMRs(r.Context())
	if err != nil {
		setFlash(w, "err", "Sync failed: "+err.Error())
	} else {
		msg := "Synced " + strconv.Itoa(res.Tracked) + " open merge request(s)."
		if res.Closed > 0 {
			msg += " " + strconv.Itoa(res.Closed) + " merged/closed and hidden."
		}
		setFlash(w, "ok", msg)
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// buildMRVM assembles the MR detail view model (everything except baseVM, which
// the caller fills). It is shared by the full page, the htmx review-section
// fragment, and finding-action responses.
func (s *Server) buildMRVM(ctx context.Context, id int64, selectedReviewID string) (mrVM, error) {
	mr, err := s.svc().DB.GetMergeRequest(ctx, id)
	if err != nil {
		return mrVM{}, err
	}
	vm := mrVM{MR: mr}
	if p, err := s.svc().DB.GetProjectByGitLabID(ctx, mr.GitLabHost, mr.ProjectID); err == nil {
		vm.ProjectPath = p.PathWithNamespace
	}
	if active, _ := s.svc().DB.HasActiveJob(ctx, state.JobReview, mr.ProjectID, mr.IID); active {
		vm.Running = true
	}
	// Latest review job for this MR → status / error / progress. Targeted query,
	// correct regardless of overall job volume.
	if j, err := s.svc().DB.GetLatestReviewJob(ctx, mr.ProjectID, mr.IID); err == nil {
		vm.JobStatus = j.Status
		if j.Status == state.JobQueued || j.Status == state.JobRunning {
			vm.JobID = j.ID
		}
		if j.Status == state.JobFailed {
			vm.JobError = j.Error
		}
		if j.Status == state.JobRunning && j.ProgressTotal > 0 {
			vm.Progress = fmt.Sprintf("%d/%d", j.ProgressCurrent, j.ProgressTotal)
		}
	}
	reviews, _ := s.svc().DB.ListReviewsByMR(ctx, id)
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
		findings, _ := s.svc().DB.ListFindingsByReview(ctx, sel.ID)
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
		if sel.SkillsJSON != "" {
			_ = json.Unmarshal([]byte(sel.SkillsJSON), &vm.UsedSkills)
		}
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
		if sel.SuppressedJSON != "" {
			var sup []review.SuppressedFinding
			if err := json.Unmarshal([]byte(sel.SuppressedJSON), &sup); err == nil {
				for _, sf := range sup {
					vm.Suppressed = append(vm.Suppressed, suppressedVM{sf})
				}
			}
		}
		for _, rv := range reviews {
			if rv.ID == sel.ID {
				continue // the one being shown is not listed under history
			}
			fs, _ := s.svc().DB.ListFindingsByReview(ctx, rv.ID)
			vm.PastReviews = append(vm.PastReviews, pastReviewVM{
				ID:      rv.ID,
				When:    time.UnixMilli(rv.CreatedAt).Format("2006-01-02 15:04"),
				HeadSHA: rv.HeadSHA, RiskLevel: rv.RiskLevel, Status: rv.Status, Findings: len(fs),
			})
		}
	}
	// New commits since the shown review's head: only when the head actually moved
	// and we're on the live (non-historical, non-running) view. Best-effort — a
	// GitLab error just leaves the banner without a commit list. The current head
	// comes from the last sync, so the set is "as of last sync", matching the list.
	if vm.HeadChanged() && !vm.Historical && !vm.Running {
		projectKey := strconv.FormatInt(mr.ProjectID, 10)
		commits, total, found, err := s.svc().Review.NewCommitsSince(ctx, projectKey, mr.IID, vm.Review.HeadSHA)
		switch {
		case err != nil:
			vm.HeadAdvanced = true // couldn't enumerate → generic "head advanced" banner
		case !found:
			vm.HistoryRewrote = true
		default:
			vm.NewCommitsN = total
			for _, c := range commits {
				vm.NewCommits = append(vm.NewCommits, newCommitVM{ShortSHA: c.ShortID, Title: c.Title})
			}
		}
	}
	return vm, nil
}

// buildDiffVM assembles the diff pane for an MR at headSHA, overlaying findings
// onto the exact hunk line each was mapped to at review time. Findings that do
// not resolve to a line land in Unanchored. Display-only: it reads persisted
// positions and never computes new ones (positions are owned by the engine).
func (s *Server) buildDiffVM(ctx context.Context, mrID int64, headSHA string, findings []*state.Finding) diffVM {
	rows, err := s.svc().DB.ListMRDiffFiles(ctx, mrID, headSHA)
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
	// Gate skills on the EFFECTIVE agent mode (both review.agent_mode and
	// llm.claude.agent_mode) — the header toggle only flips the former, and
	// skills silently do nothing unless both are on.
	vm.AgentMode = vm.UI.AgentModeEffective
	vm.AvailableSkills = discoverSkillOptions()
	// Diff pane: use the selected review's head so line numbers match its
	// findings; fall back to the MR head for a not-yet-reviewed MR.
	headSHA := vm.MR.HeadSHA
	if vm.Review != nil {
		headSHA = vm.Review.HeadSHA
	}
	vm.Diff = s.buildDiffVM(r.Context(), vm.MR.ID, headSHA, vm.findings)
	s.render(w, "mr", vm)
}

// discoverSkillOptions lists user-level Claude skills (~/.claude/skills) for the
// run-review form. Only user skills are offered: project skills live in the
// reviewed MR's worktree (created per review), not in the server's working
// directory, so they cannot be resolved reliably at form-render time. Best-effort.
func discoverSkillOptions() []skillOption {
	found := skills.Discover([]skills.Source{{Label: "user", Dir: skills.UserDir()}})
	out := make([]skillOption, 0, len(found))
	for _, sk := range found {
		out = append(out, skillOption{Name: sk.Name, Description: sk.Description})
	}
	return out
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
	vm.baseVM = baseVM{} // fragment: header not re-rendered, base fields unused
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
	mr, err := s.svc().DB.GetMergeRequest(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	_ = r.ParseForm() // populate r.Form so multi-valued "skills" is readable
	var skills []string
	for _, sk := range r.Form["skills"] {
		if sk = strings.TrimSpace(sk); sk != "" {
			skills = append(skills, sk)
		}
	}
	userContext := strings.TrimSpace(r.FormValue("context"))
	jobID, err := jobs.EnqueueReview(r.Context(), s.svc().DB, mr.ID, mr.ProjectID, mr.IID, jobs.ReviewRequest{
		UserContext:     userContext,
		Skills:          skills,
		RememberContext: r.FormValue("remember") == "on",
	})
	hadInput := userContext != "" || len(skills) > 0
	switch {
	case err != nil:
		setFlash(w, "err", "Could not queue review: "+err.Error())
	case jobID == "" && hadInput:
		// The already-running job carries its own (possibly empty) payload; the
		// context/skills just submitted are not applied. Say so rather than
		// implying they took effect.
		setFlash(w, "warn", "A review is already running for this MR — your context and skills were not applied. Wait for it to finish, then run again.")
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
	s.finishFindingAction(w, r, mrID, s.svc().Finding.Approve(r.Context(), fid))
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
	s.finishFindingAction(w, r, mrID, s.svc().Finding.Reject(r.Context(), fid, reason, fp))
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
	s.finishFindingAction(w, r, mrID, s.svc().Finding.Edit(r.Context(), fid, body))
}

// handleApproveAll approves every still-proposed finding in a review (optionally
// filtered to one severity). Full-page action — not an htmx swap.
func (s *Server) handleApproveAll(w http.ResponseWriter, r *http.Request) {
	reviewID := r.PathValue("id")
	sev := r.FormValue("severity")
	findings, err := s.svc().DB.ListFindingsByReview(r.Context(), reviewID)
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
		if e := s.svc().Finding.Approve(r.Context(), f.ID); e != nil {
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
	n, err := s.svc().Publish.CreateDrafts(r.Context(), reviewID)
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
	n, err := s.svc().Publish.PublishDrafts(r.Context(), reviewID, confirm)
	if err != nil {
		setFlash(w, "err", err.Error())
	} else {
		setFlash(w, "ok", "Published "+strconv.Itoa(n)+" comment(s) to GitLab.")
	}
	redirectBack(w, r)
}

// ---- jobs ----

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	list, err := s.svc().DB.ListJobs(r.Context(), 100)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "jobs", jobsVM{baseVM: s.baseActive(w, r, "jobs"), Jobs: list})
}

func (s *Server) handleRetryJob(w http.ResponseWriter, r *http.Request) {
	ok, err := s.svc().DB.RequeueJob(r.Context(), r.PathValue("id"))
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

func (s *Server) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	outcome, err := s.svc().DB.RequestCancelJob(r.Context(), r.PathValue("id"))
	switch {
	case err != nil:
		setFlash(w, "err", err.Error())
	case outcome == state.CancelDone:
		setFlash(w, "ok", "Job cancelled.")
	case outcome == state.CancelPending:
		setFlash(w, "ok", "Stopping review…")
	default:
		setFlash(w, "err", "Job is not queued or running.")
	}
	redirectBack(w, r)
}

func (s *Server) handlePauseJobs(w http.ResponseWriter, r *http.Request) {
	if err := s.svc().DB.SetJobsPaused(r.Context(), true); err != nil {
		setFlash(w, "err", err.Error())
	} else {
		setFlash(w, "ok", "Reviews paused. Running reviews are being returned to the queue.")
	}
	redirectBack(w, r)
}

func (s *Server) handleResumeJobs(w http.ResponseWriter, r *http.Request) {
	if err := s.svc().DB.SetJobsPaused(r.Context(), false); err != nil {
		setFlash(w, "err", err.Error())
	} else {
		setFlash(w, "ok", "Reviews resumed.")
	}
	redirectBack(w, r)
}

// ---- memory + settings ----

func (s *Server) handleMemory(w http.ResponseWriter, r *http.Request) {
	items, err := s.svc().DB.ListReviewMemory(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "memory", memoryVM{baseVM: s.baseActive(w, r, "memory"), Items: items})
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	var sv SettingsView
	if s.deps.SettingsView != nil {
		sv = s.deps.SettingsView()
	}
	s.render(w, "settings", settingsVM{baseVM: s.baseActive(w, r, "settings"), Settings: sv})
}

// ---- setup gate + header switches ----

// setupVM feeds the standalone setup page. The token is never echoed back:
// on a validation error the token field renders empty.
type setupVM struct {
	Status    SetupStatus
	Error     string // inline validation error
	Flash     string // flash carried across the redirect back to /setup
	FlashKind string
	Host      string // sticky form values on error
	Username  string
}

func (s *Server) setupStatus() SetupStatus {
	if s.deps.SetupStatus == nil {
		return SetupStatus{}
	}
	return s.deps.SetupStatus()
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	st := s.setupStatus()
	kind, msg := takeFlash(w, r)
	s.renderSetup(w, setupVM{Status: st, Flash: msg, FlashKind: kind, Host: st.Host, Username: st.Username})
}

// handleSetupCheck re-runs the environment checks (htmx "Re-check" button).
func (s *Server) handleSetupCheck(w http.ResponseWriter, r *http.Request) {
	s.renderPartial(w, "setup", "setup-claude-check", setupVM{Status: s.setupStatus()})
}

func (s *Server) handleSetupSubmit(w http.ResponseWriter, r *http.Request) {
	st := s.setupStatus()
	host := strings.TrimSpace(r.FormValue("host"))
	username := strings.TrimSpace(r.FormValue("username"))
	token := strings.TrimSpace(r.FormValue("token"))

	failWith := func(msg string) {
		s.renderSetup(w, setupVM{Status: st, Error: msg, Host: host, Username: username})
	}
	if host == "" {
		failWith("GitLab host is required.")
		return
	}
	if token == "" && !st.TokenFromEnv {
		failWith("GitLab token is required (or export it via " + st.TokenEnvName + " and restart).")
		return
	}
	apiUser, err := s.deps.ValidateGitLab(r.Context(), host, token)
	if err != nil {
		failWith("GitLab validation failed: " + err.Error())
		return
	}
	if username == "" {
		username = apiUser
	}
	// An empty token means "keep using the env-provided one" — it is validated
	// above but never written to disk.
	if err := s.deps.ApplySetup(r.Context(), host, username, token); err != nil {
		failWith("Could not save configuration: " + err.Error())
		return
	}
	// GitLab is saved, but the gate may still be closed (claude CLI missing).
	// Redirect back to /setup explicitly with a message — a silent bounce off
	// the gate would look like the Save button did nothing.
	if s.deps.NeedsSetup != nil && s.deps.NeedsSetup() {
		setFlash(w, "warn", "GitLab settings saved. Install the claude CLI and press Re-check to continue.")
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	if username != apiUser {
		setFlash(w, "warn", "Setup complete, but the token belongs to "+apiUser+" while the configured username is "+username+".")
	} else {
		setFlash(w, "ok", "Setup complete. Authenticated as "+apiUser+".")
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleApplySettings persists config changes and hot-applies them. It serves
// two callers: the header switches (plain form POST, named pipeline_mode /
// llm_model / agent_mode) and the Settings form (htmx POST, fields namespaced
// "cfg:<dotted.key>"). Unknown keys are rejected by the app layer, which also
// validates every value before writing the file.
func (s *Server) handleApplySettings(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	values := map[string]string{}
	// Settings-page fields carry a "cfg:" prefix to distinguish config keys from
	// buttons/other form state without the server needing the schema.
	for name, vals := range r.Form {
		if k, ok := strings.CutPrefix(name, "cfg:"); ok && len(vals) > 0 {
			values[k] = vals[0]
		}
	}
	// Legacy header-switch field names → their config keys.
	for form, key := range map[string]string{
		"pipeline_mode": "review.pipeline.mode",
		"llm_model":     "llm.model",
		"agent_mode":    "review.agent_mode",
	} {
		if v := strings.TrimSpace(r.Form.Get(form)); v != "" {
			values[key] = v
		}
	}

	hx := isHX(r)
	if len(values) == 0 {
		if hx {
			s.renderApplyResult(w, applyResultVM{Kind: "warn", Message: "Nothing to apply."})
			return
		}
		setFlash(w, "err", "Nothing to apply.")
		redirectBack(w, r)
		return
	}

	var (
		res ApplyResult
		err error
	)
	if s.deps.ApplyConfig != nil {
		res, err = s.deps.ApplyConfig(r.Context(), values)
	} else {
		err = errors.New("configuration editing is unavailable")
	}

	if hx {
		switch {
		case err != nil:
			s.renderApplyResult(w, applyResultVM{Kind: "err", Message: "Could not save: " + err.Error()})
		case res.Warning != "":
			s.renderApplyResult(w, applyResultVM{OK: true, Kind: "warn", Message: res.Warning})
		case res.RestartRequired:
			s.renderApplyResult(w, applyResultVM{OK: true, Kind: "warn", Message: "Saved. Restart ai-reviewer for these changes to take effect."})
		default:
			s.renderApplyResult(w, applyResultVM{OK: true, Kind: "ok", Message: "Saved."})
		}
		return
	}

	// Non-htmx path (header switches): flash + redirect back.
	switch {
	case err != nil:
		setFlash(w, "err", "Could not apply settings: "+err.Error())
	case res.Warning != "":
		setFlash(w, "warn", res.Warning)
	default:
		setFlash(w, "ok", "Settings applied.")
	}
	redirectBack(w, r)
}

// renderApplyResult writes the settings-apply-result fragment (htmx banner).
func (s *Server) renderApplyResult(w http.ResponseWriter, vm applyResultVM) {
	s.renderPartial(w, "settings", "settings-apply-result", vm)
}

type healthVM struct{ Checks []HealthCheck }

// handleHealth runs the doctor checks and returns them as a fragment, loaded
// asynchronously by the Settings page so live checks (incl. a GitLab API call)
// never block the page.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	var checks []HealthCheck
	if s.deps.Health != nil {
		checks = s.deps.Health(r.Context())
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
	vm.baseVM = baseVM{} // fragment: header not re-rendered, base fields unused
	if actionErr != nil {
		vm.ActionError = actionErr.Error()
	}
	s.renderPartial(w, "mr", "review-section", vm)
}

func (s *Server) findingMRID(ctx context.Context, fid string) (int64, error) {
	f, err := s.svc().DB.GetFinding(ctx, fid)
	if err != nil {
		return 0, err
	}
	return f.MRID, nil
}

func (s *Server) costLabel(cost float64) string {
	if cost <= 0 {
		return ""
	}
	if s.ui().SubscriptionAuth {
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
