package state

// Row models mirror the SQLite schema. Timestamps are unix-milliseconds.
// Nullable integer columns use *int64 where NULL is semantically meaningful
// (e.g. a finding on an added line has old_line = NULL).

// Project is a tracked GitLab project.
type Project struct {
	ID                int64
	GitLabHost        string
	ProjectID         int64 // GitLab numeric id
	PathWithNamespace string
	DefaultBranch     string
	CloneURLHTTP      string
	WebURL            string
	LastSeenAt        int64
	LastIndexedSHA    string
}

// MergeRequest is a tracked MR.
type MergeRequest struct {
	ID             int64
	GitLabHost     string
	ProjectID      int64
	IID            int64
	WebURL         string
	Title          string
	Description    string
	AuthorUsername string
	SourceBranch   string
	TargetBranch   string
	State          string
	Draft          bool
	HeadSHA        string
	BaseSHA        string
	StartSHA       string
	UpdatedAt      int64
	CreatedAt      int64
	LastSeenAt     int64
	ReviewStatus   string
}

// MRVersion records a diff-refs triple for an MR version.
type MRVersion struct {
	ID        int64
	MRID      int64
	VersionID int64
	BaseSHA   string
	HeadSHA   string
	StartSHA  string
	CreatedAt int64
}

// MRDiff is a single changed file within an MR at a head sha.
type MRDiff struct {
	ID         int64
	MRID       int64
	HeadSHA    string
	OldPath    string
	NewPath    string
	Diff       string
	NewFile    bool
	Renamed    bool
	Deleted    bool
	IsBinary   bool
	IsVendored bool
}

// Review status values.
const (
	ReviewPending   = "pending"
	ReviewQueued    = "queued"
	ReviewAnalyzing = "analyzing"
	ReviewReady     = "ready"
	ReviewDrafted   = "drafted"
	ReviewPublished = "published"
	ReviewFailed    = "failed"
	ReviewCancelled = "cancelled"
)

// Review is one AI review run for an MR at a head sha.
type Review struct {
	ID                    string
	MRID                  int64
	ProjectID             int64
	MRIID                 int64
	HeadSHA               string
	BaseSHA               string
	StartSHA              string
	Mode                  string
	Status                string
	RiskLevel             string
	OverallRecommendation string
	LLMProvider           string
	LLMModel              string
	ReviewerProfileID     string
	Summary               string
	RawReportJSON         string
	PipelineJSON          string // serialized []review.PassReport (per-pass cost/duration)
	RiskJSON              string // serialized review.RiskReport (deterministic risk)
	CompletenessJSON      string // serialized review.CompletenessReport
	CoverageJSON          string // serialized coverage.Report (changed-line coverage)
	SuppressedJSON        string // serialized []review.SuppressedFinding (dropped-but-surfaced)
	CostUSD               float64
	DurationMS            int64  // wall-clock time the review run took, in ms
	UserContext           string // free-form reviewer-supplied context for this run
	SkillsJSON            string // serialized []string of Claude skills used
	CreatedAt             int64
	UpdatedAt             int64
}

// Finding status values.
const (
	FindingProposed  = "proposed"
	FindingApproved  = "approved"
	FindingRejected  = "rejected"
	FindingDrafted   = "drafted"
	FindingPublished = "published"
	FindingFailed    = "failed"
)

// Finding is a single review comment candidate.
type Finding struct {
	ID                 string
	ReviewID           string
	MRID               int64
	HeadSHA            string
	Severity           string
	Category           string
	FilePath           string
	OldPath            string
	NewPath            string
	OldLine            *int64
	NewLine            *int64
	LineKind           string
	LineRangeStart     *int64
	LineRangeEnd       *int64
	Title              string
	Body               string
	Suggestion         string
	Confidence         float64
	EvidenceJSON       string
	Fingerprint        string
	Status             string
	RejectionReason    string
	GitLabPositionJSON string
	GitLabDraftNoteID  *int64
	GitLabDiscussionID string
	ValidationError    string
	Pass               string // pipeline pass that produced the finding
	Verification       string // skeptic outcome: confirmed|uncertain|unverified|""
	CreatedAt          int64
	UpdatedAt          int64
	EditedAt           int64 // when the reviewer last edited the body (0 = never)
}

// Job type + status values.
const (
	JobSync    = "sync"
	JobIndex   = "index"
	JobReview  = "review"
	JobDraft   = "draft"
	JobPublish = "publish"

	JobQueued    = "queued"
	JobRunning   = "running"
	JobSuccess   = "success"
	JobFailed    = "failed"
	JobCancelled = "cancelled"
)

// Job is a durable background work item.
type Job struct {
	ID              string
	Type            string
	Status          string
	PayloadJSON     string
	ProjectID       *int64
	MRIID           *int64
	ReviewID        string
	Priority        int
	Attempts        int
	MaxAttempts     int
	RunAfter        int64
	LockedAt        *int64
	LockedBy        string
	Error           string
	ProgressCurrent int
	ProgressTotal   int
	CreatedAt       int64
	StartedAt       *int64
	FinishedAt      *int64
	UpdatedAt       int64
}

// RepoFile is an indexed source file at a head sha.
type RepoFile struct {
	ID          int64
	ProjectID   int64
	HeadSHA     string
	Path        string
	Language    string
	PackageName string
	SizeBytes   int64
	SHA256      string
	IsGenerated bool
	IsVendor    bool
	IsTest      bool
	IndexedAt   int64
}

// Review memory scope, type, and source values.
const (
	ScopeGlobal  = "global"
	ScopeHost    = "host"
	ScopeProject = "project"

	MemRepoRule       = "repo_rule"
	MemReviewPattern  = "review_pattern"
	MemFalsePositive  = "false_positive"
	MemPreferredStyle = "preferred_style"
	MemArchNote       = "architecture_note"
	MemTestPolicy     = "test_policy"
	MemSecurityPolicy = "security_policy"

	SourceUser     = "user"
	SourceLearned  = "learned"
	SourceImported = "imported"
)

// ReviewMemory is a persistent per-repo/global rule or pattern.
type ReviewMemory struct {
	ID         string
	Scope      string
	GitLabHost string
	ProjectID  *int64
	Type       string
	Title      string
	Body       string
	TagsJSON   string
	Priority   int
	Enabled    bool
	Source     string
	CreatedAt  int64
	UpdatedAt  int64
}

// ReviewerProfile stores a named review profile as JSON.
type ReviewerProfile struct {
	ID        string
	Name      string
	DataJSON  string
	CreatedAt int64
	UpdatedAt int64
}
