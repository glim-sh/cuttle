package cli

import (
	"strings"
	"testing"

	"github.com/glim-sh/cuttle/internal/backend"
	"github.com/glim-sh/cuttle/internal/config"
)

func TestRenderBriefing(t *testing.T) {
	var sb strings.Builder
	renderBriefing(&sb, briefing{
		verb:      "ready",
		cuttle:    "cuttle",
		location:  "container 'cuttle'",
		imageTail: ", image ghcr.io/glim-sh/cuttle:latest",
		version:   "0.3.0",
		cdpURL:    "http://127.0.0.1:9222",
		viewerURL: "http://127.0.0.1:6080/",
		engine:    "Chrome/148",
		secrets:   []string{"GH_PASS"},
	})
	out := sb.String()

	wantContains := []string{
		"cuttle ready  (container 'cuttle', image ghcr.io/glim-sh/cuttle:latest)  cuttle 0.3.0",
		"CDP     http://127.0.0.1:9222  (Chrome/148)",
		"viewer  http://127.0.0.1:6080/",
		"driver: playwright-cli " + BundledPlaywrightCLIVersion + ", bundled in the container - nothing to install",
		"use     cuttle pw <command>   (`cuttle pw --help` lists every verb)",
		"loop    cuttle jev-browse --task \"...\"   (EXPERIMENTAL autonomous loop; exits blocked -> finish with cuttle pw)",
		"secrets held: GH_PASS",
		"`pw fill`",
		"`cuttle pw dialog-accept`",
		"login walls / captcha: `cuttle open <url>`",
	}
	for _, w := range wantContains {
		if !strings.Contains(out, w) {
			t.Fatalf("briefing missing %q\n---\n%s", w, out)
		}
	}
}

// The bundled driver is the one path the briefing routes to: nothing on this
// host is detected, suggested or offered for install.
func TestRenderBriefingRoutesOnlyToTheBundledDriver(t *testing.T) {
	var sb strings.Builder
	renderBriefing(&sb, briefing{
		verb: "ready", cuttle: "cuttle", location: "context 'cluster'", version: "0.3.0",
		cdpURL: "http://127.0.0.1:40001",
	})
	out := sb.String()
	for _, banned := range []string{"agent-browser", "browser-use", "install:", "on this host", "attach  ", "STOP"} {
		if strings.Contains(out, banned) {
			t.Fatalf("briefing mentions %q:\n%s", banned, out)
		}
	}
	// no viewer line, and no login-wall hint when there is no viewer
	if strings.Contains(out, "viewer  ") || strings.Contains(out, "login walls") {
		t.Fatalf("viewerless briefing should omit viewer/login hints:\n%s", out)
	}
	if strings.Contains(out, "secrets held") {
		t.Fatalf("a session holding no secrets should not list any:\n%s", out)
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
	printBriefingFor(&sb, "ready", "cuttle", "box", config.Context{Backend: config.BackendK8s}, ep, "Chrome/1", "", false, nil, "")
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

// The next-step commands a briefing prints must reach the instance it describes:
// an agent that follows a bare `cuttle pw` from a --name instance's briefing
// drives the default container instead.
func TestCuttleCmdReachesTheSameInstance(t *testing.T) {
	docker := config.Context{Backend: config.BackendLocal}
	cases := []struct {
		name    string
		sel     instanceFlags
		env     string
		ctxName string
		ctx     config.Context
		ctnName string
		want    string
	}{
		{"default instance", instanceFlags{}, "", "local", docker, defaultName, "cuttle"},
		{"--name", instanceFlags{name: "scraper"}, "", "local", docker, "scraper", "cuttle --name scraper"},
		{"--context and --name", instanceFlags{contextName: "box", name: "scraper"}, "", "box", config.Context{Backend: config.BackendSSH}, "scraper", "cuttle --context box --name scraper"},
		{"CUTTLE_CONTEXT is spelled out", instanceFlags{}, "box", "box", config.Context{Backend: config.BackendSSH}, defaultName, "cuttle --context box"},
		{"the context's own name needs no --name", instanceFlags{contextName: "box"}, "", "box", config.Context{Backend: config.BackendSSH, Name: "scraper"}, "scraper", "cuttle --context box"},
		{"k8s is named by its context", instanceFlags{contextName: "cluster"}, "", "cluster", config.Context{Backend: config.BackendK8s}, "cluster", "cuttle --context cluster"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setSelectorEnv(t, config.EnvContext, tc.env)
			withInstance(t, tc.sel)
			if got := cuttleCmd(tc.ctxName, tc.ctx, tc.ctnName); got != tc.want {
				t.Fatalf("cuttleCmd = %q, want %q", got, tc.want)
			}
		})
	}

	var sb strings.Builder
	withInstance(t, instanceFlags{name: "scraper"})
	ep := backend.Endpoint{CDPHost: "127.0.0.1", CDPPort: 9333, VNCHost: "127.0.0.1", VNCPort: 6099}
	printBriefingFor(&sb, "ready", "scraper", "local", docker, ep, "Chrome/1", "", false, nil, "")
	out := sb.String()
	for _, w := range []string{
		"use     cuttle --name scraper pw <command>",
		"loop    cuttle --name scraper jev-browse",
		"finish with cuttle --name scraper pw)",
		"`cuttle --name scraper pw --help` lists every verb",
		"`cuttle --name scraper pw dialog-accept`",
		"`cuttle --name scraper open <url>`",
		"`cuttle --name scraper logs`",
	} {
		if !strings.Contains(out, w) {
			t.Fatalf("briefing missing %q\n---\n%s", w, out)
		}
	}
}
