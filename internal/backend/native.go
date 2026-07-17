package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/glim-sh/cuttle/internal/fingerprint"
	"github.com/glim-sh/cuttle/internal/xdg"
)

// Native runs the browser as a local `cuttle serve` daemon on this macOS host -
// no Docker, no Xvfb, no VNC. It runs clark's native darwin-arm64 stealth
// Chromium directly, so the browser opens as a real desktop window (surfaced for
// handoff via `cuttle view`) and pays no Rosetta emulation tax. The persona is
// macOS (see fingerprint.ForkParityArgs): on Apple Silicon the real Metal GPU is
// unspoofable, so a genuine Mac is the only coherent identity.
//
// Unlike the docker/ssh backends, the daemon must OUTLIVE the CLI invocation
// that starts it, so Native spawns a detached process (new session) and tracks
// it by pidfile rather than going through the Runner seam.
type Native struct {
	name    string
	cdpPort int
}

const nativeStateFile = "state.json"

var (
	errNativeNotDarwin      = errors.New("the native backend is macOS-only; use `--context local` for the docker backend")
	errNativeSelfPath       = errors.New("cannot locate the cuttle executable to launch the serve daemon")
	errNativeNotRunning     = errors.New("no running native browser (run `cuttle up` first)")
	errNativeWindowNotFound = errors.New("no browser window found")
	errNativeBadName        = errors.New("invalid --name for the native backend (no path separators or ..)")
	errNativeAppPath        = errors.New("cannot locate the browser app bundle to bring forward")
)

// nativeState is the pidfile the supervisor persists so a later CLI invocation
// (status/down/view) can find and signal the running daemon.
type nativeState struct {
	PID int `json:"pid"`
}

func (n *Native) check() error {
	if runtime.GOOS != "darwin" {
		return errNativeNotDarwin
	}
	// name becomes a filesystem path component under the state dir (unlike the
	// docker backend where it is a container name docker itself validates), so
	// reject anything that could escape the cuttle/native/ subtree.
	if n.name == "" || strings.ContainsAny(n.name, `/\`) || strings.Contains(n.name, "..") {
		return fmt.Errorf("%w: %q", errNativeBadName, n.name)
	}
	return nil
}

// nativeRoot is the parent directory of every native instance's state dir.
func nativeRoot() string {
	base := xdg.DataDir()
	if base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "cuttle", "native")
}

func (n *Native) stateDir() string { return filepath.Join(nativeRoot(), n.name) }

// NativeInstance describes one native instance dir on disk for `cuttle native ls`.
type NativeInstance struct {
	Name    string
	Running bool
}

// ListNative enumerates every native instance directory, including orphans left
// by a failed `up` (a dir with no live daemon). Without this, debris under the
// data dir is invisible - `cuttle status`/`context ls` only see one named
// instance at a time. Remove an unwanted one with `cuttle down --name <n> --purge`.
func ListNative() ([]NativeInstance, error) {
	entries, err := os.ReadDir(nativeRoot())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading native instances: %w", err)
	}
	var out []NativeInstance
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		inst := &Native{name: e.Name()}
		st, ok := inst.readState()
		out = append(out, NativeInstance{Name: e.Name(), Running: ok && processAlive(st.PID)})
	}
	return out, nil
}

func (n *Native) profileDir() string { return filepath.Join(n.stateDir(), "profile") }
func (n *Native) logPath() string    { return filepath.Join(n.stateDir(), "serve.log") }
func (n *Native) statePath() string  { return filepath.Join(n.stateDir(), nativeStateFile) }

func (n *Native) readState() (nativeState, bool) {
	data, err := os.ReadFile(n.statePath())
	if err != nil {
		return nativeState{}, false
	}
	var st nativeState
	if json.Unmarshal(data, &st) != nil || st.PID <= 0 {
		return nativeState{}, false
	}
	return st, true
}

// writeState persists the pidfile. Start creates the state dir (for the serve
// log) before calling this, so it does not re-create it.
func (n *Native) writeState(st nativeState) error {
	data, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("encoding native state: %w", err)
	}
	if err := os.WriteFile(n.statePath(), data, 0o600); err != nil {
		return fmt.Errorf("writing native state: %w", err)
	}
	return nil
}

// processAlive reports whether pid names a live process. Signal 0 performs the
// permission/existence check without delivering a signal; EPERM means the
// process exists but is owned by another user (still alive).
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func (n *Native) State(_ context.Context) (State, error) {
	if err := n.check(); err != nil {
		return "", err
	}
	st, ok := n.readState()
	if !ok {
		return StateAbsent, nil
	}
	if !processAlive(st.PID) {
		// Stale pidfile from a daemon that died or was killed; a fresh Start
		// relaunches cleanly, so report absent and drop the stale marker.
		_ = os.Remove(n.statePath())
		return StateAbsent, nil
	}
	return StateRunning, nil
}

func (n *Native) Start(ctx context.Context, opts StartOpts) error {
	if err := n.check(); err != nil {
		return err
	}
	state, err := n.State(ctx)
	if err != nil {
		return err
	}
	if opts.Recreate && state != StateAbsent {
		if stopErr := n.Stop(ctx, true); stopErr != nil {
			return stopErr
		}
		state = StateAbsent
	}
	if state == StateRunning {
		return nil
	}

	// Pre-flight: if the CDP port is already held (by another instance or an
	// unrelated process), fail now with a clear reason. Without this, we would
	// launch a daemon that cannot bind, then mistake the NEIGHBOUR's CDP answering
	// on that same port for our own and report a fake "ready".
	if !portAvailable(n.cdpPort) {
		return portInUseError(n.cdpPort)
	}

	binary, err := ensureNativeBinary(ctx)
	if err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return errNativeSelfPath
	}
	if mkErr := os.MkdirAll(n.stateDir(), 0o750); mkErr != nil {
		return fmt.Errorf("creating native state dir: %w", mkErr)
	}

	logFile, err := os.Create(n.logPath())
	if err != nil {
		return fmt.Errorf("opening native serve log: %w", err)
	}
	defer func() { _ = logFile.Close() }()

	// context.Background: the daemon must OUTLIVE this CLI invocation, so it is
	// deliberately not tied to the caller's context.
	cmd := exec.CommandContext(context.Background(), self, n.serveArgs(opts)...)
	cmd.Env = append(os.Environ(), fingerprint.BinaryPathEnv+"="+binary)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Setsid detaches the daemon into its own session so it survives this CLI
	// process and terminal, and becomes the group leader we later signal.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("launching cuttle serve: %w", err)
	}
	pid := cmd.Process.Pid
	// Release so the CLI need not reap the daemon; it now lives independently.
	_ = cmd.Process.Release()

	if err := n.writeState(nativeState{PID: pid}); err != nil {
		// Without a pidfile no later command could find or stop this daemon, so
		// do not leave it running untracked.
		_ = syscall.Kill(pid, syscall.SIGTERM)
		return err
	}

	// Block until the daemon we just spawned is actually serving, or fail fast
	// with the real reason. This is what stops `cuttle up` reporting a fake
	// success when the requested --cdp-port is already held by ANOTHER instance:
	// our daemon cannot bind, exits, and we surface its bind error rather than
	// the neighbour's CDP answering on that same port.
	if err := n.awaitReady(ctx, pid); err != nil {
		_ = syscall.Kill(pid, syscall.SIGTERM)
		_ = os.Remove(n.statePath())
		return err
	}
	return nil
}

const nativeReadyTimeout = 60 * time.Second

// awaitReady waits for our daemon (pid) to answer CDP, or returns the reason it
// died. It only treats a CDP 200 as ready while our pid is still alive, so a
// neighbour holding the port can never be mistaken for our instance (only one
// process can bind 127.0.0.1:port).
func (n *Native) awaitReady(ctx context.Context, pid int) error {
	deadline := time.Now().Add(nativeReadyTimeout)
	for {
		if !processAlive(pid) {
			return n.launchFailure()
		}
		if cdpAnswers(ctx, n.cdpPort) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("native browser started but CDP never came up on port %d - see %s", //nolint:err113
				n.cdpPort, n.logPath())
		}
		select {
		case <-ctx.Done():
			return ctx.Err() //nolint:wrapcheck // caller renders cancellation
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// cdpAnswers reports whether a healthy CDP endpoint answers on the port.
func cdpAnswers(ctx context.Context, port int) bool {
	reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	endpoint := "http://" + loopbackHost + ":" + portStr(port) + "/json/version"
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// launchFailure turns a dead daemon's serve log into an actionable error. A
// --cdp-port collision (EADDRINUSE) is the common case and is called out by name.
func (n *Native) launchFailure() error {
	tail := n.lastLogLine()
	switch {
	case strings.Contains(tail, "address already in use"):
		return portInUseError(n.cdpPort)
	case tail != "":
		return fmt.Errorf("native browser failed to start: %s", tail) //nolint:err113
	default:
		return fmt.Errorf("native browser exited during startup - see %s", n.logPath()) //nolint:err113
	}
}

// portAvailable reports whether we can bind the loopback CDP port right now (i.e.
// no other instance or process already holds it).
func portAvailable(port int) bool {
	var lc net.ListenConfig
	l, err := lc.Listen(context.Background(), "tcp", net.JoinHostPort(loopbackHost, portStr(port)))
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

func portInUseError(port int) error {
	return fmt.Errorf("port %d is already in use (another cuttle instance or process holds it) - "+ //nolint:err113
		"stop it with `cuttle down`, or pick another with --cdp-port", port)
}

func (n *Native) lastLogLine() string {
	data, err := os.ReadFile(n.logPath())
	if err != nil {
		return ""
	}
	last := ""
	for l := range strings.SplitSeq(strings.TrimRight(string(data), "\n"), "\n") {
		if s := strings.TrimSpace(l); s != "" {
			last = s
		}
	}
	return last
}

// serveArgs builds the `cuttle serve` argv for the detached daemon. Headed so
// the real window can be surfaced; an explicit window size avoids clark's
// 800x600 default (see CloakBrowser #273); the profile dir is per-instance so
// the session persists across restarts.
func (n *Native) serveArgs(opts StartOpts) []string {
	args := []string{
		"serve",
		"--headless=false",
		"--port=" + portStr(n.cdpPort),
		"--data-dir=" + n.profileDir(),
	}
	if opts.IdleTimeout != "" {
		args = append(args, "--idle-timeout="+opts.IdleTimeout)
	}
	if opts.KeepProfile == nil || *opts.KeepProfile {
		args = append(args, "--keep-profile")
	}
	if opts.Proxy != "" {
		args = append(args, "--proxy="+opts.Proxy)
	}
	// Chrome passthrough (serve routes unknown flags to every seed's Chrome).
	// --use-mock-keychain stops Chromium prompting for macOS Keychain access on
	// every launch (its os_crypt Safe Storage key); cookies still persist and it
	// is not a web-visible surface. --window-size avoids clark's 800x600 default
	// (CloakBrowser #273).
	args = append(args, "--use-mock-keychain", "--window-size=1280,800")
	return args
}

func (n *Native) Stop(_ context.Context, purge bool) error {
	if err := n.check(); err != nil {
		return err
	}
	if st, ok := n.readState(); ok && processAlive(st.PID) {
		// SIGTERM lets serve drain its Chrome children cleanly; escalate to the
		// process group only if it does not exit within the grace window.
		_ = syscall.Kill(st.PID, syscall.SIGTERM)
		if !waitExit(st.PID, 8*time.Second) {
			_ = syscall.Kill(-st.PID, syscall.SIGKILL)
		}
	}
	_ = os.Remove(n.statePath())
	if purge {
		// Remove the whole instance dir (profile + log + pidfile), so `down --purge`
		// also sweeps an orphaned dir left by a failed `up`.
		if err := os.RemoveAll(n.stateDir()); err != nil {
			return fmt.Errorf("purging native instance dir: %w", err)
		}
	}
	return nil
}

func waitExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return true
		}
		time.Sleep(150 * time.Millisecond)
	}
	return !processAlive(pid)
}

// Reach is a direct loopback CDP endpoint; the native backend has no VNC (the
// browser is a real desktop window), so VNCPort is 0 and release is a no-op.
func (n *Native) Reach(_ context.Context, _, _ int) (Endpoint, func(), error) {
	return Endpoint{CDPHost: loopbackHost, CDPPort: n.cdpPort, VNCPort: 0}, func() {}, nil
}

// RaiseWindow surfaces the seed's real Chrome window on the desktop for handoff
// (captcha, Cloudflare, login) - the native-backend replacement for the VNC
// viewer. `open -a` is the primary lever: it activates the browser app without
// the Automation permission the per-window osascript raise needs. A precise
// per-window raise is attempted on top (a no-op without that permission). It
// returns activated=true when the browser was brought forward; the window itself
// may still sit on another macOS Space, which the CLI tells the user about (this
// backend cannot move Spaces - clark is a pure Go process with no window API).
func (n *Native) RaiseWindow(ctx context.Context, seed string) error {
	if err := n.check(); err != nil {
		return err
	}
	st, ok := n.readState()
	if !ok || !processAlive(st.PID) {
		return errNativeNotRunning
	}
	seedKey := seed
	if seedKey == "" {
		seedKey = fingerprint.ReservedSeed
	}
	n.warmSeed(ctx, seed)

	dir := filepath.Join(n.profileDir(), seedKey)
	pid, found := n.browserPIDForSeed(ctx, st.PID, dir)
	if !found {
		return fmt.Errorf("%w for seed %q (still launching?)", errNativeWindowNotFound, seedKey)
	}
	if err := n.activateApp(ctx); err != nil {
		return err
	}
	// Best-effort precise raise on top; silently ignored without Automation grant.
	_ = raiseWindowByPID(ctx, pid)
	return nil
}

// warmSeed pokes the mux so the pool lazily launches this seed's Chrome before
// we try to raise its window.
func (n *Native) warmSeed(ctx context.Context, seed string) {
	endpoint := "http://" + loopbackHost + ":" + portStr(n.cdpPort) + "/json/version"
	if seed != "" {
		endpoint += "?fingerprint=" + seed
	}
	warmCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(warmCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
}

// browserPIDForSeed resolves the seed's main Chrome process. serve launches each
// seed's Chrome as a direct child, so `pgrep -P <servePID>` yields exactly the
// browser main processes (their renderer/gpu helpers are children of Chrome, not
// serve). The specific seed is disambiguated by matching its --user-data-dir via
// `ps -o command=`, which prints the full untruncated argv. It retries because
// the process appears a beat after warmSeed.
func (n *Native) browserPIDForSeed(ctx context.Context, servePID int, dir string) (int, bool) {
	deadline := time.Now().Add(8 * time.Second)
	for {
		out, _ := exec.CommandContext(ctx, "pgrep", "-P", strconv.Itoa(servePID)).Output() //nolint:gosec // arg is a stringified int, no injection surface
		for field := range strings.FieldsSeq(string(out)) {
			pid, err := strconv.Atoi(field)
			if err != nil {
				continue
			}
			cmdline, _ := exec.CommandContext(ctx, "ps", "-p", field, "-o", "command=").Output()
			if cmdlineTargetsDir(string(cmdline), dir) {
				return pid, true
			}
		}
		if time.Now().After(deadline) {
			return 0, false
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// cmdlineTargetsDir reports whether cmdline runs Chrome with exactly this seed's
// --user-data-dir. The match requires the dir to end at an argument boundary
// (whitespace or end of string) so a seed name that is a prefix of another (e.g.
// "a" vs "ab") does not cross-match.
func cmdlineTargetsDir(cmdline, dir string) bool {
	needle := "--user-data-dir=" + dir
	for rest := cmdline; ; {
		i := strings.Index(rest, needle)
		if i < 0 {
			return false
		}
		end := i + len(needle)
		if end == len(rest) || isArgBoundary(rest[end]) {
			return true
		}
		rest = rest[i+1:]
	}
}

func isArgBoundary(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// raiseWindowByPID brings a specific process's window to the foreground via
// System Events. The first use may prompt for macOS Automation permission.
func raiseWindowByPID(ctx context.Context, pid int) error {
	script := fmt.Sprintf(
		`tell application "System Events" to set frontmost of (first process whose unix id is %d) to true`, pid,
	)
	if out, err := exec.CommandContext(ctx, "osascript", "-e", script).CombinedOutput(); err != nil {
		return fmt.Errorf("osascript raise: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// activateApp brings the browser app to the front via `open -a` (LaunchServices),
// which needs no Automation permission. It errors only when the app bundle cannot
// be located.
func (n *Native) activateApp(ctx context.Context) error {
	app := nativeAppPath()
	if app == "" {
		return errNativeAppPath
	}
	if out, err := exec.CommandContext(ctx, "open", "-a", app).CombinedOutput(); err != nil {
		return fmt.Errorf("open -a %s: %w: %s", app, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// nativeAppPath is the Chromium.app bundle path for `open -a`, derived from an
// explicit CUTTLE_BROWSER_BINARY override or the clark cache.
func nativeAppPath() string {
	if override := os.Getenv(fingerprint.BinaryPathEnv); override != "" {
		return appBundlePath(override)
	}
	app := filepath.Join(clarkCacheDir(), "Chromium.app")
	if _, err := os.Stat(app); err != nil {
		return ""
	}
	return app
}

// appBundlePath extracts the enclosing .app bundle from a binary path inside it
// (e.g. .../Chromium.app/Contents/MacOS/Chromium -> .../Chromium.app).
func appBundlePath(binary string) string {
	if i := strings.Index(binary, ".app/"); i != -1 {
		return binary[:i+len(".app")]
	}
	return ""
}

// Diagnostics tails the serve log for `cuttle status` triage, mirroring the
// docker backend's log tail.
func (n *Native) Diagnostics(_ context.Context) []string {
	data, err := os.ReadFile(n.logPath())
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > 20 {
		lines = lines[len(lines)-20:]
	}
	out := []string{"last 20 serve log lines (" + n.logPath() + "):"}
	for _, l := range lines {
		out = append(out, "  "+l)
	}
	return out
}
