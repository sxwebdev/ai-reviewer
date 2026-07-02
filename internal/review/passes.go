package review

import "log/slog"

// Builtin pass names.
const (
	PassGeneral     = "general"
	PassCorrectness = "correctness"
	PassConcurrency = "concurrency"
	PassSecurity    = "security"
	PassContracts   = "contracts"
)

// PassSpec describes one specialist review pass: a focused system-prompt
// suffix layered on top of the shared reviewer persona and MR context.
type PassSpec struct {
	Name         string
	SystemSuffix string // appended to BuildSystemPrompt(profile)
	Primary      bool   // its summary/risk/recommendation seeds the Result
}

// BuiltinPasses returns the registry of known passes.
func BuiltinPasses() map[string]PassSpec {
	return map[string]PassSpec{
		PassGeneral:     {Name: PassGeneral, Primary: true},
		PassCorrectness: {Name: PassCorrectness, SystemSuffix: correctnessSuffix},
		PassConcurrency: {Name: PassConcurrency, SystemSuffix: concurrencySuffix},
		PassSecurity:    {Name: PassSecurity, SystemSuffix: securitySuffix},
		PassContracts:   {Name: PassContracts, SystemSuffix: contractsSuffix},
	}
}

// ResolvePasses maps configured pass names to specs. Unknown names are skipped
// with a warning, never fatal. Exactly one pass ends up primary: "general"
// when present, otherwise the first resolved pass (so custom pass lists
// without "general" still produce a summary).
func ResolvePasses(names []string, log *slog.Logger) []PassSpec {
	builtin := BuiltinPasses()
	var specs []PassSpec
	seen := map[string]bool{}
	for _, n := range names {
		spec, ok := builtin[n]
		if !ok {
			log.Warn("unknown review pass skipped", "pass", n)
			continue
		}
		if seen[n] {
			continue
		}
		seen[n] = true
		specs = append(specs, spec)
	}
	if len(specs) == 0 {
		specs = []PassSpec{builtin[PassGeneral]}
	}
	hasPrimary := false
	for _, s := range specs {
		if s.Primary {
			hasPrimary = true
			break
		}
	}
	if !hasPrimary {
		specs[0].Primary = true
	}
	return specs
}
