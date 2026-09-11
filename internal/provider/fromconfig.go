package provider

import (
	"fmt"
	"net/http"
	"os"

	"github.com/ShriramJana/crossbar/internal/config"
)

// NewFromConfig builds the adapter described by cfg. name is the key the
// provider is registered under in config and becomes its reported Name.
// Secrets are read from the environment variable cfg names; a missing
// variable is an error at startup rather than a 401 at first request.
func NewFromConfig(name string, cfg config.ProviderConfig) (Provider, error) {
	switch cfg.Type {
	case "mock":
		return NewMock(name), nil
	case "anthropic":
		key := os.Getenv(cfg.APIKeyEnv)
		if key == "" {
			return nil, fmt.Errorf("provider %q: environment variable %s is not set", name, cfg.APIKeyEnv)
		}
		return NewAnthropic(AnthropicConfig{
			Name:    name,
			BaseURL: cfg.BaseURL,
			APIKey:  key,
			Client:  &http.Client{Timeout: cfg.Timeout},
		}), nil
	default:
		return nil, fmt.Errorf("provider %q: no adapter for provider type %q", name, cfg.Type)
	}
}
