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
	verb      string
	location  string // e.g. "container 'cuttle'" or "context 'cluster'"
	imageTail string // ", image X" or ""
	version   string
	cdpURL    string
	viewerURL string // "" = no viewer
	engine    string // browser string, "" = unknown
	cdpPort   int
	drivers   []detectedDriver
	secrets   []string // secret NAMES the session holds; never a value
}

func renderBriefing(w io.Writer, b briefing) {
	engine := ""
	if b.engine != "" {
		engine = "  (" + b.engine + ")"
	}
	fmt.Fprintf(w, "cuttle %s  (%s%s)  cuttle %s\n", b.verb, b.location, b.imageTail, b.version)
	fmt.Fprintf(w, "  CDP     %s%s\n", b.cdpURL, engine)
	if b.viewerURL != "" {
		fmt.Fprintf(w, "  viewer  %s\n", b.viewerURL)
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
		for _, d := range orderedDrivers() {
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
		fmt.Fprintln(w, "  default: all three; minimal: just playwright-cli (the default driver).")
		for _, d := range orderedDrivers() {
			fmt.Fprintf(w, "    %s\n", d.install)
		}
		fmt.Fprintln(w, "  (drivers attach to cuttle's browser - skip their own browser downloads)")
	}
	if len(b.secrets) > 0 {
		// browser-use's insight: a substitution mechanism the model is never told
		// about does not get used, so the names ride the briefing. Names only.
		fmt.Fprintf(w, "secrets held: %s\n", strings.Join(b.secrets, ", "))
		fmt.Fprintln(w, "  type {{cuttle:NAME}} as the WHOLE value in a fill - cuttle substitutes it inside")
		fmt.Fprintln(w, "  the CDP frame, so the value never reaches your context. NEVER type the value.")
	}
	if b.viewerURL != "" {
		fmt.Fprintln(w, "login walls / captcha: `cuttle open <url>`, then hand the user the viewer")
		fmt.Fprintln(w, "  link to sign in or solve it - the CDP session stays logged in.")
	}
	// The one failure that reads as a broken selector rather than a blocked page,
	// so it is worth the two lines here rather than only in the full guide.
	fmt.Fprintln(w, "page gone quiet? a native dialog (alert/confirm/\"Leave site?\") pauses it -")
	fmt.Fprintln(w, "  clear it with your driver's dialog-accept (proceeds) / dialog-dismiss (stays);")
	fmt.Fprintln(w, "  `cuttle logs` names what a click actually landed on.")
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
