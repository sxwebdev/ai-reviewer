package gitlab

// API response DTOs. Field names follow the GitLab API v4 JSON. Timestamps are
// kept as RFC3339 strings and converted by callers.

// User is the authenticated user or an author.
type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Name     string `json:"name"`
	Email    string `json:"email"`
}

// Project is a GitLab project.
type Project struct {
	ID                int64  `json:"id"`
	PathWithNamespace string `json:"path_with_namespace"`
	DefaultBranch     string `json:"default_branch"`
	HTTPURLToRepo     string `json:"http_url_to_repo"`
	WebURL            string `json:"web_url"`
}

// MergeRequest is a merge request (list or detail).
//
// Mergeability and head_pipeline are only populated by the *detail* endpoint
// (GET /merge_requests/:iid); the list endpoint leaves them zero. Callers that
// classify conflicts or pipelines must work from a detail fetch.
type MergeRequest struct {
	ID           int64    `json:"id"`
	IID          int64    `json:"iid"`
	ProjectID    int64    `json:"project_id"`
	Title        string   `json:"title"`
	Description  string   `json:"description"`
	State        string   `json:"state"`
	Draft        bool     `json:"draft"`
	WorkInProg   bool     `json:"work_in_progress"`
	WebURL       string   `json:"web_url"`
	Author       User     `json:"author"`
	SourceBranch string   `json:"source_branch"`
	TargetBranch string   `json:"target_branch"`
	SHA          string   `json:"sha"`
	DiffRefs     DiffRefs `json:"diff_refs"`
	Reviewers    []User   `json:"reviewers"`
	Assignees    []User   `json:"assignees"`
	UpdatedAt    string   `json:"updated_at"`
	CreatedAt    string   `json:"created_at"`

	// HasConflicts is GitLab's own conflict verdict. It is only meaningful once
	// mergeability has been computed — see MergeStatus.
	HasConflicts bool `json:"has_conflicts"`
	// MergeStatus is the legacy mergeability field: "can_be_merged",
	// "cannot_be_merged", "cannot_be_merged_recheck", "unchecked", "checking".
	// The last three mean "not computed yet", i.e. conflicts are unknown.
	MergeStatus string `json:"merge_status"`
	// DetailedMergeStatus is the modern, finer-grained reason an MR cannot be
	// merged: "mergeable", "conflict", "broken_status", "checking",
	// "unchecked", "ci_must_pass", "discussions_not_resolved", "draft_status",
	// "not_approved", … Used to confirm HasConflicts.
	DetailedMergeStatus string `json:"detailed_merge_status"`
	// HeadPipeline is the pipeline of the MR's head commit. GitLab exposes it
	// only if the token's user may view pipelines for the project, so a nil
	// value means "unknown", never "no pipeline".
	HeadPipeline *Pipeline `json:"head_pipeline"`
}

// IsDraft reports whether the MR is a draft/WIP (field name varies by version).
func (m MergeRequest) IsDraft() bool { return m.Draft || m.WorkInProg }

// IsOpen reports whether the MR is still open (vs merged/closed).
func (m MergeRequest) IsOpen() bool { return IsOpenState(m.State) }

// IsOpenState reports whether a GitLab MR state counts as open — i.e. the MR
// still belongs on the default dashboard view and is a candidate for review.
// "opened" and "locked" (locked discussion, still open) are open; "merged" and
// "closed" are terminal. An empty string is treated as open: it means the state
// is unknown (e.g. a partial upsert that never carried one), and we prefer to
// keep such a row visible rather than silently hide it.
func IsOpenState(state string) bool {
	switch state {
	case "", "opened", "locked":
		return true
	default:
		return false
	}
}

// MergeRequestDiff is one changed file from the /diffs endpoint.
type MergeRequestDiff struct {
	OldPath       string `json:"old_path"`
	NewPath       string `json:"new_path"`
	AMode         string `json:"a_mode"`
	BMode         string `json:"b_mode"`
	NewFile       bool   `json:"new_file"`
	RenamedFile   bool   `json:"renamed_file"`
	DeletedFile   bool   `json:"deleted_file"`
	Diff          string `json:"diff"`
	GeneratedFile bool   `json:"generated_file"`
}

// MergeRequestVersion is one diff version (from /versions).
type MergeRequestVersion struct {
	ID             int64  `json:"id"`
	HeadCommitSHA  string `json:"head_commit_sha"`
	BaseCommitSHA  string `json:"base_commit_sha"`
	StartCommitSHA string `json:"start_commit_sha"`
	CreatedAt      string `json:"created_at"`
}

// Commit is one commit of an MR (from /commits).
type Commit struct {
	ID         string `json:"id"`
	ShortID    string `json:"short_id"`
	Title      string `json:"title"`
	Message    string `json:"message"`
	AuthorName string `json:"author_name"`
	CreatedAt  string `json:"created_at"`
}

// Note is a single note within a discussion.
type Note struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
	Body string `json:"body"`
	// Author of the note. Together with CreatedAt this drives the REST
	// fallback for "has this reviewer acted since the last push?".
	Author    User   `json:"author"`
	System    bool   `json:"system"`
	CreatedAt string `json:"created_at"`
	// Resolvable distinguishes a review thread (resolvable) from a plain
	// comment. Only resolvable threads count as "unresolved discussions".
	Resolvable bool      `json:"resolvable"`
	Resolved   bool      `json:"resolved"`
	Position   *Position `json:"position"`
}

// Discussion is a thread of notes.
type Discussion struct {
	ID string `json:"id"`
	// IndividualNote is true for a standalone comment that was never a thread;
	// such discussions can never be resolved and must not be counted as
	// unresolved work.
	IndividualNote bool   `json:"individual_note"`
	Notes          []Note `json:"notes"`
}

// Approvals is the response of GET /merge_requests/:iid/approvals. The endpoint
// is available on GitLab Free (unlike the approval *rules* API).
type Approvals struct {
	ID                int64        `json:"id"`
	IID               int64        `json:"iid"`
	ProjectID         int64        `json:"project_id"`
	Approved          bool         `json:"approved"`
	ApprovalsRequired int          `json:"approvals_required"`
	ApprovalsLeft     int          `json:"approvals_left"`
	ApprovedBy        []ApprovedBy `json:"approved_by"`
}

// ApprovedBy wraps one approver; GitLab nests the user one level deep.
type ApprovedBy struct {
	User User `json:"user"`
}

// Approvers flattens ApprovedBy into the users who approved.
func (a Approvals) Approvers() []User {
	if len(a.ApprovedBy) == 0 {
		return nil
	}
	out := make([]User, 0, len(a.ApprovedBy))
	for _, ab := range a.ApprovedBy {
		out = append(out, ab.User)
	}
	return out
}

// Pipeline is a CI pipeline summary, as returned by /pipelines and as embedded
// in an MR's head_pipeline.
//
// Status is the raw GitLab value: "created", "waiting_for_resource",
// "preparing", "pending", "running", "success", "failed", "canceled",
// "canceling", "skipped", "manual", "scheduled". Classifying which of these
// mean "the author must act" is the domain layer's job, not this package's.
type Pipeline struct {
	ID        int64  `json:"id"`
	Status    string `json:"status"`
	SHA       string `json:"sha"`
	Ref       string `json:"ref"`
	Source    string `json:"source"`
	WebURL    string `json:"web_url"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}
