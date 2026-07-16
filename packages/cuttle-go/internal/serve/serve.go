// Package serve implements `cuttle serve`, the in-container CDP multiplexer:
// one stealth Chrome process per fingerprint seed, all fronted on one port,
// with per-connection fingerprint routing. It is a faithful port of the Python
// cuttleserve daemon plus a server-level default proxy and ephemeral profile
// dirs.
package serve

import (
	"context"
	"encoding/json"
	"errors"
	"log"
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

	"github.com/glim-sh/cuttle/packages/cuttle-go/internal/cli"
	"github.com/glim-sh/cuttle/packages/cuttle-go/internal/fingerprint"
)

func init() { cli.AddCommand(newServeCmd()) }

var logger = log.New(os.Stderr, "", log.Ltime)

func logInfo(format string, args ...any)  { logger.Printf("INFO "+format, args...) }
func logWarn(format string, args ...any)  { logger.Printf("WARN "+format, args...) }
func logError(format string, args ...any) { logger.Printf("ERROR "+format, args...) }

const (
	defaultPort     = 9222
	basePort        = 5100
	terminateGrace  = 5 * time.Second
	shutdownGrace   = 10 * time.Second
	reservedSeed    = fingerprint.ReservedSeed
	proxyEnv        = "CUTTLESERVE_PROXY"
	ephemeralEnv    = "CUTTLESERVE_EPHEMERAL"
	idleTimeoutEnv  = "CLOAKSERVE_IDLE_TIMEOUT"
	hostEnv         = "CUTTLESERVE_HOST"
	readHeaderLimit = 10 * time.Second
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

// serveConfig holds the parsed cuttleserve flags.
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

func newServeCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "serve [flags] [-- chrome-flags...]",
		Short:              "Run the in-container CDP multiplexer (image entrypoint)",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, a := range args {
				if a == "-h" || a == "--help" {
					return cmd.Help()
				}
			}
			return run(cmd.Context(), args)
		},
	}
}

func run(ctx context.Context, argv []string) error {
	cfg, globalArgs, err := parseCLIArgs(argv)
	if err != nil {
		return err
	}

	binary, err := fingerprint.EnsureBinary()
	if err != nil {
		return err
	}

	if cfg.defaultSeed != "" && !validSeed(cfg.defaultSeed) {
		return errInvalidDefaultSeed
	}

	pool := newChromePool(cfg, binary, globalArgs, defaultLauncher(), fingerprint.NewGeoResolver())
	mux := (&multiplexer{pool: pool, port: cfg.port}).routes()

	host := bindHost(defaultEnvProbe())
	httpServer := &http.Server{
		Addr:              net.JoinHostPort(host, strconv.Itoa(cfg.port)),
		Handler:           mux,
		ReadHeaderTimeout: readHeaderLimit,
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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

// parseCLIArgs mirrors the Python cuttleserve arg parser: it extracts the
// daemon's own flags and returns the remaining args as Chrome passthrough.
// --fingerprint / --fingerprint-locale / --fingerprint-timezone become config
// defaults so they route through build_args (locale needs both --lang and
// --fingerprint-locale). Query-string params override these per-connection.
func parseCLIArgs(argv []string) (serveConfig, []string, error) {
	cfg := serveConfig{
		port:        defaultPort,
		headless:    true,
		idleTimeout: defaultIdleTimeout(),
		proxy:       os.Getenv(proxyEnv),
		ephemeral:   parseBoolEnv(os.Getenv(ephemeralEnv)),
	}
	passthrough := []string{}
	consumedPrefixes := []string{
		"--port=",
		"--data-dir=",
		"--idle-timeout=",
		"--remote-debugging-port=",
		"--remote-debugging-address=",
	}

	for _, arg := range argv {
		switch {
		case strings.HasPrefix(arg, "--port="):
			p, err := strconv.Atoi(strings.SplitN(arg, "=", 2)[1])
			if err != nil {
				return serveConfig{}, nil, errors.New("invalid --port value") //nolint:err113
			}
			cfg.port = p
		case strings.HasPrefix(arg, "--data-dir="):
			cfg.dataDir = strings.SplitN(arg, "=", 2)[1]
		case strings.HasPrefix(arg, "--idle-timeout="):
			d, err := parseIdleTimeout(strings.SplitN(arg, "=", 2)[1])
			if err != nil {
				return serveConfig{}, nil, err
			}
			cfg.idleTimeout = d
		case strings.HasPrefix(arg, "--proxy="):
			cfg.proxy = strings.SplitN(arg, "=", 2)[1]
		case arg == "--ephemeral":
			cfg.ephemeral = true
		case arg == "--headless=false" || arg == "--headless=False":
			cfg.headless = false
			passthrough = append(passthrough, arg)
		case arg == "--keep-profile":
			cfg.keepProfile = true
		case hasAnyPrefix(arg, consumedPrefixes):
			// Strip silently.
		case strings.HasPrefix(arg, "--fingerprint-locale="):
			cfg.defaultLocale = strings.SplitN(arg, "=", 2)[1]
		case strings.HasPrefix(arg, "--fingerprint-timezone="):
			cfg.defaultTimezone = strings.SplitN(arg, "=", 2)[1]
		case strings.HasPrefix(arg, "--fingerprint="):
			cfg.defaultSeed = strings.SplitN(arg, "=", 2)[1]
		default:
			passthrough = append(passthrough, arg)
		}
	}

	if cfg.dataDir == "" {
		cfg.dataDir = defaultDataDir(defaultEnvProbe())
	}
	return cfg, passthrough, nil
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func parseBoolEnv(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func defaultIdleTimeout() time.Duration {
	v, ok := os.LookupEnv(idleTimeoutEnv)
	if !ok {
		return 0
	}
	d, err := parseIdleTimeout(v)
	if err != nil {
		return 0
	}
	return d
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
// multiplexer, and loopback-only on bare metal. CUTTLESERVE_HOST overrides.
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
		return "/tmp/cloakserve"
	}
	home, err := e.homeDir()
	if err != nil {
		return "/tmp/cloakserve"
	}
	return filepath.Join(home, ".cloakbrowser", "cloakserve")
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
