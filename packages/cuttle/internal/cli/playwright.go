package cli

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/glim-sh/cuttle/internal/backend"
	"github.com/glim-sh/cuttle/internal/jev"
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
	ex, self, err := playwrightExecer(cmd.Context())
	if err != nil {
		return err
	}
	if err := gatePlaywright(cmd.Context(), ex, self, args, takeover); err != nil {
		return err
	}

	// First attempt is buffered so that "no session yet" can be answered with an
	// attach instead of reaching the caller as an error.
	var out, errOut bytes.Buffer
	runErr := execPlaywright(cmd.Context(), cmd.InOrStdin(), ex, argv, &out, &errOut)
	if driverMissing(runErr, out.String()+errOut.String()) {
		return errDriverMissing(self)
	}
	if runErr == nil || !playwrightNeedsAttach(args, out.String()+errOut.String()) {
		replay(cmd, out.Bytes(), errOut.Bytes())
		if runErr != nil && playwrightPointerIntercepted(args, out.String()+errOut.String()) {
			if hint := dialogHint(cmd.Context(), newPlaywrightRunner(ex), self); hint != "" {
				fmt.Fprintln(cmd.ErrOrStderr(), hint)
			}
		}
		return playwrightExit(runErr)
	}

	var attachOut, attachErr bytes.Buffer
	if err := execPlaywright(cmd.Context(), cmd.InOrStdin(), ex, playwrightAttachArgv(), &attachOut, &attachErr); err != nil {
		replay(cmd, attachOut.Bytes(), attachErr.Bytes())
		return playwrightExit(err)
	}
	// The attach worked, so the first attempt's complaint and the attach's own
	// chatter are both noise: drop them and give the caller the retry verbatim,
	// streams wired straight through. One retry, never a loop.
	return playwrightExit(execPlaywright(cmd.Context(), cmd.InOrStdin(), ex, argv, cmd.OutOrStdout(), cmd.ErrOrStderr()))
}

// playwrightExecer resolves the running instance and hands back the thing that
// execs a command inside it. It is the seam `cuttle jev-browse` shares with
// `cuttle pw`: the loop drives the same bundled driver, in the same container
// and the same driver session, so a person can pick the page up mid-run with
// plain `cuttle pw` verbs.
// It also returns the cuttle invocation that reaches that instance, for hints.
func playwrightExecer(ctx context.Context) (backend.Execer, string, error) {
	// The driver runs inside the container and never reaches a published port, so
	// the port fields stay zero. Which instance it is exec'd in comes from the
	// global --context/--name selection resolve reads.
	name, ctxName, cctx, b, err := resolve(commonFlags{}, defaultImage())
	if err != nil {
		return nil, "", err
	}
	state, err := b.State(ctx)
	if err != nil {
		return nil, "", err
	}
	if state != backend.StateRunning {
		return nil, "", errNotRunning(ctxName, cctx, name, state)
	}
	ex, ok := b.(backend.Execer)
	if !ok {
		return nil, "", errNoExec
	}
	return ex, cuttleCmd(ctxName, cctx, name), nil
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
	label, closeRef, ok := jev.ParseSnapshot(snap).ActiveDialog()
	if !ok {
		return ""
	}
	how := "(e.g. `" + self + " pw press Escape`)"
	if closeRef != "" {
		how = "(e.g. `" + self + " pw click " + closeRef + "`)"
	}
	return "cuttle: an open dialog covers the page: " + cmp.Or(label, "unnamed dialog") + " - dismiss it first " + how
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
