package cli

import (
	"errors"
	"fmt"
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
)

var (
	errPlaywrightOpen     = errors.New("`open` launches its own browser - run `cuttle pw attach` instead, then drive cuttle's browser with the other verbs")
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

Session state lives in the container and persists across invocations, so a
session is started once and then driven verb by verb:

  cuttle pw attach     # start the session (also after a 'cuttle up' restart)
  cuttle pw <verb>     # any driver verb, as many invocations as you like
  cuttle pw detach     # finish: stops the session, leaves the browser running

Never 'close': the browser is cuttle's, and closing it drops the logins
everything else depends on. 'detach' is the way out.

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
// container. It keeps the driver pinned to cuttle's browser: `open` and the
// endpoint-overriding flags are the only ways a driver could reach a different
// one, and `attach` gets the in-container CDP endpoint unless the caller already
// named one.
func playwrightArgv(args []string) ([]string, error) {
	for _, a := range args {
		if strings.HasPrefix(a, "--endpoint") || strings.HasPrefix(a, "--extension") {
			return nil, errPlaywrightRedirect
		}
	}
	if args[0] == "open" {
		return nil, errPlaywrightOpen
	}
	argv := append([]string{driverPlaywright}, args...)
	if args[0] == "attach" && !hasCDPFlag(args) {
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
	exe, execArgs := ex.ExecCommand(playwrightWorkdir, argv)
	c := exec.CommandContext(cmd.Context(), exe, execArgs...)
	c.Stdin = cmd.InOrStdin()
	c.Stdout = cmd.OutOrStdout()
	c.Stderr = cmd.ErrOrStderr()
	if err := c.Run(); err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				return nil // Ctrl-C is the normal way to leave a long-running verb
			}
			return &ExitCodeError{Code: ee.ExitCode()}
		}
		return err //nolint:wrapcheck
	}
	return nil
}

// ExitCodeError carries a child process's exit status up to main, which exits
// with it verbatim. A passthrough verb must not rewrite the driver's exit code -
// a caller branching on it would otherwise see every failure as 1 - and the child
// has already written its own diagnostics to stderr, so nothing is printed.
type ExitCodeError struct{ Code int }

func (e *ExitCodeError) Error() string { return fmt.Sprintf("exited with status %d", e.Code) }
