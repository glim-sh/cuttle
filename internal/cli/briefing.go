package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"
)

// briefing is the single dynamic source of truth an agent needs to drive cuttle:
// live state plus installed drivers with attach lines and their own self-doc
// commands. cuttle carries no driver docs of its own.
type briefing struct {
	verb       string
	location   string // e.g. "container 'cuttle'" or "context 'cluster'"
	imageTail  string // ", image X" or ""
	version    string
	cdpURL     string
	viewerURL  string // "" = no viewer
	windowMode bool   // native: browser is a real desktop window, no viewer URL
	engine     string // browser string, "" = unknown
	cdpPort    int
	drivers    []detectedDriver
}

func renderBriefing(w io.Writer, b briefing) {
	engine := ""
	if b.engine != "" {
		engine = "  (" + b.engine + ")"
	}
	fmt.Fprintf(w, "cuttle %s  (%s%s)  cuttle %s\n", b.verb, b.location, b.imageTail, b.version)
	fmt.Fprintf(w, "  CDP     %s%s\n", b.cdpURL, engine)
	switch {
	case b.viewerURL != "":
		fmt.Fprintf(w, "  viewer  %s\n", b.viewerURL)
	case b.windowMode:
		fmt.Fprintln(w, "  window  the browser is a real window on your desktop - `cuttle view` raises it")
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Attach to THIS browser over CDP. NEVER launch your own browser or create a")
	fmt.Fprintln(w, "new profile/context: logins live in this one and persist across down/up.")
	fmt.Fprintln(w)

	if len(b.drivers) > 0 {
		fmt.Fprintln(w, "drivers (listed in priority order; the first is the default):")
		for _, d := range b.drivers {
			line := "  " + d.name
			if d.version != "" {
				line += "  " + d.version
			}
			fmt.Fprintln(w, line)
			fmt.Fprintf(w, "    attach  %s\n", formatAttach(d.attach, b.cdpURL, b.cdpPort))
			fmt.Fprintf(w, "    docs    %s\n", d.docs)
		}
		for _, d := range drivers {
			if !driverInstalled(b.drivers, d.name) {
				fmt.Fprintf(w, "  %s  not installed   (install: %s)\n", d.name, d.install)
			}
		}
		fmt.Fprintln(w, "routing: use the first driver listed above unless the user names another")
		fmt.Fprintln(w, "  (bu / bu-cli / browseruse = browser-use). If the named driver is not")
		fmt.Fprintln(w, "  installed, use the first listed instead and tell the user you fell back.")
		fmt.Fprintln(w, "docs: fetch each driver's own instructions with the `docs` command above -")
		fmt.Fprintln(w, "  they match the installed version; do not rely on memory or stale copies.")
	} else {
		fmt.Fprintln(w, "drivers: none installed. STOP and ask the user what to install -")
		fmt.Fprintln(w, "  default: all three; minimal: just agent-browser (the default driver).")
		for _, d := range drivers {
			fmt.Fprintf(w, "    %s\n", d.install)
		}
		fmt.Fprintln(w, "  (drivers attach to cuttle's browser - skip their own browser downloads)")
	}
	switch {
	case b.viewerURL != "":
		fmt.Fprintln(w, "login walls / captcha: `cuttle login <url>`, then hand the user the viewer")
		fmt.Fprintln(w, "  link to sign in or solve it - the CDP session stays logged in.")
	case b.windowMode:
		fmt.Fprintln(w, "login walls / captcha: `cuttle login <url>` opens the browser window on your")
		fmt.Fprintln(w, "  desktop; sign in or solve it there - the CDP session stays logged in.")
	}
	fmt.Fprintln(w, "full cuttle guide: `cuttle skill`  (prints the complete guide, always")
	fmt.Fprintf(w, "  matching this CLI %s; skip if you already loaded it this session)\n", b.version)
}

func formatAttach(tmpl, cdpURL string, port int) string {
	r := strings.NewReplacer("{cdp}", cdpURL, "{port}", strconv.Itoa(port))
	return r.Replace(tmpl)
}

func driverInstalled(installed []detectedDriver, name string) bool {
	for _, d := range installed {
		if d.name == name {
			return true
		}
	}
	return false
}
