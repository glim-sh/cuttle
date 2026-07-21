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
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
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

var logger = log.New(os.Stderr, "", log.Ltime)

func logInfo(format string, args ...any)  { logger.Printf("INFO "+format, args...) }
func logWarn(format string, args ...any)  { logger.Printf("WARN "+format, args...) }
func logError(format string, args ...any) { logger.Printf("ERROR "+format, args...) }

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
	readHeaderLimit   = 10 * time.Second
)

var (
	errIdleTimeoutNegative = errors.New("--idle-timeout must be greater than or equal to 0")
	errInvalidDefaultSeed  = errors.New("invalid --fingerprint seed")
)

// baseChromeArgs run Chrome directly (outside Playwright); Playwright normally
// adds its own version of these.
var baseChromeArgs = []string{
	"--no-first-run",
	"--no-default-browser-check",
	"--disable-dev-shm-usage",
	"--disable-extensions",
	"--disable-popup-blocking",
	"--disable-background-networking",
	"--metrics-recording-only",
	"--ignore-gpu-blocklist",
}

func validSeed(seed string) bool {
	return fingerprint.ValidSeed(seed)
}

// serveConfig holds the parsed cuttle serve flags.
type serveConfig struct {
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
}

// serveEnv maps a serve flag to its CUTTLE_* env fallback (flag > env > default).
// --headless is intentionally absent: the image always passes it explicitly, so
// it has no env override.
var serveEnv = map[string]string{
	"port":                 "CUTTLE_PORT",
	"data-dir":             "CUTTLE_DATA_DIR",
	"idle-timeout":         idleTimeoutEnv,
	"proxy":                proxyEnv,
	"ephemeral":            ephemeralEnv,
	"keep-profile":         "CUTTLE_KEEP_PROFILE",
	keyFingerprint:         "CUTTLE_FINGERPRINT",
	"fingerprint-locale":   "CUTTLE_FINGERPRINT_LOCALE",
	"fingerprint-timezone": "CUTTLE_FINGERPRINT_TIMEZONE",
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
	f.Int("port", defaultPort, "CDP listen port")
	f.String("data-dir", "", "per-seed profile storage dir (default: /tmp/cuttle in a container, else the XDG data dir)")
	f.String("idle-timeout", "", `seconds of no CDP activity before an idle per-seed browser is closed; "0" = off`)
	f.String("proxy", "", "default proxy URL applied to every seed")
	f.Bool("ephemeral", false, "use a fresh scratch profile dir per session (nothing persists)")
	f.Bool("keep-profile", false, "preserve per-seed profile dirs across sessions")
	f.String(keyFingerprint, "", "default fingerprint seed when a connection omits ?fingerprint=")
	f.String("fingerprint-locale", "", "default locale for the default seed")
	f.String("fingerprint-timezone", "", "default timezone for the default seed")
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
	port, _ := fs.GetInt("port")
	headless, _ := fs.GetBool("headless")
	dataDir, _ := fs.GetString("data-dir")
	proxy, _ := fs.GetString("proxy")
	ephemeral, _ := fs.GetBool("ephemeral")
	keepProfile, _ := fs.GetBool("keep-profile")
	seed, _ := fs.GetString(keyFingerprint)
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
	}, nil
}

// chromePassthrough reconstructs the Chrome argv passthrough. Pre-cobra,
// --headless=false was both a config setter and a Chrome flag; preserve that so
// headed Chrome still receives it explicitly.
func chromePassthrough(cfg serveConfig, passthrough []string) []string {
	if cfg.headless {
		return passthrough
	}
	return append(slices.Clone(passthrough), "--headless=false")
}

func run(ctx context.Context, cfg serveConfig, passthrough []string) error {
	binary, err := fingerprint.EnsureBinary()
	if err != nil {
		return err
	}

	if cfg.defaultSeed != "" && !validSeed(cfg.defaultSeed) {
		return errInvalidDefaultSeed
	}

	pool := newChromePool(cfg, binary, chromePassthrough(cfg, passthrough), defaultLauncher(), fingerprint.NewGeoResolver())
	mux := (&multiplexer{pool: pool, port: cfg.port}).routes()

	host := bindHost(defaultEnvProbe())
	httpServer := &http.Server{
		Addr:              net.JoinHostPort(host, strconv.Itoa(cfg.port)),
		Handler:           mux,
		ReadHeaderTimeout: readHeaderLimit,
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	pool.baseCtx = ctx

	logInfo("CDP multiplexer starting on %s:%d", host, cfg.port)
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

func defaultDataDir(e envProbe) string {
	if e.inContainer() {
		return "/tmp/cuttle"
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
