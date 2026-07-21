package backend

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/glim-sh/cuttle/internal/xdg"
)

// Tunneler is implemented by backends that reach the browser through a local
// forward (ssh, k8s). Unlike Reach's ephemeral forward, EnsureTunnel establishes
// a detached, long-lived forward on fixed ports that outlives the CLI process, so
// the briefing can advertise a stable 127.0.0.1 endpoint across invocations.
// local/direct do not implement it (their endpoint is already stable and needs
// no process).
type Tunneler interface {
	// EnsureTunnel returns a stable local endpoint, (re)spawning the detached
	// forward on the given ports when none is healthy.
	EnsureTunnel(ctx context.Context, cdpPort, vncPort int) (Endpoint, error)
	// TunnelHealthy reports whether a standing forward is up on cdpPort.
	TunnelHealthy(ctx context.Context, cdpPort int) bool
	// StopTunnel tears down the standing forward, if any.
	StopTunnel() error
}

// tunnelSpec is the recipe for a standing forward, identified by context name.
// The pidfile bookkeeping below is identical for ssh and k8s; only the argv
// (and executable) differ.
type tunnelSpec struct {
	context string
	name    string   // executable to spawn ("ssh" / "kubectl")
	args    []string // its argv
	cdpPort int
	vncPort int
}

var (
	errTunnelStart = errors.New("starting tunnel")
	errNoStateDir  = errors.New("cannot resolve a state dir (no XDG_STATE_HOME and no home dir)")
)

// ensureTunnel returns the stable endpoint, spawning a fresh detached forward
// (and recording its pid) when the current one is not healthy. The spawned
// process is deliberately not tied to ctx: it must outlive this CLI invocation.
func ensureTunnel(ctx context.Context, spec tunnelSpec) (Endpoint, error) {
	if tunnelHealthy(ctx, spec.context, spec.cdpPort) {
		return tunnelEndpoint(spec), nil
	}
	// Clear any stale process/pidfile before respawning.
	_ = stopTunnel(spec.context)

	pid, err := spawnTunnel(spec)
	if err != nil {
		return Endpoint{}, err
	}
	if err := writePidfile(spec.context, pid); err != nil {
		return Endpoint{}, err
	}
	// Give the forward a moment to bind before the caller probes CDP.
	waitPortListening(ctx, spec.cdpPort, 5*time.Second)
	return tunnelEndpoint(spec), nil
}

func tunnelEndpoint(spec tunnelSpec) Endpoint {
	return Endpoint{CDPHost: loopbackHost, CDPPort: spec.cdpPort, VNCHost: loopbackHost, VNCPort: spec.vncPort}
}

// tunnelHealthy reports whether the pidfile names a live process whose local CDP
// forward accepts connections. A stale pidfile (dead pid or unbound port) reports
// false so the caller re-establishes.
func tunnelHealthy(ctx context.Context, contextName string, cdpPort int) bool {
	pid, ok := readPidfile(contextName)
	if !ok || !processAlive(pid) {
		return false
	}
	return portListening(ctx, cdpPort)
}

// stopTunnel signals the standing forward (if alive) and removes its pidfile. It
// is idempotent and safe to call when no tunnel exists.
func stopTunnel(contextName string) error {
	if pid, ok := readPidfile(contextName); ok && processAlive(pid) {
		_ = killTunnel(pid)
	}
	if path, err := tunnelPidfile(contextName); err == nil {
		_ = os.Remove(path)
	}
	return nil
}

func spawnTunnel(spec tunnelSpec) (int, error) {
	logFile, err := openTunnelLog(spec.context)
	if err != nil {
		return 0, err
	}
	defer func() { _ = logFile.Close() }()

	// Not exec.CommandContext: the forward must survive this CLI's exit.
	cmd := exec.Command(spec.name, spec.args...) //nolint:gosec,noctx // detached forward must outlive the CLI context; argv is from resolved context config
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("%w: %w", errTunnelStart, err)
	}
	pid := cmd.Process.Pid
	// Release so this process does not linger as the child's waiter.
	_ = cmd.Process.Release()
	return pid, nil
}

func portListening(ctx context.Context, port int) bool {
	d := net.Dialer{Timeout: 500 * time.Millisecond}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(loopbackHost, portStr(port)))
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func waitPortListening(ctx context.Context, port int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if portListening(ctx, port) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// stateDir is $XDG_STATE_HOME/cuttle (default ~/.local/state/cuttle), created if
// absent. It holds the per-context tunnel pidfiles and logs.
func stateDir() (string, error) {
	base := xdg.StateDir()
	if base == "" {
		return "", errNoStateDir
	}
	dir := filepath.Join(base, "cuttle")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating state dir: %w", err)
	}
	return dir, nil
}

func tunnelPidfile(contextName string) (string, error) {
	dir, err := stateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "tunnel-"+safeFilename(contextName)+".pid"), nil
}

func openTunnelLog(contextName string) (*os.File, error) {
	dir, err := stateDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "tunnel-"+safeFilename(contextName)+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening tunnel log: %w", err)
	}
	return f, nil
}

func readPidfile(contextName string) (int, bool) {
	path, err := tunnelPidfile(contextName)
	if err != nil {
		return 0, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

func writePidfile(contextName string, pid int) error {
	path, err := tunnelPidfile(contextName)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)), 0o600); err != nil {
		return fmt.Errorf("writing pidfile: %w", err)
	}
	return nil
}

// safeFilename keeps a context name to a filesystem-safe token for the pidfile.
func safeFilename(s string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, s)
}
