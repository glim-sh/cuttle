package fingerprint

import (
	"encoding/json"
	"maps"
	"os"
	"slices"
	"testing"
)

type goldenFile struct {
	ExitIPStub         string            `json:"exit_ip_stub"`
	CountryLocaleMap   map[string]string `json:"country_locale_map"`
	DefaultStealthArgs []struct {
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
		Locale string   `json:"locale"`
		Proxy  *string  `json:"proxy"`
		Output []string `json:"output"`
	} `json:"fork_parity_args"`
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
	origSystem, origSeed := systemName, seedSource
	systemName = func() string { return "Linux" }
	seedSource = func() int { return pinnedSeed }
	t.Cleanup(func() { systemName, seedSource = origSystem, origSeed })
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
	origSeed := seedSource
	t.Cleanup(func() { seedSource = origSeed })
	for _, c := range g.DefaultStealthArgs {
		seedSource = func() int { return c.Seed }
		got := getDefaultStealthArgs()
		if !slices.Equal(got, c.Output) {
			t.Errorf("got %q\nwant %q", got, c.Output)
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
	t.Setenv(BinaryPathEnv, "/opt/clark/chrome")
	g := loadGolden(t)
	for _, c := range g.ForkParityArgs {
		got := ForkParityArgs(c.Locale, deref(c.Proxy))
		if !slices.Equal(got, c.Output) {
			t.Errorf("ForkParityArgs(%q, %v) = %q, want %q", c.Locale, c.Proxy, got, c.Output)
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
