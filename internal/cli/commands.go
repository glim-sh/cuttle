package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/glim-sh/cuttle/internal/backend"
	"github.com/glim-sh/cuttle/internal/config"
	"github.com/glim-sh/cuttle/internal/profile"
)

// boolFlag is an optional bool: unset (nil) is distinct from explicit
// true/false, so up can warn that --keep-profile is fixed at container creation.
// `--keep-profile` sets true (NoOptDefVal); `--keep-profile=false` sets false.
type boolFlag struct {
	set bool
	val bool
}

func (b *boolFlag) String() string {
	if b.val {
		return "true"
	}
	return "false"
}

func (b *boolFlag) Set(s string) error {
	v, err := strconv.ParseBool(s)
	if err != nil {
		return err //nolint:wrapcheck // pflag renders it
	}
	b.val, b.set = v, true
	return nil
}

func (b *boolFlag) Type() string { return "bool" }

func (b *boolFlag) value() *bool {
	if !b.set {
		return nil
	}
	return &b.val
}

const (
	defaultName    = "cuttle"
	defaultCDPPort = 9222
	defaultVNCPort = 6080
	imageRepo      = "ghcr.io/glim-sh/cuttle"
)

var errCDPNotAnswering = errors.New("CDP not answering - run `cuttle up` first")

func init() {
	AddCommand(newUpCmd(), newDownCmd(), newStatusCmd(), newOpenCmd(), newContextCmd())
}

// defaultImage is the published image tag matching this CLI's version, so it
// never drives a cuttle serve it was not shipped with. An uninstalled checkout
// reports "dev" (no such tag), so it falls back to latest.
func defaultImage() string {
	v := cliVersion()
	if v == devVersion {
		v = "latest"
	}
	return imageRepo + ":" + v
}

type commonFlags struct {
	contextName string
	cdpPort     int
	vncPort     int
	profile     string // seed; set only on verbs that take --profile (open)
}

func addCommonFlags(cmd *cobra.Command, cf *commonFlags) {
	f := cmd.Flags()
	f.StringVar(&cf.contextName, "context", "", "context to use (default: config default_context, else local)")
	f.IntVar(&cf.cdpPort, "cdp-port", defaultCDPPort, "host CDP port")
	f.IntVar(&cf.vncPort, "vnc-port", defaultVNCPort, "host VNC viewer port (docker/local backend only)")
}

// addProfileFlag wires --profile on the verbs that drive a session with a named
// profile (its local auth state is checked out for the session duration).
func addProfileFlag(cmd *cobra.Command, p *string) {
	cmd.Flags().StringVar(p, "profile", "", "profile name (= seed); checks out its local auth state for this session")
}

// withFingerprint appends the profile as a ?fingerprint=<seed> query so a driver
// attaching at this URL lands on the profile's seed.
func withFingerprint(base, profileName string) string {
	if profileName == "" {
		return base
	}
	return base + "?fingerprint=" + url.QueryEscape(profileName)
}

// profileRemote reports whether the named profile is configured as remote-
// persistent storage (no checkout/checkin). An unconfigured profile is local.
func profileRemote(name string) (bool, error) {
	if name == "" {
		return false, nil
	}
	cfg, err := config.Load()
	if err != nil {
		return false, err
	}
	p, ok := cfg.Profiles[name]
	return ok && p.Storage == config.StorageRemote, nil
}

// checkoutProfile starts a profile session against the reachable endpoint when
// --profile is set, returning nil when it is not. The caller MUST Close the
// returned session (via defer and a signal-aware context) to check state in.
func checkoutProfile(ctx context.Context, cf commonFlags, ep backend.Endpoint) (*profile.Session, error) {
	if cf.profile == "" {
		return nil, nil
	}
	if !profile.ValidName(cf.profile) {
		return nil, fmt.Errorf("%w: %q", errInvalidProfile, cf.profile)
	}
	remote, err := profileRemote(cf.profile)
	if err != nil {
		return nil, err
	}
	base := "http://" + net.JoinHostPort(ep.CDPHost, strconv.Itoa(ep.CDPPort))
	return profile.Checkout(ctx, profile.Options{Name: cf.profile, CDPBase: base, Remote: remote})
}

var errInvalidProfile = errors.New("invalid profile name")

// resolve loads the config, selects the active context, and builds its backend.
// The docker-container backends (local, ssh) use the fixed "cuttle" container
// name; k8s/direct are identified by their context name instead.
func resolve(cf commonFlags, image string) (string, string, config.Context, backend.Backend, error) {
	cfg, err := config.Load()
	if err != nil {
		return "", "", config.Context{}, nil, err
	}
	ctxName, ctx, err := cfg.Active(cf.contextName, os.Getenv(config.EnvContext))
	if err != nil {
		return "", "", config.Context{}, nil, err
	}
	name := ctxName
	if ctx.Backend == config.BackendLocal || ctx.Backend == config.BackendSSH {
		name = defaultName
	}
	b, err := backend.New(name, ctxName, ctx, backend.ExecRunner{}, cf.cdpPort, cf.vncPort, image)
	if err != nil {
		return "", "", config.Context{}, nil, err
	}
	return name, ctxName, ctx, b, nil
}

// reachStable yields a stable local endpoint for the briefing. A tunneled backend
// (ssh/k8s) ensures its detached standing forward on the configured ports - it
// outlives the CLI, so the returned release is a no-op and the endpoint is the
// same 127.0.0.1:cdp/vnc on every invocation. local/direct return their fixed
// endpoint. The ephemeral Reach(0,0) forward stays the internal fallback for the
// short-lived open/login flows.
func reachStable(ctx context.Context, b backend.Backend, cf commonFlags) (backend.Endpoint, func(), error) {
	if t, ok := b.(backend.Tunneler); ok {
		ep, err := t.EnsureTunnel(ctx, cf.cdpPort, cf.vncPort)
		return ep, func() {}, err
	}
	return b.Reach(ctx, 0, 0)
}

// localBackend reports whether the context runs the amd64 image in docker on this
// host - the case that has a container name, an image tail, and the arm64
// emulation tax, as opposed to a remote/tunneled backend.
func localBackend(ctx config.Context) bool {
	return ctx.Backend == config.BackendLocal || ctx.Backend == ""
}

func locationLabel(ctxName string, ctx config.Context, name string) string {
	if localBackend(ctx) {
		return "container '" + name + "'"
	}
	return "context '" + ctxName + "'"
}

func endpointURLs(ep backend.Endpoint) (string, string) {
	cdp := "http://" + net.JoinHostPort(ep.CDPHost, strconv.Itoa(ep.CDPPort))
	viewer := ""
	if ep.VNCPort != 0 {
		viewer = "http://" + net.JoinHostPort(ep.VNCHost, strconv.Itoa(ep.VNCPort)) + "/"
	}
	return cdp, viewer
}

func cdpReady(ctx context.Context, host string, port int, timeout time.Duration) map[string]any {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	endpoint := "http://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/json/version"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	// Only a live browser answers 200 here. A launch error (e.g. serve's backoff
	// 503) returns a JSON {"error":...} body that would otherwise unmarshal fine
	// and be mistaken for readiness.
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil
	}
	var v map[string]any
	if json.Unmarshal(body, &v) != nil {
		return nil
	}
	return v
}

func waitCDP(ctx context.Context, host string, port int, timeout time.Duration) map[string]any {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		// A generous per-poll timeout: a cold Chrome under CPU emulation can take
		// ~1s+ to bind CDP, and the daemon holds the request open until the browser
		// is ready. Too short a timeout just disconnects and re-polls needlessly.
		if v := cdpReady(ctx, host, port, 3*time.Second); v != nil {
			return v
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(500 * time.Millisecond):
		}
	}
	return nil
}

func browserOf(v map[string]any) string {
	if v == nil {
		return ""
	}
	b, _ := v["Browser"].(string)
	return b
}

// ---------------------------------------------------------------------------
// up
// ---------------------------------------------------------------------------

type upFlags struct {
	common      commonFlags
	image       string
	keepProfile boolFlag
	recreate    bool
	idleTimeout string
}

func newUpCmd() *cobra.Command {
	var uf upFlags
	cmd := &cobra.Command{
		Use:   "up",
		Short: "start the browser (idempotent) with VNC viewing",
		RunE:  func(cmd *cobra.Command, _ []string) error { return runUp(cmd, &uf) },
	}
	addCommonFlags(cmd, &uf.common)
	cmd.Flags().StringVar(&uf.image, "image", "", "image (default "+defaultImage()+"; docker/local backend only)")
	cmd.Flags().Var(&uf.keepProfile, "keep-profile", "persist the browser profile across restarts (default on)")
	cmd.Flags().Lookup("keep-profile").NoOptDefVal = "true"
	cmd.Flags().BoolVar(&uf.recreate, "recreate", false, "destroy any existing container and start fresh (discards the profile)")
	cmd.Flags().StringVar(&uf.idleTimeout, "idle-timeout", "", `seconds of no CDP client activity after which an idle per-seed browser is closed; "0" = off (default off)`)
	return cmd
}

func runUp(cmd *cobra.Command, uf *upFlags) error {
	name, ctxName, ctx, b, err := resolve(uf.common, defaultImage())
	if err != nil {
		return err
	}
	warnArm64Emulation(os.Stderr, ctx)
	before, err := b.State(cmd.Context())
	if err != nil {
		return err
	}

	if before != backend.StateAbsent {
		if uf.image != "" {
			fmt.Fprintf(os.Stderr, "cuttle: --image is fixed when the container is created; %q keeps the image it was created with (use --recreate to change it)\n", name)
		}
		if uf.keepProfile.set {
			fmt.Fprintf(os.Stderr, "cuttle: --keep-profile is fixed when the container is created; %q keeps its original setting (use --recreate to change it)\n", name)
		}
	}

	opts := backend.StartOpts{
		Image:       uf.image,
		Recreate:    uf.recreate,
		KeepProfile: uf.keepProfile.value(),
		Proxy:       ctx.Proxy,
		IdleTimeout: uf.idleTimeout,
		Storage:     config.StorageLocal,
	}
	if err = b.Start(cmd.Context(), opts); err != nil {
		return err
	}

	ep, release, err := reachStable(cmd.Context(), b, uf.common)
	if err != nil {
		return err
	}
	defer release()

	// 60s: a fresh container must boot the X server + KasmVNC and cold-start Chrome,
	// which is slow under CPU emulation (an amd64 image on an arm64 host).
	v := waitCDP(cmd.Context(), ep.CDPHost, ep.CDPPort, 60*time.Second)
	if v == nil {
		if before == backend.StateRunning {
			return fmt.Errorf("%q is running but CDP is not answering - run `cuttle status` to triage, then `cuttle down` and retry", name) //nolint:err113
		}
		return errors.New("started but CDP never came up - run `cuttle status` to triage (it tails the Chrome launch failure reason)") //nolint:err113
	}

	verb, showImage := "ready", true
	switch {
	case uf.recreate && before != backend.StateAbsent:
		// Distinct from "already running": --recreate discarded the old profile.
		verb, showImage = "recreated", false
	case before == backend.StateRunning:
		verb, showImage = "already running", false
	case before == backend.StateStopped:
		verb, showImage = "restarted", false
	}
	image := uf.image
	if image == "" {
		image = defaultImage()
	}
	printBriefingFor(cmd.OutOrStdout(), verb, name, ctxName, ctx, uf.common, ep, browserOf(v), image, showImage)
	if uf.recreate && before != backend.StateAbsent {
		fmt.Fprintln(cmd.OutOrStdout(), "  note: --recreate discarded the previous profile (cookies/logins) - fresh identity")
	}
	return nil
}

// warnArm64Emulation flags the Rosetta tax of running the linux/amd64 image on
// an Apple Silicon host via the local docker backend. The image is amd64-only, so
// on arm64 it runs under emulation (slow, memory-hungry). The ssh/k8s backends run
// the image on a remote amd64 host instead - point the user there.
func warnArm64Emulation(w io.Writer, ctx config.Context) {
	if runtime.GOARCH != "arm64" || !localBackend(ctx) {
		return
	}
	fmt.Fprintln(w, "cuttle: the container image is linux/amd64 only, so on this arm64 host it runs")
	fmt.Fprintln(w, "  under emulation (slower, more memory). For native speed, run the browser on a")
	fmt.Fprintln(w, "  remote amd64 host via the `ssh` or `k8s` backend - see `cuttle context --help`.")
}

func printBriefingFor(w io.Writer, verb, name, ctxName string, ctx config.Context, cf commonFlags, ep backend.Endpoint, engine, image string, showImage bool) {
	cdp, viewer := endpointURLs(ep)
	cdp = withFingerprint(cdp, cf.profile)
	imageTail := ""
	if showImage && localBackend(ctx) {
		imageTail = ", image " + image
	}
	renderBriefing(w, briefing{
		verb:      verb,
		location:  locationLabel(ctxName, ctx, name),
		imageTail: imageTail,
		version:   cliVersion(),
		cdpURL:    cdp,
		viewerURL: viewer,
		engine:    engine,
		cdpPort:   ep.CDPPort,
		drivers:   detectDrivers(),
	})
}

// ---------------------------------------------------------------------------
// down
// ---------------------------------------------------------------------------

func newDownCmd() *cobra.Command {
	var cf commonFlags
	var purge bool
	cmd := &cobra.Command{
		Use:   "down",
		Short: "stop the browser gracefully (keeps the profile)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			name, ctxName, ctx, b, err := resolve(cf, defaultImage())
			if err != nil {
				return err
			}
			// Tear the standing tunnel down regardless of container state: an absent
			// container can still have a leftover forward from a prior session.
			if t, ok := b.(backend.Tunneler); ok {
				_ = t.StopTunnel()
			}
			state, err := b.State(cmd.Context())
			if err != nil {
				return err
			}
			if state == backend.StateAbsent {
				fmt.Fprintf(cmd.OutOrStdout(), "cuttle: nothing to stop (%s)\n", locationLabel(ctxName, ctx, name))
				return nil
			}
			if err := b.Stop(cmd.Context(), purge); err != nil {
				return err
			}
			if purge {
				fmt.Fprintf(cmd.OutOrStdout(), "cuttle: removed %s (profile discarded)\n", locationLabel(ctxName, ctx, name))
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "cuttle: stopped %s (profile kept; `cuttle up` to resume)\n", locationLabel(ctxName, ctx, name))
			}
			return nil
		},
	}
	addCommonFlags(cmd, &cf)
	cmd.Flags().BoolVar(&purge, "purge", false, "also remove the container/release and discard the profile")
	return cmd
}

// ---------------------------------------------------------------------------
// status
// ---------------------------------------------------------------------------

func newStatusCmd() *cobra.Command {
	var cf commonFlags
	cmd := &cobra.Command{
		Use:   "status",
		Short: "show browser + CDP state",
		RunE:  func(cmd *cobra.Command, _ []string) error { return runStatus(cmd, cf) },
	}
	addCommonFlags(cmd, &cf)
	return cmd
}

func runStatus(cmd *cobra.Command, cf commonFlags) error {
	name, ctxName, ctx, b, err := resolve(cf, defaultImage())
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	state, err := b.State(cmd.Context())
	if err != nil {
		return err
	}
	if state == backend.StateAbsent {
		return fmt.Errorf("%s: nothing running - run `cuttle up`", locationLabel(ctxName, ctx, name)) //nolint:err113 // user-facing remedy
	}

	// reachStable health-checks and re-establishes the standing tunnel for a
	// tunneled backend, so the endpoint below is the same stable one `up` printed.
	ep, release, err := reachStable(cmd.Context(), b, cf)
	if err != nil {
		return err
	}
	defer release()

	v := waitCDP(cmd.Context(), ep.CDPHost, ep.CDPPort, 5*time.Second)
	if state == backend.StateRunning && v != nil {
		printBriefingFor(out, "running", name, ctxName, ctx, cf, ep, browserOf(v), "", false)
		if img := localImage(cmd.Context(), b); img != "" {
			fmt.Fprintf(out, "  image   %s\n", img)
		}
		return nil
	}

	cdp, viewer := endpointURLs(ep)
	fmt.Fprintf(out, "%s: %s\n", locationLabel(ctxName, ctx, name), state)
	if v == nil {
		fmt.Fprintf(out, "  CDP     %s  (not answering)\n", cdp)
	} else {
		fmt.Fprintf(out, "  CDP     %s\n", cdp)
	}
	if viewer != "" {
		fmt.Fprintf(out, "  viewer  %s\n", viewer)
	}
	if img := localImage(cmd.Context(), b); img != "" {
		fmt.Fprintf(out, "  image   %s\n", img)
	}
	if d, ok := b.(interface {
		Diagnostics(context.Context) []string
	}); ok {
		for _, line := range d.Diagnostics(cmd.Context()) {
			fmt.Fprintf(out, "  %s\n", line)
		}
	}
	fmt.Fprintln(out, "  fix: `cuttle down && cuttle up` (keeps the profile), or")
	fmt.Fprintln(out, "    `cuttle up --recreate` to rebuild from scratch (discards the profile).")
	return errUnhealthy
}

var errUnhealthy = errors.New("browser unhealthy")

func localImage(ctx context.Context, b backend.Backend) string {
	if im, ok := b.(interface {
		Image(context.Context) string
	}); ok {
		return im.Image(ctx)
	}
	return ""
}

// ---------------------------------------------------------------------------
// open
// ---------------------------------------------------------------------------

func newOpenCmd() *cobra.Command {
	var cf commonFlags
	var noOpen bool
	cmd := &cobra.Command{
		Use:   "open [url]",
		Short: "hold a session open: print the briefing, optionally navigate, open the viewer (Ctrl-C to end)",
		// login/connect are the pre-overhaul verbs; kept as aliases for one
		// release. They do not show in help, which is the intended "hidden".
		Aliases: []string{"login", "connect"},
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := ""
			if len(args) == 1 {
				target = args[0]
			}
			return runOpen(cmd, cf, target, noOpen)
		},
	}
	addCommonFlags(cmd, &cf)
	addProfileFlag(cmd, &cf.profile)
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "print the viewer URL, don't open it in a browser")
	return cmd
}

func runOpen(cmd *cobra.Command, cf commonFlags, target string, noOpen bool) error {
	name, ctxName, ctx, b, err := resolve(cf, defaultImage())
	if err != nil {
		return err
	}
	// The phase-1 standing tunnel means open no longer pins ports itself: reachStable
	// yields the same stable endpoint on every backend.
	ep, release, err := reachStable(cmd.Context(), b, cf)
	if err != nil {
		return err
	}
	defer release()

	v := waitCDP(cmd.Context(), ep.CDPHost, ep.CDPPort, 30*time.Second)
	if v == nil {
		return errCDPNotAnswering
	}

	// Install the signal handler before checkout so a Ctrl-C during or right
	// after checkout still runs the deferred Close (checkin + lock release)
	// instead of terminating the process with the profile left locked.
	sigCtx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// With --profile, check the local auth state into the seed before navigating,
	// so the user signs in on top of any prior session, and check it back in on
	// exit (including Ctrl-C) so the fresh login is captured.
	sess, err := checkoutProfile(cmd.Context(), cf, ep)
	if err != nil {
		return err
	}
	if sess != nil {
		defer func() { _ = sess.Close() }()
	}

	out := cmd.OutOrStdout()
	if target != "" {
		title, nerr := navigate(cmd.Context(), ep.CDPHost, ep.CDPPort, target, ep.VNCPort, cf.profile)
		if nerr != nil {
			return fmt.Errorf("navigation failed: %w", nerr)
		}
		line := "navigated to " + target
		if title != "" {
			line += "  (" + title + ")"
		}
		fmt.Fprintln(out, line)
	}

	printBriefingFor(out, "open", name, ctxName, ctx, cf, ep, browserOf(v), "", false)

	if _, viewer := endpointURLs(ep); viewer != "" && !noOpen {
		openBrowser(viewer)
	}

	fmt.Fprintln(out, "session held open - press Ctrl-C to end the session.")
	<-sigCtx.Done()
	if sess != nil {
		fmt.Fprintln(out, "\ncuttle: saving profile state...")
	} else {
		fmt.Fprintln(out, "\ncuttle: session ended.")
	}
	return nil
}

// ---------------------------------------------------------------------------
// context ls
// ---------------------------------------------------------------------------

func newContextCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "context",
		Short: "manage cuttle contexts",
		Long: `Manage cuttle contexts.

A context names where the browser runs. Selection precedence: --context flag >
CUTTLE_CONTEXT env > default_context in the config > built-in "local". On every
backend cuttle exposes a stable local 127.0.0.1:9222 (CDP) and :6080 (viewer);
for ssh/k8s the backend owns a standing tunnel, established by up and
re-established by status.

The config lives at $XDG_CONFIG_HOME/cuttle/config.toml (default
~/.config/cuttle/config.toml). Create or update a context with 'context add'
(flags-first, no hand-editing needed). To run the browser on a remote amd64 host
and avoid the local emulation tax on Apple Silicon, add an ssh or k8s context and
make it the default:

  # ssh: docker on a remote host, reached over ssh -L. Inherits ~/.ssh/config.
  cuttle context add box --backend ssh --host user@box.example --default

  # k8s: a Deployment reached via kubectl port-forward. Inherits your kube config.
  cuttle context add cluster --backend k8s --namespace browser --release cuttle

  # direct: an already-running CDP endpoint, used as-is.
  cuttle context add tailnet --backend direct --cdp-url http://cuttle.example:9222

Advanced k8s knobs (node_selector, tolerations, resources) have no flags; add
them by hand-editing the written stanza, e.g.:

  [context.cluster]
  backend       = "k8s"
  namespace     = "browser"
  release       = "cuttle"
  node_selector = { "glim.sh/browser" = "true" }

Then run cuttle up / status / open as usual.`,
	}
	cmd.AddCommand(newContextLsCmd(), newContextAddCmd())
	return cmd
}

func newContextLsCmd() *cobra.Command {
	var contextName string
	ls := &cobra.Command{
		Use:   "ls",
		Short: "list contexts and show the active one",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			active, _, err := cfg.Active(contextName, os.Getenv(config.EnvContext))
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, n := range cfg.Names() {
				marker := "  "
				if n == active {
					marker = "* "
				}
				fmt.Fprintf(out, "%s%-16s %s\n", marker, n, cfg.Contexts[n].Backend)
			}
			return nil
		},
	}
	ls.Flags().StringVar(&contextName, "context", "", "context to highlight as active (default: config default_context, else local)")
	return ls
}

var (
	errInvalidBackend  = errors.New("invalid --backend")
	errReservedName    = errors.New(`"local" is a reserved built-in context name`)
	errSSHNeedsHost    = errors.New("ssh backend requires --host user@host")
	errDirectNeedsURL  = errors.New("direct backend requires --cdp-url")
	errSSHOnlyFlags    = errors.New("--namespace/--release/--kube-context/--cdp-url are not valid for the ssh backend")
	errK8sOnlyFlags    = errors.New("--host/--cdp-url are not valid for the k8s backend")
	errDirectOnlyFlags = errors.New("--host/--namespace/--release/--kube-context are not valid for the direct backend")
)

func newContextAddCmd() *cobra.Command {
	var (
		backendName string
		host        string
		proxy       string
		namespace   string
		release     string
		kubeContext string
		cdpURL      string
		makeDefault bool
	)
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "add or update a context in the config file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if name == config.BackendLocal {
				return errReservedName
			}
			ctx, err := buildContext(backendName, host, proxy, namespace, release, kubeContext, cdpURL)
			if err != nil {
				return err
			}
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if cfg.Contexts == nil {
				cfg.Contexts = map[string]config.Context{}
			}
			_, existed := cfg.Contexts[name]
			cfg.Contexts[name] = ctx
			if makeDefault {
				cfg.DefaultContext = name
			}
			path := config.DefaultPath()
			if err := cfg.Save(path); err != nil {
				return err
			}
			verb := "added"
			if existed {
				verb = "updated"
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "cuttle: %s context %q (%s) in %s\n", verb, name, ctx.Backend, path)
			if makeDefault {
				fmt.Fprintln(out, "  set as default_context")
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&backendName, "backend", "", "backend: ssh | k8s | direct (required)")
	f.StringVar(&host, "host", "", "ssh: user@host (required for ssh)")
	f.StringVar(&proxy, "proxy", "", "default proxy URL applied per seed (optional)")
	f.StringVar(&namespace, "namespace", "", `k8s: namespace (default "default")`)
	f.StringVar(&release, "release", "", `k8s: helm release name (default "cuttle")`)
	f.StringVar(&kubeContext, "kube-context", "", "k8s: kube context (optional; current if omitted)")
	f.StringVar(&cdpURL, "cdp-url", "", "direct: CDP endpoint URL, e.g. http://host:9222 (required for direct)")
	f.BoolVar(&makeDefault, "default", false, "set this context as default_context")
	_ = cmd.MarkFlagRequired("backend")
	return cmd
}

// buildContext validates the add flags per backend and returns the Context to
// persist. Flags belonging to a different backend are rejected rather than
// silently dropped, so a written context is never half-configured.
func buildContext(backendName, host, proxy, namespace, release, kubeContext, cdpURL string) (config.Context, error) {
	ctx := config.Context{Backend: backendName, Proxy: proxy}
	switch backendName {
	case config.BackendSSH:
		if host == "" {
			return config.Context{}, errSSHNeedsHost
		}
		if namespace != "" || release != "" || kubeContext != "" || cdpURL != "" {
			return config.Context{}, errSSHOnlyFlags
		}
		ctx.Host = host
	case config.BackendK8s:
		if host != "" || cdpURL != "" {
			return config.Context{}, errK8sOnlyFlags
		}
		ctx.Namespace, ctx.Release, ctx.KubeContext = namespace, release, kubeContext
	case config.BackendDirect:
		if cdpURL == "" {
			return config.Context{}, errDirectNeedsURL
		}
		if host != "" || namespace != "" || release != "" || kubeContext != "" {
			return config.Context{}, errDirectOnlyFlags
		}
		ctx.CDPURL = cdpURL
	default:
		return config.Context{}, fmt.Errorf("%w %q (want ssh, k8s, or direct)", errInvalidBackend, backendName)
	}
	return ctx, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// openBrowser best-effort opens a URL in the user's default browser.
func openBrowser(link string) {
	var name string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		name = "open"
		args = []string{link}
	case "windows":
		name, args = "rundll32", []string{"url.dll,FileProtocolHandler", link}
	default:
		name, args = "xdg-open", []string{link}
	}
	if _, err := exec.LookPath(name); err != nil {
		return
	}
	_ = exec.CommandContext(context.Background(), name, args...).Start()
}
