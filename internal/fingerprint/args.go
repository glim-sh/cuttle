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

// chromiumVersion is the pinned stealth-Chromium build and the single source of
// every version string this package emits. packages/browser/versions.env pins
// the same value for the build pipeline and the validate harness reads it from
// there, so a browser bump touches exactly two files. TestChromiumVersionPin
// fails if the two drift.
const chromiumVersion = "151.0.7922.137"

// chromeUAVersion is the reduced major.0.0.0 form Chrome puts in
// navigator.userAgent. The full 4-part build appears only in UA-CH, and the two
// must be derived from one value: the binary rewrites navigator.userAgent to its
// own real version whenever a fingerprint persona is active, so a --user-agent
// that disagrees with the build produces a UA/UA-CH split no real Chrome shows.
var chromeUAVersion = majorVersion(chromiumVersion) + ".0.0.0"

func majorVersion(version string) string {
	major, _, _ := strings.Cut(version, ".")
	return major
}

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
// baseChromeArgs run Chrome directly (outside Playwright); Playwright normally
// adds its own version of these.
//
// The three backgrounding switches matter because we spawn Chrome ourselves: a
// client attaching over CDP cannot supply launch flags, so whatever is missing
// here it can never add. They do NOT keep a hidden tab compositing (that is
// Emulation.setFocusEmulationEnabled, pinned per page in wsproxy.go) - they stop
// a long-hidden tab's renderer being deprioritized and its timers clamped.
// Measured: a tab hidden ~35min fell to ~1fps and 3s clicks even with focus
// emulation, against sub-second freshly hidden.
var baseChromeArgs = []string{
	"--no-first-run",
	"--no-default-browser-check",
	"--disable-dev-shm-usage",
	"--disable-extensions",
	"--disable-popup-blocking",
	"--disable-background-networking",
	"--metrics-recording-only",
	"--ignore-gpu-blocklist",
	"--disable-renderer-backgrounding",
	"--disable-backgrounding-occluded-windows",
	"--disable-background-timer-throttling",
}

// BaseChromeArgs returns the flags the daemon launches every Chrome with.
//
// Exported because the posture bench has to launch the browser the same way the
// daemon does. It used to replay only the fingerprint args and silently dropped
// this list, including --disable-dev-shm-usage - so on a container with the
// default 64MB /dev/shm the renderer died partway through a heavy detector page,
// intermittently. That reads as a flaky harness, a slow host or a bad build, and
// was misdiagnosed as all three. The list is dumped into golden.json so Python
// reads it rather than keeping a copy that can drift.
//
// Returns a clone: the caller appends to it.
func BaseChromeArgs() []string {
	return slices.Clone(baseChromeArgs)
}

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

// ForkParityArgs replicates clark's own launcher flag set, which the
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
// Both personas re-enable coherent referrers: patch 0040 flips
// kMinimalReferrers and kNoCrossOriginReferrers on, and suppressed referrers
// serialize a same-origin POST's Origin to "null" per the Fetch spec - rejected
// by strict-Origin CSRF (GitHub's Rails /session) with HTTP 422.
// --disable-features restores an Origin + Referer that match a real Chrome.
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
		"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + chromeUAVersion + " Safari/537.36"
	// One path for both personas: the image ships only the pack matching its arch
	// (Dockerfile personafonts-${TARGETARCH}), so there is nothing to choose here.
	const fontsDir = "/opt/personafonts"
	if personaIsMacOS() {
		// Measured on a real Mac running macOS 26.7: Chrome reports
		// platformVersion "26.7.0" while the UA keeps the frozen 10_15_7 token.
		// This must stay paired with the macOS voice table, which was captured on
		// that same machine - a persona claiming an older macOS would ship a voice
		// list that OS never had.
		platformVersion = "26.7.0"
		userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
			"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + chromeUAVersion + " Safari/537.36"
	}
	args := []string{
		"--fingerprint-platform=" + platform,
		"--fingerprint-platform-version=" + platformVersion,
		"--fingerprint-brand=Chrome",
		// Feeds Sec-CH-UA-Full-Version-List, which real Chrome fills with the actual
		// 4-part build - hence the FULL pinned version here, not the reduced
		// X.0.0.0 form the UA uses, which would advertise a build number no Chrome
		// release ever had. It also seeds the GREASE brand, so it must track the
		// binary: real Chrome derives both from the same version.
		"--fingerprint-brand-version=" + chromiumVersion,
		"--user-agent=" + userAgent,
		"--fingerprint-fonts-dir=" + fontsDir,
		"--fingerprinting-client-rects-noise",
		"--fingerprinting-canvas-measuretext-noise",
		"--fingerprinting-canvas-image-data-noise",
		// Deliberately NOT disabling WebGPU here. The container has no Vulkan driver,
		// so navigator.gpu is present while requestAdapter() returns null - which
		// looks like a mismatch beside a high-confidence WebGL GPU, but is not one:
		// clark's own conformance test folds "no adapter" and "no navigator.gpu"
		// into the same `supported: false` profile, and packages/browser/README.md
		// lists an absent adapter as a PASS. The documented failure is an adapter
		// that CONTRADICTS the WebGL GPU, which cannot happen while there is none.
		// Disabling the feature outright would be worse: navigator.gpu has shipped
		// since Chrome 113, so its absence on a browser claiming 151 is the rarer
		// state of the two. If a Vulkan driver ever lands in the image, patch 0049
		// makes the adapter match the WebGL pool - that is the upgrade path, not
		// this flag. (Any addition here must join THIS value, never a second
		// --disable-features: Chrome takes the last one and BuildArgs dedupes by
		// key, so a second flag would silently drop the referrer fix.)
		// RemoveClientHints: patch 0019 turns ungoogled's kRemoveClientHints on,
		// which strips every Sec-CH-UA header. Real Chrome has sent the low-entropy
		// trio on every request since M89, so sending none is a one-header "not
		// Chrome" check - and it silently discarded everything patch 0007 builds.
		// With it off, the headers match real Chrome 151 byte for byte, GREASE brand
		// and ordering included - the header path derives both from
		// --fingerprint-brand-version. navigator.userAgentData does NOT come from
		// that path; patch 0007's Blink half computes it separately, which is why
		// that half has to run the same GREASE algorithm as the header half.
		"--disable-features=NoReferrers,NoCrossOriginReferrers,MinimalReferrers," +
			"RemoveClientHints",
		// Blink defaults these to POINTER_TYPE_NONE/HOVER_TYPE_NONE and normally
		// overwrites them from the platform's detected input devices. Under Xvfb
		// there are none, so the defaults stand and every desktop persona answers
		// (pointer:none) + (hover:none) - "this machine has no pointing device",
		// which no real desktop reports. Measured against real Chrome 151 on both
		// macOS and Windows: (pointer:fine) + (hover:hover), and the any-* variants
		// match. Values are the ui:: bitfields - POINTER_TYPE_FINE=4, HOVER_TYPE_HOVER=2.
		// Verified on our own binary: without this the container reports pointer:none,
		// with it pointer:fine, so no C++ patch is needed for this surface.
		// preferredColorScheme is blink::mojom::PreferredColorScheme, where
		// kDark=0 and kLight=1. Dark is cuttle's default: CreepJS scores
		// prefers-color-scheme:light as a headless tell, and a container has no
		// OS theme to inherit, so the value is ours to choose either way. It must
		// join THIS flag - Chrome takes the last --blink-settings and BuildArgs
		// dedupes by key, so a second one would silently drop the pointer/hover
		// fix above.
		"--blink-settings=availablePointerTypes=4,availableHoverTypes=2," +
			"primaryPointerType=4,primaryHoverType=2,preferredColorScheme=0",
		// A container has no camera or microphone, so enumerateDevices() returned an
		// empty list - real desktop Chrome returns three entries (audioinput,
		// videoinput, audiooutput) with EMPTY labels until a permission grant, which
		// is exactly what this produces. Measured against real Chrome 151 on macOS
		// and verified on our own binary.
		//
		// Deliberately NOT --use-fake-ui-for-media-stream: that auto-accepts the
		// permission prompt, which would both contradict patch 0048 (permissions
		// default to "prompt") and expose the synthetic stream without a real grant.
		// This flag only populates the device list; getUserMedia still needs consent.
		"--use-fake-device-for-media-stream",
		// kWebBluetooth is off by default on Linux but stable on Windows and macOS,
		// so the container exposed navigator.usb/.serial/.hid but not .bluetooth -
		// a host-origin tell no real desktop Chrome produces. Measured against real
		// Chrome 151 on both personas; must join THIS value, per the note above.
		"--enable-features=WebBluetooth",
		// Both are runtime-enabled Blink features that real Chrome ships and an
		// unbranded Linux Chromium does not, so they read as absent and cost us
		// on exactly the checks that ask "is this really Chrome".
		//
		// WebShare is status:"test" in runtime_enabled_features.json5, but real
		// Chrome exposes navigator.share and canShare on BOTH desktop personas -
		// measured on a real Mac and a real Windows box, both "function".
		//
		// BarcodeDetector is status {Mac, Android, ChromeOS: stable, default:
		// test}, so it is macOS-only here: a real Windows Chrome does NOT have it
		// (measured false on real Windows), and adding it there would invent a
		// tell rather than remove one. It is also the single signal separating
		// CreepJS's Windows and Mac platform estimates - without it the macOS
		// persona scores as Windows.
		blinkFeatures(),
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
	memoryGB int
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
	{"ANGLE (Apple, ANGLE Metal Renderer: Apple M1, Unspecified Version)", 8, 16},
	{"ANGLE (Apple, ANGLE Metal Renderer: Apple M2, Unspecified Version)", 8, 16},
	{"ANGLE (Apple, ANGLE Metal Renderer: Apple M3, Unspecified Version)", 8, 16},
	{"ANGLE (Apple, ANGLE Metal Renderer: Apple M1 Pro, Unspecified Version)", 10, 16},
	{"ANGLE (Apple, ANGLE Metal Renderer: Apple M2 Pro, Unspecified Version)", 12, 32},
	{"ANGLE (Apple, ANGLE Metal Renderer: Apple M3 Max, Unspecified Version)", 16, 32},
}

// UA-CH-style vendor strings Chrome reports for each GPU maker.
const (
	gpuVendorIntel  = "Google Inc. (Intel)"
	gpuVendorAMD    = "Google Inc. (AMD)"
	gpuVendorNVIDIA = "Google Inc. (NVIDIA)"
)

// blinkFeatures enables the Chrome-shipped Blink features our build hides.
// FaceDetector and TextDetector are deliberately NOT enabled: a real Mac reports
// both absent, so turning them on would create a divergence while closing one.
func blinkFeatures() string {
	features := []string{"WebShare"}
	if personaIsMacOS() {
		features = append(features, "BarcodeDetector")
	}
	return "--enable-blink-features=" + strings.Join(features, ",")
}

type windowsMachine struct {
	vendor   string
	renderer string
	cores    int
	memoryGB int
}

// windowsMachines is the Windows persona's machine pool. Like appleModels it
// exists because the binary draws the parts of a machine INDEPENDENTLY - GPU
// from Hash("webgl-pool"), cores from Hash("hwc") over {4,6,8,12,16}, and memory
// from its own pool - so nothing stops it pairing them into hardware that does
// not exist. Measured on the shipped 151 binary: seed 88 reported 16 threads
// with 4GB of RAM, and the pool can equally hand a thin-and-light Iris Xe iGPU
// 16 threads. One draw per machine removes the whole class.
//
// Core counts are the shipping thread count of a part that actually carries
// that GPU, verified against the vendor spec pages rather than chosen to look
// plausible - the device ID in the renderer string names the exact silicon, so a
// wrong pairing is checkable by anyone.
//
// Integrated parts are the majority of the table on purpose. Stealth tooling
// tends to list gaming GPUs, but the general population runs laptop integrated
// graphics, so a discrete card is the conspicuous choice rather than the safe
// one.
//
// deviceMemory is per-machine for the same reason as the cores: it is clamped to
// [2, 32] on desktop at 151, not to 8, so 16 and 32 are the common answers. Steam
// puts 32GB at ~44% and 16GB at ~43% of gaming machines; the general population
// skews to 16. Budget laptops keep 8.
var windowsMachines = []windowsMachine{
	// Intel integrated. Device IDs identify the SKU, hence the thread counts:
	// 0x9A49 Tiger Lake i7-1185G7 4C/8T, 0x46A8 Alder Lake i5-1235U 10C/12T,
	// 0xA7A1 Raptor Lake i7-1355U 10C/12T, 0x9BC8 Comet Lake i5-10400 6C/12T,
	// 0x3EA0 Whiskey Lake i5-8265U 4C/8T, 0x46B3 Alder Lake i3-1215U 6C/8T.
	{gpuVendorIntel, "ANGLE (Intel, Intel(R) Iris(R) Xe Graphics (0x00009A49) Direct3D11 vs_5_0 ps_5_0, D3D11)", 8, 16},
	{gpuVendorIntel, "ANGLE (Intel, Intel(R) Iris(R) Xe Graphics (0x000046A8) Direct3D11 vs_5_0 ps_5_0, D3D11)", 12, 16},
	{gpuVendorIntel, "ANGLE (Intel, Intel(R) Iris(R) Xe Graphics (0x0000A7A1) Direct3D11 vs_5_0 ps_5_0, D3D11)", 12, 16},
	{gpuVendorIntel, "ANGLE (Intel, Intel(R) UHD Graphics 630 (0x00009BC8) Direct3D11 vs_5_0 ps_5_0, D3D11)", 12, 16},
	{gpuVendorIntel, "ANGLE (Intel, Intel(R) UHD Graphics 620 (0x00003EA0) Direct3D11 vs_5_0 ps_5_0, D3D11)", 8, 8},
	// AMD never branded the Renoir-era iGPUs, so the unnumbered name is correct.
	// Renoir U-series ships SMT DISABLED (Ryzen 5 4500U is 6C/6T), so 12 threads
	// behind this device ID would be an H-series part in a U-series machine.
	{gpuVendorAMD, "ANGLE (AMD, AMD Radeon(TM) Graphics (0x00001636) Direct3D11 vs_5_0 ps_5_0, D3D11)", 6, 8},
	// Discrete desktop. A 3060 or 7600 sits next to a 6C/12T or 8C/16T part.
	{gpuVendorNVIDIA, "ANGLE (NVIDIA, NVIDIA GeForce RTX 3060 (0x00002503) Direct3D11 vs_5_0 ps_5_0, D3D11)", 12, 16},
	{gpuVendorAMD, "ANGLE (AMD, AMD Radeon RX 7600 Direct3D11 vs_5_0 ps_5_0, D3D11)", 16, 32},
}

// WindowsMachineArgs pins the seed's Windows machine: GPU, core count and memory
// as one coherent set. Returns nil on the macOS persona and without a fork
// binary. Mirrors AppleSiliconArgs; see windowsMachines for why the binary's own
// pools are not enough.
func WindowsMachineArgs(seed string) []string {
	if os.Getenv(BinaryPathEnv) == "" || personaIsMacOS() {
		return nil
	}
	m := windowsMachines[seedIndex(seed, "winmachine", len(windowsMachines))]
	return []string{
		"--fingerprint-gpu-vendor=" + m.vendor,
		"--fingerprint-gpu-renderer=" + m.renderer,
		fmt.Sprintf("--fingerprint-hardware-concurrency=%d", m.cores),
		fmt.Sprintf("--fingerprint-device-memory=%d", m.memoryGB),
	}
}

// AppleSiliconArgs pins the seed's Mac machine: GPU, core count and memory as
// one coherent set. Returns nil on the Windows persona (whose own pools are
// already plausible) and without a fork binary.
//
// deviceMemory tracks the machine. The often-repeated "Chrome clamps
// navigator.deviceMemory to 8" is out of date: at 151
// blink/common/device_memory/approximated_device_memory.cc clamps to
// [2, 32] on desktop (Android keeps [1, 8]), so 16 and 32 are reportable and
// are what most real machines report. A real Mac measured at 16; a real Windows
// desktop measured at 16. Reporting 8 everywhere made every persona look like a
// low-RAM machine.
func AppleSiliconArgs(seed string) []string {
	if os.Getenv(BinaryPathEnv) == "" || !personaIsMacOS() {
		return nil
	}
	m := appleModels[seedIndex(seed, "apple", len(appleModels))]
	return []string{
		"--fingerprint-gpu-vendor=Google Inc. (Apple)",
		"--fingerprint-gpu-renderer=" + m.renderer,
		fmt.Sprintf("--fingerprint-hardware-concurrency=%d", m.cores),
		fmt.Sprintf("--fingerprint-device-memory=%d", m.memoryGB),
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
// ScreenPinned reports whether this build pins a screen at all, so a caller can
// find out before it does any work that only matters when one is pinned.
func ScreenPinned() bool { return os.Getenv(BinaryPathEnv) != "" }

// WindowSize is the window ScreenArgs sizes a seed's browser to: its fake screen
// less the taskbar. Exported so the viewer can size the X display to the window
// without formatting an argv and parsing it back.
func WindowSize(seed string) (int, int, bool) {
	if !ScreenPinned() {
		return 0, 0, false
	}
	choices := screenChoices()
	s := choices[seedIndex(seed, "screen", len(choices))]
	return s.width, s.height - taskbarHeight(), true
}

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
