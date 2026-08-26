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
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
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

// secretValueLimit caps what `set` will read from stdin. A credential is a
// credential, not a payload.
const secretValueLimit = 64 << 10

// fillHint is the one line that turns "cuttle holds a secret" into something an
// agent can act on, printed after every successful set because that is the moment
// the name is in front of the reader.
const fillHint = "type {{cuttle:%s}} as the WHOLE value in any driver's fill"

var (
	errSecretNoInput   = errors.New("no value on stdin: pipe it in, e.g. `op read op://vault/item/password | cuttle secret set NAME --stdin`")
	errSecretNeedStdin = errors.New("pass --stdin and pipe the value in: `... | cuttle secret set NAME --stdin`")
)

func init() { AddCommand(newSecretCmd()) }

func newSecretCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "hand the session a credential to type, without it entering argv or a transcript",
		Long: `Store a credential in the running session's memory under a name, then have a
driver type it by name instead of by value:

  op read op://vault/github/password | cuttle secret set GH_PASS --stdin
  playwright-cli fill e17 '{{cuttle:GH_PASS}}'

The sentinel works in every driver and every call shape, because the
substitution happens inside cuttle's CDP proxy rather than in the driver. The
value lives in daemon memory only - never on disk, never in a log, never
printed back - and expires on its own.`,
	}
	cmd.AddCommand(newSecretSetCmd(), newSecretListCmd(), newSecretRemoveCmd(), newSecretAllowLiteralCmd())
	return cmd
}

func newSecretSetCmd() *cobra.Command {
	var cf commonFlags
	var stdin bool
	var ttl time.Duration
	cmd := &cobra.Command{
		Use:   "set NAME --stdin",
		Short: "store a value the session can type by name (value on stdin, never argv)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !stdin {
				return errSecretNeedStdin
			}
			return runSecretSet(cmd, cf, args[0], ttl)
		},
	}
	addCommonFlags(cmd, &cf)
	cmd.Flags().BoolVar(&stdin, "stdin", false, "read the value from standard input (required)")
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
		Short:   "forget a secret entirely",
		Args:    cobra.ExactArgs(1),
		RunE:    func(cmd *cobra.Command, args []string) error { return runSecretRemove(cmd, cf, args[0]) },
	}
	addCommonFlags(cmd, &cf)
	return cmd
}

func newSecretAllowLiteralCmd() *cobra.Command {
	var cf commonFlags
	var ttl time.Duration
	cmd := &cobra.Command{
		Use:   "allow-literal",
		Short: "let the NEXT fill type a literal into a password or one-time-code field",
		Long: `cuttle refuses a literal typed into a password or one-time-code field, because
that is how a credential ends up in a driver command line and an agent
transcript. This arms a single-use exemption for the next such fill on this
session - it is consumed by that fill, it expires on its own (default 60s), and
its use is logged. There is deliberately no persistent form.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runSecretAllowLiteral(cmd, cf, ttl) },
	}
	addCommonFlags(cmd, &cf)
	cmd.Flags().DurationVar(&ttl, "ttl", 0, "how long the exemption stays armed if unused (default 60s)")
	return cmd
}

// secretEndpoint resolves the running session's loopback base URL, the same way
// downloads does. Every secret route sits behind the daemon's loopback guard.
func secretEndpoint(cmd *cobra.Command, cf *commonFlags) (string, func(), error) {
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
	value, err := readSecretStdin(cmd.InOrStdin())
	if err != nil {
		return err
	}
	defer clear(value)

	base, release, err := secretEndpoint(cmd, &cf)
	if err != nil {
		return err
	}
	defer release()
	return putSecret(cmd, base, name, value, "stdin", ttl)
}

// putSecret hands one value to the daemon. Shared by `set` and (from Phase 2)
// `refresh`, which is why it takes the source rather than assuming stdin.
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
	value = bytes.TrimRight(value, "\r\n")
	if len(value) == 0 {
		return nil, errSecretNoInput
	}
	return value, nil
}

func runSecretList(cmd *cobra.Command, cf commonFlags) error {
	base, release, err := secretEndpoint(cmd, &cf)
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
	if payload.AllowLiteral {
		fmt.Fprintln(out, "note: `cuttle secret allow-literal` is armed - the next credential-field fill may type a literal")
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
	AllowLiteral bool `json:"allow_literal"`
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
func secretNames(ctx context.Context, base string) []string {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	payload, err := fetchSecrets(ctx, base)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(payload.Secrets))
	for _, s := range payload.Secrets {
		names = append(names, s.Name)
	}
	return names
}

func runSecretRemove(cmd *cobra.Command, cf commonFlags, name string) error {
	base, release, err := secretEndpoint(cmd, &cf)
	if err != nil {
		return err
	}
	defer release()
	if err := requestJSON(cmd.Context(), http.MethodDelete, base+"/secret/"+url.PathEscape(name), nil, nil); err != nil {
		return fmt.Errorf("removing secret %s: %w", name, err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "removed %s\n", name)
	return nil
}

func runSecretAllowLiteral(cmd *cobra.Command, cf commonFlags, ttl time.Duration) error {
	base, release, err := secretEndpoint(cmd, &cf)
	if err != nil {
		return err
	}
	defer release()
	body := map[string]any{}
	if ttl > 0 {
		body["ttl_seconds"] = int(ttl.Seconds())
	}
	var reply struct {
		TTLSeconds int `json:"ttl_seconds"`
	}
	if err := requestJSON(cmd.Context(), http.MethodPost, base+"/secret/allow-literal", body, &reply); err != nil {
		return fmt.Errorf("arming the literal-fill exemption: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(),
		"armed for one fill, expiring in %s - the next literal typed into a password or one-time-code field is allowed\n",
		time.Duration(reply.TTLSeconds)*time.Second)
	return nil
}

// requestJSON performs one JSON request against the daemon and decodes its reply.
// It exists because getJSON is GET-only and every write here is a PUT, a POST or
// a DELETE carrying a body no query string may ever hold.
func requestJSON(ctx context.Context, method, endpoint string, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding request: %w", err)
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, payload)
	if err != nil {
		return err //nolint:wrapcheck
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err //nolint:wrapcheck
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err //nolint:wrapcheck
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Error != "" {
			return fmt.Errorf("%s (HTTP %d)", e.Error, resp.StatusCode) //nolint:err113
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data))) //nolint:err113
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out) //nolint:wrapcheck
}
