package cli

import (
	"bytes"
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
