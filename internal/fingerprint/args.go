// Package fingerprint builds the stealth Chrome argument vector and resolves
// proxy geo/exit-IP metadata. Its output is pinned by the golden snapshot in
// testdata and regression-tested, because a silent drift is a silent stealth
// loss.
package fingerprint

import (
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
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

// MatchesSeedGrammar reports whether name fits the seed character grammar,
// INCLUDING the reserved default sentinel (which is a legal snapshot filename
// stem even though it is not a user-supplied seed). The serve daemon's snapshot
// store uses it as a path-safety guard so a store key can never contain a path
// separator; it is the single source for that grammar.
func MatchesSeedGrammar(name string) bool {
	return seedRE.MatchString(name)
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

// personaArch selects the stealth persona by build target: the arm64 image runs
// native on Apple Silicon and presents a macOS persona; every other target
// presents Windows. Both images are Linux, so the selector is GOARCH, not GOOS.
// Overridable so the golden test pins both personas host-independently.
var personaArch = func() string { return runtime.GOARCH }

func personaIsMacOS() bool { return personaArch() == "arm64" }

// personaPlatform is the --fingerprint-platform value for the current persona.
func personaPlatform() string {
	if personaIsMacOS() {
		return "macos"
	}
	return "windows"
}

func getDefaultStealthArgs() []string {
	return []string{
		"--no-sandbox",
		fmt.Sprintf("--fingerprint=%d", seedSource()),
		"--fingerprint-platform=" + personaPlatform(),
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
// canvas/client-rects noise switches, UA-CH brand/platform coherence, a font
// dir, the Accept-Language header, and a residential network profile.
// Returns nil unless a fork binary is selected via CUTTLE_BROWSER_BINARY.
//
// The persona is selected by build target (personaIsMacOS):
//   - linux/amd64 -> Windows. The container spoofs a Direct3D11 GPU pair, so a
//     forced Windows UA + Windows font dir + platform=windows are all coherent.
//   - linux/arm64 -> macOS. Runs native on Apple Silicon; a real Mac reports the
//     frozen Intel Mac OS X 10_15_7 Chrome UA, UA-CH architecture=arm (the arm64
//     binary derives it from its compile target - clark patch 0007), and an Apple
//     Metal WebGL string (pinned below via --fingerprint-gpu-*, since clark's
//     platform=macos GPU default is actually an Intel-Mac card). UA/CH values are
//     pinned to one source to close clark's two-code-path leak (see
//     docs/2607-17-native-macos-backend.md). Fonts come from the baked
//     /opt/personafonts pack (see packages/browser/README.md).
//
// Both personas re-enable coherent referrers: clark's patch 0041 flips
// kNoReferrers on, which per the Fetch spec serializes a same-origin POST's
// Origin to "null" - rejected by strict-Origin CSRF (GitHub's Rails /session)
// with HTTP 422. --disable-features restores an Origin + Referer that match a
// real Chrome.
func ForkParityArgs(locale, proxy string) []string {
	if os.Getenv(BinaryPathEnv) == "" {
		return nil
	}
	// Windows (amd64) and macOS (arm64) differ only in these four values; the
	// stealth flags below are shared, so table the delta instead of forking the
	// whole slice (a copy-paste split lets one branch silently drift, and the
	// golden snapshots each persona separately so it wouldn't trip the tripwire).
	platform := personaPlatform()
	platformVersion := "19.0.0"
	userAgent := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) " +
		"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36"
	// One path for both personas: the image ships only the pack matching its arch
	// (Dockerfile personafonts-${TARGETARCH}), so there is nothing to choose here.
	const fontsDir = "/opt/personafonts"
	if personaIsMacOS() {
		platformVersion = "15.0.0"
		userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
			"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36"
	}
	args := []string{
		"--fingerprint-platform=" + platform,
		"--fingerprint-platform-version=" + platformVersion,
		"--fingerprint-brand=Chrome",
		"--fingerprint-brand-version=148.0.0.0",
		"--user-agent=" + userAgent,
		"--fingerprint-fonts-dir=" + fontsDir,
		"--fingerprinting-client-rects-noise",
		"--fingerprinting-canvas-measuretext-noise",
		"--fingerprinting-canvas-image-data-noise",
		// WebGPU is disabled deliberately, and it belongs in THIS value rather than a
		// second --disable-features flag: Chrome takes the last one, and BuildArgs
		// dedupes by flag key, so a second flag would silently drop the referrer fix
		// above. The container has no Vulkan driver, so leaving WebGPU on gives the
		// worst of both worlds - navigator.gpu present but requestAdapter() null,
		// i.e. a machine that claims a working discrete GPU over WebGL and cannot
		// produce an adapter (measured on both personas). Absent is coherent with
		// the many real setups that have no WebGPU; broken is coherent with nothing.
		"--disable-features=NoReferrers,NoCrossOriginReferrers,MinimalReferrers,WebGPU",
		acceptLangArg(locale),
	}
	if proxy != "" {
		args = append(args, "--fingerprint-network-profile=residential")
	}
	return args
}

// appleModel is one coherent Apple Silicon machine: the Metal renderer string
// and the CPU core count have to agree, because a detector can read both.
type appleModel struct {
	renderer string
	cores    int
}

// appleModels is the macOS persona's machine pool. The Windows persona gets its
// GPU from the binary's own seeded pool (three cards), so pinning macOS to a
// single machine would leave it with strictly less entropy than Windows for no
// reason - these personas are held to the same bar.
//
// The pool cannot simply be left to the binary: its macos GPU table contains an
// Intel-Mac card (AMD Radeon Pro 5500M) that would contradict the
// architecture=arm the arm64 build reports, and its CPU table is PC-shaped
// (4/6/8/12/16), handing out core counts no Apple Silicon Mac has. So cuttle
// owns the pairing. Core counts are the shipping configurations: base M-series
// is 8, Pro is 10-12, Max is 14-16.
var appleModels = []appleModel{
	{"ANGLE (Apple, ANGLE Metal Renderer: Apple M1, Unspecified Version)", 8},
	{"ANGLE (Apple, ANGLE Metal Renderer: Apple M2, Unspecified Version)", 8},
	{"ANGLE (Apple, ANGLE Metal Renderer: Apple M3, Unspecified Version)", 8},
	{"ANGLE (Apple, ANGLE Metal Renderer: Apple M1 Pro, Unspecified Version)", 10},
	{"ANGLE (Apple, ANGLE Metal Renderer: Apple M2 Pro, Unspecified Version)", 12},
	{"ANGLE (Apple, ANGLE Metal Renderer: Apple M3 Max, Unspecified Version)", 16},
}

// AppleSiliconArgs pins the seed's Mac machine: GPU, core count and memory as
// one coherent set. Returns nil on the Windows persona (whose own pools are
// already plausible) and without a fork binary.
//
// deviceMemory is 8 for every entry and that is not a shortcut: Chrome clamps
// navigator.deviceMemory to a maximum of 8, and no Apple Silicon Mac ships less
// than 8GB, so 8 is the only value a real Mac can report. The binary's 4GB
// option is simply unreachable hardware.
func AppleSiliconArgs(seed string) []string {
	if os.Getenv(BinaryPathEnv) == "" || !personaIsMacOS() {
		return nil
	}
	m := appleModels[seedIndex(seed, "apple", len(appleModels))]
	return []string{
		"--fingerprint-gpu-vendor=Google Inc. (Apple)",
		"--fingerprint-gpu-renderer=" + m.renderer,
		fmt.Sprintf("--fingerprint-hardware-concurrency=%d", m.cores),
		"--fingerprint-device-memory=8",
	}
}

// seedIndex maps a seed onto a table slot deterministically - the same seed must
// present the same machine on every launch, or its identity changes underneath a
// sticky proxy exit. The salt keeps independent tables from correlating, so a
// seed's screen choice does not give away its GPU choice.
func seedIndex(seed, salt string, n int) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(salt))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(seed))
	// Mask off the sign bit before widening: keeps the index non-negative on a
	// 32-bit int without a lossy conversion of len().
	return int(h.Sum32()&0x7fffffff) % n
}

type screenSize struct{ width, height int }

// Screen tables are per persona, because a display is as much a platform tell as
// a font is. 1366x768 is a budget-PC panel Apple has never shipped, and 1536x864
// is literally Windows' 1920x1080 at 125% scaling - either one under a macOS UA
// is a free contradiction, made worse by the Retina dPR of 2 the Mac persona
// reports (1366x768 at 2x implies a 2732x1536 panel that does not exist).
//
// Both tables stay within the container's fixed 1920x1080 X display once the
// taskbar is subtracted: X does not clamp an oversized window, so a larger pair
// would raster off-screen pixels for the browser's whole life on a memory-capped
// node. Pairs only - never split a width and height across two entries.
var (
	// Stock Windows desktop/laptop resolutions.
	screenChoicesWindows = []screenSize{
		{1920, 1080}, {1536, 864}, {1366, 768}, {1440, 900},
	}
	// Default scaled resolutions of Apple Silicon notebooks: MacBook Air 13" (M1
	// and M2), MacBook Pro 14", MacBook Air 15", MacBook Pro 16".
	screenChoicesMacOS = []screenSize{
		{1440, 900}, {1470, 956}, {1512, 982}, {1710, 1112}, {1728, 1117},
	}
)

func screenChoices() []screenSize {
	if personaIsMacOS() {
		return screenChoicesMacOS
	}
	return screenChoicesWindows
}

// taskbarHeight is the desktop chrome the OS reserves off the screen edge, which
// is what makes screen.availHeight smaller than screen.height.
func taskbarHeight() int {
	if personaIsMacOS() {
		return 95
	}
	return 48
}

// ScreenArgs pins the seed's display and sizes the OS window to match it. Both
// halves are one decision: the binary spoofs screen.* and window.outer* from the
// seed, but window.inner* is the REAL window, which otherwise keeps whatever
// size the window manager happened to give it - so a seed reports outerWidth
// 1536 around an innerWidth of 780, a window with 750px of invisible chrome. No
// real browser looks like that. Sizing the window to the screen minus the
// taskbar is the maximized state most real desktops are in, and makes inner
// track outer.
//
// The taskbar height is pinned too rather than left to the binary's per-platform
// default: the window arithmetic here depends on it, so a drift in that default
// would silently desync inner from outer again.
//
// Returns nil unless a fork binary is selected via CUTTLE_BROWSER_BINARY: stock
// Chrome does not spoof screen.*, so there is no incoherence to close.
func ScreenArgs(seed string) []string {
	if os.Getenv(BinaryPathEnv) == "" {
		return nil
	}
	taskbar := taskbarHeight()
	choices := screenChoices()
	s := choices[seedIndex(seed, "screen", len(choices))]
	return []string{
		fmt.Sprintf("--fingerprint-screen-width=%d", s.width),
		fmt.Sprintf("--fingerprint-screen-height=%d", s.height),
		fmt.Sprintf("--fingerprint-taskbar-height=%d", taskbar),
		fmt.Sprintf("--window-size=%d,%d", s.width, s.height-taskbar),
	}
}

// screenArgKeys are the flag keys ScreenArgs owns.
var screenArgKeys = []string{
	"--fingerprint-screen-width",
	"--fingerprint-screen-height",
	"--fingerprint-taskbar-height",
	"--window-size",
}

// PinsScreen reports whether args already pin any part of the display. A caller
// that sets one of them takes over the whole set: the values are a coherent
// group, so contributing half of it - a caller's width against cuttle's height
// and window size - is worse than staying out entirely.
func PinsScreen(args []string) bool {
	return slices.ContainsFunc(args, func(a string) bool {
		return slices.Contains(screenArgKeys, argKey(a))
	})
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
