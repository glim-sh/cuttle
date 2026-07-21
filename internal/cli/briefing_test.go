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
			{driver: drivers[0], version: "0.31.1"}, // agent-browser installed
		},
	})
	out := sb.String()

	wantContains := []string{
		"cuttle ready  (container 'cuttle', image ghcr.io/glim-sh/cuttle:latest)  cuttle 0.3.0",
		"CDP     http://127.0.0.1:9222  (Chrome/148)",
		"viewer  http://127.0.0.1:6080/",
		"agent-browser  0.31.1",
		"attach  agent-browser --cdp 9222 <cmd>",
		"browser-use  not installed   (install: uv tool install browser-use)",
		"playwright-cli  not installed",
		"login walls / captcha: `cuttle login <url>`",
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
	for _, d := range drivers {
		if !strings.Contains(out, d.install) {
			t.Fatalf("missing install hint %q", d.install)
		}
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
	cdp, viewer := endpointURLs(ep, false)
	if cdp != "http://127.0.0.1:9222" || viewer != "http://127.0.0.1:6080/" {
		t.Fatalf("urls: %q %q", cdp, viewer)
	}
	if _, viewer := endpointURLs(ep, true); viewer != "" {
		t.Fatalf("no-vnc should suppress viewer, got %q", viewer)
	}
}

func TestPrintBriefingUsesResolvedContext(t *testing.T) {
	var sb strings.Builder
	ep := backend.Endpoint{CDPHost: "127.0.0.1", CDPPort: 9222, VNCHost: "127.0.0.1", VNCPort: 6080}
	// cf.contextName is empty (context came from default_context); the label must
	// render the resolved name, not the raw flag (bug 3: `context ''`).
	printBriefingFor(&sb, "ready", "cuttle", "box", config.Context{Backend: config.BackendK8s}, commonFlags{}, ep, "Chrome/1", "", false)
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
		t.Fatalf("remote label: %q", got)
	}
}
