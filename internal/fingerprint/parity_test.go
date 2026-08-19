package fingerprint

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
)

type goldenFile struct {
	ExitIPStub         string            `json:"exit_ip_stub"`
	CountryLocaleMap   map[string]string `json:"country_locale_map"`
	DefaultStealthArgs []struct {
		Arch   string   `json:"arch"`
		Seed   int      `json:"seed"`
		Output []string `json:"output"`
	} `json:"default_stealth_args"`
	EnsureProxyScheme []struct {
		Input  string `json:"input"`
		Output string `json:"output"`
	} `json:"ensure_proxy_scheme"`
	NormalizeSocks []struct {
		Input  string `json:"input"`
		Output string `json:"output"`
	} `json:"normalize_socks"`
	ResolveWebrtc []struct {
		InputArgs []string `json:"input_args"`
		Proxy     *string  `json:"proxy"`
		ExitIP    *string  `json:"exit_ip"`
		Output    []string `json:"output"`
	} `json:"resolve_webrtc"`
	BuildArgs []struct {
		Name  string `json:"name"`
		Input struct {
			StealthArgs    *bool    `json:"stealth_args"`
			ExtraArgs      []string `json:"extra_args"`
			Timezone       string   `json:"timezone"`
			Locale         string   `json:"locale"`
			Headless       *bool    `json:"headless"`
			ExtensionPaths []string `json:"extension_paths"`
			StartMaximized *bool    `json:"start_maximized"`
		} `json:"input"`
		Output []string `json:"output"`
	} `json:"build_args"`
	ComposeArgv []struct {
		Seed     *string  `json:"seed"`
		Proxy    *string  `json:"proxy"`
		Timezone *string  `json:"timezone"`
		Locale   *string  `json:"locale"`
		Webrtc   string   `json:"webrtc"`
		Output   []string `json:"output"`
	} `json:"compose_argv"`
	SplitProxyAuth []struct {
		Input    string `json:"input"`
		Server   string `json:"server"`
		Username string `json:"username"`
		Password string `json:"password"`
	} `json:"split_proxy_auth"`
	ForkParityArgs []struct {
		Arch   string   `json:"arch"`
		Locale string   `json:"locale"`
		Proxy  *string  `json:"proxy"`
		Output []string `json:"output"`
	} `json:"fork_parity_args"`
	ScreenArgs []struct {
		Arch   string   `json:"arch"`
		Seed   string   `json:"seed"`
		Output []string `json:"output"`
	} `json:"screen_args"`
	AppleSiliconArgs []struct {
		Arch   string   `json:"arch"`
		Seed   string   `json:"seed"`
		Output []string `json:"output"`
	} `json:"apple_silicon_args"`
}

const pinnedSeed = 55555

func loadGolden(t *testing.T) goldenFile {
	t.Helper()
	raw, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var g goldenFile
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatalf("unmarshal golden: %v", err)
	}
	return g
}

// pinLinux forces the container-target platform and a fixed stealth seed so
// BuildArgs output matches the committed golden snapshot.
func pinLinux(t *testing.T) {
	t.Helper()
	origSystem, origSeed, origArch := systemName, seedSource, personaArch
	systemName = func() string { return "Linux" }
	seedSource = func() int { return pinnedSeed }
	// The general BuildArgs/ComposeArgv golden fixes the Windows/amd64 persona
	// (the production default); getDefaultStealthArgs reads personaArch, so pin it
	// too or the snapshot flips with the test host's GOARCH. The macOS persona is
	// covered by the dedicated arch-tagged default_stealth_args / fork_parity_args
	// cases.
	personaArch = func() string { return "amd64" }
	t.Cleanup(func() { systemName, seedSource, personaArch = origSystem, origSeed, origArch })
}

func TestCountryLocaleMapParity(t *testing.T) {
	g := loadGolden(t)
	if !maps.Equal(CountryLocaleMap, g.CountryLocaleMap) {
		t.Errorf("COUNTRY_LOCALE_MAP diverged from golden (%d vs %d entries)",
			len(CountryLocaleMap), len(g.CountryLocaleMap))
	}
}

func TestDefaultStealthArgsParity(t *testing.T) {
	g := loadGolden(t)
	origArch, origSeed := personaArch, seedSource
	t.Cleanup(func() { personaArch, seedSource = origArch, origSeed })
	for _, c := range g.DefaultStealthArgs {
		personaArch = func() string { return c.Arch }
		seedSource = func() int { return c.Seed }
		got := getDefaultStealthArgs()
		if !slices.Equal(got, c.Output) {
			t.Errorf("arch %s:\n got %q\nwant %q", c.Arch, got, c.Output)
		}
	}
}

func TestEnsureProxySchemeParity(t *testing.T) {
	g := loadGolden(t)
	for _, c := range g.EnsureProxyScheme {
		if got := EnsureProxyScheme(c.Input); got != c.Output {
			t.Errorf("EnsureProxyScheme(%q) = %q, want %q", c.Input, got, c.Output)
		}
	}
}

func TestNormalizeSocksParity(t *testing.T) {
	g := loadGolden(t)
	for _, c := range g.NormalizeSocks {
		if got := NormalizeSocksStringURL(c.Input); got != c.Output {
			t.Errorf("NormalizeSocksStringURL(%q) = %q, want %q", c.Input, got, c.Output)
		}
	}
}

func TestResolveWebRTCArgsParity(t *testing.T) {
	g := loadGolden(t)
	for _, c := range g.ResolveWebrtc {
		proxy := ""
		if c.Proxy != nil {
			proxy = *c.Proxy
		}
		exitIP := ""
		if c.ExitIP != nil {
			exitIP = *c.ExitIP
		}
		got := ResolveWebRTCArgs(slices.Clone(c.InputArgs), proxy, func(string) string { return exitIP })
		if !slices.Equal(got, c.Output) {
			t.Errorf("ResolveWebRTCArgs(%q, %q) = %q, want %q", c.InputArgs, proxy, got, c.Output)
		}
	}
}

func TestBuildArgsParity(t *testing.T) {
	pinLinux(t)
	g := loadGolden(t)
	for _, c := range g.BuildArgs {
		t.Run(c.Name, func(t *testing.T) {
			in := BuildArgsInput{
				ExtraArgs:      c.Input.ExtraArgs,
				Timezone:       c.Input.Timezone,
				Locale:         c.Input.Locale,
				ExtensionPaths: c.Input.ExtensionPaths,
				Headless:       true,
			}
			if c.Input.StealthArgs != nil {
				in.StealthArgs = *c.Input.StealthArgs
			}
			if c.Input.Headless != nil {
				in.Headless = *c.Input.Headless
			}
			if c.Input.StartMaximized != nil {
				in.StartMaximized = *c.Input.StartMaximized
			}
			got := BuildArgs(in)
			if !slices.Equal(got, c.Output) {
				t.Errorf("%s:\n got %q\nwant %q", c.Name, got, c.Output)
			}
		})
	}
}

// TestScreenArgsParity pins the seed -> screen/window mapping against the golden
// and, separately, the invariant that makes the mapping worth having: the window
// must never be wider or taller than the screen the same seed reports.
func TestScreenArgsParity(t *testing.T) {
	t.Setenv(BinaryPathEnv, "/opt/browser/chrome")
	g := loadGolden(t)
	if len(g.ScreenArgs) == 0 {
		t.Fatal("golden has no screen_args cases")
	}
	for _, c := range g.ScreenArgs {
		got := screenArgsFor(c.Arch, c.Seed)
		if !slices.Equal(got, c.Output) {
			t.Errorf("ScreenArgs(%q) arch=%s:\n got %q\nwant %q", c.Seed, c.Arch, got, c.Output)
			continue
		}
		w := intArg(t, got, "--fingerprint-screen-width=%d")
		h := intArg(t, got, "--fingerprint-screen-height=%d")
		winW, winH := windowSizeArg(t, got)
		if winW > w || winH > h {
			t.Errorf("seed %q arch=%s: window %dx%d exceeds screen %dx%d",
				c.Seed, c.Arch, winW, winH, w, h)
		}
		// The X display the container runs is a fixed 1920x1080 and X does not clamp
		// an oversized window, so a WINDOW larger than that would raster off-screen
		// pixels for the browser's whole life. The screen may legitimately exceed
		// the display (a Mac reports 1710x1112 while its window is 1710x1017); only
		// the window has to fit.
		if winW > 1920 || winH > 1080 {
			t.Errorf("seed %q arch=%s: window %dx%d exceeds the 1920x1080 display",
				c.Seed, c.Arch, winW, winH)
		}
	}
}

// intArg pulls a single int-valued flag out of an arg vector, failing the test if
// it is absent - so a renamed flag surfaces as a failure rather than silently
// degrading an invariant check to 0-vs-0.
func intArg(t *testing.T, args []string, format string) int {
	t.Helper()
	for _, a := range args {
		var v int
		if n, err := fmt.Sscanf(a, format, &v); n == 1 && err == nil {
			return v
		}
	}
	t.Fatalf("no arg matching %q in %q", format, args)
	return 0
}

func windowSizeArg(t *testing.T, args []string) (int, int) {
	t.Helper()
	for _, a := range args {
		var w, h int
		if n, err := fmt.Sscanf(a, "--window-size=%d,%d", &w, &h); n == 2 && err == nil {
			return w, h
		}
	}
	t.Fatal("no --window-size in " + strings.Join(args, " "))
	return 0, 0
}

// A seed's display must be stable across launches: a sticky proxy exit whose
// screen changes between sessions is the same contradiction, spread over time.
func TestScreenArgsStablePerSeed(t *testing.T) {
	t.Setenv(BinaryPathEnv, "/opt/browser/chrome")
	for _, seed := range []string{"reddit-3", "crawl-2", "a", "zzz"} {
		first := ScreenArgs(seed)
		for range 5 {
			if got := ScreenArgs(seed); !slices.Equal(got, first) {
				t.Fatalf("seed %q not stable: %q vs %q", seed, got, first)
			}
		}
	}
}

// TestAppleSiliconArgsParity pins the seed -> Mac machine mapping and the
// invariant behind it: the GPU names a machine, so the core count has to be one
// that machine actually ships with. An Apple GPU beside 4 or 6 cores, or beside
// 4GB of memory, is hardware that has never existed.
func TestAppleSiliconArgsParity(t *testing.T) {
	t.Setenv(BinaryPathEnv, "/opt/browser/chrome")
	g := loadGolden(t)
	if len(g.AppleSiliconArgs) == 0 {
		t.Fatal("golden has no apple_silicon_args cases")
	}
	// Core counts Apple actually ships: base M-series 8, Pro 10-12, Max 14-16.
	valid := map[int]bool{8: true, 10: true, 12: true, 14: true, 16: true}
	sawArm := 0
	for _, c := range g.AppleSiliconArgs {
		got := appleSiliconArgsFor(c.Arch, c.Seed)
		if !slices.Equal(got, c.Output) {
			t.Errorf("AppleSiliconArgs(%q) arch=%s:\n got %q\nwant %q", c.Seed, c.Arch, got, c.Output)
			continue
		}
		if c.Arch != "arm64" {
			if got != nil {
				t.Errorf("arch=%s must be left to the binary's own pool, got %q", c.Arch, got)
			}
			continue
		}
		sawArm++
		renderer := stringArg(t, got, "--fingerprint-gpu-renderer=")
		if !strings.Contains(renderer, "Apple") {
			t.Errorf("seed %q: renderer %q is not an Apple machine", c.Seed, renderer)
		}
		if cores := intArg(t, got, "--fingerprint-hardware-concurrency=%d"); !valid[cores] {
			t.Errorf("seed %q: %d cores is not a shipping Apple Silicon configuration (%s)",
				c.Seed, cores, renderer)
		}
		mem := intArg(t, got, "--fingerprint-device-memory=%d")
		if !reportableMemory[mem] {
			t.Errorf("seed %q: deviceMemory %d is not a value the quantizer can produce "+
				"(desktop clamps to [2,32], powers of two)", c.Seed, mem)
		}
		if mem < 8 {
			t.Errorf("seed %q: deviceMemory %d - no Apple Silicon Mac ships less than 8GB",
				c.Seed, mem)
		}
	}
	if sawArm == 0 {
		t.Fatal("no arm64 cases covered")
	}
}

// reportableMemory is what navigator.deviceMemory can actually be: the
// approximator rounds physical RAM to a power of two and clamps it to [2, 32] on
// desktop (blink/common/device_memory/approximated_device_memory.cc at 151).
// Anything outside this set could not come from a real machine.
var reportableMemory = map[int]bool{2: true, 4: true, 8: true, 16: true, 32: true}

// stringArg pulls a flag's value out of an arg vector, failing if it is absent.
func stringArg(t *testing.T, args []string, prefix string) string {
	t.Helper()
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, prefix); ok {
			return v
		}
	}
	t.Fatalf("no arg with prefix %q in %q", prefix, args)
	return ""
}

// A seed's machine must be stable across launches, for the same reason its
// display must be: a sticky proxy exit whose GPU changes between sessions is a
// contradiction spread over time.
func TestAppleSiliconStablePerSeed(t *testing.T) {
	t.Setenv(BinaryPathEnv, "/opt/browser/chrome")
	orig := personaArch
	t.Cleanup(func() { personaArch = orig })
	personaArch = func() string { return "arm64" }
	for _, seed := range []string{"reddit-3", "crawl-2", "a"} {
		first := AppleSiliconArgs(seed)
		if len(first) == 0 {
			t.Fatalf("seed %q got no args", seed)
		}
		for range 5 {
			if got := AppleSiliconArgs(seed); !slices.Equal(got, first) {
				t.Fatalf("seed %q not stable: %q vs %q", seed, got, first)
			}
		}
	}
}

// The screen and machine tables are salted independently, so knowing one of a
// seed's choices must not reveal the other. Without the salt both would be the
// same hash mod different lengths, correlating the two surfaces.
func TestSeedTablesDoNotCorrelate(t *testing.T) {
	if seedIndex("same-seed", "screen", 6) == seedIndex("same-seed", "apple", 6) &&
		seedIndex("other-seed", "screen", 6) == seedIndex("other-seed", "apple", 6) &&
		seedIndex("third-seed", "screen", 6) == seedIndex("third-seed", "apple", 6) {
		t.Error("screen and apple tables appear to share an index - is the salt applied?")
	}
}

// PinsScreen decides whether cuttle contributes its display set at all. Half a
// set is worse than none: a caller's width against cuttle's height and window
// size is exactly the incoherence ScreenArgs exists to prevent.
func TestPinsScreen(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{"empty", nil, false},
		{"unrelated fingerprint args", []string{"--fingerprint-platform=windows", "--lang=en-US"}, false},
		{"width alone", []string{"--fingerprint-screen-width=1024"}, true},
		{"height alone", []string{"--fingerprint-screen-height=768"}, true},
		{"taskbar alone", []string{"--fingerprint-taskbar-height=0"}, true},
		{"window-size alone", []string{"--window-size=800,600"}, true},
		{"valueless flag form", []string{"--window-size"}, true},
		// Substring neighbours must not trip it: these are different flags.
		{"screen-width prefix neighbour", []string{"--fingerprint-screen-width-scale=2"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := PinsScreen(tc.args); got != tc.want {
				t.Errorf("PinsScreen(%q) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// Without a fork binary there is no spoofed screen, so there is nothing to keep
// the window coherent with and cuttle must not pin either one.
func TestScreenArgsNilWithoutForkBinary(t *testing.T) {
	t.Setenv(BinaryPathEnv, "")
	if got := ScreenArgs("seed"); got != nil {
		t.Errorf("want nil without a fork binary, got %q", got)
	}
}

func TestComposeArgvParity(t *testing.T) {
	pinLinux(t)
	g := loadGolden(t)
	stub := func(string) string { return g.ExitIPStub }
	for i, c := range g.ComposeArgv {
		got := composeArgv(c.Seed, c.Proxy, deref(c.Timezone), deref(c.Locale), c.Webrtc, stub)
		if !slices.Equal(got, c.Output) {
			t.Errorf("compose[%d] seed=%v proxy=%v tz=%v loc=%v webrtc=%s:\n got %q\nwant %q",
				i, c.Seed, c.Proxy, c.Timezone, c.Locale, c.Webrtc, got, c.Output)
		}
	}
}

func TestSplitProxyAuthParity(t *testing.T) {
	g := loadGolden(t)
	for _, c := range g.SplitProxyAuth {
		server, user, pass := SplitProxyAuth(c.Input)
		if server != c.Server || user != c.Username || pass != c.Password {
			t.Errorf("SplitProxyAuth(%q) = (%q,%q,%q), want (%q,%q,%q)",
				c.Input, server, user, pass, c.Server, c.Username, c.Password)
		}
	}
}

func TestForkParityArgsParity(t *testing.T) {
	t.Setenv(BinaryPathEnv, "/opt/browser/chrome")
	g := loadGolden(t)
	orig := personaArch
	t.Cleanup(func() { personaArch = orig })
	for _, c := range g.ForkParityArgs {
		personaArch = func() string { return c.Arch }
		got := ForkParityArgs(c.Locale, deref(c.Proxy))
		if !slices.Equal(got, c.Output) {
			t.Errorf("ForkParityArgs(arch=%s, %q, %v) = %q, want %q", c.Arch, c.Locale, c.Proxy, got, c.Output)
		}
	}
}

// composeArgv drives proxy + WebRTC through BuildArgs using only the ported
// primitives, so the full argv is exercised for the golden snapshot.
func composeArgv(seed, proxy *string, timezone, locale, webrtc string, exitIP func(string) string) []string {
	var extra []string
	if seed != nil {
		extra = append(extra, "--fingerprint="+*seed)
	}
	if proxy != nil {
		stripped, _, _ := SplitProxyAuth(*proxy)
		extra = append(extra, "--proxy-server="+NormalizeSocksStringURL(stripped))
	}
	if webrtc == "auto" {
		extra = append(extra, "--fingerprint-webrtc-ip=auto")
	}
	extra = ResolveWebRTCArgs(extra, deref(proxy), exitIP)
	return BuildArgs(BuildArgsInput{StealthArgs: true, ExtraArgs: extra, Timezone: timezone, Locale: locale, Headless: true})
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
