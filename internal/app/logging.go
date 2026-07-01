package app

import (
	"log/slog"
	"os"

	"github.com/sxwebdev/ai-reviewer/internal/security"
)

// NewLogger builds the application slog.Logger. Output is wrapped by the
// redaction handler so secrets never reach the logs. When debug is false the
// level is Info; debug raises it to Debug.
func NewLogger(debug bool) *slog.Logger {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	base := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	return slog.New(security.NewRedactingHandler(base))
}
