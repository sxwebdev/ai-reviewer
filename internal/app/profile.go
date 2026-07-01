package app

import (
	"github.com/sxwebdev/ai-reviewer/internal/config"
	"github.com/sxwebdev/ai-reviewer/internal/review"
)

// profileFromConfig builds the reviewer profile from config, overriding the
// built-in defaults with the review.* settings the user can control: comment
// language, max comments, and severity threshold. Category enablement is left
// to the profile default for now — config's include_* flags don't map 1:1 to
// the profile's category list (they omit correctness/architecture), so wiring
// them naively would drop important categories.
func profileFromConfig(rc config.ReviewConfig) *review.Profile {
	p := review.DefaultProfile()
	if rc.PreferredCommentLanguage != "" {
		p.Language = rc.PreferredCommentLanguage
	}
	if rc.MaxComments > 0 {
		p.MaxComments = rc.MaxComments
	}
	if rc.SeverityThreshold != "" {
		p.SeverityThreshold = rc.SeverityThreshold
	}
	return p
}
