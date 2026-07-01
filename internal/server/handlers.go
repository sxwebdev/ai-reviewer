package server

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sxwebdev/ai-reviewer/internal/jobs"
	"github.com/sxwebdev/ai-reviewer/internal/service"
	"github.com/sxwebdev/ai-reviewer/internal/state"
)

type baseVM struct {
	Host      string
	Flash     string
	FlashKind string
}

type dashboardVM struct {
	baseVM
	MRs []state.DashboardRow
}

type mrVM struct {
	baseVM
	MR            *state.MergeRequest
	ProjectPath   string
	Running       bool
	Review        *state.Review
	Findings      []*state.Finding
	ApprovedCount int
	DraftedCount  int
	PublishPhrase string
	PastReviews   []pastReviewVM
}

type pastReviewVM struct {
	When      string
	HeadSHA   string
	RiskLevel string
	Status    string
	Findings  int
}

func (s *Server) base(w http.ResponseWriter, r *http.Request) baseVM {
	kind, msg := takeFlash(w, r)
	return baseVM{Host: s.host, Flash: msg, FlashKind: kind}
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	rows, err := s.svc.DB.DashboardRows(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "dashboard", dashboardVM{baseVM: s.base(w, r), MRs: rows})
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

func (s *Server) handleMR(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	mr, err := s.svc.DB.GetMergeRequest(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	vm := mrVM{baseVM: s.base(w, r), MR: mr}
	if p, err := s.svc.DB.GetProjectByGitLabID(ctx, mr.GitLabHost, mr.ProjectID); err == nil {
		vm.ProjectPath = p.PathWithNamespace
	}
	if active, _ := s.svc.DB.HasActiveJob(ctx, state.JobReview, mr.ProjectID, mr.IID); active {
		vm.Running = true
	}

	reviews, _ := s.svc.DB.ListReviewsByMR(ctx, id)
	if len(reviews) > 0 {
		vm.Review = reviews[0]
		vm.Findings, _ = s.svc.DB.ListFindingsByReview(ctx, reviews[0].ID)
		for _, f := range vm.Findings {
			switch f.Status {
			case state.FindingApproved:
				vm.ApprovedCount++
			case state.FindingDrafted:
				vm.DraftedCount++
			}
		}
		vm.PublishPhrase = service.ConfirmPhrase(vm.DraftedCount)
		for _, rv := range reviews[1:] {
			fs, _ := s.svc.DB.ListFindingsByReview(ctx, rv.ID)
			vm.PastReviews = append(vm.PastReviews, pastReviewVM{
				When:    time.UnixMilli(rv.CreatedAt).Format("2006-01-02 15:04"),
				HeadSHA: rv.HeadSHA, RiskLevel: rv.RiskLevel, Status: rv.Status, Findings: len(fs),
			})
		}
	}
	s.render(w, "mr", vm)
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
		setFlash(w, "ok", "Review queued. Refresh in a moment to see findings.")
	}
	http.Redirect(w, r, "/mr/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	fid := r.PathValue("id")
	if err := s.svc.Finding.Approve(r.Context(), fid); err != nil {
		setFlash(w, "err", err.Error())
	}
	redirectBack(w, r)
}

func (s *Server) handleReject(w http.ResponseWriter, r *http.Request) {
	fid := r.PathValue("id")
	reason := r.FormValue("reason")
	fp := r.FormValue("fp") != ""
	if err := s.svc.Finding.Reject(r.Context(), fid, reason, fp); err != nil {
		setFlash(w, "err", err.Error())
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

// ---- helpers ----

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
