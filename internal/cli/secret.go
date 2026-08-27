package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/glim-sh/cuttle/internal/backend"
	"github.com/glim-sh/cuttle/internal/config"
)

// `cuttle secret` - the host half of daemon-owned secrets. A value is handed to
// the daemon over the loopback CDP endpoint and lives only in its memory, under a
// TTL, per seed; a driver then types the sentinel {{cuttle:NAME}} and the daemon
// substitutes it inside the CDP frame. So the value never enters argv, a driver
// command line, an agent transcript, or a log.
//
// Two rules hold across every verb here and are not negotiable:
//
//   - No verb ever prints a stored value. `ls` reports shape only.
//   - A value only ever travels on stdin or in a request body, never in argv,
//     because argv is world-readable in /proc and lands in shell history.

const (
	// daemonTimeout is the ceiling on an ordinary daemon call; grabWait is the
	// longer one for a grab, whose daemon-side budget is shorter, so this only
	// ever fires when the daemon itself is gone.
	daemonTimeout = 10 * time.Second
	grabWait      = 60 * time.Second
	// jsonReplyLimit caps a JSON reply. It is generous because a listing (targets,
	// downloads, cookies) is the big one, and a truncated body surfaces as
	// "unexpected end of JSON input", which explains nothing.
	jsonReplyLimit    = 8 << 20
	grabResponseLimit = 8 << 20
)

// secretValueLimit caps what `set` will read from stdin. A credential is a
// credential, not a payload.
const secretValueLimit = 64 << 10

// fillHint is the one line that turns "cuttle holds a secret" into something an
// agent can act on, printed after every successful set because that is the moment
// the name is in front of the reader.
const fillHint = "type {{cuttle:%s}} as the WHOLE value in any driver's fill"

// secretNamePattern mirrors the daemon's grammar (internal/serve/secrets.go),
// which stays authoritative. It exists here only so a bad name is refused before
// a resolver spends a one-time code or a human types one at a prompt - a 400
// after the fact wastes exactly the thing this feature protects.
var secretNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

func checkSecretName(name string) error {
	if secretNamePattern.MatchString(name) {
		return nil
	}
	return fmt.Errorf("%w: %q - letters, digits and underscore only, starting with a letter or underscore",
		errSecretBadName, name)
}

// Where a value came from, mirroring the daemon's own set. The daemon rejects
// anything else, because the source decides what a stale-value error can suggest.
const (
	sourceStdin  = "stdin"
	sourceExec   = "exec"
	sourcePrompt = "prompt"
)

// execTimeout bounds a host-side resolver. It is generous because the common
// resolver is a vault CLI that may prompt for biometrics; it exists at all
// because `op` can hang indefinitely with no output in a headless context, and a
// hung resolver would otherwise hang the verb forever.
const execTimeout = 2 * time.Minute

var (
	errSecretNoInput    = errors.New("no value on stdin: pipe it in, e.g. `op read op://vault/item/password | cuttle secret set NAME --stdin`")
	errSecretNeedSource = errors.New("pass --stdin and pipe the value in, or --exec with a command that prints it: `... | cuttle secret set NAME --stdin`")
	errSecretBothInputs = errors.New("pass either --stdin or --exec, not both")
	errSecretExecEmpty  = errors.New("the --exec command printed nothing - a resolver must print the value on stdout")
	errSecretExecFailed = errors.New("the --exec command failed")
	errSecretNoRecipe   = errors.New("nothing to refresh")
	errDaemonNotFound   = errors.New("not found")
	errSecretBadName    = errors.New("invalid secret name")
	errSecretNotATTY    = errors.New("`secret prompt` needs a terminal to ask on; pipe the value to `secret set NAME --stdin` instead")
)

func init() { AddCommand(newSecretCmd(), newGrabCmd()) }

func newGrabCmd() *cobra.Command {
	var cf commonFlags
	cmd := &cobra.Command{
		Use:   "grab <url> [dest]",
		Short: "fetch a URL through the signed-in browser and hand the bytes back",
		Long: `Fetches a URL from inside the running browser, with its cookies, and returns
what came back - the read half of an authenticated session, for the case that
has no download button.

  cuttle grab https://app.example.com/api/me            # prints the body
  cuttle grab https://app.example.com/export.csv out.csv # saves it 0600, prints the path

Cookie auth only: the request carries the browser's cookies for that origin and
no Authorization header, so a token-auth API is out of reach this way. A
same-origin URL (and a blob: URL the page made) is fetched in the page itself;
anything cross-origin goes through a scratch tab, because a page cannot read a
cross-origin response without CORS. A response the browser turns into a
download has no body to read - pull that with "cuttle downloads".`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			dest := ""
			if len(args) == 2 {
				dest = args[1]
			}
			return runGrab(cmd, cf, args[0], dest)
		},
	}
	addCommonFlags(cmd, &cf)
	return cmd
}

func runGrab(cmd *cobra.Command, cf commonFlags, target, dest string) error {
	base, release, err := sessionEndpoint(cmd, &cf)
	if err != nil {
		return err
	}
	defer release()

	data, err := daemonRequest(cmd.Context(), http.MethodPost, base+"/grab",
		map[string]any{"url": target}, grabResponseLimit, grabWait)
	if err != nil {
		return fmt.Errorf("grabbing %s: %w", target, err)
	}
	if dest == "" {
		_, werr := cmd.OutOrStdout().Write(data)
		return werr //nolint:wrapcheck
	}
	// 0600 and path-only, like a pulled download: the bytes may be exactly the
	// credential this whole feature exists to keep out of a transcript.
	if err := writeSecretFile(dest, data); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "saved %s (%d bytes)\n", dest, len(data))
	return nil
}

func newSecretCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "hand the session a credential to type, without it entering argv or a transcript",
		Long: `Store a credential in the running session's memory under a name, then have a
driver type it by name instead of by value:

  op read op://vault/github/password | cuttle secret set GH_PASS --stdin
  playwright-cli fill e17 '{{cuttle:GH_PASS}}'

The substitution happens inside cuttle's CDP proxy rather than in the driver, so
it works on any driver's FILL. A per-character type (keyboard.type,
agent-browser type) or a value set through eval never assembles the sentinel and
types its literal text instead. The value lives in daemon memory only - never on
disk, never in a log, never printed back - and expires on its own.

Every verb here acts on ONE session. With more than one running, name it:

  cuttle secret ls --name staging --cdp-port 9333

Without those flags the default session is the target, which on a host running
several is not necessarily the one you are driving.`,
	}
	cmd.AddCommand(newSecretSetCmd(), newSecretRefreshCmd(), newSecretPromptCmd(),
		newSecretCaptureCmd(), newSecretListCmd(), newSecretRemoveCmd())
	return cmd
}

func newSecretSetCmd() *cobra.Command {
	var cf commonFlags
	var stdin bool
	var execCmd string
	var ttl time.Duration
	cmd := &cobra.Command{
		Use:   "set NAME (--stdin | --exec 'command')",
		Short: "store a value the session can type by name (value on stdin or from a resolver, never argv)",
		Long: `Hand the running session a value it can type by name.

  op read op://vault/github/password | cuttle secret set GH_PASS --stdin
  cuttle secret set GH_TOTP --exec 'op item get GitHub --otp'

--exec registers the command in the config file and runs it once, HERE, on this
host - the daemon is in a container with no vault, no keychain and no
biometrics, and never learns the command. Because it resolves at set time, a
time-bounded value (a TOTP) needs "cuttle secret refresh NAME" immediately
before it is used; the substitution path says so when the value has expired.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch {
			case stdin && execCmd != "":
				return errSecretBothInputs
			case stdin:
				return runSecretSet(cmd, cf, args[0], ttl)
			case execCmd != "":
				return runSecretSetExec(cmd, cf, args[0], execCmd, ttl)
			default:
				return errSecretNeedSource
			}
		},
	}
	addCommonFlags(cmd, &cf)
	cmd.Flags().BoolVar(&stdin, "stdin", false, "read the value from standard input")
	cmd.Flags().StringVar(&execCmd, "exec", "", "shell command that prints the value on stdout; stored in the config and re-runnable with `secret refresh`")
	cmd.Flags().DurationVar(&ttl, "ttl", 0, "how long the daemon keeps the value (default 15m)")
	return cmd
}

func newSecretRefreshCmd() *cobra.Command {
	var cf commonFlags
	var ttl time.Duration
	cmd := &cobra.Command{
		Use:   "refresh NAME",
		Short: "re-run a registered --exec resolver and hand the session a fresh value",
		Long: `Re-runs the command registered with "secret set NAME --exec" and stores what it
prints under a fresh TTL. This is the verb to run immediately before a
time-bounded value is typed - a TOTP resolved at set time is dead in 30 seconds
- and the one the substitution error names when a value has expired.

It works with no daemon entry at all: after "cuttle down && cuttle up" the
recipe is still in the config, so one refresh restores both the value and the
name the sentinel resolves.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return runSecretRefresh(cmd, cf, args[0], ttl) },
	}
	addCommonFlags(cmd, &cf)
	cmd.Flags().DurationVar(&ttl, "ttl", 0, "how long the daemon keeps the value (default 15m)")
	return cmd
}

func newSecretPromptCmd() *cobra.Command {
	var cf commonFlags
	var ttl time.Duration
	cmd := &cobra.Command{
		Use:   "prompt NAME",
		Short: "ask the human at the terminal for a value, without it entering the transcript",
		Long: `Reads a value from the terminal with echo off and hands it straight to the
session. Nothing is displayed, nothing is stored on disk, and the value never
appears in the conversation - which is the point: the alternative is a person
pasting a one-time code into a chat window, where it stays forever.

Use it for a code only a human has (an SMS or authenticator code with no
retrievable source). A code you CAN retrieve belongs in
"cuttle secret set NAME --exec" plus "cuttle secret refresh NAME" instead.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return runSecretPrompt(cmd, cf, args[0], ttl) },
	}
	addCommonFlags(cmd, &cf)
	cmd.Flags().DurationVar(&ttl, "ttl", 0, "how long the daemon keeps the value (default 15m)")
	return cmd
}

func newSecretListCmd() *cobra.Command {
	var cf commonFlags
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "show which secrets the session holds (names and shape - never values)",
		Args:    cobra.NoArgs,
		RunE:    func(cmd *cobra.Command, _ []string) error { return runSecretList(cmd, cf) },
	}
	addCommonFlags(cmd, &cf)
	return cmd
}

func newSecretRemoveCmd() *cobra.Command {
	var cf commonFlags
	cmd := &cobra.Command{
		Use:     "rm NAME",
		Aliases: []string{"remove"},
		Short:   "forget a secret entirely - the value AND any registered resolver",
		Args:    cobra.ExactArgs(1),
		RunE:    func(cmd *cobra.Command, args []string) error { return runSecretRemove(cmd, cf, args[0]) },
	}
	addCommonFlags(cmd, &cf)
	return cmd
}

// sessionEndpoint resolves the running session's loopback base URL. Every verb
// that reaches the daemon - secrets, capture, grab, auth status, downloads -
// needs exactly this preamble, and every one of those routes sits behind the
// daemon's loopback guard.
func sessionEndpoint(cmd *cobra.Command, cf *commonFlags) (string, func(), error) {
	_, _, _, b, err := resolveRunning(cmd, cf, defaultImage())
	if err != nil {
		return "", nil, err
	}
	ep, release, err := reachStable(cmd.Context(), b, *cf)
	if err != nil {
		return "", nil, err
	}
	if waitCDP(cmd.Context(), ep.CDPHost, ep.CDPPort, 5*time.Second) == nil {
		release()
		return "", nil, errCDPNotAnswering
	}
	base, _ := endpointURLs(ep)
	return base, release, nil
}

func runSecretSet(cmd *cobra.Command, cf commonFlags, name string, ttl time.Duration) error {
	base, release, err := secretTarget(cmd, &cf, name)
	if err != nil {
		return err
	}
	defer release()

	value, err := readSecretStdin(cmd.InOrStdin())
	if err != nil {
		return err
	}
	defer clear(value)
	return putSecret(cmd, base, name, value, sourceStdin, ttl)
}

// secretTarget is the preamble for every verb that is about to ACQUIRE a value:
// validate the name, then prove there is a session to hand it to, and only then
// let the caller read a pipe, prompt a human, or run a resolver. Reading a
// one-time code and then failing on a stopped daemon or a typo burns the code.
func secretTarget(cmd *cobra.Command, cf *commonFlags, name string) (string, func(), error) {
	if err := checkSecretName(name); err != nil {
		return "", nil, err
	}
	return sessionEndpoint(cmd, cf)
}

// runSecretSetExec registers a resolver and runs it once. Two writes, and both
// matter: the recipe to the host's config file, so `refresh` can re-run it after
// a daemon restart, and the resolved value to the daemon, whose entry survives
// its own TTL as a registration - without which the daemon could not tell
// "unknown name" from "expired value" and the refresh affordance would be
// unreachable from the error an agent actually sees.
func runSecretSetExec(cmd *cobra.Command, cf commonFlags, name, execCmd string, ttl time.Duration) error {
	base, release, err := secretTarget(cmd, &cf, name)
	if err != nil {
		return err
	}
	defer release()

	value, err := resolveExec(cmd.Context(), execCmd)
	if err != nil {
		return err
	}
	defer clear(value)
	// The recipe is saved BEFORE the value is handed over. The other order can
	// leave a daemon entry whose expiry error names a `refresh` with no recipe to
	// re-run; this one, at worst, leaves a recipe with no live value - which is
	// exactly what a daemon restart leaves, and `refresh` alone recovers it.
	//
	// A failed save does NOT abandon the value: the resolver has already spent
	// whatever it spent, so the value still goes to the daemon and the config
	// failure is reported after. Losing it would make a config problem cost a
	// one-time code.
	saveErr := saveSecretExec(name, execCmd)
	if err := putSecret(cmd, base, name, value, sourceExec, ttl); err != nil {
		if saveErr != nil {
			return fmt.Errorf("%w (the resolver could not be saved either: %w)", err, saveErr)
		}
		return fmt.Errorf("%w - the resolver WAS saved, so `cuttle secret refresh %s` can retry it", err, name)
	}
	if saveErr != nil {
		return fmt.Errorf("the value is in the session, but the resolver was not saved, so `cuttle secret refresh %s` will not work: %w", name, saveErr)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "  resolver registered in %s - `cuttle secret refresh %s` re-runs it\n",
		config.DefaultPath(), name)
	return nil
}

func runSecretRefresh(cmd *cobra.Command, cf commonFlags, name string, ttl time.Duration) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	execCmd, ok := cfg.SecretExec(name)
	if !ok {
		return fmt.Errorf("%w: %s has no --exec resolver to re-run; register one with `cuttle secret set %s --exec '...'`, or pipe a new value in with --stdin",
			errSecretNoRecipe, name, name)
	}
	base, release, err := secretTarget(cmd, &cf, name)
	if err != nil {
		return err
	}
	defer release()

	value, err := resolveExec(cmd.Context(), execCmd)
	if err != nil {
		return err
	}
	defer clear(value)
	return putSecret(cmd, base, name, value, sourceExec, ttl)
}

// resolveExec runs a host-side resolver and returns what it printed. It runs
// through a shell because a real resolver is a pipeline
// (`op item get X --format json | jq -r .field`), and it deliberately DISCARDS
// the command's stderr: vault error text routinely quotes item names and partial
// values, and cuttle wrapping it would put exactly that into the transcript this
// feature exists to keep clean.
func resolveExec(ctx context.Context, command string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()
	c := exec.CommandContext(ctx, "sh", "-c", command)
	var out bytes.Buffer
	c.Stdout = &out
	c.Stderr = io.Discard
	// WaitDelay makes the context deadline real: `op` can hang with no output in a
	// headless context, holding the pipe open past the kill.
	c.WaitDelay = 5 * time.Second
	err := c.Run()
	value := trimOneNewline(out.Bytes())
	if err != nil {
		clear(value)
		return nil, fmt.Errorf("%w: %w - run it yourself to see why (cuttle does not capture its stderr, which routinely quotes item names and partial values)",
			errSecretExecFailed, err)
	}
	if len(value) == 0 {
		return nil, errSecretExecEmpty
	}
	return value, nil
}

// saveSecretExec records a resolver in the host config. The value it produced is
// NOT written anywhere - only the recipe.
func saveSecretExec(name, execCmd string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	cfg.SetSecretExec(name, execCmd)
	if err := cfg.Save(config.DefaultPath()); err != nil {
		return fmt.Errorf("saving the resolver: %w", err)
	}
	return nil
}

// putSecret hands one value to the daemon. Every verb that produces a value
// funnels through it, which is why it takes the source: the source is what a
// later stale-value error uses to say how to get a fresh one.
func putSecret(cmd *cobra.Command, base, name string, value []byte, source string, ttl time.Duration) error {
	body := map[string]any{"value": string(value), "source": source}
	if ttl > 0 {
		body["ttl_seconds"] = int(ttl.Seconds())
	}
	var reply struct {
		Name       string `json:"name"`
		Length     int    `json:"length"`
		TTLSeconds int    `json:"ttl_seconds"`
	}
	if err := requestJSON(cmd.Context(), http.MethodPut, base+"/secret/"+url.PathEscape(name), body, &reply); err != nil {
		return fmt.Errorf("storing secret %s: %w", name, err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s  %d bytes  expires in %s\n  "+fillHint+"\n",
		reply.Name, reply.Length, time.Duration(reply.TTLSeconds)*time.Second, reply.Name)
	return nil
}

// readSecretStdin reads a value from a pipe. A single trailing newline is
// stripped - `echo`, `op read` without --no-newline and most shells add one, and
// a password with a stray \n fails a login in a way nothing explains.
func readSecretStdin(in io.Reader) ([]byte, error) {
	value, err := io.ReadAll(io.LimitReader(in, secretValueLimit))
	if err != nil {
		return nil, fmt.Errorf("reading the value from stdin: %w", err)
	}
	value = trimOneNewline(value)
	if len(value) == 0 {
		return nil, errSecretNoInput
	}
	return value, nil
}

// trimOneNewline removes exactly one trailing line ending, never more. TrimRight
// with a cutset strips every trailing \r and \n, so a credential that genuinely
// ends in a blank line came back silently shortened - and the symptom is a login
// that fails with nothing to explain it.
func trimOneNewline(value []byte) []byte {
	value = bytes.TrimSuffix(value, []byte("\n"))
	return bytes.TrimSuffix(value, []byte("\r"))
}

func runSecretList(cmd *cobra.Command, cf commonFlags) error {
	base, release, err := sessionEndpoint(cmd, &cf)
	if err != nil {
		return err
	}
	defer release()

	payload, err := fetchSecrets(cmd.Context(), base)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if len(payload.Secrets) == 0 {
		fmt.Fprintln(out, "no secrets held by this session")
		return nil
	}
	fmt.Fprintln(out, "NAME\tSOURCE\tSTATE\tLENGTH\tEXPIRES IN\tORIGIN")
	for _, s := range payload.Secrets {
		state, expires, length := "expired", "-", "-"
		if s.Live {
			state = "live"
			expires = (time.Duration(s.TTLRemaining) * time.Second).String()
			length = strconv.Itoa(s.Length) + " bytes"
		}
		origin := s.Origin
		if origin == "" {
			origin = "-"
		}
		fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\t%s\n", s.Name, s.Source, state, length, expires, origin)
	}
	return nil
}

// secretListPayload mirrors the daemon's list route. Shape only: there is no
// field here that could carry a value, which is the invariant, not an omission.
type secretListPayload struct {
	Secrets []struct {
		Name         string `json:"name"`
		Source       string `json:"source"`
		Live         bool   `json:"live"`
		Length       int    `json:"length"`
		TTLRemaining int    `json:"ttl_remaining_seconds"`
		Origin       string `json:"origin"`
	} `json:"secrets"`
}

func fetchSecrets(ctx context.Context, base string) (secretListPayload, error) {
	var payload secretListPayload
	if err := getJSON(ctx, base+"/secret", &payload); err != nil {
		return payload, fmt.Errorf("listing secrets: %w", err)
	}
	return payload, nil
}

// secretNames is the briefing's fetch: names only, best-effort. A daemon that
// cannot answer just means the briefing carries no secrets line.
func secretNames(ctx context.Context, ep backend.Endpoint) []string {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	base, _ := endpointURLs(ep)
	payload, err := fetchSecrets(ctx, base)
	if err != nil {
		return nil
	}
	// Live only. The briefing is what tells an agent which sentinels it may type,
	// and an expired entry is deliberately KEPT as a registration - so listing it
	// here advertised names whose fill answers "its value expired" instead. An
	// agent following the briefing over anything it cached deserves a list where
	// every name works.
	names := make([]string, 0, len(payload.Secrets))
	for _, s := range payload.Secrets {
		if s.Live {
			names = append(names, s.Name)
		}
	}
	return names
}

func runSecretPrompt(cmd *cobra.Command, cf commonFlags, name string, ttl time.Duration) error {
	// Whether there is a terminal to ask on is a PRECONDITION, not an
	// acquisition: checking it costs nothing and takes nothing, and doing it
	// first means a piped invocation gets the error that names --stdin instead of
	// one about the daemon.
	tty, err := promptTTY(cmd)
	if err != nil {
		return err
	}
	base, release, err := secretTarget(cmd, &cf, name)
	if err != nil {
		return err
	}
	defer release()

	// Only now is the human actually asked: a code typed at the prompt and then
	// dropped because the daemon was not running is a code that has to be re-sent.
	value, err := readSecretTTY(cmd, tty, name)
	if err != nil {
		return err
	}
	defer clear(value)
	return putSecret(cmd, base, name, value, sourcePrompt, ttl)
}

// promptTTY reports the terminal to ask on. It insists on a real one:
// term.ReadPassword fails with ENOTTY on a pipe, and silently falling back to
// reading that pipe would turn "ask the human" into "read whatever was piped",
// which is what --stdin is for and says so.
func promptTTY(cmd *cobra.Command) (*os.File, error) {
	in, ok := cmd.InOrStdin().(*os.File)
	if !ok || !term.IsTerminal(int(in.Fd())) {
		return nil, errSecretNotATTY
	}
	return in, nil
}

// readSecretTTY reads one value with echo off from the terminal promptTTY
// resolved.
func readSecretTTY(cmd *cobra.Command, in *os.File, name string) ([]byte, error) {
	fmt.Fprintf(cmd.ErrOrStderr(), "value for %s (not echoed): ", name)
	value, err := term.ReadPassword(int(in.Fd()))
	fmt.Fprintln(cmd.ErrOrStderr())
	if err != nil {
		return nil, fmt.Errorf("reading the value: %w", err)
	}
	if len(value) == 0 {
		return nil, errSecretNoInput
	}
	return value, nil
}

func runSecretRemove(cmd *cobra.Command, cf commonFlags, name string) error {
	base, release, err := sessionEndpoint(cmd, &cf)
	if err != nil {
		return err
	}
	defer release()
	// A 404 means the daemon never held it, which is the normal shape when only a
	// recipe existed. Any other failure means it may still HOLD the value - and
	// the recipe must stay put in that case, or the value is live in a daemon
	// with no way left to refresh it.
	daemonErr := requestJSON(cmd.Context(), http.MethodDelete, base+"/secret/"+url.PathEscape(name), nil, nil)
	if daemonErr != nil && !errors.Is(daemonErr, errDaemonNotFound) {
		return fmt.Errorf("removing secret %s from the session: %w", name, daemonErr)
	}
	// Only now the recipe: "forget it" that leaves a resolver behind, ready to be
	// re-run by `refresh`, is not forgetting it.
	dropped, cfgErr := dropSecretExec(name)
	if cfgErr != nil {
		return cfgErr
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "removed %s\n", name)
	if dropped {
		fmt.Fprintf(out, "  its --exec resolver is gone from %s too\n", config.DefaultPath())
	}
	return nil
}

// dropSecretExec removes a name's resolver from the host config, reporting
// whether there was one to remove.
func dropSecretExec(name string) (bool, error) {
	cfg, err := config.Load()
	if err != nil {
		return false, err
	}
	if !cfg.RemoveSecret(name) {
		return false, nil
	}
	if err := cfg.Save(config.DefaultPath()); err != nil {
		return false, fmt.Errorf("removing the resolver: %w", err)
	}
	return true, nil
}

// daemonRequest performs one request against the daemon and returns its body.
// Every CLI-to-daemon call goes through here: the non-200 shape is the daemon's
// `{"error": ...}` envelope, and decoding it in one place is what lets a caller
// tell "no such thing" from "the daemon is gone".
func daemonRequest(ctx context.Context, method, endpoint string, body any, limit int64, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encoding request: %w", err)
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, payload)
	if err != nil {
		return nil, err //nolint:wrapcheck
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err //nolint:wrapcheck
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, err //nolint:wrapcheck
	}
	if resp.StatusCode == http.StatusOK {
		return data, nil
	}
	// The daemon's own message leads. Prefixing the internal URL put ~50 characters
	// of endpoint in front of every explanation - so a capture that failed because
	// a selector matched nothing opened with a loopback URL and an (HTTP 502) that
	// reads like infrastructure, and the sentence that says what to do next started
	// past the point an agent stops reading.
	return nil, daemonError(resp.StatusCode, data)
}

// daemonError turns a non-200 into a readable error, preferring the daemon's own
// message. A 404 is wrapped in errDaemonNotFound so a caller can act on it.
func daemonError(status int, data []byte) error {
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(data, &e)
	msg := e.Error
	if msg == "" {
		msg = strings.TrimSpace(string(data))
	}
	if status == http.StatusNotFound {
		return fmt.Errorf("%w: %s", errDaemonNotFound, msg)
	}
	if msg == "" {
		return fmt.Errorf("HTTP %d", status) //nolint:err113
	}
	return fmt.Errorf("%s (HTTP %d)", msg, status) //nolint:err113
}

// requestJSON is daemonRequest for a route that answers with JSON.
func requestJSON(ctx context.Context, method, endpoint string, body, out any) error {
	data, err := daemonRequest(ctx, method, endpoint, body, jsonReplyLimit, daemonTimeout)
	if err != nil || out == nil {
		return err
	}
	return json.Unmarshal(data, out) //nolint:wrapcheck
}
