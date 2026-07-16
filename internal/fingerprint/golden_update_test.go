package fingerprint

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"slices"
	"testing"
)

var update = flag.Bool("update", false, "regenerate testdata/golden.json")

const exitIPStub = "203.0.113.7"

// countryLocaleKeyOrder is the emission order of country_locale_map. Go maps
// serialize with sorted keys, but the golden preserves the original insertion
// order (which matches the CountryLocaleMap literal), so the order is pinned
// here and the values are read from the live map.
var countryLocaleKeyOrder = []string{
	"US", "GB", "AU", "CA", "NZ", "IE", "ZA", "SG", "DE", "AT",
	"CH", "FR", "BE", "ES", "MX", "AR", "CO", "CL", "BR", "PT",
	"IT", "NL", "JP", "KR", "CN", "TW", "HK", "RU", "UA", "PL",
	"CZ", "RO", "IL", "TR", "SA", "AE", "EG", "IN", "ID", "PH",
	"TH", "VN", "MY", "SE", "NO", "DK", "FI", "GR", "HU", "BG",
	"SI", "SK", "HR", "RS", "LT", "LV", "EE", "IS", "LU", "MT",
	"CY", "MD", "BY", "GE", "AL", "MK", "BA", "PE", "VE", "EC",
	"UY", "CR", "DO", "GT", "BO", "PY", "PK", "BD", "LK", "KZ",
	"IR", "IQ", "JO", "LB", "KW", "QA", "OM", "BH", "NG", "KE",
	"MA", "DZ", "TN", "GH", "AM", "AZ", "UZ", "KG", "TJ", "TM",
	"ME", "XK", "LI", "MC", "AD", "MM", "KH", "LA", "MN", "BN",
	"MO", "YE", "SY", "PS", "LY", "ET", "TZ", "UG", "SN", "CI",
	"CM", "AO", "MZ", "ZM", "ZW", "HN", "NI", "SV", "PA", "JM",
	"TT", "PR",
}

// localeMapDump serializes CountryLocaleMap in countryLocaleKeyOrder. The outer
// json.Encoder re-indents this compact object into the surrounding 2-space tree.
type localeMapDump struct{}

func (localeMapDump) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range countryLocaleKeyOrder {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		vb, err := json.Marshal(CountryLocaleMap[k])
		if err != nil {
			return nil, err
		}
		b.Write(kb)
		b.WriteByte(':')
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

type ioCaseDump struct {
	Input  string `json:"input"`
	Output string `json:"output"`
}

type stealthCaseDump struct {
	System string   `json:"system"`
	Seed   int      `json:"seed"`
	Output []string `json:"output"`
}

type splitCaseDump struct {
	Input    string `json:"input"`
	Server   string `json:"server"`
	Username string `json:"username"`
	Password string `json:"password"`
}

type forkCaseDump struct {
	Locale string   `json:"locale"`
	Proxy  *string  `json:"proxy"`
	Output []string `json:"output"`
}

type webrtcCaseDump struct {
	InputArgs []string `json:"input_args"`
	Proxy     *string  `json:"proxy"`
	ExitIP    *string  `json:"exit_ip"`
	Output    []string `json:"output"`
}

// buildInputDump mirrors the vendored build_args kwargs. Field order and the
// omitempty set reproduce the original's per-case presence exactly: stealth_args
// and extra_args are always emitted (extra_args as null when unset); the rest
// appear only when supplied.
type buildInputDump struct {
	StealthArgs    bool     `json:"stealth_args"`
	ExtraArgs      []string `json:"extra_args"`
	Timezone       string   `json:"timezone,omitempty"`
	Locale         string   `json:"locale,omitempty"`
	Headless       *bool    `json:"headless,omitempty"`
	ExtensionPaths []string `json:"extension_paths,omitempty"`
	StartMaximized *bool    `json:"start_maximized,omitempty"`
}

type buildCaseDump struct {
	Name   string         `json:"name"`
	Input  buildInputDump `json:"input"`
	Output []string       `json:"output"`
}

type composeCaseDump struct {
	Seed     *string  `json:"seed"`
	Proxy    *string  `json:"proxy"`
	Timezone *string  `json:"timezone"`
	Locale   *string  `json:"locale"`
	Webrtc   string   `json:"webrtc"`
	Output   []string `json:"output"`
}

// goldenDump fixes the top-level key order of golden.json. It intentionally
// differs from goldenFile (the reader), whose field order is irrelevant to
// unmarshaling.
type goldenDump struct {
	ExitIPStub         string            `json:"exit_ip_stub"`
	CountryLocaleMap   localeMapDump     `json:"country_locale_map"`
	DefaultStealthArgs []stealthCaseDump `json:"default_stealth_args"`
	EnsureProxyScheme  []ioCaseDump      `json:"ensure_proxy_scheme"`
	NormalizeSocks     []ioCaseDump      `json:"normalize_socks"`
	SplitProxyAuth     []splitCaseDump   `json:"split_proxy_auth"`
	ForkParityArgs     []forkCaseDump    `json:"fork_parity_args"`
	ResolveWebrtc      []webrtcCaseDump  `json:"resolve_webrtc"`
	BuildArgs          []buildCaseDump   `json:"build_args"`
	ComposeArgv        []composeCaseDump `json:"compose_argv"`
}

// TestGolden regenerates testdata/golden.json from the Go primitives when run
// with -update, and otherwise asserts the committed file still matches what the
// generator produces (a regression snapshot of the ported primitives).
func TestGolden(t *testing.T) {
	data := buildGolden(t)

	if *update {
		if err := os.WriteFile("testdata/golden.json", data, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}

	committed, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(data, committed) {
		t.Errorf("golden.json is stale; run `go test ./internal/fingerprint -run TestGolden -update`")
	}
}

func buildGolden(t *testing.T) []byte {
	t.Helper()
	t.Setenv(BinaryPathEnv, "/opt/clark/chrome")

	dump := goldenDump{
		ExitIPStub:         exitIPStub,
		DefaultStealthArgs: dumpDefaultStealthArgs(),
		EnsureProxyScheme:  dumpEnsureProxyScheme(),
		NormalizeSocks:     dumpNormalizeSocks(),
		SplitProxyAuth:     dumpSplitProxyAuth(),
		ForkParityArgs:     dumpForkParityArgs(),
		ResolveWebrtc:      dumpResolveWebrtc(),
	}

	pinLinux(t)
	dump.BuildArgs = dumpBuildArgs()
	dump.ComposeArgv = dumpComposeArgv()

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(dump); err != nil {
		t.Fatalf("encode golden: %v", err)
	}
	return buf.Bytes()
}

func dumpDefaultStealthArgs() []stealthCaseDump {
	systems := []string{"Linux", "Darwin", "Windows"}
	out := make([]stealthCaseDump, len(systems))
	for i, sys := range systems {
		out[i] = stealthCaseDump{System: sys, Seed: pinnedSeed, Output: stealthArgsFor(sys, pinnedSeed)}
	}
	return out
}

func stealthArgsFor(system string, seed int) []string {
	origSystem, origSeed := systemName, seedSource
	defer func() { systemName, seedSource = origSystem, origSeed }()
	systemName = func() string { return system }
	seedSource = func() int { return seed }
	return getDefaultStealthArgs()
}

func dumpEnsureProxyScheme() []ioCaseDump {
	inputs := []string{
		"proxy.example:8080",
		"http://proxy.example:8080",
		"socks5://proxy.example:1080",
	}
	out := make([]ioCaseDump, len(inputs))
	for i, in := range inputs {
		out[i] = ioCaseDump{Input: in, Output: EnsureProxyScheme(in)}
	}
	return out
}

func dumpNormalizeSocks() []ioCaseDump {
	inputs := []string{
		"http://proxy.example:8080",
		"socks5://proxy.example:1080",
		"http://user:p%40ss@proxy.example:8080",
		"socks5://user:p%40ss@proxy.example:1080",
		"socks5://user:p@ss=word@proxy.example:1080",
		"socks5://USER:P@SS@HOST.example:1080",
		"socks5://user:@proxy.example:1080",
		"socks5://user@proxy.example:1080",
		"socks5://user:pass@[2001:db8::1]:1080",
		"socks5://proxy.example:not-a-port",
	}
	out := make([]ioCaseDump, len(inputs))
	for i, in := range inputs {
		out[i] = ioCaseDump{Input: in, Output: NormalizeSocksStringURL(in)}
	}
	return out
}

func dumpSplitProxyAuth() []splitCaseDump {
	inputs := []string{
		"http://bob:secret@proxy.example:8080",
		"http://bob:secret@Proxy.EXAMPLE:8080",
		"http://user%40host:p%40ss@host.example:8080",
		"https://u:p@[2001:db8::1]:8443",
		"http://bob@proxy.example:8080",
		"http://user:@proxy.example:8080",
		"http://proxy.example:8080",
		"socks5://user:pass@proxy.example:1080",
	}
	out := make([]splitCaseDump, len(inputs))
	for i, in := range inputs {
		server, user, pass := SplitProxyAuth(in)
		out[i] = splitCaseDump{Input: in, Server: server, Username: user, Password: pass}
	}
	return out
}

func dumpForkParityArgs() []forkCaseDump {
	cases := []struct {
		locale string
		proxy  *string
	}{
		{"", nil},
		{"en-US", nil},
		{"de-DE", new("http://p:1")},
		{"fr", new("socks5://p:1")},
		{"", new("http://p:1")},
	}
	out := make([]forkCaseDump, len(cases))
	for i, c := range cases {
		out[i] = forkCaseDump{Locale: c.locale, Proxy: c.proxy, Output: ForkParityArgs(c.locale, deref(c.proxy))}
	}
	return out
}

func dumpResolveWebrtc() []webrtcCaseDump {
	cases := []struct {
		args   []string
		proxy  *string
		exitIP *string
	}{
		{[]string{"--fingerprint=1", "--no-sandbox"}, new("http://proxy.example:8080"), new(exitIPStub)},
		{[]string{"--fingerprint-webrtc-ip=auto"}, new("http://proxy.example:8080"), new(exitIPStub)},
		{[]string{"--fingerprint-webrtc-ip=auto"}, new("socks5://proxy.example:1080"), new(exitIPStub)},
		{[]string{"--fingerprint-webrtc-ip=auto"}, nil, new(exitIPStub)},
		{[]string{"--x", "--fingerprint-webrtc-ip=auto"}, new("http://proxy.example:8080"), nil},
	}
	out := make([]webrtcCaseDump, len(cases))
	for i, c := range cases {
		exitIP := deref(c.exitIP)
		got := ResolveWebRTCArgs(slices.Clone(c.args), deref(c.proxy), func(string) string { return exitIP })
		out[i] = webrtcCaseDump{InputArgs: c.args, Proxy: c.proxy, ExitIP: c.exitIP, Output: got}
	}
	return out
}

func dumpBuildArgs() []buildCaseDump {
	inputs := []struct {
		name  string
		input buildInputDump
	}{
		{"headed-adds-gpu-flag", buildInputDump{StealthArgs: true, ExtraArgs: []string{"--fingerprint=1"}, Headless: new(false)}},
		{"timezone-only", buildInputDump{StealthArgs: true, Timezone: "Europe/Berlin"}},
		{"locale-only", buildInputDump{StealthArgs: true, Locale: "de-DE"}},
		{"no-stealth", buildInputDump{StealthArgs: false, ExtraArgs: []string{"--foo=bar"}, Timezone: "UTC", Locale: "en-US"}},
		{"override-fingerprint", buildInputDump{StealthArgs: true, ExtraArgs: []string{"--fingerprint=99999", "--proxy-server=http://h:1"}}},
		{"extensions", buildInputDump{StealthArgs: true, ExtraArgs: []string{"--fingerprint=1"}, ExtensionPaths: []string{"/opt/ext/a", "/opt/ext/b"}}},
		{"start-maximized", buildInputDump{StealthArgs: true, ExtraArgs: []string{"--fingerprint=1"}, StartMaximized: new(true)}},
		{"start-maximized-suppressed", buildInputDump{StealthArgs: true, ExtraArgs: []string{"--fingerprint=1", "--window-size=800,600"}, StartMaximized: new(true)}},
	}
	out := make([]buildCaseDump, len(inputs))
	for i, c := range inputs {
		out[i] = buildCaseDump{Name: c.name, Input: c.input, Output: BuildArgs(buildArgsInputFrom(c.input))}
	}
	return out
}

// buildArgsInputFrom mirrors TestBuildArgsParity: Headless defaults to true.
func buildArgsInputFrom(d buildInputDump) BuildArgsInput {
	in := BuildArgsInput{
		StealthArgs:    d.StealthArgs,
		ExtraArgs:      d.ExtraArgs,
		Timezone:       d.Timezone,
		Locale:         d.Locale,
		ExtensionPaths: d.ExtensionPaths,
		Headless:       true,
	}
	if d.Headless != nil {
		in.Headless = *d.Headless
	}
	if d.StartMaximized != nil {
		in.StartMaximized = *d.StartMaximized
	}
	return in
}

func dumpComposeArgv() []composeCaseDump {
	seeds := []*string{nil, new("12345")}
	proxies := []*string{
		nil,
		new("http://proxy.example:8080"),
		new("socks5://proxy.example:1080"),
		new("http://user:p%40ss@proxy.example:8080"),
		new("socks5://user:p%40ss@proxy.example:1080"),
		new("socks5://user:p@ss=word@proxy.example:1080"),
	}
	tzLocales := []struct{ tz, locale *string }{
		{nil, nil},
		{new("America/New_York"), new("en-US")},
	}
	webrtcs := []string{"none", "auto"}
	stub := func(string) string { return exitIPStub }

	out := make([]composeCaseDump, 0, len(seeds)*len(proxies)*len(tzLocales)*len(webrtcs))
	for _, seed := range seeds {
		for _, proxy := range proxies {
			for _, tl := range tzLocales {
				for _, webrtc := range webrtcs {
					got := composeArgv(seed, proxy, deref(tl.tz), deref(tl.locale), webrtc, stub)
					out = append(out, composeCaseDump{
						Seed:     seed,
						Proxy:    proxy,
						Timezone: tl.tz,
						Locale:   tl.locale,
						Webrtc:   webrtc,
						Output:   got,
					})
				}
			}
		}
	}
	return out
}
