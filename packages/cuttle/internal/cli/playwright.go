package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/glim-sh/cuttle/internal/backend"
)

func init() { AddCommand(newPlaywrightCmd()) }

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
	// playwrightNotOpenMarker is what the driver prints when a verb runs with no
	// live session ("The browser 'cuttle' is not open, please run open first").
	// Matching on its wording is safe only because the driver version is pinned in
	// lockstep with the image (versions.env, the Dockerfile ARG and the pin test),
	// so it cannot drift underneath us without someone seeing it.
	//
	// It is also the ONLY wording a browser that dies mid-session produces: the
	// session daemon holds the CDP connection and exits with it, so there is no
	// "daemon alive, browser gone" state to match separately, and re-attaching is
	// the whole recovery. Re-attach cannot reach a foreign browser here - the
	// endpoint is cuttle's own, and with it unset the driver fails loudly rather
	// than launching one ("Chromium distribution 'chrome' is not found at
	// /opt/google/chrome/chrome"; the image downloads no playwright browser).
	// The smoke harness's driver-attachment-drift check is what holds that.
	playwrightNotOpenMarker = "is not open, please run"
	// verbAttach is the driver verb that starts a session daemon; `open` starts one
	// too, and in this image it can only attach as well.
	verbAttach = "attach"
	verbOpen   = "open"
)

var (
	errPlaywrightRedirect = errors.New("--endpoint/--extension would point the driver at another browser - drop it, `cuttle pw` already targets cuttle's")
	errNoExec             = errors.New("`cuttle pw` needs a container to exec into, which the direct backend has none of - run playwright-cli yourself against that browser's CDP endpoint")
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
  cuttle pw open <url>       # attach + navigate; in this image it can only attach

Session state lives in the container and persists across invocations. It dies
with the container, and the first verb after a restart simply reconnects.
'detach' and 'close' end the driver session only - cuttle's browser, its tabs
and its logins stay up.

Files written with --filename land in the container's download dir; pull them
to this host with 'cuttle downloads'. Arguments, stdin, stdout, stderr and the
exit code all pass through verbatim.`, BundledPlaywrightCLIVersion),
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

func runPlaywright(cmd *cobra.Command, args []string) error {
	// DisableFlagParsing also disables cobra's own help handling, so serve it here.
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		return cmd.Help() //nolint:wrapcheck // cobra's own writer error
	}
	argv, err := playwrightArgv(args)
	if err != nil {
		return err
	}
	// The driver runs inside the container and never reaches a published port, so
	// the port fields stay zero; CUTTLE_CONTEXT still selects the context.
	name, ctxName, ctx, b, err := resolve(commonFlags{}, defaultImage())
	if err != nil {
		return err
	}
	state, err := b.State(cmd.Context())
	if err != nil {
		return err
	}
	if state != backend.StateRunning {
		return fmt.Errorf("%s: %s - run `cuttle up` first", locationLabel(ctxName, ctx, name), state) //nolint:err113 // user-facing remedy
	}
	ex, ok := b.(backend.Execer)
	if !ok {
		return errNoExec
	}

	// First attempt is buffered so that "no session yet" can be answered with an
	// attach instead of reaching the caller as an error.
	var out, errOut bytes.Buffer
	runErr := execPlaywright(cmd, ex, argv, &out, &errOut)
	if runErr == nil || !playwrightNeedsAttach(args, out.String()+errOut.String()) {
		replay(cmd, out.Bytes(), errOut.Bytes())
		return playwrightExit(runErr)
	}

	var attachOut, attachErr bytes.Buffer
	attachArgv := []string{driverPlaywright, verbAttach, "--cdp=" + playwrightCDPEndpoint}
	if err := execPlaywright(cmd, ex, attachArgv, &attachOut, &attachErr); err != nil {
		replay(cmd, attachOut.Bytes(), attachErr.Bytes())
		return playwrightExit(err)
	}
	// The attach worked, so the first attempt's complaint and the attach's own
	// chatter are both noise: drop them and give the caller the retry verbatim,
	// streams wired straight through. One retry, never a loop.
	return playwrightExit(execPlaywright(cmd, ex, argv, cmd.OutOrStdout(), cmd.ErrOrStderr()))
}

func execPlaywright(cmd *cobra.Command, ex backend.Execer, argv []string, stdout, stderr io.Writer) error {
	exe, execArgs := ex.ExecCommand(playwrightWorkdir, argv)
	c := exec.CommandContext(cmd.Context(), exe, execArgs...)
	c.Stdin = cmd.InOrStdin()
	c.Stdout = stdout
	c.Stderr = stderr
	return c.Run() //nolint:wrapcheck // the caller classifies the exit status
}

// playwrightNeedsAttach decides whether a failed verb failed only for want of a
// session. `attach` and `open` start one themselves, so their failure is real.
func playwrightNeedsAttach(args []string, combined string) bool {
	if len(args) == 0 || args[0] == verbAttach || args[0] == verbOpen {
		return false
	}
	return strings.Contains(combined, playwrightNotOpenMarker)
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
