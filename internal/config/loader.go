package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/sxwebdev/xconfig"
	"github.com/sxwebdev/xconfig/decoders/xconfigyaml"
	"github.com/sxwebdev/xconfig/plugins/loader"
)

// DefaultConfigPath returns ~/.ai-reviewer/config.yaml.
func DefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".ai-reviewer", "config.yaml")
	}
	return filepath.Join(home, ".ai-reviewer", "config.yaml")
}

// Load reads configuration from the given YAML path (empty = default path),
// overlaying it on DefaultConfig() and applying AI_REVIEWER_* env overrides.
//
// xconfig's own defaults/customdefaults plugins are skipped on purpose: our
// defaults live in DefaultConfig() to avoid the zero-value reset footgun. Flags
// are skipped because the CLI layer (urfave/cli) owns flag parsing.
func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultConfigPath()
	}

	cfg := DefaultConfig()

	l, err := loader.NewLoader(map[string]loader.Unmarshal{
		"yaml": xconfigyaml.New().Unmarshal,
		"yml":  xconfigyaml.New().Unmarshal,
	})
	if err != nil {
		return nil, fmt.Errorf("build config loader: %w", err)
	}
	// optional=true: a missing file is fine (fresh install runs on defaults).
	if err := l.AddFile(path, true); err != nil {
		return nil, fmt.Errorf("add config file %q: %w", path, err)
	}

	if _, err := xconfig.Load(cfg,
		xconfig.WithLoader(l),
		xconfig.WithEnvPrefix("AI_REVIEWER"),
		xconfig.WithSkipDefaults(),
		xconfig.WithSkipCustomDefaults(),
		xconfig.WithSkipFlags(),
	); err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	if err := cfg.ExpandPaths(); err != nil {
		return nil, fmt.Errorf("expand config paths: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return cfg, nil
}
