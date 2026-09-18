package cli

import (
	"fmt"
	"io"
	"strings"
)

// briefing is the single dynamic source of truth an agent needs to drive cuttle:
// live state, the bundled driver's invocation for this instance, and the secret
// names the session holds.
type briefing struct {
	verb      string
	cuttle    string // the invocation that reaches this instance, e.g. "cuttle --name scraper"
	location  string // e.g. "container 'cuttle'" or "context 'cluster'"
	imageTail string // ", image X" or ""
	version   string
	cdpURL    string
	viewerURL string   // "" = no viewer
	engine    string   // browser string, "" = unknown
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
	fmt.Fprintln(w, "Drive THIS browser - with `pw` below, or your own client attached over CDP.")
	fmt.Fprintln(w, "NEVER launch your own browser or create a new profile/context: logins live in")
	fmt.Fprintln(w, "this one and persist across down/up.")
	fmt.Fprintln(w)

	// The bundled driver is the only one the briefing names: it ships in the
	// image, so it is always there, needs nothing installed on this host, and
	// writes its files where `cuttle downloads` finds them.
	fmt.Fprintf(w, "driver: %s %s, bundled in the container - nothing to install\n", driverPlaywright, BundledPlaywrightCLIVersion)
	fmt.Fprintf(w, "  use     %s pw <command>   (`%s pw --help` lists every verb)\n", b.cuttle, b.cuttle)
	// The loop drives that same bundled driver, so it belongs to its block: an
	// agent that sees only `cuttle pw` hand-rolls what one command already does.
	fmt.Fprintf(w, "  loop    %s jev-browse --task \"...\"   (EXPERIMENTAL autonomous loop; exits blocked -> finish with %s pw)\n", b.cuttle, b.cuttle)
	if len(b.secrets) > 0 {
		// A substitution mechanism the model is never told about does not get
		// used, so the names ride the briefing. Names only.
		fmt.Fprintf(w, "secrets held: %s\n", strings.Join(b.secrets, ", "))
		fmt.Fprintln(w, "  type {{cuttle:NAME}} as the WHOLE value in a `pw fill` - cuttle substitutes")
		fmt.Fprintln(w, "  it inside the CDP frame, so the value never reaches your context. A per-character")
		fmt.Fprintln(w, "  type never assembles it and types the literal instead. NEVER type the value.")
	}
	if b.viewerURL != "" {
		fmt.Fprintf(w, "login walls / captcha: `%s open <url>`, then hand the user the viewer\n", b.cuttle)
		fmt.Fprintln(w, "  link to sign in or solve it - the CDP session stays logged in.")
	}
	// The one failure that reads as a broken selector rather than a blocked page,
	// so it is worth the two lines here rather than only in the full guide.
	fmt.Fprintln(w, "page gone quiet? a native dialog (alert/confirm/\"Leave site?\") pauses it -")
	fmt.Fprintf(w, "  clear it with `%s pw dialog-accept` (proceeds) / `dialog-dismiss` (stays);\n", b.cuttle)
	fmt.Fprintf(w, "  `%s logs` names what a click actually landed on.\n", b.cuttle)
	fmt.Fprintln(w, "full cuttle guide: `cuttle skill`  (prints the complete guide, always")
	fmt.Fprintf(w, "  matching this CLI %s; skip if you already loaded it this session)\n", b.version)
}
