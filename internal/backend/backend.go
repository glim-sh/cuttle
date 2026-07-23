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
	"strings"

	"github.com/glim-sh/cuttle/internal/config"
)

// Shared literals for the docker/kubectl/ssh command construction.
const (
	loopbackHost = "127.0.0.1"
	dockerExe    = "docker"
	helmInstall  = "--install"
)

// State is a browser's lifecycle state as a backend sees it.
type State string

const (
	StateRunning State = "running"
	StateStopped State = "stopped"
	StateAbsent  State = "absent"
)

// dockerStatusState maps `docker inspect -f {{.State.Status}}` output and its
// exit code to a State: a non-zero exit or empty status is an absent container,
// "running" is running, anything else is stopped. Shared by the local and ssh
// backends.
func dockerStatusState(status string, code int) State {
	if code != 0 {
		return StateAbsent
	}
	switch strings.TrimSpace(status) {
	case "":
		return StateAbsent
	case string(StateRunning):
		return StateRunning
	default:
		return StateStopped
	}
}

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
// applies to every backend (e.g. Recreate is docker-only); a backend ignores
// what it does not use.
type StartOpts struct {
	Image    string
	Recreate bool
	// KeepProfile is the legacy override for the durable-profile behavior:
	// nil = backend default (persist), false = ephemeral. --ephemeral is the
	// preferred opt-out and sets Ephemeral instead. See persistProfile.
	KeepProfile *bool
	// Ephemeral opts out of the persistent default profile: no named volume, a
	// fresh scratch profile that is discarded on recreate/down --purge.
	Ephemeral bool
	// PurgeProfile resets the persistent profile before (re)creating the
	// container/pod: it drops the named volume (docker local/ssh) or the PVC (k8s)
	// so the next start begins from a clean profile.
	PurgeProfile bool
	Proxy        string
	IdleTimeout  string // seconds of idle before a per-seed browser is reaped; "" = off
	// Humanize overrides behavioral input humanization: nil = daemon default (on),
	// &false = disable (CUTTLE_HUMANIZE=0), &true = force on. Only the disable case
	// is passed through, since the daemon defaults humanize on.
	Humanize *bool
}

// Persistent reports whether the default profile is durable (a named volume /
// PVC mounted at the container's data dir, plus CUTTLE_KEEP_PROFILE=1 so the
// daemon treats it as the source of truth). Persist-by-default: --ephemeral (or
// the legacy --keep-profile=false) opts out. It is the single source of truth
// for the persist decision - the CLI and every backend derive from it.
func (o StartOpts) Persistent() bool {
	if o.Ephemeral {
		return false
	}
	if o.KeepProfile != nil {
		return *o.KeepProfile
	}
	return true
}

// Backend manages one browser's lifecycle and reachability.
type Backend interface {
	State(ctx context.Context) (State, error)
	Start(ctx context.Context, opts StartOpts) error
	Stop(ctx context.Context, purge bool) error
	// Reach yields a local endpoint plus a release func that tears down any
	// tunnel opened to reach it. release is always safe to call (no-op for
	// direct/local). cdpPort/vncPort request specific local ports for a tunneled
	// backend (k8s/ssh) so a held forward is deterministic and a driver can attach
	// to it; 0 auto-picks a free port. The forward Reach opens is ephemeral by
	// design - it lives only until release is called (the CLI exit) - and is the
	// internal fallback for the short-lived open/login flows. A backend that also
	// implements Tunneler (k8s/ssh) additionally offers a detached, standing
	// forward that outlives the CLI (see tunnel.go); that is what up/status
	// advertise so the briefing endpoint is stable across invocations.
	Reach(ctx context.Context, cdpPort, vncPort int) (Endpoint, func(), error)
}

// ProfilePurger removes the persistent profile's backing store - the named Docker
// volume on local/ssh, the PVC on k8s - so the next start begins from a clean
// profile. Implemented by local, ssh, and k8s; the direct backend has no store
// cuttle manages.
type ProfilePurger interface {
	PurgeProfileVolume(ctx context.Context) error
}

var (
	errNotManaged     = errors.New("not managed by cuttle")
	errUnknownBackend = errors.New("unknown backend")
	errNoTCPAddr      = errors.New("listener address is not TCP")
)

// DefaultContainerName is the docker container name used when --name is not set.
// The CLI's default flows through here so a caller can distinguish the default
// from an explicit --name (which keys a per-instance tunnel identity below).
const DefaultContainerName = "cuttle"

// New builds the backend for a resolved context. Ports are the host-side CDP/VNC
// ports for the local backend (and the remote container ports for ssh). ctxName
// is the resolved context name; tunneled backends use it as the standing-tunnel
// pidfile identity - a non-default container name (--name) is folded in so two
// named instances sharing one ssh context get distinct tunnels.
func New(name, ctxName string, ctx config.Context, r Runner, cdpPort, vncPort int, image string) (Backend, error) {
	switch ctx.Backend {
	case config.BackendLocal, "":
		return &Local{runner: r, name: name, cdpPort: cdpPort, vncPort: vncPort, image: image}, nil
	case config.BackendK8s:
		k := newK8s(ctx, r)
		k.tunnelContext = ctxName
		k.image = image
		return k, nil
	case config.BackendSSH:
		tunnelID := ctxName
		if name != DefaultContainerName {
			tunnelID = ctxName + "-" + name
		}
		return &SSH{runner: r, host: ctx.Host, name: name, cdpPort: cdpPort, vncPort: vncPort, image: image, proxy: ctx.Proxy, tunnelContext: tunnelID}, nil
	case config.BackendDirect:
		return newDirect(ctx)
	default:
		return nil, fmt.Errorf("%w %q for context %q", errUnknownBackend, ctx.Backend, name)
	}
}

// freePort picks a free local TCP port by binding :0 and releasing it. There is
// an inherent race between release and reuse, but it makes a forward collision
// with an existing local container on a fixed port (9222) vanishingly unlikely.
func freePort() (int, error) {
	var lc net.ListenConfig
	l, err := lc.Listen(context.Background(), "tcp", loopbackHost+":0")
	if err != nil {
		return 0, fmt.Errorf("picking free port: %w", err)
	}
	defer func() { _ = l.Close() }()
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		return 0, errNoTCPAddr
	}
	return addr.Port, nil
}

// chooseLocalPort returns the requested local port, or a free one when preferred
// is 0.
func chooseLocalPort(preferred int) (int, error) {
	if preferred > 0 {
		return preferred, nil
	}
	return freePort()
}

func requireExe(r Runner, exe, hint string) error {
	if _, err := r.LookPath(exe); err != nil {
		return fmt.Errorf("%s not found on PATH - %s", exe, hint) //nolint:err113 // operator-facing detail
	}
	return nil
}

func portStr(p int) string { return strconv.Itoa(p) }
