// Package fingerprint builds the stealth Chrome argument vector and resolves
// proxy geo/exit-IP metadata. It is a faithful port of the vendored cloakbrowser
// arg-building subset; its output must stay byte-identical to that oracle
// (parity tests enforce this) because a silent drift is a silent stealth loss.
package fingerprint

import (
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"runtime"
	"strings"
)

// systemName and seedSource are overridable so parity tests can pin the platform
// and fingerprint seed the Python oracle used. Defaults mirror the runtime.
var (
	systemName = defaultSystemName
	seedSource = defaultSeed
)

func defaultSystemName() string {
	switch runtime.GOOS {
	case "darwin":
		return "Darwin"
	case "windows":
		return "Windows"
	default:
		return "Linux"
	}
}

func defaultSeed() int {
	// A fingerprint seed, not a security token; math/rand mirrors the oracle.
	return rand.IntN(90000) + 10000 //nolint:gosec // non-cryptographic seed
}

func getDefaultStealthArgs() []string {
	base := []string{"--no-sandbox", fmt.Sprintf("--fingerprint=%d", seedSource())}
	if systemName() == "Darwin" {
		return append(base, "--fingerprint-platform=macos")
	}
	return append(base, "--fingerprint-platform=windows")
}

// BuildArgsInput holds the parameters of the vendored build_args function.
type BuildArgsInput struct {
	StealthArgs    bool
	ExtraArgs      []string
	Timezone       string
	Locale         string
	Headless       bool
	ExtensionPaths []string
	StartMaximized bool
}

// BuildArgs combines stealth defaults, user args, and locale/timezone flags,
// deduplicating by flag key (everything before '='). Priority: stealth defaults
// < user args < dedicated params. Insertion order is preserved, and updating an
// existing key keeps its original position.
func BuildArgs(in BuildArgsInput) []string {
	seen := newOrderedArgs()

	if in.StealthArgs {
		for _, arg := range getDefaultStealthArgs() {
			seen.set(argKey(arg), arg)
		}
	}

	if !in.Headless || systemName() == "Windows" {
		seen.set("--ignore-gpu-blocklist", "--ignore-gpu-blocklist")
	}

	for _, arg := range in.ExtraArgs {
		seen.set(argKey(arg), arg)
	}

	if in.Timezone != "" {
		seen.set("--fingerprint-timezone", "--fingerprint-timezone="+in.Timezone)
	}
	if in.Locale != "" {
		for _, key := range []string{"--lang", "--fingerprint-locale"} {
			seen.set(key, key+"="+in.Locale)
		}
	}

	if len(in.ExtensionPaths) > 0 {
		absPaths := make([]string, len(in.ExtensionPaths))
		for i, p := range in.ExtensionPaths {
			ap, err := filepath.Abs(p)
			if err != nil {
				ap = p
			}
			absPaths[i] = ap
		}
		extVal := strings.Join(absPaths, ",")
		seen.set("--load-extension", "--load-extension="+extVal)
		seen.set("--disable-extensions-except", "--disable-extensions-except="+extVal)
	}

	if in.StartMaximized && !seen.has("--start-maximized") &&
		!seen.has("--window-size") && !seen.has("--window-position") {
		seen.set("--start-maximized", "--start-maximized")
	}

	return seen.values()
}

func argKey(arg string) string {
	key, _, _ := strings.Cut(arg, "=")
	return key
}

// orderedArgs is an insertion-ordered string map that mirrors CPython dict
// semantics: a repeated key updates its value in place without moving.
type orderedArgs struct {
	keys []string
	vals map[string]string
}

func newOrderedArgs() *orderedArgs {
	return &orderedArgs{vals: make(map[string]string)}
}

func (o *orderedArgs) set(key, val string) {
	if _, ok := o.vals[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = val
}

func (o *orderedArgs) has(key string) bool {
	_, ok := o.vals[key]
	return ok
}

func (o *orderedArgs) values() []string {
	out := make([]string, len(o.keys))
	for i, k := range o.keys {
		out[i] = o.vals[k]
	}
	return out
}
