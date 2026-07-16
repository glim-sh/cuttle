// Package backend obtains a reachable local CDP/VNC endpoint for a browser that
// runs in one of four places: a local docker container, a Kubernetes Deployment,
// docker on an ssh host, or a pre-exposed direct URL. Every CDP/VNC-facing
// operation runs against the Endpoint a backend yields, so the rest of cuttle is
// transport-agnostic.
package backend

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"

	"github.com/glim-sh/cuttle/packages/cuttle-go/internal/config"
)

// State is a browser's lifecycle state as a backend sees it.
type State string

const (
	StateRunning State = "running"
	StateStopped State = "stopped"
	StateAbsent  State = "absent"
)

// Endpoint is a reachable CDP (and optional VNC) address. For tunneled backends
// the host is loopback and the ports are auto-picked local forwards; for direct
// it is the configured host/port as-is.
type Endpoint struct {
	CDPHost string
	CDPPort int
	VNCHost string
	VNCPort int // 0 = no VNC
}

// StartOpts carries the per-invocation choices for Start. Not every field
// applies to every backend (e.g. IdleTimeout is local-only, Recreate is docker-
// only); a backend ignores what it does not use.
type StartOpts struct {
	Image       string
	Recreate    bool
	KeepProfile *bool // nil = backend default (on)
	NoVNC       bool
	Proxy       string
	IdleTimeout string // local only
	Storage     string // profile storage: "local" | "remote"
}

// Backend manages one browser's lifecycle and reachability.
type Backend interface {
	State(ctx context.Context) (State, error)
	Start(ctx context.Context, opts StartOpts) error
	Stop(ctx context.Context, purge bool) error
	// Reach yields a local endpoint plus a release func that tears down any
	// tunnel opened to reach it. release is always safe to call (no-op for
	// direct/local).
	Reach(ctx context.Context) (Endpoint, func(), error)
}

var errNotManaged = errors.New("not managed by cuttle")

// New builds the backend for a resolved context. Ports are the host-side CDP/VNC
// ports for the local backend (and the remote container ports for ssh).
func New(name string, ctx config.Context, r Runner, cdpPort, vncPort int, image string) (Backend, error) {
	switch ctx.Backend {
	case config.BackendLocal, "":
		return &Local{runner: r, name: name, cdpPort: cdpPort, vncPort: vncPort, image: image}, nil
	case config.BackendK8s:
		return newK8s(ctx, r), nil
	case config.BackendSSH:
		return &SSH{runner: r, host: ctx.Host, name: name, cdpPort: cdpPort, vncPort: vncPort, image: image, proxy: ctx.Proxy}, nil
	case config.BackendDirect:
		return newDirect(ctx)
	default:
		return nil, fmt.Errorf("unknown backend %q for context %q", ctx.Backend, name)
	}
}

// freePort picks a free local TCP port by binding :0 and releasing it. There is
// an inherent race between release and reuse, but it makes a forward collision
// with an existing local container on a fixed port (9222) vanishingly unlikely.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("picking free port: %w", err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func requireExe(r Runner, exe, hint string) error {
	if _, err := r.LookPath(exe); err != nil {
		return fmt.Errorf("%s not found on PATH - %s", exe, hint) //nolint:err113 // operator-facing detail
	}
	return nil
}

func portStr(p int) string { return strconv.Itoa(p) }
