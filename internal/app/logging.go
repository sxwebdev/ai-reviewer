package app

import (
	"github.com/sxwebdev/ai-reviewer/internal/security"
	"github.com/sxwebdev/ai-reviewer/internal/version"
	"github.com/tkcrm/mx/logger"
	"go.uber.org/zap"
)

// AppName is the logger's `app` field and the CLI's command name.
const AppName = "ai-reviewer"

// NewLogger builds the application logger from the resolved log config.
//
// Every record passes through the redacting core, so a token that reaches a log
// message, a field or a nested group is masked on the way out. Wrapping the core
// (rather than filtering at the call sites) is what makes that unconditional:
// there is no way to log around it.
func NewLogger(cfg logger.Config, extra ...logger.Option) logger.ExtendedLogger {
	opts := []logger.Option{
		logger.WithAppName(AppName),
		logger.WithAppVersion(version.Version),
		logger.WithConfig(cfg),
		logger.WithZapOption(zap.WrapCore(security.NewRedactingCore)),
	}
	return logger.NewExtended(append(opts, extra...)...)
}
