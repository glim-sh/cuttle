package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glim-sh/cuttle/internal/config"
)

// runContextAdd executes `context add` against a temp XDG config home and returns
// its stdout, the reloaded config, and any error.
func runContextAdd(t *testing.T, args ...string) (string, *config.Config, error) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cmd := newContextAddCmd()
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	cfg, lerr := config.Load()
	if lerr != nil {
		t.Fatalf("reload config: %v", lerr)
	}
	return out.String(), cfg, err
}

func TestContextAddNew(t *testing.T) {
	out, cfg, err := runContextAdd(t, "box", "--backend", "ssh", "--host", "user@box.example", "--proxy", "http://p:1")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if !strings.Contains(out, "added context \"box\"") {
		t.Fatalf("output: %q", out)
	}
	ctx := cfg.Contexts["box"]
	if ctx.Backend != config.BackendSSH || ctx.Host != "user@box.example" || ctx.Proxy != "http://p:1" {
		t.Fatalf("persisted context: %+v", ctx)
	}
	if cfg.DefaultContext != "" {
		t.Fatalf("default should be unset, got %q", cfg.DefaultContext)
	}
}

func TestContextAddUpdateExisting(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	// First add.
	first := newContextAddCmd()
	first.SetArgs([]string{"box", "--backend", "ssh", "--host", "old@host"})
	if err := first.Execute(); err != nil {
		t.Fatalf("first add: %v", err)
	}
	// Update the same name.
	second := newContextAddCmd()
	var out bytes.Buffer
	second.SetOut(&out)
	second.SetArgs([]string{"box", "--backend", "ssh", "--host", "new@host", "--default"})
	if err := second.Execute(); err != nil {
		t.Fatalf("update: %v", err)
	}
	if !strings.Contains(out.String(), "updated context \"box\"") {
		t.Fatalf("output: %q", out.String())
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if cfg.Contexts["box"].Host != "new@host" {
		t.Fatalf("host not updated: %+v", cfg.Contexts["box"])
	}
	if cfg.DefaultContext != "box" {
		t.Fatalf("--default not applied: %q", cfg.DefaultContext)
	}
}

func TestContextAddK8sDefaultsAndDirect(t *testing.T) {
	_, cfg, err := runContextAdd(t, "cluster", "--backend", "k8s")
	if err != nil {
		t.Fatalf("k8s add: %v", err)
	}
	// namespace/release omitted: persisted empty, backend applies its defaults.
	if c := cfg.Contexts["cluster"]; c.Backend != config.BackendK8s || c.Namespace != "" || c.Release != "" {
		t.Fatalf("k8s context: %+v", c)
	}

	_, cfg2, err := runContextAdd(t, "tailnet", "--backend", "direct", "--cdp-url", "http://cuttle.example:9222")
	if err != nil {
		t.Fatalf("direct add: %v", err)
	}
	if c := cfg2.Contexts["tailnet"]; c.CDPURL != "http://cuttle.example:9222" {
		t.Fatalf("direct context: %+v", c)
	}
}

func TestContextAddValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"reserved name local", []string{"local", "--backend", "ssh", "--host", "h"}},
		{"invalid backend", []string{"c", "--backend", "podman"}},
		{"ssh without host", []string{"c", "--backend", "ssh"}},
		{"direct without url", []string{"c", "--backend", "direct"}},
		{"ssh with k8s flag", []string{"c", "--backend", "ssh", "--host", "h", "--namespace", "x"}},
		{"k8s with host", []string{"c", "--backend", "k8s", "--host", "h"}},
		{"missing backend", []string{"c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := runContextAdd(t, tc.args...); err == nil {
				t.Fatalf("expected error for %v", tc.args)
			}
		})
	}
}

// The global --name reaches `context add` too, where it would read as setting the
// stanza's `name` yet save nothing - so it is refused, and nothing is written.
func TestContextAddRefusesName(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	withInstance(t, instanceFlags{})
	rootCmd.SetOut(&bytes.Buffer{})
	rootCmd.SetArgs([]string{"context", "add", "box", "--backend", "ssh", "--host", "h", "--name", "scraper"})
	if err := rootCmd.Execute(); !errors.Is(err, errAddWithName) {
		t.Fatalf("error = %v, want errAddWithName", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, ok := cfg.Contexts["box"]; ok {
		t.Fatalf("a refused add must not write the context: %+v", cfg.Contexts["box"])
	}
}

// TestDefaultImageNeverLatest locks the image contract: the CLI must never
// default to a floating :latest (which decoupled the CLI from its daemon and once
// resolved to an unrelated image). A release build pins to repo:<version>; a dev
// build uses the local-build tag `just build-image` produces.
func TestDefaultImageNeverLatest(t *testing.T) {
	img := defaultImage()
	if strings.HasSuffix(img, ":latest") {
		t.Fatalf("defaultImage() must never be :latest, got %q", img)
	}
	// A `go test` build carries no release ldflags, so the default is the local-
	// build tag; a release build pins to its version.
	if cliVersion() == devVersion {
		if img != localImageTag {
			t.Fatalf("dev build defaultImage() = %q, want %q", img, localImageTag)
		}
		return
	}
	if img != imageRepo+":"+cliVersion() {
		t.Fatalf("release build defaultImage() = %q, want %s:%s", img, imageRepo, cliVersion())
	}
}

// withInstance points the global instance selection at sel for one test. The
// flags live on the root command, which a test driving a single subcommand -
// or resolve directly - never goes through.
func withInstance(t *testing.T, sel instanceFlags) {
	t.Helper()
	prev := instance
	instance = sel
	t.Cleanup(func() { instance = prev })
}

func TestContainerNamePrecedence(t *testing.T) {
	pinned := config.Context{Backend: config.BackendSSH, Name: "from-config"}
	cases := []struct {
		name string
		ctx  config.Context
		flag string
		env  string
		want string
	}{
		{"flag wins over env and config", pinned, "from-flag", "from-env", "from-flag"},
		{"env wins over config", pinned, "", "from-env", "from-env"},
		{"config wins over the built-in default", pinned, "", "", "from-config"},
		{"built-in default when nothing selects", config.Context{Backend: config.BackendLocal}, "", "", defaultName},
		{"k8s is named by its context, ignoring all three", config.Context{Backend: config.BackendK8s}, "from-flag", "from-env", "cluster"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := containerName("cluster", tc.ctx, tc.flag, tc.env); got != tc.want {
				t.Fatalf("containerName = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestResolveInstanceSelection walks the same precedence through resolve, the
// one place it is decided, so the config file and the env var are exercised as
// the CLI actually reads them.
func TestResolveInstanceSelection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "cuttle"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := "default_context = \"box\"\n\n[context.box]\nbackend = \"local\"\nname = \"from-config\"\n\n[context.plain]\nbackend = \"local\"\n"
	if err := os.WriteFile(filepath.Join(home, "cuttle", "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cases := []struct {
		name        string
		sel         instanceFlags
		envContext  string
		envName     string
		wantCtx     string
		wantCtnName string
	}{
		{"config default_context and its pinned name", instanceFlags{}, "", "", "box", "from-config"},
		{"CUTTLE_CONTEXT selects the context", instanceFlags{}, "plain", "", "plain", defaultName},
		{"--context beats CUTTLE_CONTEXT", instanceFlags{contextName: "plain"}, "box", "", "plain", defaultName},
		{"CUTTLE_NAME beats the context's name", instanceFlags{}, "", "from-env", "box", "from-env"},
		{"--name beats CUTTLE_NAME", instanceFlags{name: "from-flag"}, "", "from-env", "box", "from-flag"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(config.EnvContext, tc.envContext)
			t.Setenv(config.EnvName, tc.envName)
			withInstance(t, tc.sel)
			name, ctxName, _, _, err := resolve(commonFlags{}, defaultImage())
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if ctxName != tc.wantCtx || name != tc.wantCtnName {
				t.Fatalf("resolve = context %q / container %q, want %q / %q", ctxName, name, tc.wantCtx, tc.wantCtnName)
			}
		})
	}
}

// TestInstanceFlagsAreGlobal drives the real command tree: --context/--name are
// root persistent flags, so every verb that reaches an instance honors them -
// `pw`, whose DisableFlagParsing means cobra hands it those flags unparsed,
// included. A context that cannot resolve fails in resolve, before any docker,
// ssh or network call, so seeing its name back proves the selection arrived.
func TestInstanceFlagsAreGlobal(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"pw", []string{"--context", "no-such-context", "pw", "snapshot"}},
		{"pw with --context=value", []string{"--context=no-such-context", "pw", "snapshot"}},
		{"pw with the flag after the subcommand", []string{"pw", "--context", "no-such-context", "snapshot"}},
		{"jev-browse", []string{"--context", "no-such-context", "jev-browse", "--mock", "a task"}},
		{"status", []string{"--context", "no-such-context", "status"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv(config.EnvContext, "")
			t.Setenv(config.EnvName, "")
			withInstance(t, instanceFlags{})
			var out bytes.Buffer
			rootCmd.SetOut(&out)
			rootCmd.SetErr(&out)
			rootCmd.SetArgs(tc.args)
			err := rootCmd.Execute()
			if err == nil || !strings.Contains(err.Error(), `unknown context "no-such-context"`) {
				t.Fatalf("error = %v, want the unknown-context error naming the selected context", err)
			}
		})
	}
}
