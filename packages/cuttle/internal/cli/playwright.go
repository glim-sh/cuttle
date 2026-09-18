package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/glim-sh/cuttle/internal/backend"
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
console, tab-list, ...) still run. --takeover, before the verb, takes the
browser over:

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

// splitInstanceFlags peels cuttle's own --context/--name off the front of the
// passthrough args. DisableFlagParsing switches parsing off for the WHOLE
// invocation, root's persistent flags included, so `cuttle --name x pw snapshot`
// arrives here as ["--name","x","snapshot"] with cobra having ignored it. Only
// the leading run is cuttle's: from the driver verb on every arg is the
// driver's, so `cuttle pw click --name` still passes through untouched.
func splitInstanceFlags(sel instanceFlags, args []string) (instanceFlags, []string, error) {
	for len(args) > 0 {
		flag, value, hasValue := strings.Cut(args[0], "=")
		var target *string
		switch flag {
		case "--context":
			target = &sel.contextName
		case "--name":
			target = &sel.name
		default:
			return sel, args, nil
		}
		if hasValue {
			*target, args = value, args[1:]
			continue
		}
		if len(args) < 2 {
			return sel, nil, fmt.Errorf("%s %w", flag, errInstanceFlagValue)
		}
		*target, args = args[1], args[2:]
	}
	return sel, args, nil
}

func runPlaywright(cmd *cobra.Command, args []string) error {
	sel, args, err := splitInstanceFlags(instance, args)
	if err != nil {
		return err
	}
	instance = sel
	// DisableFlagParsing also disables cobra's own help handling, so serve it here.
	// `--help <verb>` is the driver's own per-verb help and passes through.
	if len(args) == 0 || (len(args) == 1 && isHelpFlag(args[0])) {
		return playwrightHelp(cmd)
	}
	// --takeover is cuttle's, not the driver's, so it is only recognized in front
	// of the verb, where no driver flag can be mistaken for it.
	takeover := args[0] == flagTakeover
	if takeover {
		args = args[1:]
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
	if runErr == nil || !playwrightNeedsAttach(args, out.String()+errOut.String()) {
		replay(cmd, out.Bytes(), errOut.Bytes())
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

const helpFlag = "--help"

func isHelpFlag(a string) bool { return a == "-h" || a == helpFlag }

func playwrightAttachArgv() []string {
	return []string{driverPlaywright, verbAttach, "--cdp=" + playwrightCDPEndpoint}
}

func execPlaywright(ctx context.Context, stdin io.Reader, ex backend.Execer, argv []string, stdout, stderr io.Writer) error {
	exe, execArgs := ex.ExecCommand(playwrightWorkdir, argv)
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
