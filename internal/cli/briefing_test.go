package cli

import (
	"strings"
	"testing"

	"github.com/glim-sh/cuttle/internal/backend"
	"github.com/glim-sh/cuttle/internal/config"
)

func TestRenderBriefingWithDrivers(t *testing.T) {
	var sb strings.Builder
	renderBriefing(&sb, briefing{
		verb:      "ready",
		location:  "container 'cuttle'",
		imageTail: ", image ghcr.io/glim-sh/cuttle:latest",
		version:   "0.3.0",
		cdpURL:    "http://127.0.0.1:9222",
		viewerURL: "http://127.0.0.1:6080/",
		engine:    "Chrome/148",
		cdpPort:   9222,
		drivers: []detectedDriver{
			{driver: drivers["playwright-cli"], version: "0.31.1"}, // the default driver, installed
		},
	})
	out := sb.String()

	wantContains := []string{
		"cuttle ready  (container 'cuttle', image ghcr.io/glim-sh/cuttle:latest)  cuttle 0.3.0",
		"CDP     http://127.0.0.1:9222  (Chrome/148)",
		"viewer  http://127.0.0.1:6080/",
		"playwright-cli  0.31.1",
		"attach  playwright-cli attach --cdp=http://127.0.0.1:9222",
		"agent-browser  not installed   (install: npm install -g agent-browser)",
		"browser-use  not installed   (install: uv tool install browser-use)",
		"login walls / captcha: `cuttle open <url>`",
	}
	for _, w := range wantContains {
		if !strings.Contains(out, w) {
			t.Fatalf("briefing missing %q\n---\n%s", w, out)
		}
	}
}

func TestRenderBriefingNoDrivers(t *testing.T) {
	var sb strings.Builder
	renderBriefing(&sb, briefing{
		verb: "ready", location: "context 'cluster'", version: "0.3.0",
		cdpURL: "http://127.0.0.1:40001", cdpPort: 40001,
	})
	out := sb.String()
	if !strings.Contains(out, "drivers: none installed. STOP and ask the user") {
		t.Fatalf("expected none-installed branch:\n%s", out)
	}
	// no viewer line, and no login-wall hint when there is no viewer
	if strings.Contains(out, "viewer  ") || strings.Contains(out, "login walls") {
		t.Fatalf("viewerless briefing should omit viewer/login hints:\n%s", out)
	}
	for _, d := range orderedDrivers() {
		if !strings.Contains(out, d.install) {
			t.Fatalf("missing install hint %q", d.install)
		}
	}
}

// TestDriverRankMatchesRegistry keeps priority (driverRank) exhaustive and in sync
// with the registry, so a driver can never be silently unranked or ranked twice.
func TestDriverRankMatchesRegistry(t *testing.T) {
	if len(driverRank) != len(drivers) {
		t.Fatalf("driverRank ranks %d drivers, registry has %d - every driver must be ranked exactly once", len(driverRank), len(drivers))
	}
	seen := map[string]bool{}
	for _, name := range driverRank {
		if _, ok := drivers[name]; !ok {
			t.Fatalf("driverRank names %q, which is not in the registry", name)
		}
		if seen[name] {
			t.Fatalf("driverRank lists %q twice", name)
		}
		seen[name] = true
	}
}

func TestFormatAttach(t *testing.T) {
	tests := []struct {
		tmpl string
		want string
	}{
		{"agent-browser --cdp {port} <cmd>", "agent-browser --cdp 8080 <cmd>"},
		{"BU_CDP_URL={cdp} browser-use", "BU_CDP_URL=http://127.0.0.1:8080 browser-use"},
		{"playwright-cli attach --cdp={cdp}", "playwright-cli attach --cdp=http://127.0.0.1:8080"},
	}
	for _, tt := range tests {
		if got := formatAttach(tt.tmpl, "http://127.0.0.1:8080", 8080); got != tt.want {
			t.Fatalf("formatAttach(%q)=%q want %q", tt.tmpl, got, tt.want)
		}
	}
}

func TestBoolFlagOptional(t *testing.T) {
	var b boolFlag
	if b.value() != nil {
		t.Fatal("unset boolFlag should be nil")
	}
	if err := b.Set("true"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if v := b.value(); v == nil || !*v {
		t.Fatalf("want true, got %v", v)
	}
}

func TestEndpointURLs(t *testing.T) {
	ep := backend.Endpoint{CDPHost: "127.0.0.1", CDPPort: 9222, VNCHost: "127.0.0.1", VNCPort: 6080}
	cdp, viewer := endpointURLs(ep)
	if cdp != "http://127.0.0.1:9222" || viewer != "http://127.0.0.1:6080/" {
		t.Fatalf("urls: %q %q", cdp, viewer)
	}
	// A backend with no viewer port (VNCPort 0) suppresses the viewer URL.
	if _, viewer := endpointURLs(backend.Endpoint{CDPHost: "127.0.0.1", CDPPort: 9222}); viewer != "" {
		t.Fatalf("no viewer port should suppress viewer, got %q", viewer)
	}
}

func TestPrintBriefingUsesResolvedContext(t *testing.T) {
	var sb strings.Builder
	ep := backend.Endpoint{CDPHost: "127.0.0.1", CDPPort: 9222, VNCHost: "127.0.0.1", VNCPort: 6080}
	// cf.contextName is empty (context came from default_context); the label must
	// render the resolved name, not the raw flag (bug 3: `context ''`).
	printBriefingFor(&sb, "ready", "cuttle", "box", config.Context{Backend: config.BackendK8s}, "", ep, "Chrome/1", "", false)
	out := sb.String()
	if !strings.Contains(out, "context 'box'") {
		t.Fatalf("expected resolved context label, got:\n%s", out)
	}
	if strings.Contains(out, "context ''") {
		t.Fatalf("empty context label leaked:\n%s", out)
	}
}

func TestLocationLabel(t *testing.T) {
	if got := locationLabel("local", config.Context{Backend: config.BackendLocal}, "cuttle"); got != "container 'cuttle'" {
		t.Fatalf("local label: %q", got)
	}
	if got := locationLabel("cluster", config.Context{Backend: config.BackendK8s}, "cuttle"); got != "context 'cluster'" {
		t.Fatalf("remote default label: %q", got)
	}
	// A non-default --name on a remote context must name the instance, so a
	// message about it never reads as if it were about the whole context.
	if got := locationLabel("bl", config.Context{Backend: config.BackendSSH}, "cuttle-dltest"); got != "container 'cuttle-dltest' on context 'bl'" {
		t.Fatalf("remote named label: %q", got)
	}
}
