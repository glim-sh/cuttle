package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleTOML = `
default_context = "cluster"

[context.cluster]
backend = "k8s"
namespace = "browser"
release = "cuttle"
node_selector = { "glim.sh/browser" = "true" }
proxy = "http://user:pass@proxy.example:8080"

[context.box]
backend = "ssh"
host = "user@box.example"

[context.tailnet]
backend = "direct"
cdp_url = "http://cuttle.example:9222"
vnc_url = "http://cuttle.example:6080"

[profile.default]
storage = "local"

[profile.work]
storage = "remote"
`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadFromMissingFileYieldsBuiltinLocal(t *testing.T) {
	cfg, err := LoadFrom(filepath.Join(t.TempDir(), "does-not-exist.toml"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	name, ctx, err := cfg.Active("", "")
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if name != BackendLocal || ctx.Backend != BackendLocal {
		t.Fatalf("want built-in local, got name=%q backend=%q", name, ctx.Backend)
	}
}

func TestActivePrecedence(t *testing.T) {
	cfg, err := LoadFrom(writeConfig(t, sampleTOML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	tests := []struct {
		name string
		flag string
		env  string
		want string
	}{
		{"flag wins over env and default", "box", "tailnet", "box"},
		{"env wins over default", "", "box", "box"},
		{"default_context used when no flag/env", "", "", "cluster"},
		{"flag over default", "tailnet", "", "tailnet"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _, err := cfg.Active(tt.flag, tt.env)
			if err != nil {
				t.Fatalf("Active: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestActiveBuiltinLocalWhenNoDefault(t *testing.T) {
	cfg, err := LoadFrom(writeConfig(t, "[context.box]\nbackend = \"ssh\"\nhost = \"h\"\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got, ctx, err := cfg.Active("", "")
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if got != BackendLocal || ctx.Backend != BackendLocal {
		t.Fatalf("want built-in local, got %q/%q", got, ctx.Backend)
	}
}

func TestActiveUnknownContextErrors(t *testing.T) {
	cfg, err := LoadFrom(writeConfig(t, sampleTOML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, _, err := cfg.Active("typo", ""); err == nil {
		t.Fatal("expected error for unknown context")
	}
}

func TestStrictModeRejectsUnknownKey(t *testing.T) {
	_, err := LoadFrom(writeConfig(t, "defaultcontext = \"x\"\n")) // typo: should be default_context
	if err == nil {
		t.Fatal("expected strict-mode error for unknown key")
	}
}

func TestParsedFields(t *testing.T) {
	cfg, err := LoadFrom(writeConfig(t, sampleTOML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cluster := cfg.Contexts["cluster"]
	if cluster.Namespace != "browser" || cluster.Release != "cuttle" {
		t.Fatalf("cluster fields: %+v", cluster)
	}
	if cluster.NodeSelector["glim.sh/browser"] != "true" {
		t.Fatalf("node_selector not parsed: %+v", cluster.NodeSelector)
	}
	if cfg.Contexts["tailnet"].CDPURL != "http://cuttle.example:9222" {
		t.Fatalf("direct cdp_url: %q", cfg.Contexts["tailnet"].CDPURL)
	}
	if cfg.Profiles["work"].Storage != StorageRemote {
		t.Fatalf("profile storage: %+v", cfg.Profiles["work"])
	}
}

func TestNamesLocalFirstThenSorted(t *testing.T) {
	cfg, err := LoadFrom(writeConfig(t, sampleTOML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got := cfg.Names()
	want := []string{"local", "box", "cluster", "tailnet"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func TestSaveRoundTripsAndOmitsBuiltinLocal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.toml") // dir does not exist yet
	cfg := &Config{
		DefaultContext: "box",
		Contexts: map[string]Context{
			BackendLocal: {Backend: BackendLocal}, // built-in, must not persist
			"box":        {Backend: BackendSSH, Host: "user@box.example", Proxy: "http://p:1"},
		},
	}
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	got := string(data)
	for _, want := range []string{"default_context", "box", "[context.box]", "user@box.example", "http://p:1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	// The built-in local stanza is not written, and omitempty keeps empty keys out.
	for _, unwanted := range []string{"[context.local]", `namespace = ""`, `cdp_url = ""`} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("did not expect %q in:\n%s", unwanted, got)
		}
	}
	reloaded, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	name, ctx, err := reloaded.Active("", "")
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if name != "box" || ctx.Host != "user@box.example" || ctx.Proxy != "http://p:1" {
		t.Fatalf("round-trip lost data: name=%q ctx=%+v", name, ctx)
	}
}
