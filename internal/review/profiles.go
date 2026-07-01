package review

// Profile controls reviewer behaviour and comment style. It is persisted as
// JSON in reviewer_profiles and injected into prompts.
type Profile struct {
	Name                   string   `json:"name"`
	Language               string   `json:"language"` // ru|en|auto
	Tone                   string   `json:"tone"`     // direct|neutral|friendly
	Strictness             string   `json:"strictness"`
	MaxComments            int      `json:"max_comments"`
	SeverityThreshold      string   `json:"severity_threshold"`
	CategoriesEnabled      []string `json:"categories_enabled"`
	AllowNits              bool     `json:"allow_nits"`
	PreferQuestions        bool     `json:"prefer_questions"`
	IncludeAIMarker        bool     `json:"include_ai_marker"`
	TestStrictness         string   `json:"test_strictness"`
	SecurityStrictness     string   `json:"security_strictness"`
	ArchitectureStrictness string   `json:"architecture_strictness"`
}

// DefaultProfile is a careful senior engineer: direct but polite, high signal,
// prioritizing correctness/security/tests/architecture, asking when uncertain,
// avoiding cosmetics. It never auto-approves or auto-merges (enforced in code).
func DefaultProfile() *Profile {
	return &Profile{
		Name:              "default",
		Language:          "auto",
		Tone:              "direct",
		Strictness:        "high",
		MaxComments:       12,
		SeverityThreshold: "medium",
		CategoriesEnabled: []string{
			"correctness", "security", "tests", "performance",
			"architecture", "maintainability", "observability", "question",
		},
		AllowNits:              false,
		PreferQuestions:        true,
		IncludeAIMarker:        false,
		TestStrictness:         "high",
		SecurityStrictness:     "high",
		ArchitectureStrictness: "medium",
	}
}
