// Package config provides a strongly-typed, validated configuration loader
// for the GhostProxy application. It decouples YAML deserialization from
// application logic, mapping all external configuration state into safe
// Go primitives with explicit default fallbacks.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// OperationalMode defines the proxy pipeline lifecycle state.
// Each mode activates a distinct request-handling strategy within the
// reverse proxy engine.
type OperationalMode string

const (
	// ModeRecord captures downstream HTTP responses and persists
	// structured snapshots to local storage for later replay.
	ModeRecord OperationalMode = "record"

	// ModeReplay short-circuits live network requests by serving
	// previously recorded snapshots from local storage.
	ModeReplay OperationalMode = "replay"

	// ModeChaos evaluates per-route chaos injection rules to simulate
	// downstream failures, latency spikes, and service outages.
	ModeChaos OperationalMode = "chaos"
)

// ServerConfig holds the HTTP listener binding parameters.
type ServerConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

// UpstreamConfig defines the downstream target service that GhostProxy
// will forward traffic to during record and passthrough operations.
type UpstreamConfig struct {
	Target         string `yaml:"target"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
}

// Timeout returns the upstream timeout as a time.Duration, derived from
// the integer seconds value in the configuration.
func (u *UpstreamConfig) Timeout() time.Duration {
	if u.TimeoutSeconds <= 0 {
		return 30 * time.Second
	}
	return time.Duration(u.TimeoutSeconds) * time.Second
}

// ChaosSpec defines the chaos engineering parameters for a specific route.
// When enabled, the proxy will inject artificial latency and/or return
// simulated error responses instead of forwarding to the upstream target.
type ChaosSpec struct {
	Enabled      bool   `yaml:"enabled"`
	LatencyMs    int    `yaml:"latency_ms"`
	ErrorCode    int    `yaml:"error_code"`
	ErrorMessage string `yaml:"error_message"`
}

// RouteRule binds a URL path pattern to a set of allowed HTTP methods
// and optional chaos injection metadata. Routes are evaluated in
// declaration order during request processing.
type RouteRule struct {
	Path    string    `yaml:"path"`
	Methods []string  `yaml:"methods"`
	Chaos   ChaosSpec `yaml:"chaos"`
}

// MatchesRequest returns true if the given HTTP method and request path
// match this route rule. Path matching uses prefix comparison to support
// both exact and hierarchical route trees.
func (r *RouteRule) MatchesRequest(method string, path string) bool {
	if !strings.HasPrefix(path, r.Path) {
		return false
	}

	if len(r.Methods) == 0 {
		return true
	}

	upperMethod := strings.ToUpper(method)
	for _, m := range r.Methods {
		if strings.ToUpper(m) == upperMethod {
			return true
		}
	}
	return false
}

// StorageConfig defines where GhostProxy persists recorded response
// snapshots on the local filesystem.
type StorageConfig struct {
	Directory string `yaml:"directory"`
}

// AppConfig is the top-level configuration structure that aggregates all
// subsystem configurations. It is the single source of truth for the
// entire GhostProxy application runtime.
type AppConfig struct {
	Server   ServerConfig    `yaml:"server"`
	Mode     OperationalMode `yaml:"mode"`
	Upstream UpstreamConfig  `yaml:"upstream"`
	Routes   []RouteRule     `yaml:"routes"`
	Storage  StorageConfig   `yaml:"storage"`
}

// ListenAddr returns the fully qualified host:port address string
// for binding the HTTP server listener.
func (c *AppConfig) ListenAddr() string {
	return fmt.Sprintf("%s:%d", c.Server.Host, c.Server.Port)
}

// FindRoute searches the route table for the first rule matching the
// given HTTP method and path. Returns nil if no matching route exists.
func (c *AppConfig) FindRoute(method string, path string) *RouteRule {
	for i := range c.Routes {
		if c.Routes[i].MatchesRequest(method, path) {
			return &c.Routes[i]
		}
	}
	return nil
}

// LoadConfig reads and parses a YAML configuration file from the given
// filesystem path. It applies sensible defaults before unmarshaling and
// validates all critical fields before returning. Returns a fully
// hydrated AppConfig or a descriptive error.
func LoadConfig(path string) (*AppConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: failed to read file %q: %w", path, err)
	}

	cfg := &AppConfig{
		Server: ServerConfig{
			Host: "0.0.0.0",
			Port: 8080,
		},
		Mode: ModeRecord,
		Upstream: UpstreamConfig{
			TimeoutSeconds: 30,
		},
		Storage: StorageConfig{
			Directory: "./mappings",
		},
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("config: failed to parse YAML from %q: %w", path, err)
	}

	if err := validate(cfg); err != nil {
		return nil, fmt.Errorf("config: validation failed: %w", err)
	}

	return cfg, nil
}

// validate performs comprehensive integrity checks on the parsed
// configuration to catch misconfigurations before server startup.
func validate(cfg *AppConfig) error {
	if cfg.Server.Port < 1 || cfg.Server.Port > 65535 {
		return fmt.Errorf("server.port must be between 1 and 65535, got %d", cfg.Server.Port)
	}

	if strings.TrimSpace(cfg.Server.Host) == "" {
		return fmt.Errorf("server.host must not be empty")
	}

	if strings.TrimSpace(cfg.Upstream.Target) == "" {
		return fmt.Errorf("upstream.target must specify a valid URL")
	}

	switch cfg.Mode {
	case ModeRecord, ModeReplay, ModeChaos:
		// valid operational mode
	default:
		return fmt.Errorf("mode must be one of [record, replay, chaos], got %q", cfg.Mode)
	}

	if strings.TrimSpace(cfg.Storage.Directory) == "" {
		return fmt.Errorf("storage.directory must not be empty")
	}

	for i, route := range cfg.Routes {
		if strings.TrimSpace(route.Path) == "" {
			return fmt.Errorf("routes[%d].path must not be empty", i)
		}
		if route.Chaos.Enabled && route.Chaos.ErrorCode != 0 {
			if route.Chaos.ErrorCode < 100 || route.Chaos.ErrorCode > 599 {
				return fmt.Errorf("routes[%d].chaos.error_code must be a valid HTTP status (100-599), got %d",
					i, route.Chaos.ErrorCode)
			}
		}
		if route.Chaos.LatencyMs < 0 {
			return fmt.Errorf("routes[%d].chaos.latency_ms must not be negative, got %d",
				i, route.Chaos.LatencyMs)
		}
	}

	return nil
}
