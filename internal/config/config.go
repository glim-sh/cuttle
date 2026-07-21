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
	"reflect"
	"slices"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/glim-sh/cuttle/internal/xdg"
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

// Config is the parsed config file. Missing keys leave zero values. omitempty on
// the optional fields keeps a Save-written config free of empty-key noise.
type Config struct {
	DefaultContext string             `toml:"default_context,omitempty"`
	Contexts       map[string]Context `toml:"context,omitempty"`
	Profiles       map[string]Profile `toml:"profile,omitempty"`
}

// Context describes where and how a browser runs. Which fields are meaningful
// depends on Backend; unused fields stay zero.
type Context struct {
	Backend string `toml:"backend"`

	// k8s
	Namespace    string            `toml:"namespace,omitempty"`
	Release      string            `toml:"release,omitempty"`
	KubeContext  string            `toml:"kube_context,omitempty"`
	NodeSelector map[string]string `toml:"node_selector,omitempty"`
	Tolerations  []Toleration      `toml:"tolerations,omitempty"`
	Resources    *Resources        `toml:"resources,omitempty"`

	// ssh
	Host string `toml:"host,omitempty"`

	// direct
	CDPURL string `toml:"cdp_url,omitempty"`
	VNCURL string `toml:"vnc_url,omitempty"`

	// applied at browser startup regardless of backend
	Proxy string `toml:"proxy,omitempty"`
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
	return filepath.Join(xdg.ConfigDir(), "cuttle", "config.toml")
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

// Save writes the config to path, creating the parent directory if needed. It
// round-trips only Config's own known schema: any key the current cuttle build
// does not model is dropped, so `context add` never has to preserve foreign keys
// (KISS). The built-in "local" context injected by Load is omitted so it is not
// persisted as an explicit stanza.
func (c *Config) Save(path string) error {
	out := *c
	if out.Contexts != nil {
		filtered := make(map[string]Context, len(out.Contexts))
		builtinLocal := Context{Backend: BackendLocal}
		for name, ctx := range out.Contexts {
			if name == BackendLocal && reflect.DeepEqual(ctx, builtinLocal) {
				continue
			}
			filtered[name] = ctx
		}
		out.Contexts = filtered
	}
	data, err := toml.Marshal(out)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("writing config %s: %w", path, err)
	}
	return nil
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
