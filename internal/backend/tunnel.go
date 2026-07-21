package backend

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
// (and recording its pid) when the current one is not healthy. The whole
// check-spawn-record sequence is serialized per context by a cross-process lock,
// so two concurrent invocations (e.g. an agent's `up` racing a monitor's
// `status`) cannot both spawn a forward and leave one holding the ports with a
// stale pidfile that `down` can no longer find. The spawned process is
// deliberately not tied to ctx: it must outlive this CLI invocation.
func ensureTunnel(ctx context.Context, spec tunnelSpec) (Endpoint, error) {
	var ep Endpoint
	err := withStateLock(spec.context, func() error {
		var e error
		ep, e = ensureTunnelLocked(ctx, spec)
		return e
	})
	return ep, err
}

func ensureTunnelLocked(ctx context.Context, spec tunnelSpec) (Endpoint, error) {
	if tunnelHealthy(ctx, spec.context, spec.cdpPort) {
		return tunnelEndpoint(spec), nil
	}
	// Clear any stale process/pidfile before respawning.
	_ = stopTunnelLocked(spec.context)

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
// is idempotent and safe to call when no tunnel exists. It takes the same
// per-context lock as ensureTunnel so a stop cannot race a concurrent spawn.
func stopTunnel(contextName string) error {
	return withStateLock(contextName, func() error {
		return stopTunnelLocked(contextName)
	})
}

func stopTunnelLocked(contextName string) error {
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
	return filepath.Join(dir, "tunnel-"+safeToken(contextName)+".pid"), nil
}

func openTunnelLog(contextName string) (*os.File, error) {
	dir, err := stateDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "tunnel-"+safeToken(contextName)+".log")
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

// safeToken maps s to a filesystem-safe, collision-free token used for the
// per-context pidfile/log and the ssh ControlMaster socket. Unsafe runes become
// '_', and when any rune had to be rewritten a short hash of the original is
// appended so two names differing only by an unsafe rune ("my box" vs "my_box")
// never collide onto the same file. Already-safe names pass through unchanged, so
// the common case keeps readable paths.
func safeToken(s string) string {
	changed := false
	sanitized := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		changed = true
		return '_'
	}, s)
	if !changed {
		return sanitized
	}
	sum := sha256.Sum256([]byte(s))
	return sanitized + "-" + hex.EncodeToString(sum[:4])
}
