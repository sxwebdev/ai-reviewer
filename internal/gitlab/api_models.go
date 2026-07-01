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
	UpdatedAt    string   `json:"updated_at"`
	CreatedAt    string   `json:"created_at"`
}

// IsDraft reports whether the MR is a draft/WIP (field name varies by version).
func (m MergeRequest) IsDraft() bool { return m.Draft || m.WorkInProg }

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

// Note is a single note within a discussion.
type Note struct {
	ID       int64     `json:"id"`
	Type     string    `json:"type"`
	Body     string    `json:"body"`
	Author   User      `json:"author"`
	System   bool      `json:"system"`
	Resolved bool      `json:"resolved"`
	Position *Position `json:"position"`
}

// Discussion is a thread of notes.
type Discussion struct {
	ID    string `json:"id"`
	Notes []Note `json:"notes"`
}

// DraftNote is a pending (unpublished) review note.
type DraftNote struct {
	ID       int64     `json:"id"`
	Note     string    `json:"note"`
	Position *Position `json:"position"`
}

// Pipeline is a CI pipeline summary.
type Pipeline struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
	SHA    string `json:"sha"`
	WebURL string `json:"web_url"`
}
