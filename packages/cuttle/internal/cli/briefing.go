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

	fmt.Fprintln(w, "drivers (listed in priority order; the first is the default):")
	// The bundled driver leads and is listed unconditionally: it ships in the
	// image, so it is always there and needs nothing installed on this host.
	fmt.Fprintf(w, "  %s  %s  (bundled in the container)\n", driverPlaywright, BundledPlaywrightCLIVersion)
	fmt.Fprintln(w, "    use     cuttle pw <command>")
	// The loop drives that same bundled driver, so it belongs to its block: an
	// agent that sees only `cuttle pw` hand-rolls what one command already does.
	fmt.Fprintln(w, "    loop    cuttle jev-browse --task \"...\"   (autonomous; exits blocked -> finish with cuttle pw)")
	for _, d := range b.drivers {
		line := "  " + d.name
		if d.version != "" {
			line += "  " + d.version
		}
		fmt.Fprintln(w, line+"  (on this host)")
		fmt.Fprintf(w, "    attach  %s\n", formatAttach(d.attach, b.cdpURL, b.cdpPort))
		fmt.Fprintf(w, "    docs    %s\n", d.docs)
	}
	for _, d := range orderedDrivers() {
		// playwright-cli is bundled above, so a missing host copy is nothing to fix.
		if d.name != driverPlaywright && !driverInstalled(b.drivers, d.name) {
			fmt.Fprintf(w, "  %s  not installed   (install: %s)\n", d.name, d.install)
		}
	}
	fmt.Fprintln(w, "routing: use the first driver listed above unless the user names another")
	fmt.Fprintln(w, "  (bu / bu-cli / browseruse = browser-use). If the named driver is not")
	fmt.Fprintln(w, "  installed, use the first listed instead and tell the user you fell back.")
	fmt.Fprintln(w, "docs: `cuttle pw --help` documents the bundled driver; a host driver's own")
	fmt.Fprintln(w, "  instructions come from its `docs` command above - they match the installed")
	fmt.Fprintln(w, "  version, so do not rely on memory or stale copies.")
	if len(b.secrets) > 0 {
		// browser-use's insight: a substitution mechanism the model is never told
		// about does not get used, so the names ride the briefing. Names only.
		fmt.Fprintf(w, "secrets held: %s\n", strings.Join(b.secrets, ", "))
		fmt.Fprintln(w, "  type {{cuttle:NAME}} as the WHOLE value in a driver's `fill` - cuttle substitutes")
		fmt.Fprintln(w, "  it inside the CDP frame, so the value never reaches your context. A per-character")
		fmt.Fprintln(w, "  type never assembles it and types the literal instead. NEVER type the value.")
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
