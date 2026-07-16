// Package config loads the cuttle TOML config and resolves the active context.
//
// The config is read-mostly: a single file at $XDG_CONFIG_HOME/cuttle/config.toml
// declares named contexts (where the browser runs) and named profiles (= seeds).
// A missing file yields a built-in "local" context, so cuttle needs zero config
// to drive a local docker browser.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	toml "github.com/pelletier/go-toml/v2"
)

// Backend names selectable per context.
const (
	BackendLocal  = "local"
	BackendK8s    = "k8s"
	BackendSSH    = "ssh"
	BackendDirect = "direct"
)

// Storage modes for a profile.
const (
	StorageLocal  = "local"
	StorageRemote = "remote"
)

// EnvContext is the environment variable that selects the active context.
const EnvContext = "CUTTLE_CONTEXT"

// Config is the parsed config file. Missing keys leave zero values.
type Config struct {
	DefaultContext string             `toml:"default_context"`
	Contexts       map[string]Context `toml:"context"`
	Profiles       map[string]Profile `toml:"profile"`
}

// Context describes where and how a browser runs. Which fields are meaningful
// depends on Backend; unused fields stay zero.
type Context struct {
	Backend string `toml:"backend"`

	// k8s
	Namespace    string            `toml:"namespace"`
	Release      string            `toml:"release"`
	KubeContext  string            `toml:"kube_context"`
	NodeSelector map[string]string `toml:"node_selector"`
	Tolerations  []Toleration      `toml:"tolerations"`
	Resources    *Resources        `toml:"resources"`

	// ssh
	Host string `toml:"host"`

	// direct
	CDPURL string `toml:"cdp_url"`
	VNCURL string `toml:"vnc_url"`

	// applied at browser startup regardless of backend (see plan 8.4)
	Proxy string `toml:"proxy"`
}

// Toleration mirrors a Kubernetes toleration passed through to the Helm chart.
type Toleration struct {
	Key      string `toml:"key"`
	Operator string `toml:"operator"`
	Value    string `toml:"value"`
	Effect   string `toml:"effect"`
}

// Resources mirrors a Kubernetes resource requests/limits block.
type Resources struct {
	Requests map[string]string `toml:"requests"`
	Limits   map[string]string `toml:"limits"`
}

// Profile is a named cuttle seed with a storage policy.
type Profile struct {
	Storage string `toml:"storage"` // "local" (default) | "remote"
}

var errUnknownContext = errors.New("unknown context")

// Load reads the config from the default XDG path. A missing file is not an
// error: it returns a Config carrying only the built-in local context.
func Load() (*Config, error) {
	return LoadFrom(DefaultPath())
}

// DefaultPath is $XDG_CONFIG_HOME/cuttle/config.toml, falling back to
// ~/.config/cuttle/config.toml.
func DefaultPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, ".config")
		}
	}
	return filepath.Join(dir, "cuttle", "config.toml")
}

// LoadFrom reads a specific config file. A missing file yields the built-in
// local context. Unknown keys are rejected (strict mode) so a typo surfaces
// instead of silently doing nothing.
func LoadFrom(path string) (*Config, error) {
	cfg := &Config{}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// zero config; fall through to built-in local injection
	case err != nil:
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	default:
		dec := toml.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(cfg); err != nil {
			return nil, fmt.Errorf("parsing config %s: %w", path, err)
		}
	}

	if cfg.Contexts == nil {
		cfg.Contexts = map[string]Context{}
	}
	if _, ok := cfg.Contexts[BackendLocal]; !ok {
		cfg.Contexts[BackendLocal] = Context{Backend: BackendLocal}
	}
	return cfg, nil
}

// Active resolves the active context by name. Precedence: flag > env >
// default_context > built-in "local". A named context that does not exist is an
// error (typo protection).
func (c *Config) Active(flag, env string) (string, Context, error) {
	name := BackendLocal
	switch {
	case flag != "":
		name = flag
	case env != "":
		name = env
	case c.DefaultContext != "":
		name = c.DefaultContext
	}
	ctx, ok := c.Contexts[name]
	if !ok {
		return "", Context{}, fmt.Errorf("%w %q (check %s)", errUnknownContext, name, DefaultPath())
	}
	if ctx.Backend == "" {
		ctx.Backend = BackendLocal
	}
	return name, ctx, nil
}

// Names returns the context names in a stable order (local first, then the rest
// sorted) for listing.
func (c *Config) Names() []string {
	names := make([]string, 0, len(c.Contexts))
	for name := range c.Contexts {
		if name != BackendLocal {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return append([]string{BackendLocal}, names...)
}
