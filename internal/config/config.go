// Package config loads, validates, and hot-reloads the gateway configuration.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the fully parsed and validated gateway configuration. Instances
// are immutable once returned from Parse or Load; a reload produces a new one.
type Config struct {
	Admin     AdminConfig               `yaml:"admin"`
	Providers map[string]ProviderConfig `yaml:"providers"`
	Tiers     map[string][]Target       `yaml:"tiers"`
	Pricing   []Price                   `yaml:"pricing"`
	Breaker   BreakerConfig             `yaml:"breaker"`
	Teams     []Team                    `yaml:"teams"`

	// Derived indexes, built during validation.
	byKey   map[string]*Team
	byID    map[string]*Team
	byPrice map[Target]Price
}

// AdminConfig guards the control-plane endpoints.
type AdminConfig struct {
	// KeySHA256 is the hex SHA-256 of the admin bearer key. Empty disables admin endpoints.
	KeySHA256 string `yaml:"key_sha256"`
}

// ProviderConfig describes one upstream backend.
type ProviderConfig struct {
	// Type selects the adapter: anthropic, openai, ollama, or mock.
	Type string `yaml:"type"`
	// BaseURL overrides the adapter's default endpoint.
	BaseURL string `yaml:"base_url"`
	// APIKeyEnv names the environment variable holding the upstream key.
	// Secrets never appear in the file itself.
	APIKeyEnv string `yaml:"api_key_env"`
	// Timeout bounds a single upstream call. Zero means the adapter default.
	Timeout time.Duration `yaml:"timeout"`
}

// Target is one (provider, model) pair in a tier's fallback chain.
type Target struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
}

// Price is the USD cost per million tokens for one (provider, model).
type Price struct {
	Provider         string  `yaml:"provider"`
	Model            string  `yaml:"model"`
	InputPerMillion  float64 `yaml:"input_per_million"`
	OutputPerMillion float64 `yaml:"output_per_million"`
}

// Cost returns the USD cost of a call with the given actual token counts.
func (p Price) Cost(inputTokens, outputTokens int) float64 {
	return float64(inputTokens)*p.InputPerMillion/1e6 + float64(outputTokens)*p.OutputPerMillion/1e6
}

// BreakerConfig tunes every circuit breaker. Zero values take the defaults below.
type BreakerConfig struct {
	FailureThreshold float64       `yaml:"failure_threshold"`
	MinRequests      int           `yaml:"min_requests"`
	Window           time.Duration `yaml:"window"`
	Cooldown         time.Duration `yaml:"cooldown"`
	MaxCooldown      time.Duration `yaml:"max_cooldown"`
}

// Breaker defaults, per the product spec.
const (
	DefaultFailureThreshold = 0.5
	DefaultMinRequests      = 20
	DefaultWindow           = 30 * time.Second
	DefaultCooldown         = 15 * time.Second
	DefaultMaxCooldown      = 2 * time.Minute
)

// Team is a tenant of the gateway with its own keys, limits, and budgets.
type Team struct {
	ID   string `yaml:"id"`
	Name string `yaml:"name"`
	// KeysSHA256 holds hex SHA-256 digests of the team's bearer keys.
	KeysSHA256 []string `yaml:"keys_sha256"`
	// AllowedModels restricts which models the team may request. Empty allows all.
	AllowedModels     []string `yaml:"allowed_models"`
	RequestsPerMinute int      `yaml:"requests_per_minute"`
	TokensPerMinute   int      `yaml:"tokens_per_minute"`
	DailyBudgetUSD    float64  `yaml:"daily_budget_usd"`
	MonthlyBudgetUSD  float64  `yaml:"monthly_budget_usd"`
}

// AllowsModel reports whether the team may request model.
func (t *Team) AllowsModel(model string) bool {
	if len(t.AllowedModels) == 0 {
		return true
	}
	for _, m := range t.AllowedModels {
		if m == model {
			return true
		}
	}
	return false
}

// HashKey returns the lowercase hex SHA-256 of a bearer key, the form stored in config.
func HashKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// Load reads and validates the config file at path.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening config: %w", err)
	}
	defer func() { _ = f.Close() }()
	cfg, err := Parse(f)
	if err != nil {
		return nil, fmt.Errorf("loading %s: %w", path, err)
	}
	return cfg, nil
}

// Parse decodes YAML from r and validates it. Unknown fields are an error so
// a typo in a limit name cannot silently disable the limit.
func Parse(r io.Reader) (*Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}
	return &cfg, nil
}

// TeamByKey resolves a raw bearer key to its team.
func (c *Config) TeamByKey(raw string) (*Team, bool) {
	if raw == "" {
		return nil, false
	}
	t, ok := c.byKey[HashKey(raw)]
	return t, ok
}

// TeamByID looks a team up by its identifier.
func (c *Config) TeamByID(id string) (*Team, bool) {
	t, ok := c.byID[id]
	return t, ok
}

// IsAdminKey reports whether raw is the configured admin key. With no admin
// key configured, nothing is admin.
func (c *Config) IsAdminKey(raw string) bool {
	if c.Admin.KeySHA256 == "" || raw == "" {
		return false
	}
	return HashKey(raw) == c.Admin.KeySHA256
}

// Price looks up the pricing entry for a (provider, model).
func (c *Config) Price(provider, model string) (Price, bool) {
	p, ok := c.byPrice[Target{Provider: provider, Model: model}]
	return p, ok
}

func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

var providerTypes = map[string]bool{
	"anthropic": true,
	"openai":    true,
	"ollama":    true,
	"mock":      true,
}

// providerTypesNeedingKey lists adapter types that cannot work without a secret.
var providerTypesNeedingKey = map[string]bool{
	"anthropic": true,
	"openai":    true,
}

func (c *Config) validate() error {
	if c.Admin.KeySHA256 != "" && !isSHA256Hex(c.Admin.KeySHA256) {
		return errors.New("admin.key_sha256: not a sha256 hex digest")
	}

	for name, p := range c.Providers {
		if !providerTypes[p.Type] {
			return fmt.Errorf("provider %q: unknown type %q", name, p.Type)
		}
		if providerTypesNeedingKey[p.Type] && p.APIKeyEnv == "" {
			return fmt.Errorf("provider %q: api_key_env is required for type %s", name, p.Type)
		}
		if p.Timeout < 0 {
			return fmt.Errorf("provider %q: timeout must not be negative", name)
		}
	}

	for name, targets := range c.Tiers {
		if len(targets) == 0 {
			return fmt.Errorf("tier %q: must list at least one target", name)
		}
		for _, t := range targets {
			if _, ok := c.Providers[t.Provider]; !ok {
				return fmt.Errorf("tier %q: unknown provider %q", name, t.Provider)
			}
			if t.Model == "" {
				return fmt.Errorf("tier %q: target for provider %q has no model", name, t.Provider)
			}
		}
	}

	c.byPrice = make(map[Target]Price, len(c.Pricing))
	for _, p := range c.Pricing {
		if _, ok := c.Providers[p.Provider]; !ok {
			return fmt.Errorf("pricing: unknown provider %q", p.Provider)
		}
		if p.Model == "" {
			return fmt.Errorf("pricing: entry for provider %q has no model", p.Provider)
		}
		if p.InputPerMillion < 0 || p.OutputPerMillion < 0 {
			return fmt.Errorf("pricing %s/%s: prices must not be negative", p.Provider, p.Model)
		}
		key := Target{Provider: p.Provider, Model: p.Model}
		if _, dup := c.byPrice[key]; dup {
			return fmt.Errorf("pricing %s/%s: duplicate entry", p.Provider, p.Model)
		}
		c.byPrice[key] = p
	}

	if err := c.Breaker.applyDefaults(); err != nil {
		return err
	}

	c.byKey = make(map[string]*Team)
	c.byID = make(map[string]*Team, len(c.Teams))
	for i := range c.Teams {
		t := &c.Teams[i]
		if t.ID == "" {
			return fmt.Errorf("team #%d: id is required", i)
		}
		if _, dup := c.byID[t.ID]; dup {
			return fmt.Errorf("duplicate team id %q", t.ID)
		}
		if len(t.KeysSHA256) == 0 {
			return fmt.Errorf("team %q: at least one key is required", t.ID)
		}
		for _, k := range t.KeysSHA256 {
			if !isSHA256Hex(k) {
				return fmt.Errorf("team %q: key %q is not a sha256 hex digest", t.ID, k)
			}
			if other, dup := c.byKey[k]; dup {
				return fmt.Errorf("team %q: key already assigned to team %q", t.ID, other.ID)
			}
			c.byKey[k] = t
		}
		if t.RequestsPerMinute <= 0 {
			return fmt.Errorf("team %q: requests_per_minute must be positive", t.ID)
		}
		if t.TokensPerMinute <= 0 {
			return fmt.Errorf("team %q: tokens_per_minute must be positive", t.ID)
		}
		if t.DailyBudgetUSD < 0 {
			return fmt.Errorf("team %q: daily_budget_usd must not be negative", t.ID)
		}
		if t.MonthlyBudgetUSD < 0 {
			return fmt.Errorf("team %q: monthly_budget_usd must not be negative", t.ID)
		}
		c.byID[t.ID] = t
	}
	return nil
}

func (b *BreakerConfig) applyDefaults() error {
	if b.FailureThreshold == 0 {
		b.FailureThreshold = DefaultFailureThreshold
	}
	if b.FailureThreshold <= 0 || b.FailureThreshold > 1 {
		return errors.New("breaker: failure_threshold must be in (0, 1]")
	}
	if b.MinRequests == 0 {
		b.MinRequests = DefaultMinRequests
	}
	if b.MinRequests < 1 {
		return errors.New("breaker: min_requests must be at least 1")
	}
	if b.Window == 0 {
		b.Window = DefaultWindow
	}
	if b.Cooldown == 0 {
		b.Cooldown = DefaultCooldown
	}
	if b.MaxCooldown == 0 {
		b.MaxCooldown = DefaultMaxCooldown
	}
	if b.Window < 0 || b.Cooldown < 0 || b.MaxCooldown < 0 {
		return errors.New("breaker: durations must not be negative")
	}
	if b.MaxCooldown < b.Cooldown {
		return errors.New("breaker: max_cooldown must be at least cooldown")
	}
	return nil
}
