package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/glim-sh/cuttle/internal/backend"
)

const (
	// flagTakeover is how a driver takes the browser from whoever holds its lease.
	flagTakeover    = "--takeover"
	leaseParamOwner = "owner"
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
	target := playwrightCDPEndpoint + "/lease"
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	var out, errOut bytes.Buffer
	argv := []string{"curl", "-sS", "-X", method, "-w", "\n%{http_code}", target}
	if err := execPlaywright(ctx, nil, ex, argv, &out, &errOut); err != nil {
		return 0, leaseReply{}, fmt.Errorf("reaching the session lease: %w: %s", err, strings.TrimSpace(errOut.String()))
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

// heartbeat renews the lease every third of its TTL until ctx ends. A renew the
// daemon refuses means someone took the browser over, and cancels the run with
// that as the cause; a renew that merely fails to arrive is retried, since the
// lease outlives two missed beats.
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
		code, r, err := leaseCall(ctx, l.ex, http.MethodPost, url.Values{leaseParamOwner: {l.owner}, "token": {l.token}})
		if err == nil && code == http.StatusConflict {
			if r.Owner == "" {
				cancel(errSessionTakenOver)
			} else {
				cancel(fmt.Errorf("%w by %s", errSessionTakenOver, r.Owner))
			}
			return
		}
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
	"help": true, "docs": true,
}

// playwrightReadOnly reports whether a pw invocation only reads. The verb is the
// first non-flag argument; a help flag anywhere makes it a help request.
func playwrightReadOnly(args []string) bool {
	for _, a := range args {
		if a == "-h" || a == "--help" {
			return true
		}
	}
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return playwrightReadVerbs[a]
		}
	}
	return true // flags only, such as --version
}

// gatePlaywright refuses a verb that drives the page while another client holds
// the lease. It only ever decides whether the command runs, never changes it.
func gatePlaywright(ctx context.Context, ex backend.Execer, args []string, takeover bool) error {
	if takeover {
		return forceRelease(ctx, ex, leaseOwner("cuttle pw"))
	}
	if playwrightReadOnly(args) {
		return nil
	}
	code, r, err := leaseCall(ctx, ex, http.MethodGet, nil)
	if err != nil {
		return err
	}
	if code == http.StatusOK && r.Held {
		// The args are not echoed: a fill value may be a password.
		return heldError(r, "rerun as `cuttle pw "+flagTakeover+" <verb> ...` to take it over")
	}
	return nil
}
