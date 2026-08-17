package config

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/sxwebdev/ai-reviewer/internal/security"
	"github.com/sxwebdev/xconfig"
	"github.com/sxwebdev/xconfig/decoders/xconfigdotenv"
	"github.com/sxwebdev/xconfig/decoders/xconfigyaml"
	"github.com/sxwebdev/xconfig/plugins"
	"github.com/sxwebdev/xconfig/plugins/loader"
	"github.com/sxwebdev/xconfig/plugins/validate"
	"github.com/sxwebdev/xconfig/sourcers/xconfigvault"
	"github.com/tkcrm/mx/logger"
)

// DefaultConfigPath is the config file used when none is given.
const DefaultConfigPath = "config.yaml"

// VaultConfig holds the HashiCorp Vault bootstrap. It is loaded before the main
// config, from an optional .env file plus the process environment — the Vault
// address cannot itself live in Vault, and it must not be prefixed like the rest
// of the schema because deployments share these variable names across services.
// The conditionally-required fields carry no `validate:"required_if"` tags:
// they are checked in validate() below, for the reason spelled out in
// Config.Validate's doc comment. A tag failure here renders as "Key:
// 'VaultConfig.Token' Error:Field validation for 'Token' failed on the
// 'required_if' tag" — a Go field path rather than VAULT_TOKEN, and only the
// first problem. This struct fails earlier than anything else, before the main
// config is read at all, so its message is the first thing an operator setting
// Vault up ever sees.
type VaultConfig struct {
	Enabled       bool   `env:"VAULT_ENABLED"`
	Address       string `env:"VAULT_ADDR"`
	SecretPath    string `env:"VAULT_SECRET_PATH"`
	KubeRole      string `env:"VAULT_KUBE_ROLE"`
	KubeJWTPath   string `env:"VAULT_KUBE_JWT_PATH"`
	KubeMountPath string `env:"VAULT_KUBE_MOUNT_PATH" default:"kubernetes"`
	AuthKind      string `env:"VAULT_AUTH_KIND" default:"kubernetes" example:"kubernetes,token"`
	// Token is a Secret like every other credential in the schema: it is the
	// root of trust for all the others, and xconfigvault's own event callback
	// logs e.Error on every auth failure, which is exactly where a bare string
	// would surface.
	Token           Secret        `env:"VAULT_TOKEN" secret:"true"`
	RefreshInterval time.Duration `env:"VAULT_REFRESH_INTERVAL" default:"20s"`
}

// validate checks the Vault bootstrap in the same vocabulary as
// Config.Validate, collecting every problem rather than stopping at the first.
// The struct has no YAML keys, so the messages name the environment variables
// directly — unprefixed, because that is how they are actually spelled.
func (c VaultConfig) validate() error {
	if !c.Enabled {
		return nil
	}
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if strings.TrimSpace(c.Address) == "" {
		add("VAULT_ADDR is empty but VAULT_ENABLED is true")
	}
	if strings.TrimSpace(c.SecretPath) == "" {
		add("VAULT_SECRET_PATH is empty but VAULT_ENABLED is true")
	}

	switch c.AuthKind {
	case "token":
		if !c.Token.IsSet() {
			add("VAULT_TOKEN is empty but VAULT_AUTH_KIND=token requires it")
		}
	case "kubernetes":
		for _, f := range []struct{ name, value string }{
			{"VAULT_KUBE_ROLE", c.KubeRole},
			{"VAULT_KUBE_JWT_PATH", c.KubeJWTPath},
			{"VAULT_KUBE_MOUNT_PATH", c.KubeMountPath},
		} {
			if strings.TrimSpace(f.value) == "" {
				add("%s is empty but VAULT_AUTH_KIND=kubernetes requires it", f.name)
			}
		}
	default:
		// newVaultClient rejects this too, but only after everything else has
		// already been accepted.
		add("VAULT_AUTH_KIND=%q is not one of: kubernetes, token", c.AuthKind)
	}
	return joinConfigErrors(errs)
}

// LoadResult carries what the caller needs after a successful load.
type LoadResult struct {
	XConfig xconfig.Config
	// Cleanup releases resources acquired during the load (the Vault client and
	// its token renewer). Always non-nil; safe to call once on shutdown.
	Cleanup     func()
	Vault       VaultConfig
	VaultClient *xconfigvault.Client
}

// Load reads the configuration into conf. Sources, in increasing priority: the
// `default:` tags → YAML → env → Vault. A field explicitly present in the YAML
// keeps its value even when it equals the zero value, which is what stops a
// configured `false` from being refilled by a `true` default; see the package
// doc. Callers pass Default() so the mx sub-configs mentioned in defaultOps are
// already seeded.
//
// lg is used only for Vault lifecycle events; it is the bootstrap logger built
// in main from a minimal pre-CLI load, since the real logger config is part of
// what this function resolves.
func Load(ctx context.Context, lg logger.Logger, conf *Config, configPaths []string) (*LoadResult, error) {
	v := validator.New()
	validatePlugin := validate.New(func(a any) error { return v.Struct(a) })

	vaultCfg, err := loadVaultConfig(validatePlugin)
	if err != nil {
		return nil, err
	}
	// Before the Vault client is built, because its constructor is the first
	// thing that can fail with the token in the error.
	vaultCfg.RegisterSecrets()

	l, err := loader.NewLoader(map[string]loader.Unmarshal{
		"yaml": xconfigyaml.New().Unmarshal,
		"yml":  xconfigyaml.New().Unmarshal,
	})
	if err != nil {
		return nil, fmt.Errorf("create config loader: %w", err)
	}
	// optional=true: a deployment configured purely through env + Vault has no
	// file at all.
	if err := l.AddFiles(configPaths, true); err != nil {
		return nil, fmt.Errorf("add config files: %w", err)
	}

	result := &LoadResult{Cleanup: func() {}, Vault: vaultCfg}

	userPlugins := make([]plugins.Plugin, 0, 2)
	if vaultCfg.Enabled {
		client, err := newVaultClient(ctx, lg, vaultCfg)
		if err != nil {
			return nil, err
		}
		result.VaultClient = client
		result.Cleanup = func() {
			if err := client.Close(); err != nil {
				lg.Errorw("failed to close vault client", "error", err)
			}
		}
		// Registered before the validator so secrets are in place by the time
		// validation reports on them; xconfig runs plugins in order, and Vault
		// must have the last word over env and file.
		userPlugins = append(userPlugins, client.Plugin(ctx))
	}
	userPlugins = append(userPlugins, validatePlugin)

	xc, err := xconfig.Load(conf,
		xconfig.WithLoader(l),
		xconfig.WithEnvPrefix(EnvPrefix),
		xconfig.WithDisallowUnknownFields(),
		// urfave/cli owns os.Args; xconfig's flag plugin would fight it.
		xconfig.WithSkipFlags(),
		xconfig.WithPlugins(userPlugins...),
	)
	if err != nil {
		result.Cleanup()
		return nil, fmt.Errorf("load config: %w", err)
	}
	result.XConfig = xc

	// Every resolved secret goes to the redactor before anything can log it:
	// from here on it is masked in logs, in claude's subprocess output and in
	// error text.
	conf.RegisterSecrets()

	if err := conf.Validate(); err != nil {
		result.Cleanup()
		return nil, err
	}
	return result, nil
}

// loadVaultConfig reads the Vault bootstrap from an optional .env file plus the
// environment. It deliberately runs without an env prefix.
func loadVaultConfig(validatePlugin plugins.Plugin) (VaultConfig, error) {
	vaultLoader, err := loader.NewLoader(map[string]loader.Unmarshal{
		"env": xconfigdotenv.New().Unmarshal,
	})
	if err != nil {
		return VaultConfig{}, fmt.Errorf("create vault loader: %w", err)
	}
	if err := vaultLoader.AddFile(".env", true); err != nil {
		return VaultConfig{}, fmt.Errorf("add .env: %w", err)
	}

	var cfg VaultConfig
	if _, err := xconfig.Load(&cfg,
		xconfig.WithSkipFlags(),
		xconfig.WithLoader(vaultLoader),
		xconfig.WithPlugins(validatePlugin),
	); err != nil {
		return VaultConfig{}, fmt.Errorf("load vault config: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return VaultConfig{}, fmt.Errorf("vault bootstrap: %w", err)
	}
	return cfg, nil
}

func newVaultClient(ctx context.Context, lg logger.Logger, cfg VaultConfig) (*xconfigvault.Client, error) {
	var auth xconfigvault.AuthMethod
	switch cfg.AuthKind {
	case "kubernetes":
		auth = xconfigvault.WithKubernetesPath(cfg.KubeRole, cfg.KubeJWTPath, cfg.KubeMountPath)
	case "token":
		auth = xconfigvault.WithToken(cfg.Token.Unmask())
	default:
		return nil, fmt.Errorf("unsupported vault auth kind: %s", cfg.AuthKind)
	}

	client, err := xconfigvault.New(ctx, &xconfigvault.Config{
		Address:    cfg.Address,
		Auth:       auth,
		SecretPath: cfg.SecretPath,
		Metrics: xconfigvault.MetricsFunc(func(e xconfigvault.Event) {
			kv := []any{"type", string(e.Type)}
			if e.Message != "" {
				kv = append(kv, "message", e.Message)
			}
			if e.Error != nil {
				kv = append(kv, "error", e.Error, "attempt", e.Attempt)
			}
			lg.Infow("vault event", kv...)
		}),
	})
	if err != nil {
		return nil, fmt.Errorf("create vault client: %w", err)
	}
	return client, nil
}

// RegisterSecrets hands every resolved secret to the process-wide redactor. It
// is idempotent, so callers that reload configuration may call it again.
//
// The list must stay in step with the `Secret` fields of the schema: a field
// declared `secret:"true" vault:"true"` but missing here is masked in a config
// dump and sourced from Vault, yet printed in the clear the moment a library
// puts it in an error — which is precisely the gap Postgres.Username had.
//
// Note the redactor ignores literals shorter than six bytes, so a short
// username is simply not registered rather than corrupting unrelated text; a
// long one that happens to be an ordinary word will mask that word in logs,
// which is the accepted cost of the field being declared a secret at all.
func (c *Config) RegisterSecrets() {
	for _, s := range []Secret{
		c.GitLab.Token,
		c.Slack.Token,
		c.LLM.Claude.Auth.OAuthToken,
		c.LLM.Claude.Auth.APIKey,
		c.Postgres.Username,
		c.Postgres.Password,
	} {
		if s.IsSet() {
			security.RegisterSecret(s.Unmask())
		}
	}
}

// RegisterSecrets registers the Vault bootstrap's own credential. It is
// separate from Config.RegisterSecrets because the bootstrap is loaded first,
// from a different source set, and its token must be masked before the Vault
// client that uses it is even constructed.
func (c VaultConfig) RegisterSecrets() {
	if c.Token.IsSet() {
		security.RegisterSecret(c.Token.Unmask())
	}
}
