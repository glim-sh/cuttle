package cli

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/glim-sh/cuttle/internal/backend"
	"github.com/glim-sh/cuttle/internal/xdg"
)

func init() { AddCommand(newPlaywrightCmd()) }

// driverPlaywright is the executable name of the driver the cuttle image bundles.
const driverPlaywright = "playwright-cli"

// BundledPlaywrightCLIVersion is the playwright-cli the cuttle image bundles and
// `cuttle pw` execs. It is a literal because the Go build cannot read
// packages/browser/versions.env; TestBundledPlaywrightCLIPin cross-checks it
// against that file's PLAYWRIGHT_CLI_VERSION and the Dockerfile's ARG, so the
// briefing can never name a version the image does not carry.
const BundledPlaywrightCLIVersion = "0.1.20"

const (
	// playwrightCDPEndpoint is the CDP address as seen from INSIDE the container -
	// the daemon's own port, never a host-published one, since the driver runs
	// beside it.
	playwrightCDPEndpoint = "http://127.0.0.1:9222"
	// playwrightWorkdir is the exec working directory: the default seed's download
	// dir, which `cuttle serve` creates at startup. `--filename` paths resolve
	// against it, so a screenshot or PDF is reachable with `cuttle downloads`,
	// while the driver's own auto-named output lands in a dotdir the downloads
	// listing hides. It is the container data dir + fingerprint.ReservedSeed + the
	// daemon's downloads dir; all three are unexported in packages this one must
	// not import (internal/fingerprint importing back would cycle its pin test).
	playwrightWorkdir = "/data/__default__/Downloads"
	// verbAttach is the driver verb that starts a session daemon; `open` starts one
	// too, and in this image it can only attach as well.
	verbAttach = "attach"
	verbOpen   = "open"
)

// playwrightNotOpenMarkers are the two ways the driver says a verb found no live
// session, and re-attaching is the whole recovery for both:
//   - "The browser 'cuttle' is not open, please run open first" (output.js) when
//     there is no session file at all, as after a clean container stop.
//   - "Browser 'cuttle' is not open. Run" (session.js, thrown as a Node stack trace)
//     when a session file survived but its daemon did not: `docker kill`, an
//     unclean restart, or the driver process dying on its own.
//
// Matching on wording is safe only because the driver version is pinned in
// lockstep with the image (versions.env, the Dockerfile ARG and the pin test), so
// it cannot drift underneath us without someone seeing it.
//
// A browser that dies mid-session needs no third marker: the session daemon holds
// the CDP connection and exits with it, so the next verb reports the first one.
// Re-attach cannot reach a foreign browser here - the endpoint is cuttle's own,
// and with it unset the driver fails loudly rather than launching one ("Chromium
// distribution 'chrome' is not found at /opt/google/chrome/chrome"; the image
// downloads no playwright browser). The smoke harness's driver-attachment-drift
// check is what holds that.
var playwrightNotOpenMarkers = []string{"is not open, please run", "is not open. Run"}

var (
	errPlaywrightRedirect = errors.New("--endpoint/--extension would point the driver at another browser - drop it, `cuttle pw` already targets cuttle's")
	errNoExec             = errors.New("`cuttle pw` needs a container to exec into, which the direct backend has none of - run playwright-cli yourself against that browser's CDP endpoint")
	errPlaywrightNoVerb   = errors.New("no playwright-cli verb to run")
	errInstanceFlagValue  = errors.New("needs a value")
	errFlagTakesNoValue   = errors.New("takes no value")
)

func newPlaywrightCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "playwright-cli [args...]",
		Aliases: []string{"pw"},
		Short:   "drive cuttle's browser with the playwright-cli bundled in the image",
		Long: fmt.Sprintf(`Run the playwright-cli driver bundled in the cuttle image (%s) against
cuttle's browser. It is execed inside the container, preconfigured to attach
over CDP, so nothing is installed on this host and it can never launch a
browser of its own.

Any verb works from cold - with no live session the wrapper attaches first and
then runs what you asked for:

  cuttle pw snapshot         # reads cuttle's page, attaching if needed
  cuttle pw goto <url>
  cuttle pw click <ref>

Navigate with 'goto', not 'open': 'open' and 'attach' restart the driver
session, dropping every ref and the selected tab (the browser's tabs stay), and
'open' with no URL sends the current tab to about:blank.

Session state lives in the container and persists across invocations. It dies
with the container, and the first verb after a restart simply reconnects.
'detach' and 'close' end the driver session only - cuttle's browser, its tabs
and its logins stay up.

Files written with --filename land in the container's download dir; pull them
to this host with 'cuttle downloads'. Arguments, stdin, stdout, stderr and the
exit code all pass through verbatim.

Because everything from the first driver arg on is the driver's, cuttle's own
--context and --name - which pick the instance to run in - have to come FIRST,
before the verb and any driver flag:

  cuttle --name scraper pw snapshot    # the container named "scraper"

CUTTLE_CONTEXT and CUTTLE_NAME select the same thing without a flag.

While another client holds the session lease - a running 'cuttle jev-browse' -
verbs that drive the page are refused, naming the holder; read verbs (snapshot,
console, tab-list, ...) still run. --takeover, before the verb - in any order
with --context/--name - takes the browser over:

  cuttle pw --takeover click <ref>

With the instance running, this help ends with the driver's own, listing every
verb; `+"`cuttle pw --help <verb>`"+` prints one verb's arguments and options.`, BundledPlaywrightCLIVersion),
		// The args are the driver's own flags (--cdp, --filename, -s), not cuttle's;
		// parsing them here would swallow the ones cobra happens to recognize.
		DisableFlagParsing: true,
		RunE:               runPlaywright,
	}
	return cmd
}

// playwrightArgv turns the passthrough args into the argv to exec in the
// container. It keeps the driver pinned to cuttle's browser: the image's
// PLAYWRIGHT_MCP_CDP_ENDPOINT routes every verb - `open` included - through
// connectOverCDP, so the endpoint-overriding flags are the only remaining way to
// reach a different browser. `attach` gets the in-container CDP endpoint unless
// the caller already named one.
func playwrightArgv(args []string) ([]string, error) {
	// `cuttle pw` answers a verbless invocation with its help before it gets here,
	// but the loop's runner calls this directly, so the check belongs to the
	// function rather than to one of its two callers.
	if len(args) == 0 {
		return nil, errPlaywrightNoVerb
	}
	for _, a := range args {
		if strings.HasPrefix(a, "--endpoint") || strings.HasPrefix(a, "--extension") {
			return nil, errPlaywrightRedirect
		}
	}
	argv := append([]string{driverPlaywright}, args...)
	if args[0] == verbAttach && !hasCDPFlag(args) {
		argv = append(argv, "--cdp="+playwrightCDPEndpoint)
	}
	return argv, nil
}

func hasCDPFlag(args []string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "--cdp") {
			return true
		}
	}
	return false
}

// splitCuttleFlags peels cuttle's own leading flags - --context/--name and
// --takeover, in any order - off the passthrough args. DisableFlagParsing switches
// parsing off for the WHOLE invocation, root's persistent flags included, so
// `cuttle --name x pw snapshot` arrives here as ["--name","x","snapshot"] with
// cobra having ignored it. Only the leading run is cuttle's: from the driver verb
// on every arg is the driver's, so `cuttle pw click --name` still passes through
// untouched.
func splitCuttleFlags(sel instanceFlags, args []string) (instanceFlags, bool, []string, error) {
	takeover := false
	for len(args) > 0 {
		flag, value, hasValue := strings.Cut(args[0], "=")
		var target *string
		switch {
		case args[0] == flagTakeover:
			takeover, args = true, args[1:]
			continue
		case flag == flagTakeover:
			// Passed on, `--takeover=true` would end the run and hand any instance
			// flag after it to the driver, leaving the verb on the default instance.
			return sel, takeover, nil, fmt.Errorf("%s %w", flagTakeover, errFlagTakesNoValue)
		case flag == "--context":
			target = &sel.contextName
		case flag == "--name":
			target = &sel.name
		default:
			return sel, takeover, args, nil
		}
		if !hasValue {
			if len(args) < 2 {
				return sel, takeover, nil, fmt.Errorf("%s %w", flag, errInstanceFlagValue)
			}
			value, args = args[1], args[1:]
		}
		// Empty would select the default instance, not none.
		if value == "" {
			return sel, takeover, nil, fmt.Errorf("%s %w", flag, errInstanceFlagValue)
		}
		*target, args = value, args[1:]
	}
	return sel, takeover, args, nil
}

func runPlaywright(cmd *cobra.Command, args []string) error {
	// --takeover is cuttle's, not the driver's, so like --context/--name it is only
	// recognized in front of the verb, where no driver flag can be mistaken for it.
	sel, takeover, args, err := splitCuttleFlags(instance, args)
	if err != nil {
		return err
	}
	instance = sel
	// DisableFlagParsing also disables cobra's own help handling, so serve it here.
	// `--help <verb>` is the driver's own per-verb help and passes through.
	if len(args) == 0 || (len(args) == 1 && isHelpFlag(args[0])) {
		return playwrightHelp(cmd)
	}
	argv, err := playwrightArgv(args)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	ex, self, checkRunning, err := resolvePlaywrightExecer(ctx)
	if err != nil {
		return err
	}
	driving := !playwrightReadOnly(args)
	if driving {
		argv = snapshotArgv(argv)
	}
	first := argv
	switch {
	case !driving:
		// A read verb passes a held lease, so it runs unwrapped.
	case takeover:
		// The takeover is an exec of its own ahead of the verb, so it is the one
		// path that asks the state first: a stopped instance must say so, not fail
		// to reach its lease.
		if err := checkRunning(ctx); err != nil {
			return err
		}
		if err := gatePlaywright(ctx, ex, self, args, true); err != nil {
			return err
		}
	default:
		first = leaseGatedArgv(argv)
	}

	// First attempt is buffered so that "no session yet" can be answered with an
	// attach instead of reaching the caller as an error.
	var out, errOut bytes.Buffer
	runErr := execPlaywright(ctx, cmd.InOrStdin(), ex, first, &out, &errOut)
	if leaseUnsettled(runErr, out.String(), errOut.String()) {
		// The in-exec check saw anything but a free lease: the full gate decides,
		// and the verb has not run yet.
		if err := gatePlaywright(ctx, ex, self, args, false); err != nil {
			return err
		}
		out.Reset()
		errOut.Reset()
		runErr = execPlaywright(ctx, cmd.InOrStdin(), ex, argv, &out, &errOut)
	}
	combined := out.String() + errOut.String()
	if driverMissing(runErr, combined) {
		return errDriverMissing(self)
	}
	if runErr == nil {
		replay(cmd, hostSnapshot(snapshotHostDir(), out.Bytes()), errOut.Bytes())
		return nil
	}
	if !playwrightNeedsAttach(args, combined) {
		// The state is only asked of a failure the container did not explain: an
		// exec into a stopped or absent instance always fails, and a missing driver
		// or session is only ever reported from inside a running one.
		if err := checkRunning(ctx); err != nil {
			return err
		}
		replay(cmd, out.Bytes(), errOut.Bytes())
		if playwrightPointerIntercepted(args, combined) {
			if hint := dialogHint(ctx, newPlaywrightRunner(ex), self); hint != "" {
				fmt.Fprintln(cmd.ErrOrStderr(), hint)
			}
		}
		return playwrightExit(runErr)
	}

	var attachOut, attachErr bytes.Buffer
	if err := execPlaywright(ctx, cmd.InOrStdin(), ex, playwrightAttachArgv(), &attachOut, &attachErr); err != nil {
		replay(cmd, attachOut.Bytes(), attachErr.Bytes())
		return playwrightExit(err)
	}
	// The attach can wait minutes on another invocation's, long enough for a
	// driver to take the lease, so a driving retry is gated again.
	if driving && !takeover {
		if err := gatePlaywright(ctx, ex, self, args, false); err != nil {
			return err
		}
	}
	// The attach worked, so the first attempt's complaint and the attach's own
	// chatter are both noise: drop them and give the caller the retry, stderr
	// wired straight through and stdout held only for its snapshot link. One
	// retry, never a loop.
	out.Reset()
	runErr = execPlaywright(ctx, cmd.InOrStdin(), ex, argv, &out, cmd.ErrOrStderr())
	if runErr == nil {
		_, _ = cmd.OutOrStdout().Write(hostSnapshot(snapshotHostDir(), out.Bytes()))
		return nil
	}
	_, _ = cmd.OutOrStdout().Write(out.Bytes())
	return playwrightExit(runErr)
}

// playwrightExecer resolves the running instance and hands back the thing that
// execs a command inside it. It is the seam `cuttle jev-browse` shares with
// `cuttle pw`: the loop drives the same bundled driver, in the same container
// and the same driver session, so a person can pick the page up mid-run with
// plain `cuttle pw` verbs.
// It also returns the cuttle invocation that reaches that instance, for hints.
func playwrightExecer(ctx context.Context) (backend.Execer, string, error) {
	ex, self, checkRunning, err := resolvePlaywrightExecer(ctx)
	if err != nil {
		return nil, "", err
	}
	if err := checkRunning(ctx); err != nil {
		return nil, "", err
	}
	return ex, self, nil
}

// resolvePlaywrightExecer is playwrightExecer with the state check handed back
// instead of run: it costs a process (an ssh round trip on a remote host) that
// `cuttle pw` only needs once an exec has failed.
func resolvePlaywrightExecer(ctx context.Context) (backend.Execer, string, func(context.Context) error, error) {
	// The driver runs inside the container and never reaches a published port, so
	// the port fields stay zero. Which instance it is exec'd in comes from the
	// global --context/--name selection resolve reads.
	name, ctxName, cctx, b, err := resolve(commonFlags{}, defaultImage())
	if err != nil {
		return nil, "", nil, err
	}
	checkRunning := func(ctx context.Context) error {
		state, err := b.State(ctx)
		if err != nil {
			return err
		}
		if state != backend.StateRunning {
			return errNotRunning(ctxName, cctx, name, state)
		}
		return nil
	}
	ex, ok := b.(backend.Execer)
	if !ok {
		if err := checkRunning(ctx); err != nil {
			return nil, "", nil, err
		}
		return nil, "", nil, errNoExec
	}
	return ex, cuttleCmd(ctxName, cctx, name), checkRunning, nil
}

// playwrightHelp prints the wrapper's help and then the bundled driver's own,
// which lists every verb. The driver's help can only come from the container, so
// it is best-effort: when it cannot run, a note says why and the help still
// exits 0, as it did before it carried the driver's half.
func playwrightHelp(cmd *cobra.Command) error {
	if err := cmd.Help(); err != nil {
		return err //nolint:wrapcheck // cobra's own writer error
	}
	ex, _, err := playwrightExecer(cmd.Context())
	if err != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "\nThe bundled driver's own help, listing every verb, is not available: %v\n", err)
		return nil
	}
	writeDriverHelp(cmd.Context(), ex, cmd.OutOrStdout())
	return nil
}

func writeDriverHelp(ctx context.Context, ex backend.Execer, w io.Writer) {
	var out bytes.Buffer
	if err := execPlaywright(ctx, nil, ex, []string{driverPlaywright, helpFlag}, &out, &out); err != nil {
		// Released images before the driver was bundled land here with an exec
		// "not found", as does a backend that cannot exec right now.
		fmt.Fprintf(w, "\nThe bundled driver's own help, listing every verb, is not available (%v): %s\n"+
			"An image that predates the bundled driver cannot run `cuttle pw` at all.\n", err, strings.TrimSpace(out.String()))
		return
	}
	fmt.Fprint(w, "\n--- the bundled driver's own help: run each verb as `cuttle pw <verb>`,\n--- one verb's options with `cuttle pw --help <verb>`\n\n")
	_, _ = w.Write(out.Bytes())
}

// execNotFound is the shell's and docker's exit status for a command that is
// not there, which the driver itself never exits with.
const execNotFound = 127

// driverMissing reports whether an exec failed because the container has no
// driver at all - an image that predates the bundled one.
func driverMissing(err error, combined string) bool {
	ee, ok := errors.AsType[*exec.ExitError](err)
	return ok && ee.ExitCode() == execNotFound && strings.Contains(combined, driverPlaywright) && strings.Contains(combined, "not found")
}

func errDriverMissing(self string) error {
	return fmt.Errorf("this container's image predates the bundled %s - run `%s up --recreate` to upgrade it to this CLI's image (a persistent profile is kept)", driverPlaywright, self) //nolint:err113 // user-facing remedy
}

// noDriverMarker is what bundledDriverAbsent's probe prints when the driver is
// not on the container's PATH.
const noDriverMarker = "cuttle-no-driver"

// bundledDriverAbsent reports whether the instance's image predates the bundled
// driver, so the briefing does not advertise a `cuttle pw` that cannot run. The
// probe is a shell builtin, far cheaper than starting the driver. Only a positive answer
// counts: an exec that fails or stalls (a restarting container, a dropped ssh
// link) says nothing about the image, and the verb reports that failure itself.
func bundledDriverAbsent(ctx context.Context, ex backend.Execer) bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var out bytes.Buffer
	probe := []string{"sh", "-c", "command -v " + driverPlaywright + " >/dev/null || echo " + noDriverMarker}
	err := execIn(ctx, nil, ex, "/", probe, &out, io.Discard)
	return err == nil && strings.Contains(out.String(), noDriverMarker)
}

const helpFlag = "--help"

func isHelpFlag(a string) bool { return a == "-h" || a == helpFlag }

// playwrightAttachArgv is the auto-attach. Every invocation that finds the
// session gone attaches, and an attach replaces the session, so concurrent ones -
// parallel agents right after a crash - kill each other's daemons mid-start
// (ENOENT or EADDRINUSE on its socket, "Session closed") and all but one fail. So
// the attach runs under a container-wide lock and is skipped when an invocation
// ahead of it already brought the session back; `list` shows only sessions whose
// daemon answers. The wait for the lock is bounded: an attach to a tab that never
// answers runs to the driver's own 30s timeout, and a queue of those must not
// hold every invocation behind it for minutes.
func playwrightAttachArgv() []string {
	attach := driverPlaywright + " " + verbAttach + " --cdp=" + playwrightCDPEndpoint
	script := driverPlaywright + ` list | grep -qxF -- "- $PLAYWRIGHT_CLI_SESSION:" || exec ` + attach
	locked := `flock -w 60 -E 75 /tmp/cuttle-pw-attach.lock sh -c "$0"; rc=$?; ` +
		`[ "$rc" -ne 75 ] || echo "cuttle: gave up after 60s waiting for another invocation's attach" >&2; exit "$rc"`
	return []string{"sh", "-c", locked, script}
}

func execPlaywright(ctx context.Context, stdin io.Reader, ex backend.Execer, argv []string, stdout, stderr io.Writer) error {
	return execIn(ctx, stdin, ex, playwrightWorkdir, argv, stdout, stderr)
}

// execIn runs argv in the instance with workdir as its working directory.
func execIn(ctx context.Context, stdin io.Reader, ex backend.Execer, workdir string, argv []string, stdout, stderr io.Writer) error {
	exe, execArgs := ex.ExecCommand(workdir, argv)
	c := exec.CommandContext(ctx, exe, execArgs...)
	c.Stdin = stdin
	c.Stdout = stdout
	c.Stderr = stderr
	return c.Run() //nolint:wrapcheck // the caller classifies the exit status
}

// playwrightRunner runs one driver verb and returns its captured output.
type playwrightRunner = func(ctx context.Context, args ...string) (string, error)

// newPlaywrightRunner returns a func that runs one driver verb in the resolved
// instance, with the same auto-attach recovery `cuttle pw` has.
// Unlike `cuttle pw` it CAPTURES the output instead of streaming it, and returns
// it even on a non-zero exit: a pending native dialog makes `snapshot` an error
// whose body carries the `### Modal state` block that says how to recover, and a
// caller that threw it away on the error exit would lose exactly that.
func newPlaywrightRunner(ex backend.Execer) playwrightRunner {
	return func(ctx context.Context, args ...string) (string, error) {
		argv, err := playwrightArgv(args)
		if err != nil {
			return "", err
		}
		var out bytes.Buffer
		runErr := execPlaywright(ctx, nil, ex, argv, &out, &out)
		if runErr == nil || !playwrightNeedsAttach(args, out.String()) {
			return out.String(), runErr
		}
		var attachOut bytes.Buffer
		if err := execPlaywright(ctx, nil, ex, playwrightAttachArgv(), &attachOut, &attachOut); err != nil {
			return attachOut.String(), err
		}
		out.Reset()
		runErr = execPlaywright(ctx, nil, ex, argv, &out, &out)
		return out.String(), runErr
	}
}

// playwrightNeedsAttach decides whether a failed verb failed only for want of a
// session. `attach` and `open` start one themselves, so their failure is real.
func playwrightNeedsAttach(args []string, combined string) bool {
	if len(args) == 0 || args[0] == verbAttach || args[0] == verbOpen {
		return false
	}
	return slices.ContainsFunc(playwrightNotOpenMarkers, func(m string) bool { return strings.Contains(combined, m) })
}

// playwrightPointerIntercepted reports a pointer action that another element
// took until the driver timed out - what an in-page modal does to a click behind
// it. fill and type do not hit-test, so they never report it. A -s/--session
// invocation is left out: the hint's snapshot reads the default session's page.
func playwrightPointerIntercepted(args []string, combined string) bool {
	namesSession := slices.ContainsFunc(args, func(a string) bool {
		flag, _, _ := strings.Cut(a, "=")
		return flag == "-s" || flag == "--session"
	})
	return !namesSession && strings.Contains(combined, "intercepts pointer events")
}

// dialogHint takes one snapshot after a pointer action was intercepted and, when
// an in-page dialog holds focus, names it and its dismiss control. Any failure of
// the snapshot yields no hint: the verb's own error already went out.
func dialogHint(ctx context.Context, run playwrightRunner, self string) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	snap, err := run(ctx, "snapshot")
	if err != nil {
		return ""
	}
	label, closeRef, ok := activeDialog(snap)
	if !ok {
		return ""
	}
	how := "(e.g. `" + self + " pw press Escape`)"
	if closeRef != "" {
		how = "(e.g. `" + self + " pw click " + closeRef + "`)"
	}
	return "cuttle: an open dialog covers the page: " + cmp.Or(label, "unnamed dialog") + " - dismiss it first " + how
}

// dismissNames are the whole button names that only close a dialog. They are
// matched whole, never as a substring, so "Cancel subscription" or "Close account"
// is never offered as the way out; bare "Cancel" is left out for the same reason.
var dismissNames = map[string]bool{
	"close": true, "dismiss": true, "not now": true, "no thanks": true, "no, thanks": true, "x": true, "×": true,
}

var (
	ariaHeadRE = regexp.MustCompile(`^([a-z]+)\s*(.*)$`)
	ariaRefRE  = regexp.MustCompile(`\[ref=([A-Za-z0-9]+)\]`)
)

// ariaNode is one node line of an aria snapshot.
type ariaNode struct {
	depth      int
	role, name string
	attrs      string // the bracketed attributes after the name
}

// activeDialog finds the last dialog or alertdialog holding focus - a modal traps
// [active] inside itself - in the last snapshot section of playwright-cli output,
// and returns its name (else its first heading) and the ref of a button in it
// whose whole name only dismisses. It reads the snapshot itself rather than
// through internal/jev, so jev-browse stays a removable module.
func activeDialog(out string) (string, string, bool) {
	var tree []ariaNode
	inSnapshot := false
	for line := range strings.SplitSeq(out, "\n") {
		if header, ok := strings.CutPrefix(line, "### "); ok {
			if inSnapshot = strings.TrimSpace(header) == "Snapshot"; inSnapshot {
				tree = nil
			}
			continue
		}
		if n, ok := parseAriaNode(line); inSnapshot && ok {
			tree = append(tree, n)
		}
	}
	label, closeRef, found := "", "", false
	for i, d := range tree {
		if d.role != "dialog" && d.role != "alertdialog" {
			continue
		}
		end := i + 1
		for end < len(tree) && tree[end].depth > d.depth {
			end++
		}
		sub := tree[i:end]
		if !slices.ContainsFunc(sub, func(n ariaNode) bool { return strings.Contains(n.attrs, "[active]") }) {
			continue
		}
		label, closeRef, found = d.name, "", true
		for _, n := range sub[1:] {
			if label == "" && n.role == "heading" {
				label = n.name
			}
			if m := ariaRefRE.FindStringSubmatch(n.attrs); closeRef == "" && m != nil && n.role == "button" && dismissNames[strings.ToLower(n.name)] {
				closeRef = m[1]
			}
		}
	}
	return strings.Join(strings.Fields(label), " "), closeRef, found
}

// parseAriaNode reads `- role "name" [attrs]`, optionally followed by ":" or
// ": text". Playwright single-quotes the whole key, attributes included, when
// it would not read back as a yaml key - most often a name holding ": " - and a
// doubled single quote inside it stands for one.
func parseAriaNode(line string) (ariaNode, bool) {
	body := strings.TrimLeft(line, " ")
	n := ariaNode{depth: len(line) - len(body)}
	body, ok := strings.CutPrefix(body, "- ")
	if !ok {
		return n, false
	}
	key := ""
	if quoted, isQuoted := strings.CutPrefix(body, "'"); isQuoted {
		var b strings.Builder
		i := 0
		for ; i < len(quoted); i++ {
			if quoted[i] == '\'' {
				if i+1 == len(quoted) || quoted[i+1] != '\'' {
					break
				}
				i++
			}
			b.WriteByte(quoted[i])
		}
		if i == len(quoted) {
			return n, false
		}
		key, body = b.String(), quoted[i+1:]
	}
	rest, _, _ := strings.Cut(body, ": ")
	m := ariaHeadRE.FindStringSubmatch(key + strings.TrimSuffix(rest, ":"))
	if m == nil {
		return n, false
	}
	n.role, n.attrs = m[1], m[2]
	if strings.HasPrefix(n.attrs, `"`) {
		dec := json.NewDecoder(strings.NewReader(n.attrs))
		if dec.Decode(&n.name) != nil {
			return n, false
		}
		n.attrs = n.attrs[dec.InputOffset():]
	}
	return n, true
}

// snapshotLinkRE is the link playwright-cli prints after an action verb in
// place of the page snapshot, a path relative to playwrightWorkdir that only
// exists inside the container.
var snapshotLinkRE = regexp.MustCompile(`\[Snapshot\]\((\.playwright-cli/page-[0-9TZ-]+\.yml)\)`)

const (
	snapshotsKept = 50
	// snapshotMarker separates the driver's own stdout from the masked snapshot
	// snapshotArgv appends after it.
	snapshotMarker = "cuttle-host-snapshot-7c1e"
)

// snapshotArgv wraps a driving verb so the same exec also fetches the snapshot
// it linked, through the daemon's /snapshot route (which masks held secrets),
// and appends it to stdout behind snapshotMarker. The verb's exit status is
// kept, and a failed fetch appends nothing.
func snapshotArgv(argv []string) []string {
	script := `f=$(mktemp) || exec "$@"; "$@" >"$f"; rc=$?; cat "$f"; ` +
		`p=$(sed -n 's/.*\[Snapshot\](\(\.playwright-cli\/page-[0-9TZ-]*\.yml\)).*/\1/p' "$f" | head -n 1); ` +
		`if [ "$rc" -eq 0 ] && [ -n "$p" ] && curl -sf --max-time 2 -o "$f" "` + playwrightCDPEndpoint + `/snapshot?file=$p"; then ` +
		`printf '\n%s\n' ` + snapshotMarker + `; cat "$f"; fi; rm -f "$f"; exit "$rc"`
	return append([]string{"sh", "-c", script, "sh"}, argv...)
}

// snapshotHostDir is where this instance's snapshots land on the host, or ""
// when no state dir resolves.
func snapshotHostDir() string {
	state := xdg.StateDir()
	name, _, _, _, err := resolve(commonFlags{}, defaultImage())
	if state == "" || err != nil {
		return ""
	}
	return filepath.Join(state, "cuttle", name, "snapshots")
}

// hostSnapshot splits the snapshot snapshotArgv appended off the verb's stdout,
// saves it in dir and points the printed link at the copy. Without an appended
// snapshot, or when it cannot be saved, the driver's own output comes back as it
// printed it.
func hostSnapshot(dir string, out []byte) []byte {
	i := bytes.LastIndex(out, []byte("\n"+snapshotMarker+"\n"))
	if i < 0 {
		return out
	}
	out, snap := out[:i], out[i+len(snapshotMarker)+2:]
	m := snapshotLinkRE.FindSubmatchIndex(out)
	if m == nil || dir == "" {
		return out
	}
	path := filepath.Join(dir, filepath.Base(string(out[m[2]:m[3]])))
	if os.MkdirAll(dir, 0o700) != nil || os.WriteFile(path, snap, 0o600) != nil {
		return out
	}
	pruneSnapshots(dir)
	return slices.Concat(out[:m[2]], []byte(path), out[m[3]:])
}

// pruneSnapshots keeps the newest snapshotsKept files. The driver's names carry
// an ISO timestamp, so name order is age order.
func pruneSnapshots(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasPrefix(e.Name(), "page-") && strings.HasSuffix(e.Name(), ".yml") {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)
	for _, name := range names[:max(0, len(names)-snapshotsKept)] {
		_ = os.Remove(filepath.Join(dir, name))
	}
}

func replay(cmd *cobra.Command, stdout, stderr []byte) {
	_, _ = cmd.OutOrStdout().Write(stdout)
	_, _ = cmd.ErrOrStderr().Write(stderr)
}

func playwrightExit(err error) error {
	if err == nil {
		return nil
	}
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return nil // Ctrl-C is the normal way to leave a long-running verb
		}
		return &ExitCodeError{Code: ee.ExitCode()}
	}
	return err
}

// ExitCodeError carries a child process's exit status up to main, which exits
// with it verbatim. A passthrough verb must not rewrite the driver's exit code -
// a caller branching on it would otherwise see every failure as 1 - and the child
// has already written its own diagnostics to stderr, so nothing is printed.
type ExitCodeError struct{ Code int }

func (e *ExitCodeError) Error() string { return fmt.Sprintf("exited with status %d", e.Code) }
