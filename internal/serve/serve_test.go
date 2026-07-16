package serve

import (
	"testing"
	"time"
)

func TestParseIdleTimeout(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"zero", "0", 0, false},
		{"disabled-word", "disabled", 0, false},
		{"off", "off", 0, false},
		{"none", "none", 0, false},
		{"seconds", "30", 30 * time.Second, false},
		{"fractional", "1.5", 1500 * time.Millisecond, false},
		{"negative", "-1", 0, true},
		{"garbage", "soon", 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseIdleTimeout(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if err == nil && got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestParseCLIArgs(t *testing.T) {
	t.Setenv(proxyEnv, "")
	t.Setenv(ephemeralEnv, "")
	t.Setenv(idleTimeoutEnv, "")
	t.Setenv("HOME", "/home/tester")

	cfg, passthrough, err := parseCLIArgs([]string{
		"--port=9333",
		"--data-dir=/data",
		"--idle-timeout=45",
		"--keep-profile",
		"--ephemeral",
		"--proxy=http://user:pass@proxy.example:8080",
		"--fingerprint=abc",
		"--fingerprint-locale=en-GB",
		"--fingerprint-timezone=Europe/London",
		"--headless=false",
		"--remote-debugging-port=1", // consumed, stripped
		"--some-chrome-flag",
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if cfg.port != 9333 {
		t.Errorf("port=%d want 9333", cfg.port)
	}
	if cfg.dataDir != "/data" {
		t.Errorf("dataDir=%q", cfg.dataDir)
	}
	if cfg.idleTimeout != 45*time.Second {
		t.Errorf("idleTimeout=%v", cfg.idleTimeout)
	}
	if !cfg.keepProfile || !cfg.ephemeral {
		t.Errorf("keepProfile=%v ephemeral=%v", cfg.keepProfile, cfg.ephemeral)
	}
	if cfg.proxy != "http://user:pass@proxy.example:8080" {
		t.Errorf("proxy=%q", cfg.proxy)
	}
	if cfg.defaultSeed != "abc" || cfg.defaultLocale != "en-GB" || cfg.defaultTimezone != "Europe/London" {
		t.Errorf("fingerprint defaults: %q %q %q", cfg.defaultSeed, cfg.defaultLocale, cfg.defaultTimezone)
	}
	if cfg.headless {
		t.Errorf("headless should be false")
	}
	// --headless=false and the unknown chrome flag pass through; consumed flags do not.
	wantPass := []string{"--headless=false", "--some-chrome-flag"}
	if len(passthrough) != len(wantPass) {
		t.Fatalf("passthrough=%v want %v", passthrough, wantPass)
	}
	for i := range wantPass {
		if passthrough[i] != wantPass[i] {
			t.Fatalf("passthrough=%v want %v", passthrough, wantPass)
		}
	}
}

func TestParseCLIArgsEnvDefaults(t *testing.T) {
	t.Setenv(proxyEnv, "http://env-proxy:3128")
	t.Setenv(ephemeralEnv, "true")
	t.Setenv(idleTimeoutEnv, "60")
	t.Setenv("HOME", "/home/tester")

	cfg, _, err := parseCLIArgs(nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if cfg.proxy != "http://env-proxy:3128" {
		t.Errorf("proxy from env=%q", cfg.proxy)
	}
	if !cfg.ephemeral {
		t.Errorf("ephemeral from env not set")
	}
	if cfg.idleTimeout != 60*time.Second {
		t.Errorf("idleTimeout from env=%v", cfg.idleTimeout)
	}
	// A CLI flag overrides the env default.
	cfg2, _, _ := parseCLIArgs([]string{"--proxy=http://cli-proxy:8888"})
	if cfg2.proxy != "http://cli-proxy:8888" {
		t.Errorf("cli proxy override=%q", cfg2.proxy)
	}
}

func TestValidSeed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		seed string
		want bool
	}{
		{"abc123", true},
		{"seed_with-dashes", true},
		{"__default__", false},
		{"", false},
		{"has space", false},
		{"has/slash", false},
		{"has.dot", false},
	}
	for _, tc := range tests {
		if got := validSeed(tc.seed); got != tc.want {
			t.Errorf("validSeed(%q)=%v want %v", tc.seed, got, tc.want)
		}
	}
}

func TestBindHost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		probe envProbe
		want  string
	}{
		{
			name: "env override wins",
			probe: envProbe{
				getenv: func(k string) string {
					if k == hostEnv {
						return "10.0.0.5"
					}
					return ""
				},
				stat:     func(string) bool { return true },
				readFile: func(string) ([]byte, error) { return nil, nil },
			},
			want: "10.0.0.5",
		},
		{
			name: "dockerenv marker -> 0.0.0.0",
			probe: envProbe{
				getenv:   func(string) string { return "" },
				stat:     func(p string) bool { return p == "/.dockerenv" },
				readFile: func(string) ([]byte, error) { return nil, errFakeNoFile },
			},
			want: "0.0.0.0",
		},
		{
			name: "kubernetes env -> 0.0.0.0",
			probe: envProbe{
				getenv: func(k string) string {
					if k == "KUBERNETES_SERVICE_HOST" {
						return "10.96.0.1"
					}
					return ""
				},
				stat:     func(string) bool { return false },
				readFile: func(string) ([]byte, error) { return nil, errFakeNoFile },
			},
			want: "0.0.0.0",
		},
		{
			name: "containerd cgroup (no marker files) -> 0.0.0.0",
			probe: envProbe{
				getenv:   func(string) string { return "" },
				stat:     func(string) bool { return false },
				readFile: func(string) ([]byte, error) { return []byte("0::/kubepods/pod123/abc"), nil },
			},
			want: "0.0.0.0",
		},
		{
			name: "bare metal -> loopback",
			probe: envProbe{
				getenv:   func(string) string { return "" },
				stat:     func(string) bool { return false },
				readFile: func(string) ([]byte, error) { return []byte("0::/user.slice/session.scope"), nil },
			},
			want: "127.0.0.1",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := bindHost(tc.probe); got != tc.want {
				t.Errorf("bindHost=%q want %q", got, tc.want)
			}
		})
	}
}

func TestDefaultDataDir(t *testing.T) {
	t.Parallel()
	container := envProbe{
		getenv:   func(string) string { return "" },
		stat:     func(p string) bool { return p == "/.dockerenv" },
		readFile: func(string) ([]byte, error) { return nil, errFakeNoFile },
	}
	if got := defaultDataDir(container); got != "/tmp/cloakserve" {
		t.Errorf("container dataDir=%q", got)
	}
	bare := envProbe{
		getenv:   func(string) string { return "" },
		stat:     func(string) bool { return false },
		readFile: func(string) ([]byte, error) { return nil, errFakeNoFile },
		homeDir:  func() (string, error) { return "/home/tester", nil },
	}
	if got := defaultDataDir(bare); got != "/home/tester/.cloakbrowser/cloakserve" {
		t.Errorf("bare dataDir=%q", got)
	}
}
