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
	AddCommand(newUpCmd(), newDownCmd(), newStatusCmd(), newLoginCmd(), newViewCmd(), newConnectCmd(), newMCPCmd(), newContextCmd(), newNativeCmd())
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
	name        string
	cdpPort     int
	vncPort     int
	noVNC       bool
	profile     string // seed; set only on verbs that take --profile (login/connect/mcp)
}

func addCommonFlags(cmd *cobra.Command, cf *commonFlags) {
	f := cmd.Flags()
	f.StringVar(&cf.contextName, "context", "", "context to use (default: config default_context, else local)")
	f.StringVar(&cf.name, "name", defaultName, "container name")
	f.IntVar(&cf.cdpPort, "cdp-port", defaultCDPPort, "host CDP port")
	f.IntVar(&cf.vncPort, "vnc-port", defaultVNCPort, "host VNC viewer port (docker/local backend only)")
	f.BoolVar(&cf.noVNC, "no-vnc", false, "run without the VNC viewer (docker/local backend only)")
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
// For the docker-container backends (local, ssh) the container is named by the
// --name flag (default "cuttle", matching the Python CLI); k8s/direct ignore it
// and are identified by their context.
func resolve(cf commonFlags, image string) (string, config.Context, backend.Backend, error) {
	cfg, err := config.Load()
	if err != nil {
		return "", config.Context{}, nil, err
	}
	ctxName, ctx, err := cfg.Active(cf.contextName, os.Getenv(config.EnvContext))
	if err != nil {
		return "", config.Context{}, nil, err
	}
	name := ctxName
	if ctx.Backend == config.BackendLocal || ctx.Backend == config.BackendSSH || ctx.Backend == config.BackendNative {
		name = cf.name
	}
	b, err := backend.New(name, ctx, backend.ExecRunner{}, cf.cdpPort, cf.vncPort, image)
	if err != nil {
		return "", config.Context{}, nil, err
	}
	return name, ctx, b, nil
}

// flagSuffix echoes the non-default --name/--cdp-port a command was invoked
// with, so a remedy hint copy-pastes back to the SAME instance instead of the
// default one (the trap in a multi-instance native setup).
func flagSuffix(cf commonFlags) string {
	s := ""
	if cf.name != "" && cf.name != defaultName {
		s += " --name " + cf.name
	}
	if cf.cdpPort != 0 && cf.cdpPort != defaultCDPPort {
		s += " --cdp-port " + strconv.Itoa(cf.cdpPort)
	}
	return s
}

func resumeCmd(cf commonFlags) string { return "cuttle up" + flagSuffix(cf) }

func locationLabel(ctxName string, ctx config.Context, name string) string {
	if ctx.Backend == config.BackendLocal || ctx.Backend == "" {
		return "container '" + name + "'"
	}
	if ctx.Backend == config.BackendNative {
		return "native '" + name + "'"
	}
	return "context '" + ctxName + "'"
}

func endpointURLs(ep backend.Endpoint, noVNC bool) (string, string) {
	cdp := "http://" + net.JoinHostPort(ep.CDPHost, strconv.Itoa(ep.CDPPort))
	viewer := ""
	if !noVNC && ep.VNCPort != 0 {
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
	return cmd
}

func runUp(cmd *cobra.Command, uf *upFlags) error {
	name, ctx, b, err := resolve(uf.common, defaultImage())
	if err != nil {
		return err
	}
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
		NoVNC:       uf.common.noVNC,
		Proxy:       ctx.Proxy,
		Storage:     config.StorageLocal,
	}
	if err = b.Start(cmd.Context(), opts); err != nil {
		return err
	}

	ep, release, err := b.Reach(cmd.Context(), 0, 0)
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
	printBriefingFor(cmd.OutOrStdout(), verb, name, ctx, uf.common, ep, browserOf(v), image, showImage)
	if uf.recreate && before != backend.StateAbsent {
		fmt.Fprintln(cmd.OutOrStdout(), "  note: --recreate discarded the previous profile (cookies/logins) - fresh identity")
	}
	return nil
}

func printBriefingFor(w io.Writer, verb, name string, ctx config.Context, cf commonFlags, ep backend.Endpoint, engine, image string, showImage bool) {
	cdp, viewer := endpointURLs(ep, cf.noVNC)
	cdp = withFingerprint(cdp, cf.profile)
	imageTail := ""
	if showImage && (ctx.Backend == config.BackendLocal || ctx.Backend == "") {
		imageTail = ", image " + image
	}
	renderBriefing(w, briefing{
		verb:       verb,
		location:   locationLabel(cf.contextName, ctx, name),
		imageTail:  imageTail,
		version:    cliVersion(),
		cdpURL:     cdp,
		viewerURL:  viewer,
		windowMode: ctx.Backend == config.BackendNative,
		engine:     engine,
		cdpPort:    ep.CDPPort,
		drivers:    detectDrivers(),
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
			name, ctx, b, err := resolve(cf, defaultImage())
			if err != nil {
				return err
			}
			state, err := b.State(cmd.Context())
			if err != nil {
				return err
			}
			if state == backend.StateAbsent {
				// A native instance can leave an orphaned dir behind (e.g. a failed
				// `up`); --purge sweeps it even though nothing is running.
				if purge && ctx.Backend == config.BackendNative {
					if err := b.Stop(cmd.Context(), true); err != nil {
						return err
					}
					fmt.Fprintf(cmd.OutOrStdout(), "cuttle: removed leftover state for %s\n", locationLabel(cf.contextName, ctx, name))
					return nil
				}
				fmt.Fprintf(cmd.OutOrStdout(), "cuttle: nothing to stop (%s)\n", locationLabel(cf.contextName, ctx, name))
				return nil
			}
			if err := b.Stop(cmd.Context(), purge); err != nil {
				return err
			}
			if purge {
				fmt.Fprintf(cmd.OutOrStdout(), "cuttle: removed %s (profile discarded)\n", locationLabel(cf.contextName, ctx, name))
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "cuttle: stopped %s (profile kept; `cuttle up` to resume)\n", locationLabel(cf.contextName, ctx, name))
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
	name, ctx, b, err := resolve(cf, defaultImage())
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	state, err := b.State(cmd.Context())
	if err != nil {
		return err
	}
	if state == backend.StateAbsent {
		return fmt.Errorf("%s: nothing running - run `%s`", locationLabel(cf.contextName, ctx, name), resumeCmd(cf)) //nolint:err113 // user-facing remedy
	}

	ep, release, err := b.Reach(cmd.Context(), 0, 0)
	if err != nil {
		return err
	}
	defer release()

	v := waitCDP(cmd.Context(), ep.CDPHost, ep.CDPPort, 5*time.Second)
	if state == backend.StateRunning && v != nil {
		printBriefingFor(out, "running", name, ctx, cf, ep, browserOf(v), "", false)
		if img := localImage(cmd.Context(), b); img != "" {
			fmt.Fprintf(out, "  image   %s\n", img)
		}
		return nil
	}

	cdp, viewer := endpointURLs(ep, cf.noVNC)
	fmt.Fprintf(out, "%s: %s\n", locationLabel(cf.contextName, ctx, name), state)
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
	fmt.Fprintf(out, "  fix: `cuttle down%s && %s` (keeps the profile), or\n", flagSuffix(cf), resumeCmd(cf))
	fmt.Fprintf(out, "    `%s --recreate` to rebuild from scratch (discards the profile).\n", resumeCmd(cf))
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
// login
// ---------------------------------------------------------------------------

func newLoginCmd() *cobra.Command {
	var cf commonFlags
	var noOpen bool
	cmd := &cobra.Command{
		Use:   "login <url>",
		Short: "navigate to a URL and open the viewer to sign in",
		Args:  cobra.ExactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return runLogin(cmd, cf, args[0], noOpen) },
	}
	addCommonFlags(cmd, &cf)
	addProfileFlag(cmd, &cf.profile)
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "print the viewer URL, don't open it")
	return cmd
}

func runLogin(cmd *cobra.Command, cf commonFlags, target string, noOpen bool) error {
	_, ctx, b, err := resolve(cf, defaultImage())
	if err != nil {
		return err
	}
	ep, release, err := b.Reach(cmd.Context(), 0, 0)
	if err != nil {
		return err
	}
	defer release()

	if cdpReady(cmd.Context(), ep.CDPHost, ep.CDPPort, 5*time.Second) == nil {
		return errCDPNotAnswering
	}

	// Install the signal handler before checkout so a Ctrl-C during or right
	// after checkout still runs the deferred Close (checkin + lock release).
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

	vncPort := 0
	if !cf.noVNC {
		vncPort = ep.VNCPort
	}
	title, err := navigate(cmd.Context(), ep.CDPHost, ep.CDPPort, target, vncPort, cf.profile)
	if err != nil {
		return fmt.Errorf("navigation failed: %w", err)
	}
	out := cmd.OutOrStdout()
	line := "navigated to " + target
	if title != "" {
		line += "  (" + title + ")"
	}
	fmt.Fprintln(out, line)
	_, viewer := endpointURLs(ep, cf.noVNC)
	signInWhere := "the viewer"
	switch {
	case viewer != "":
		fmt.Fprintf(out, "open the viewer to sign in:  %s\n", viewer)
		if !noOpen {
			openBrowser(viewer)
		}
	case ctx.Backend == config.BackendNative:
		signInWhere = "the browser window"
		if r, ok := b.(windowRaiser); ok && !noOpen {
			_ = r.RaiseWindow(cmd.Context(), cf.profile)
			fmt.Fprintln(out, "brought the browser to the front - sign in there.")
			fmt.Fprintln(out, "  Not visible? The window can be on another macOS Space - find it via Mission Control (Ctrl-Up).")
		} else {
			fmt.Fprintln(out, "run `cuttle view` to bring the browser window forward and sign in.")
		}
	}

	if sess != nil {
		fmt.Fprintf(out, "profile checked out - sign in via %s, then press Ctrl-C to save the session.\n", signInWhere)
		<-sigCtx.Done()
		fmt.Fprintln(out, "\ncuttle: saving profile state...")
	}
	return nil
}

// ---------------------------------------------------------------------------
// view (native macOS backend: raise the real browser window for handoff)
// ---------------------------------------------------------------------------

// windowRaiser is the native backend's handoff surface: bring a seed's real
// desktop browser to the front in place of a VNC viewer. It errors only when the
// browser could not be surfaced at all (not running / still launching); a nil
// return means it was activated, though the window can still sit on another
// macOS Space (the CLI tells the user how to reach it).
type windowRaiser interface {
	RaiseWindow(ctx context.Context, seed string) error
}

func newViewCmd() *cobra.Command {
	var cf commonFlags
	cmd := &cobra.Command{
		Use:   "view [profile]",
		Short: "raise the browser window on your desktop (native macOS backend)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			seed := ""
			if len(args) == 1 {
				seed = args[0]
			}
			return runView(cmd, cf, seed)
		},
	}
	addCommonFlags(cmd, &cf)
	return cmd
}

func runView(cmd *cobra.Command, cf commonFlags, seed string) error {
	name, ctx, b, err := resolve(cf, defaultImage())
	if err != nil {
		return err
	}
	r, ok := b.(windowRaiser)
	if !ok {
		return fmt.Errorf("`cuttle view` needs the native macOS backend, but the active context uses the %q backend", ctx.Backend) //nolint:err113
	}
	state, err := b.State(cmd.Context())
	if err != nil {
		return err
	}
	if state == backend.StateAbsent {
		return fmt.Errorf("%s: nothing running - run `%s`", locationLabel(cf.contextName, ctx, name), resumeCmd(cf)) //nolint:err113 // user-facing remedy
	}
	out := cmd.OutOrStdout()
	loc := locationLabel(cf.contextName, ctx, name)
	if err := r.RaiseWindow(cmd.Context(), seed); err != nil {
		fmt.Fprintf(out, "cuttle: couldn't bring the browser forward for %s: %v\n", loc, err)
		return nil
	}
	fmt.Fprintf(out, "cuttle: brought the browser to the front for %s.\n", loc)
	fmt.Fprintln(out, "  Not visible? The window can open on another macOS Space - find it via Mission Control (Ctrl-Up).")
	fmt.Fprintln(out, "  For precise per-window focus, grant Automation permission (System Settings > Privacy & Security > Automation).")
	return nil
}

// ---------------------------------------------------------------------------
// connect
// ---------------------------------------------------------------------------

func newConnectCmd() *cobra.Command {
	var cf commonFlags
	cmd := &cobra.Command{
		Use:   "connect",
		Short: "hold a forward open in the foreground and print the driver briefing (Ctrl-C to end)",
		RunE:  func(cmd *cobra.Command, _ []string) error { return runConnect(cmd, cf) },
	}
	addCommonFlags(cmd, &cf)
	addProfileFlag(cmd, &cf.profile)
	return cmd
}

func runConnect(cmd *cobra.Command, cf commonFlags) error {
	name, ctx, b, err := resolve(cf, defaultImage())
	if err != nil {
		return err
	}
	// connect holds the forward open for a driver to attach to, so it pins the
	// local ports (matching what `cuttle mcp` writes); the ephemeral commands
	// auto-pick instead.
	ep, release, err := b.Reach(cmd.Context(), cf.cdpPort, cf.vncPort)
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

	sess, err := checkoutProfile(cmd.Context(), cf, ep)
	if err != nil {
		return err
	}
	if sess != nil {
		defer func() { _ = sess.Close() }()
	}

	out := cmd.OutOrStdout()
	printBriefingFor(out, "connected", name, ctx, cf, ep, browserOf(v), "", false)
	fmt.Fprintln(out, "forward held open - press Ctrl-C to end the session.")

	<-sigCtx.Done()
	if sess != nil {
		fmt.Fprintln(out, "\ncuttle: saving profile state...")
	} else {
		fmt.Fprintln(out, "\ncuttle: forward closed.")
	}
	return nil
}

// ---------------------------------------------------------------------------
// context ls
// ---------------------------------------------------------------------------

func newContextCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "context", Short: "inspect cuttle contexts"}
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
	ls.Flags().StringVar(&contextName, "context", "", "context to mark active (default: config default_context, else local)")
	cmd.AddCommand(ls)
	return cmd
}

// ---------------------------------------------------------------------------
// native ls (native macOS backend: list instances incl. orphaned dirs)
// ---------------------------------------------------------------------------

func newNativeCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "native", Short: "inspect native macOS instances"}
	ls := &cobra.Command{
		Use:   "ls",
		Short: "list native instances (running and stopped/orphaned)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			insts, err := backend.ListNative()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(insts) == 0 {
				fmt.Fprintln(out, "no native instances")
				return nil
			}
			for _, in := range insts {
				state := "stopped"
				if in.Running {
					state = "running"
				}
				fmt.Fprintf(out, "  %-20s %s\n", in.Name, state)
			}
			fmt.Fprintln(out, "remove one (and its profile) with `cuttle down --name <name> --purge`")
			return nil
		},
	}
	cmd.AddCommand(ls)
	return cmd
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
