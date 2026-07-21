// Package fingerprint builds the stealth Chrome argument vector and resolves
// proxy geo/exit-IP metadata. Its output is pinned by the golden snapshot in
// testdata and regression-tested, because a silent drift is a silent stealth
// loss.
package fingerprint

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// ReservedSeed is the sentinel seed that maps to the shared default Chrome
// instance; it is not a valid user-supplied seed.
const ReservedSeed = "__default__"

var seedRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// ValidSeed reports whether name is a legal fingerprint seed: 1-128 chars of
// [A-Za-z0-9_-] and not the reserved default sentinel. Seeds and profile names
// share this grammar, so both the serve multiplexer and local profiles call it.
func ValidSeed(name string) bool {
	return name != ReservedSeed && seedRE.MatchString(name)
}

// systemName and seedSource are overridable so parity tests can pin the platform
// and fingerprint seed the original used. Defaults mirror the runtime.
var (
	systemName = defaultSystemName
	seedSource = defaultSeed
)

func defaultSystemName() string {
	if runtime.GOOS == "windows" {
		return "Windows"
	}
	return "Linux"
}

func defaultSeed() int {
	// A fingerprint seed, not a security token; math/rand mirrors the original.
	return rand.IntN(90000) + 10000 //nolint:gosec // non-cryptographic seed
}

func getDefaultStealthArgs() []string {
	return []string{
		"--no-sandbox",
		fmt.Sprintf("--fingerprint=%d", seedSource()),
		"--fingerprint-platform=windows",
	}
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

// ForkParityArgs replicates clark/clearcote's own launcher flag set, which the
// vendored build_args (tuned for the Pro binary) omits but the fork binaries
// require: an explicit --user-agent matching navigator.userAgent, the ungoogled
// canvas/client-rects noise switches, UA-CH brand/platform coherence, a Windows
// font dir, the Accept-Language header, and a residential network profile.
// Returns nil unless a fork binary is selected via CUTTLE_BROWSER_BINARY.
//
// The persona is Windows: the container spoofs a Direct3D11 GPU pair, so a forced
// Windows UA + Windows font dir + platform=windows are all coherent.
func ForkParityArgs(locale, proxy string) []string {
	if os.Getenv(BinaryPathEnv) == "" {
		return nil
	}
	args := []string{
		"--fingerprint-platform=windows",
		"--fingerprint-platform-version=19.0.0",
		"--fingerprint-brand=Chrome",
		"--fingerprint-brand-version=148.0.0.0",
		"--user-agent=Mozilla/5.0 (Windows NT 10.0; Win64; x64) " +
			"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36",
		"--fingerprint-fonts-dir=/opt/winfonts",
		"--fingerprinting-client-rects-noise",
		"--fingerprinting-canvas-measuretext-noise",
		"--fingerprinting-canvas-image-data-noise",
		acceptLangArg(locale),
	}
	if proxy != "" {
		args = append(args, "--fingerprint-network-profile=residential")
	}
	return args
}

// acceptLangArg builds the --accept-lang header from a locale, appending the
// bare base ("en" from "en-US") as a secondary preference.
func acceptLangArg(locale string) string {
	lang := locale
	if lang == "" {
		lang = "en-US"
	}
	base, _, _ := strings.Cut(lang, "-")
	if base != lang {
		return "--accept-lang=" + lang + "," + base
	}
	return "--accept-lang=" + lang
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
