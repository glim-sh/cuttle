// Package mcp installs a CDP browser driver and generates the MCP client config
// that points it at cuttle's browser for a given context and profile seed.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/glim-sh/cuttle/internal/xdg"
)

// Driver describes a CDP driver that exposes an MCP server. Names and install
// commands mirror the CLI's driver briefing table; MCPCommand/MCPArgs carry the
// extra bit the briefing does not: how to run the driver as an MCP server. The
// CDP endpoint reaches the server either through EnvVar or, when the driver
// takes it as a launch flag, through CDPFlag.
type Driver struct {
	Name       string
	Install    string
	MCPCommand string
	MCPArgs    []string
	// EnvVar names the env var that carries the CDP endpoint (browser-use style).
	EnvVar string
	// CDPFlag, when set, prepends `<CDPFlag> <cdpURL>` to MCPArgs (agent-browser
	// style, whose global flags must precede the subcommand).
	CDPFlag string
}

// DefaultDriver is used when `cuttle mcp` is invoked without a driver argument.
// It matches the first-priority driver in the `cuttle up` briefing.
const DefaultDriver = "agent-browser"

const browserUse = "browser-use"

var drivers = map[string]Driver{
	DefaultDriver: {
		Name:       DefaultDriver,
		Install:    "npm install -g agent-browser",
		MCPCommand: "agent-browser",
		MCPArgs:    []string{"mcp"},
		CDPFlag:    "--cdp",
	},
	browserUse: {
		Name:       browserUse,
		Install:    "uv tool install browser-use",
		MCPCommand: browserUse,
		MCPArgs:    []string{"--mcp"},
		EnvVar:     "BU_CDP_URL",
	},
}

var (
	errUnknownDriver = errors.New("unknown MCP driver")
	errInstallFailed = errors.New("driver install failed")
)

// Lookup returns the driver metadata for name.
func Lookup(name string) (Driver, error) {
	d, ok := drivers[name]
	if !ok {
		return Driver{}, fmt.Errorf("%w %q (known: %s)", errUnknownDriver, name, strings.Join(driverNames(), ", "))
	}
	return d, nil
}

// driverNames returns the known driver names in alphabetical order.
func driverNames() []string {
	names := make([]string, 0, len(drivers))
	for n := range drivers {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

type serverConfig struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// Config is the MCP client config: a map of server name to its launch spec.
type Config struct {
	MCPServers map[string]serverConfig `json:"mcpServers"`
}

// BuildConfig produces the MCP config for a driver pointed at cdpURL (which
// already carries any ?fingerprint=<seed>).
func BuildConfig(d Driver, cdpURL string) Config {
	args := d.MCPArgs
	var env map[string]string
	switch {
	case d.CDPFlag != "":
		args = append([]string{d.CDPFlag, cdpURL}, args...)
	case d.EnvVar != "":
		env = map[string]string{d.EnvVar: cdpURL}
	}
	return Config{
		MCPServers: map[string]serverConfig{
			d.Name: {
				Command: d.MCPCommand,
				Args:    args,
				Env:     env,
			},
		},
	}
}

// Marshal renders the config as indented JSON.
func Marshal(c Config) ([]byte, error) {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding MCP config: %w", err)
	}
	return b, nil
}

// EnsureInstalled installs the driver via its install command when it is not
// already on PATH. It is a no-op when the driver is present.
func EnsureInstalled(ctx context.Context, d Driver) error {
	if _, err := exec.LookPath(d.MCPCommand); err == nil {
		return nil
	}
	fields := strings.Fields(d.Install)
	if len(fields) == 0 {
		return fmt.Errorf("%w: no install command for %s", errInstallFailed, d.Name)
	}
	cmd := exec.CommandContext(ctx, fields[0], fields[1:]...) //nolint:gosec // install command is a fixed table entry
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w (%s): %w", errInstallFailed, d.Install, err)
	}
	return nil
}

// DefaultConfigPath is where a driver's MCP config is written when no explicit
// path is given: $XDG_CONFIG_HOME/cuttle/mcp/<driver>.json.
func DefaultConfigPath(driver string) string {
	return filepath.Join(xdg.ConfigDir(), "cuttle", "mcp", driver+".json")
}

// WriteConfig writes the rendered config to path, creating parent dirs.
func WriteConfig(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating MCP config dir: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("writing MCP config: %w", err)
	}
	return nil
}
