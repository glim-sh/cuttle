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

	"github.com/glim-sh/cuttle/packages/cuttle-go/internal/backend"
	"github.com/glim-sh/cuttle/packages/cuttle-go/internal/config"
	"github.com/glim-sh/cuttle/packages/cuttle-go/internal/profile"
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

var (
	errCDPNotAnswering = errors.New("CDP not answering - run `cuttle up` first")
	errNoContainer     = errors.New("no container (run `cuttle up`)")
)

func init() {
	AddCommand(newUpCmd(), newDownCmd(), newStatusCmd(), newLoginCmd(), newConnectCmd(), newMCPCmd(), newContextCmd())
}

// defaultImage is the published image tag matching this CLI's version, so it
// never drives a cuttleserve it was not shipped with. An uninstalled checkout
// reports "dev" (no such tag), so it falls back to latest.
func defaultImage() string {
	v := version
	if v == "dev" {
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
	f.IntVar(&cf.vncPort, "vnc-port", defaultVNCPort, "host VNC viewer port")
	f.BoolVar(&cf.noVNC, "no-vnc", false, "run without the VNC viewer")
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
func resolve(cf commonFlags, image string) (string, config.Context, backend.Backend, error) {
	cfg, err := config.Load()
	if err != nil {
		return "", config.Context{}, nil, err
	}
	name, ctx, err := cfg.Active(cf.contextName, os.Getenv(config.EnvContext))
	if err != nil {
		return "", config.Context{}, nil, err
	}
	b, err := backend.New(name, ctx, backend.ExecRunner{}, cf.cdpPort, cf.vncPort, image)
	if err != nil {
		return "", config.Context{}, nil, err
	}
	return name, ctx, b, nil
}

func locationLabel(ctxName string, ctx config.Context, name string) string {
	if ctx.Backend == config.BackendLocal || ctx.Backend == "" {
		return "container '" + name + "'"
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
		if v := cdpReady(ctx, host, port, 500*time.Millisecond); v != nil {
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
	cmd.Flags().StringVar(&uf.image, "image", "", "image (default "+defaultImage()+"; use cuttle:local for a local build)")
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

	ep, release, err := b.Reach(cmd.Context())
	if err != nil {
		return err
	}
	defer release()

	v := waitCDP(cmd.Context(), ep.CDPHost, ep.CDPPort, 30*time.Second)
	if v == nil {
		if before == backend.StateRunning {
			return fmt.Errorf("%q is running but CDP is not answering - run `cuttle status` to triage, then `cuttle down` and retry", name) //nolint:err113
		}
		return errors.New("started but CDP never came up - run `cuttle status` to triage") //nolint:err113
	}

	verb, showImage := "ready", true
	switch before {
	case backend.StateRunning:
		verb, showImage = "already running", false
	case backend.StateStopped:
		verb, showImage = "restarted", false
	case backend.StateAbsent:
	}
	image := uf.image
	if image == "" {
		image = defaultImage()
	}
	printBriefingFor(cmd.OutOrStdout(), verb, name, ctx, uf.common, ep, browserOf(v), image, showImage)
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
		verb:      verb,
		location:  locationLabel(cf.contextName, ctx, name),
		imageTail: imageTail,
		version:   version,
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
			name, ctx, b, err := resolve(cf, defaultImage())
			if err != nil {
				return err
			}
			state, err := b.State(cmd.Context())
			if err != nil {
				return err
			}
			if state == backend.StateAbsent {
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
		return errNoContainer
	}

	ep, release, err := b.Reach(cmd.Context())
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
	_, _, b, err := resolve(cf, defaultImage())
	if err != nil {
		return err
	}
	ep, release, err := b.Reach(cmd.Context())
	if err != nil {
		return err
	}
	defer release()

	if cdpReady(cmd.Context(), ep.CDPHost, ep.CDPPort, 5*time.Second) == nil {
		return errCDPNotAnswering
	}

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
	if viewer != "" {
		fmt.Fprintf(out, "open the viewer to sign in:  %s\n", viewer)
		if !noOpen {
			openBrowser(viewer)
		}
	}

	if sess != nil {
		fmt.Fprintln(out, "profile checked out - sign in via the viewer, then press Ctrl-C to save the session.")
		sigCtx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		<-sigCtx.Done()
		fmt.Fprintln(out, "\ncuttle: saving profile state...")
	}
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
	ep, release, err := b.Reach(cmd.Context())
	if err != nil {
		return err
	}
	defer release()

	v := waitCDP(cmd.Context(), ep.CDPHost, ep.CDPPort, 30*time.Second)
	if v == nil {
		return errCDPNotAnswering
	}

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

	sigCtx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
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
