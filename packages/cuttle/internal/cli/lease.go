package cli

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/glim-sh/cuttle/internal/backend"
)

const (
	// flagTakeover is how a driver takes the browser from whoever holds its lease.
	flagTakeover    = "--takeover"
	leaseParamOwner = "owner"
	// curlConnectFailed is curl's exit status for a refused connection.
	curlConnectFailed = 7
	// daemonBootWait bounds how long a lease call waits for a starting daemon.
	daemonBootWait = 20 * time.Second
	leaseURL       = playwrightCDPEndpoint + "/lease"
)

var (
	errSessionLeased    = errors.New("the browser is already being driven")
	errSessionTakenOver = errors.New("session was taken over")
)

// leaseReply is the daemon's lease payload: a grant, a status, or a refusal.
type leaseReply struct {
	Held       bool   `json:"held"`
	Owner      string `json:"owner"`
	Token      string `json:"token"`
	AgeSeconds int    `json:"age_seconds"`
	TTLSeconds int    `json:"ttl_seconds"`
	Error      string `json:"error"`
}

// leaseCall makes one request to the daemon's /lease with curl INSIDE the
// container, over the same exec the driver runs through. The gate then needs no
// published port or tunnel - exactly what `cuttle pw` itself needs - and works
// on every backend the driver does. A 404 means a daemon that predates leases.
func leaseCall(ctx context.Context, ex backend.Execer, method string, q url.Values) (int, leaseReply, error) {
	target := leaseURL
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	var out, errOut bytes.Buffer
	argv := []string{"curl", "-sS", "-X", method, "-w", "\n%{http_code}", target}
	// Right after a container (re)start the daemon refuses connections for a few
	// seconds - curl's exit 7 - so that is waited out instead of failing the verb.
	deadline := time.Now().Add(daemonBootWait)
	for {
		out.Reset()
		errOut.Reset()
		err := execIn(ctx, nil, ex, "/", argv, &out, &errOut)
		if err == nil {
			break
		}
		ee, ok := errors.AsType[*exec.ExitError](err)
		if !ok || ee.ExitCode() != curlConnectFailed || time.Now().After(deadline) || ctx.Err() != nil {
			// docker prints its own exec failures on stdout, where curl's is only -w.
			reason := cmp.Or(strings.TrimSpace(errOut.String()), strings.TrimSpace(out.String()))
			return 0, leaseReply{}, fmt.Errorf("reaching the session lease: %w: %s", err, reason)
		}
		select {
		case <-ctx.Done():
		case <-time.After(500 * time.Millisecond):
		}
	}
	body := strings.TrimRight(out.String(), "\n")
	codeAt := strings.LastIndexByte(body, '\n')
	code, _ := strconv.Atoi(body[codeAt+1:])
	var reply leaseReply
	if codeAt >= 0 {
		_ = json.Unmarshal([]byte(body[:codeAt]), &reply)
	}
	return code, reply, nil
}

// leaseOwner labels a lease holder for the person reading a refusal.
func leaseOwner(tool string) string {
	host, _ := os.Hostname()
	return fmt.Sprintf("%s %d@%s", tool, os.Getpid(), host)
}

func heldError(r leaseReply, remedy string) error {
	age := time.Duration(r.AgeSeconds) * time.Second
	return fmt.Errorf("%w by %s (for %s) - %s", errSessionLeased, r.Owner, age, remedy)
}

// forceRelease takes the lease from whoever holds it, recording owner as the
// taker so the evicted holder can say who took over.
func forceRelease(ctx context.Context, ex backend.Execer, owner string) error {
	code, r, err := leaseCall(ctx, ex, http.MethodDelete, url.Values{"force": {"true"}, leaseParamOwner: {owner}})
	if err != nil {
		return err
	}
	if code != http.StatusOK && code != http.StatusNotFound {
		return fmt.Errorf("taking over the session: %s (HTTP %d)", r.Error, code) //nolint:err113 // the daemon's own reason
	}
	return nil
}

// sessionLease is a held driving lease. A zero token means the daemon predates
// leases, and every method is then a no-op.
type sessionLease struct {
	ex    backend.Execer
	owner string
	token string
	ttl   time.Duration
}

// acquireLease claims the browser for owner, first taking it from its holder
// when takeover is set. A live foreign lease is refused naming the holder.
func acquireLease(ctx context.Context, ex backend.Execer, owner string, takeover bool) (*sessionLease, error) {
	if takeover {
		if err := forceRelease(ctx, ex, owner); err != nil {
			return nil, err
		}
	}
	code, r, err := leaseCall(ctx, ex, http.MethodPost, url.Values{leaseParamOwner: {owner}})
	switch {
	case err != nil:
		return nil, err
	case code == http.StatusNotFound:
		return &sessionLease{ex: ex, owner: owner}, nil
	case code == http.StatusConflict:
		return nil, heldError(r, "rerun with "+flagTakeover+" to take it over")
	case code != http.StatusOK || r.Token == "" || r.TTLSeconds <= 0:
		return nil, fmt.Errorf("acquiring the session lease: %s (HTTP %d)", r.Error, code) //nolint:err113 // the daemon's own reason
	}
	return &sessionLease{ex: ex, owner: owner, token: r.Token, ttl: time.Duration(r.TTLSeconds) * time.Second}, nil
}

// renew extends the lease. A renew the daemon refuses means someone took the
// browser over, and is returned as that; a renew that merely fails to arrive is
// not, since the lease outlives two missed heartbeats.
func (l *sessionLease) renew(ctx context.Context) error {
	if l.token == "" {
		return nil
	}
	code, r, err := leaseCall(ctx, l.ex, http.MethodPost, url.Values{leaseParamOwner: {l.owner}, "token": {l.token}})
	if err != nil || code != http.StatusConflict {
		return nil //nolint:nilerr // a lost renew is retried; only a refusal ends the run
	}
	if r.Owner == "" {
		return errSessionTakenOver
	}
	return fmt.Errorf("%w by %s", errSessionTakenOver, r.Owner)
}

// heartbeat renews the lease every third of its TTL until ctx ends, so a long
// model call cannot let it lapse, and cancels the run when a renew is refused.
func (l *sessionLease) heartbeat(ctx context.Context, cancel context.CancelCauseFunc) {
	if l.token == "" {
		return
	}
	tick := time.NewTicker(l.ttl / 3)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if err := l.renew(ctx); err != nil {
			cancel(err)
			return
		}
	}
}

// guard renews the lease right before each verb that drives the page, so a run
// that was taken over stops before its next action rather than at the next
// heartbeat, up to a third of the TTL later.
func (l *sessionLease) guard(drive playwrightRunner, cancel context.CancelCauseFunc) playwrightRunner {
	return func(ctx context.Context, args ...string) (string, error) {
		if !playwrightReadOnly(args) {
			if err := l.renew(ctx); err != nil {
				cancel(err)
				return "", err
			}
		}
		return drive(ctx, args...)
	}
}

// release frees the lease. It runs on the way out, often after the run's context
// is already canceled, so it carries its own short deadline.
func (l *sessionLease) release() {
	if l.token == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _, _ = leaseCall(ctx, l.ex, http.MethodDelete, url.Values{"token": {l.token}})
}

// playwrightReadVerbs are the driver verbs that only read, so they pass while
// someone else holds the lease. Anything not listed - a new verb included - is
// treated as driving the page.
var playwrightReadVerbs = map[string]bool{
	"snapshot": true, "find": true, "console": true, "screenshot": true, "pdf": true,
	"tab-list": true, "list": true, "route-list": true, "webmcp-list": true, "generate-locator": true,
	"cookie-list": true, "cookie-get": true,
	"localstorage-list": true, "localstorage-get": true,
	"sessionstorage-list": true, "sessionstorage-get": true,
	"requests": true, "request": true, "request-headers": true, "request-body": true,
	"response-headers": true, "response-body": true,
}

// playwrightReadOnly reports whether a pw invocation only reads. The verb is the
// first non-flag argument. A help flag makes the whole invocation a help request
// wherever the driver's parser (minimist) reads it as one - anywhere before a
// `--` - but after it, it is a value to type or fill.
func playwrightReadOnly(args []string) bool {
	flags := args
	if end := slices.Index(args, "--"); end >= 0 {
		flags = args[:end]
	}
	if slices.ContainsFunc(flags, isHelpFlag) {
		return true
	}
	for _, a := range flags {
		switch {
		case !strings.HasPrefix(a, "-"):
			return playwrightReadVerbs[a]
		case strings.Contains(a, "=") || playwrightBoolFlags[a]:
			// Takes no next arg, so the verb is still ahead.
		default:
			// A flag that takes a value swallows the next arg, so `-s snapshot click`
			// runs click: the word after it is not the verb.
			return false
		}
	}
	// Flags only, such as --version. A verb seen only after a `--` is refused
	// rather than guessed at.
	return len(flags) == len(args)
}

// playwrightBoolFlags are the boolean flags an invocation puts in front of its
// verb, spelled exactly: minimist reads `-json` as `-j -s -o -n`, and the last
// of those takes the next arg. Any other bare flag there is taken to swallow the
// next arg, which only ever errs toward refusing.
var playwrightBoolFlags = map[string]bool{"--json": true, "--raw": true, "--version": true, "--all": true, "-g": true, "--global": true}

// gatePlaywright refuses a verb that drives the page while another client holds
// the lease. It only ever decides whether the command runs, never changes it.
// self is the cuttle invocation that reaches this instance ([cuttleCmd]), so the
// takeover it suggests lands on the same browser.
func gatePlaywright(ctx context.Context, ex backend.Execer, self string, args []string, takeover bool) error {
	// A read verb needs no lease, so --takeover in front of one - `--help`
	// included - would evict a running driver for a command that never drives.
	if playwrightReadOnly(args) {
		return nil
	}
	if takeover {
		return forceRelease(ctx, ex, leaseOwner("cuttle pw"))
	}
	code, r, err := leaseCall(ctx, ex, http.MethodGet, nil)
	if err != nil {
		return err
	}
	if code == http.StatusOK && r.Held {
		// The args are not echoed: a fill value may be a password.
		return heldError(r, "rerun as `"+self+" pw "+flagTakeover+" <verb> ...` to take it over")
	}
	return nil
}

const (
	// leaseUnsettledMarker and leaseUnsettledExit are how leaseGatedArgv reports,
	// on stderr and in its exit status, that it did not run the verb. The driver
	// only exits 0 or 1; the marker is required too, since taking a verb that ran
	// for one that did not would run it twice.
	leaseUnsettledMarker = "cuttle: lease not confirmed free"
	leaseUnsettledExit   = 75
)

// leaseGatedArgv wraps a driving verb in the lease check, run in the same exec as
// the verb so the check costs no process of its own. Only a daemon answering
// that the lease is free, or one predating leases (404), runs the verb; anything
// else - held, unreachable, still booting, hung, no curl - exits unrun for
// gatePlaywright to decide, with its retries and its refusal wording.
// The script is one line because ssh hands it to the remote login shell, and csh
// rejects a newline inside quotes. The curl is bounded because the exec outlives
// a Ctrl-C'd client: a check stuck on a hung daemon must not run the verb later.
func leaseGatedArgv(argv []string) []string {
	script := `case $(curl -s --max-time 5 -w ' %{http_code}' ` + leaseURL + ` 2>/dev/null) in ` +
		`'{"held":false}'*' 200' | *' 404') exec "$@" ;; esac; ` +
		`echo "` + leaseUnsettledMarker + `" >&2; exit ` + strconv.Itoa(leaseUnsettledExit)
	return append([]string{"sh", "-c", script, "sh"}, argv...)
}

// leaseUnsettled reports whether leaseGatedArgv stopped short of the verb: its
// exit status, nothing on stdout, and its marker as the last line on stderr.
func leaseUnsettled(err error, stdout, stderr string) bool {
	ee, ok := errors.AsType[*exec.ExitError](err)
	return ok && ee.ExitCode() == leaseUnsettledExit && stdout == "" &&
		strings.HasSuffix(strings.TrimRight(stderr, "\n"), leaseUnsettledMarker)
}
