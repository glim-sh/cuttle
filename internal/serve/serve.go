// Package serve implements `cuttle serve`, the in-container CDP multiplexer:
// one stealth Chrome process per fingerprint seed, all fronted on one port,
// with per-connection fingerprint routing. It is a faithful port of the Python
// cuttle serve daemon plus a server-level default proxy and ephemeral profile
// dirs.
package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/glim-sh/cuttle/internal/cli"
	"github.com/glim-sh/cuttle/internal/fingerprint"
)

func init() { cli.AddCommand(newServeCmd()) }

// logger is the daemon's structured logger: a one-line human-readable TextHandler
// on stderr. The logInfo/logWarn/logError shims keep the daemon's many
// printf-style, fully-formatted call sites terse while slog owns the level prefix
// and timestamp (replacing the old hand-rolled "INFO "+format prefixing).
var logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

func logInfo(format string, args ...any)  { logger.Info(fmt.Sprintf(format, args...)) }
func logWarn(format string, args ...any)  { logger.Warn(fmt.Sprintf(format, args...)) }
func logError(format string, args ...any) { logger.Error(fmt.Sprintf(format, args...)) }

const (
	defaultPort    = 9222
	basePort       = 5100
	terminateGrace = 5 * time.Second
	shutdownGrace  = 10 * time.Second
	// After a failed launch a seed enters a cooldown before it will be respawned,
	// so a browser that cannot start (a broken image, no display) throttles to one
	// attempt per backoff window instead of respawning on every inbound poll. The
	// window grows per consecutive failure up to launchBackoffMax.
	launchBackoffStep = 2 * time.Second
	launchBackoffMax  = 30 * time.Second
	reservedSeed      = fingerprint.ReservedSeed
	proxyEnv          = "CUTTLE_PROXY"
	ephemeralEnv      = "CUTTLE_EPHEMERAL"
	idleTimeoutEnv    = "CUTTLE_IDLE_TIMEOUT"
	hostEnv           = "CUTTLE_HOST"
	modeEnv           = "CUTTLE_MODE"
	readHeaderLimit   = 10 * time.Second
)

// serveMode decides what a connection's ?fingerprint= means. Session mode is
// the default and the CLI's only mode: the container IS one browser, every
// attach lands on the reserved default seed, and a seeded request is refused so
// an agent can never quietly land on a second cookie jar (and a human in the
// viewer never has two windows to confuse). Pool mode is the headless many-
// identities server: a seed is required, so a bare discovery GET can no longer
// spawn an unproxied default browser.
type serveMode string

const (
	modeSession serveMode = "session"
	modePool    serveMode = "pool"
)

var (
	errIdleTimeoutNegative = errors.New("--idle-timeout must be greater than or equal to 0")
	errInvalidDefaultSeed  = errors.New("invalid --fingerprint seed")
	errInvalidMode         = errors.New(`--mode must be "session" or "pool"`)
	errSessionDefaultSeed  = errors.New("--fingerprint needs --mode=pool: session mode always runs the reserved default seed")
)

func validSeed(seed string) bool {
	return fingerprint.ValidSeed(seed)
}

// serveConfig holds the parsed cuttle serve flags.
type serveConfig struct {
	mode            serveMode
	port            int
	headless        bool
	dataDir         string
	defaultSeed     string
	defaultLocale   string
	defaultTimezone string
	idleTimeout     time.Duration
	keepProfile     bool
	proxy           string
	ephemeral       bool
	humanize        bool
	allowContexts   bool
}

// serveEnv maps a serve flag to its CUTTLE_* env fallback (flag > env > default).
// --headless is intentionally absent: the image always passes it explicitly, so
// it has no env override.
var serveEnv = map[string]string{
	"mode":                   modeEnv,
	"port":                   "CUTTLE_PORT",
	"data-dir":               "CUTTLE_DATA_DIR",
	"idle-timeout":           idleTimeoutEnv,
	"proxy":                  proxyEnv,
	"ephemeral":              ephemeralEnv,
	"keep-profile":           "CUTTLE_KEEP_PROFILE",
	"humanize":               "CUTTLE_HUMANIZE",
	"allow-context-creation": "CUTTLE_ALLOW_CONTEXT_CREATION",
	keyFingerprint:           "CUTTLE_FINGERPRINT",
	"fingerprint-locale":     "CUTTLE_FINGERPRINT_LOCALE",
	"fingerprint-timezone":   "CUTTLE_FINGERPRINT_TIMEZONE",
}

func newServeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "serve [flags] [-- chrome-flags...]",
		Short:  "Run the in-container CDP multiplexer (image entrypoint)",
		Hidden: true, // the image entrypoint, not a user verb
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, passthrough, err := parseServe(cmd, args)
			if err != nil {
				return err
			}
			return run(cmd.Context(), cfg, passthrough)
		},
	}
	f := cmd.Flags()
	f.String("mode", string(modeSession), `"session" (default): one browser per container, ?fingerprint= refused; "pool": one browser per ?fingerprint= seed, which every connection must carry`)
	f.Int("port", defaultPort, "CDP listen port")
	f.String("data-dir", "", "per-seed profile storage dir (default: /data in a container, else the XDG data dir)")
	f.String("idle-timeout", "", `seconds of no CDP activity before an idle per-seed browser is closed; "0" = off`)
	f.String("proxy", "", "default proxy URL applied to every seed")
	f.Bool("ephemeral", false, "use a fresh scratch profile dir per session (nothing persists)")
	f.Bool("keep-profile", false, "preserve per-seed profile dirs across sessions")
	f.Bool("humanize", true, "rewrite CDP Input events into human-like motion (curved, Fitts-timed mouse; skewed timing) so interactions defeat behavioral detection; on by default, disable with --humanize=false or CUTTLE_HUMANIZE=0")
	f.Bool("allow-context-creation", false, "let drivers call Target.createBrowserContext instead of rejecting it; for stacks whose browser.newContext() is not optional. Identity is a launch flag, so every context still inherits the seed's fingerprint/proxy/geo")
	f.String(keyFingerprint, "", "default fingerprint seed when a connection omits ?fingerprint=")
	f.String("fingerprint-locale", "", "default locale for the default seed")
	f.String("fingerprint-timezone", "", "default timezone for the default seed")
	// Config only, never forwarded to Chrome: Chrome reads any --headless=<x>,
	// including --headless=false, as a request for headless mode, which silently
	// turned the image's headed-on-Xvfb launch windowless.
	f.Bool("headless", true, "run Chrome headless (the image runs headed on Xvfb via --headless=false)")
	return cmd
}

// parseServe resolves the daemon config from flags (with CUTTLE_* env fallback)
// and splits off the Chrome passthrough, which is strictly whatever follows `--`.
func parseServe(cmd *cobra.Command, args []string) (serveConfig, []string, error) {
	passthrough := []string{}
	if n := cmd.ArgsLenAtDash(); n >= 0 {
		passthrough = args[n:]
	}
	cfg, err := serveConfigFromFlags(cmd.Flags())
	if err != nil {
		return serveConfig{}, nil, err
	}
	return cfg, passthrough, nil
}

// applyEnvFallback fills each flag not set on the command line from its CUTTLE_*
// env var, giving flag > env > default precedence without a config framework. A
// bool env keeps the historical lenient forms (1/true/yes/on).
func applyEnvFallback(fs *pflag.FlagSet) error {
	for name, env := range serveEnv {
		f := fs.Lookup(name)
		if f == nil || f.Changed {
			continue
		}
		v, ok := os.LookupEnv(env)
		if !ok || v == "" {
			continue
		}
		if f.Value.Type() == "bool" {
			v = strconv.FormatBool(parseBoolEnv(v))
		}
		if err := fs.Set(name, v); err != nil {
			return fmt.Errorf("env %s: %w", env, err)
		}
	}
	return nil
}

func serveConfigFromFlags(fs *pflag.FlagSet) (serveConfig, error) {
	if err := applyEnvFallback(fs); err != nil {
		return serveConfig{}, err
	}
	modeStr, _ := fs.GetString("mode")
	mode := serveMode(modeStr)
	if mode != modeSession && mode != modePool {
		return serveConfig{}, errInvalidMode
	}
	port, _ := fs.GetInt("port")
	headless, _ := fs.GetBool("headless")
	dataDir, _ := fs.GetString("data-dir")
	proxy, _ := fs.GetString("proxy")
	ephemeral, _ := fs.GetBool("ephemeral")
	humanize, _ := fs.GetBool("humanize")
	allowContexts, _ := fs.GetBool("allow-context-creation")
	keepProfile, _ := fs.GetBool("keep-profile")
	seed, _ := fs.GetString(keyFingerprint)
	if seed != "" && mode == modeSession {
		return serveConfig{}, errSessionDefaultSeed
	}
	locale, _ := fs.GetString("fingerprint-locale")
	timezone, _ := fs.GetString("fingerprint-timezone")

	idle := time.Duration(0)
	if idleStr, _ := fs.GetString("idle-timeout"); idleStr != "" {
		d, err := parseIdleTimeout(idleStr)
		if err != nil {
			return serveConfig{}, err
		}
		idle = d
	}
	if dataDir == "" {
		dataDir = defaultDataDir(defaultEnvProbe())
	}
	return serveConfig{
		mode:            mode,
		port:            port,
		headless:        headless,
		dataDir:         dataDir,
		defaultSeed:     seed,
		defaultLocale:   locale,
		defaultTimezone: timezone,
		idleTimeout:     idle,
		keepProfile:     keepProfile,
		proxy:           proxy,
		ephemeral:       ephemeral,
		humanize:        humanize,
		allowContexts:   allowContexts,
	}, nil
}

func run(ctx context.Context, cfg serveConfig, passthrough []string) error {
	binary, err := fingerprint.EnsureBinary()
	if err != nil {
		return err
	}

	if cfg.defaultSeed != "" && !validSeed(cfg.defaultSeed) {
		return errInvalidDefaultSeed
	}

	pool := newChromePool(cfg, binary, passthrough, defaultLauncher(), fingerprint.NewGeoResolver())
	mux := (&multiplexer{
		pool: pool, port: cfg.port,
		humanize: cfg.humanize, allowContexts: cfg.allowContexts,
	}).routes()

	host := bindHost(defaultEnvProbe())
	httpServer := &http.Server{
		Addr:              net.JoinHostPort(host, strconv.Itoa(cfg.port)),
		Handler:           mux,
		ReadHeaderTimeout: readHeaderLimit,
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	pool.baseCtx = ctx
	pool.startSupervisor(ctx)

	logInfo("CDP multiplexer starting on %s:%d", host, cfg.port)
	warnWideBind(host, cfg.port)
	warnSmallShm(defaultEnvProbe())
	serveErr := make(chan error, 1)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		pool.shutdown()
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
	pool.shutdown()
	return nil
}

func parseBoolEnv(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func parseIdleTimeout(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	switch strings.ToLower(value) {
	case "0", "false", "off", "none", "disabled":
		return 0, nil
	}
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, errors.New("invalid --idle-timeout value") //nolint:err113
	}
	if seconds < 0 {
		return 0, errIdleTimeoutNegative
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

// envProbe abstracts the filesystem/env reads that drive container detection so
// they can be faked in tests.
type envProbe struct {
	stat     func(string) bool
	getenv   func(string) string
	readFile func(string) ([]byte, error)
	homeDir  func() (string, error)
}

func defaultEnvProbe() envProbe {
	return envProbe{
		stat: func(path string) bool {
			_, err := os.Stat(path)
			return err == nil
		},
		getenv:   os.Getenv,
		readFile: os.ReadFile,
		homeDir:  os.UserHomeDir,
	}
}

// inContainer reports whether the process runs inside a container (docker,
// podman, or k8s/containerd). The plain-file markers /.dockerenv and
// /run/.containerenv are docker/podman-only and BOTH are absent under
// Kubernetes+containerd, which would silently pin the CDP listener to loopback
// and refuse every cross-pod client; the KUBERNETES_SERVICE_HOST env and the
// container cgroup close that gap.
func (e envProbe) inContainer() bool {
	if e.stat("/.dockerenv") || e.stat("/run/.containerenv") {
		return true
	}
	if e.getenv("KUBERNETES_SERVICE_HOST") != "" {
		return true
	}
	data, err := e.readFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	cgroup := string(data)
	for _, marker := range []string{"kubepods", "docker", "containerd", "crio"} {
		if strings.Contains(cgroup, marker) {
			return true
		}
	}
	return false
}

// bindHost binds 0.0.0.0 in a container so cross-pod/host clients can reach the
// multiplexer, and loopback-only on bare metal. CUTTLE_HOST overrides.
func bindHost(e envProbe) string {
	if h := e.getenv(hostEnv); h != "" {
		return h
	}
	if e.inContainer() {
		return "0.0.0.0"
	}
	return "127.0.0.1"
}

// warnWideBind surfaces what a non-loopback bind actually means, because the
// address alone does not say it. The CDP surface has NO authentication: /json/*
// answers any caller (and /json/version even launches a browser), and the
// WebSocket upgrade only screens browser Origins, which non-browser clients omit
// by design. So anything that can reach this port can drive the browser.
//
// The fix is deliberately NOT to bind loopback inside a container: Docker can
// only forward a published port to a process listening on 0.0.0.0 in the
// namespace, so that would break `docker run -p` outright. Constrain it on the
// host side instead, or override the bind with CUTTLE_HOST.
// Chrome needs a large /dev/shm or it dies under load. BaseChromeArgs carries
// --disable-dev-shm-usage so that never crashes us - Chrome falls back to /tmp -
// but in a container /tmp is usually the disk-backed overlay, so the fallback
// trades a crash for slow shared-memory I/O on every page. The Helm chart mounts
// a Memory emptyDir at /dev/shm and internal/backend/local.go passes
// --shm-size=2g precisely to avoid that; a hand-written `docker run` inherits
// Docker's 64MB default and gets neither.
//
// Say so at startup. The symptom otherwise shows up as unexplained slowness, or
// - for anything that launches Chrome WITHOUT our base args - as intermittent
// renderer death on heavy pages, which is a genuinely miserable thing to chase.
func warnSmallShm(e envProbe) {
	if !e.inContainer() {
		return
	}
	mounts, err := e.readFile("/proc/mounts")
	if err != nil {
		return
	}
	mb, ok := shmSizeMB(string(mounts))
	if !ok || mb >= 1024 {
		return
	}
	logWarn("/dev/shm is %dMB. cuttle passes --disable-dev-shm-usage so Chrome "+
		"will not crash, but shared memory falls back to /tmp, which is "+
		"disk-backed in most containers. Start the container with --shm-size=2g, "+
		"or mount an in-memory volume at /dev/shm.", mb)
}

// shmSizeMB reads the tmpfs size= option for /dev/shm out of /proc/mounts.
// Parsed rather than statfs'd so this stays pure Go and cross-compiles to
// Windows, where the file simply does not exist. Reports ok=false when the
// mount has no explicit size, which means the kernel default of half of RAM -
// not a value worth warning about.
func shmSizeMB(mounts string) (uint64, bool) {
	for line := range strings.SplitSeq(mounts, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || f[1] != "/dev/shm" {
			continue
		}
		for opt := range strings.SplitSeq(f[3], ",") {
			after, found := strings.CutPrefix(opt, "size=")
			if !found {
				continue
			}
			kb, err := strconv.ParseUint(strings.TrimSuffix(after, "k"), 10, 64)
			if err != nil {
				return 0, false
			}
			return kb / 1024, true
		}
	}
	return 0, false
}

func warnWideBind(host string, port int) {
	if isLoopbackHost(host) {
		return
	}
	logWarn("CDP is bound to %s:%d with no authentication - any client that can "+
		"reach this port can drive the browser. Publish it narrowly on the host "+
		"(-p 127.0.0.1:%d:%d), or set CUTTLE_HOST to bind elsewhere.",
		host, port, port, port)
}

func defaultDataDir(e envProbe) string {
	if e.inContainer() {
		return "/data"
	}
	if dir := e.getenv("XDG_DATA_HOME"); dir != "" {
		return filepath.Join(dir, "cuttle", "serve")
	}
	home, err := e.homeDir()
	if err != nil {
		return "/tmp/cuttle"
	}
	return filepath.Join(home, ".local", "share", "cuttle", "serve")
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
