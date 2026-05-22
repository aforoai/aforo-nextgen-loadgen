// Package config holds the loadgen runtime configuration.
//
// Session 1 ships only the type and a no-op loader so the CLI's --config flag
// has somewhere to land. Real schema, validation, and YAML loading arrive in
// Session 2 alongside the scenarios catalog.
package config

// Config is the top-level loadgen configuration.
//
// Fields will be populated as later sessions land:
//   - Session 2: scenarios + tenants
//   - Session 3: target endpoints, auth, rate shaping
//   - Session 6: payment/tax/ERP fixtures
type Config struct {
	// Target is the base URL of the Aforo platform under test
	// (e.g. https://usage-ingestor.aforo.ai). Overridable via --target.
	Target string `yaml:"target"`
}

// Load reads a config file from the given path. Session 1 returns an empty
// Config regardless of input — see package doc.
func Load(_ string) (*Config, error) {
	return &Config{}, nil
}
