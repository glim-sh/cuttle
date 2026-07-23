package cli

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// driver is a CDP driver CLI cuttle knows how to route agents to. cuttle never
// bakes in driver documentation - each driver self-documents at runtime (docs),
// so instructions always match the installed version. The briefing only carries
// the attach incantation and where the docs live.
type driver struct {
	name    string
	attach  string // {cdp} = http endpoint, {port} = CDP port
	docs    string
	install string
	// nil = never probe: browser-use treats unknown argv as harness input and
	// would launch its daemon from a mere version check.
	versionArgs []string
}

// Driver executable names, used as registry keys, rank entries, and the drivers'
// own name field - one constant each so the three never drift.
const (
	driverAgentBrowser = "agent-browser"
	driverBrowserUse   = "browser-use"
	driverPlaywright   = "playwright-cli"
)

// drivers is the registry of supported driver CLIs, keyed by executable name.
// Declaration order carries NO meaning - priority lives in driverRank.
var drivers = map[string]driver{
	driverAgentBrowser: {
		name:        driverAgentBrowser,
		attach:      "agent-browser --cdp {port} <cmd>   # --cdp on EVERY command; never `connect`",
		docs:        "agent-browser skills get core --full",
		install:     "npm install -g agent-browser",
		versionArgs: []string{"--version"},
	},
	driverBrowserUse: {
		name:        driverBrowserUse,
		attach:      "BU_CDP_URL={cdp} browser-use <<'PY' ... PY",
		docs:        "browser-use skill show",
		install:     "uv tool install browser-use",
		versionArgs: nil,
	},
	driverPlaywright: {
		name:        driverPlaywright,
		attach:      "playwright-cli attach --cdp={cdp}",
		docs:        "playwright-cli --help   # its 'Agent skill:' line -> full SKILL.md + references/",
		install:     "npm install -g @playwright/cli",
		versionArgs: []string{"--version"},
	},
}

// driverRank is the single place driver priority is expressed, highest first: the
// first INSTALLED driver is the default, and the briefing lists drivers in this
// order. Change the default by reordering these names - the registry never moves,
// and TestDriverRankMatchesRegistry keeps this exhaustive and in sync.
var driverRank = []string{driverPlaywright, driverAgentBrowser, driverBrowserUse}

// orderedDrivers returns the registry in driverRank (priority) order.
func orderedDrivers() []driver {
	out := make([]driver, 0, len(driverRank))
	for _, name := range driverRank {
		if d, ok := drivers[name]; ok {
			out = append(out, d)
		}
	}
	return out
}

type detectedDriver struct {
	driver
	version string // "" when unknown or not probed
}

// detectDrivers returns the installed drivers in briefing order, each with its
// version when cheaply knowable. Version probes run in parallel so the briefing
// stays fast; a probe failure degrades to a versionless line, never an error.
func detectDrivers() []detectedDriver {
	type found struct {
		d   driver
		exe string
	}
	var installed []found
	for _, d := range orderedDrivers() {
		if exe, err := exec.LookPath(d.name); err == nil {
			installed = append(installed, found{d, exe})
		}
	}
	if len(installed) == 0 {
		return nil
	}

	versions := make([]string, len(installed))
	var wg sync.WaitGroup
	for i, f := range installed {
		if f.d.versionArgs == nil {
			continue
		}
		wg.Go(func() {
			versions[i] = driverVersion(f.d.name, f.exe, f.d.versionArgs)
		})
	}
	wg.Wait()

	out := make([]detectedDriver, len(installed))
	for i, f := range installed {
		out[i] = detectedDriver{driver: f.d, version: versions[i]}
	}
	return out
}

func driverVersion(name, exe string, versionArgs []string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, versionArgs...)
	cmd.Env = append(os.Environ(), "NO_UPDATE_NOTIFIER=1")
	out, err := cmd.CombinedOutput()
	if err != nil && len(out) == 0 {
		return ""
	}
	first := ""
	for line := range strings.SplitSeq(string(out), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			first = s
			break
		}
	}
	// Some drivers echo their own name ("agent-browser 0.31.1"); the briefing
	// already prints the name, so keep just the version.
	v := strings.TrimSpace(strings.TrimPrefix(first, name))
	if len(v) > 40 {
		v = v[:40]
	}
	return v
}
