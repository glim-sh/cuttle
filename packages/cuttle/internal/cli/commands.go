package cli

import (
	"cmp"
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
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/glim-sh/cuttle/internal/backend"
	"github.com/glim-sh/cuttle/internal/config"
	"github.com/glim-sh/cuttle/internal/mask"
)

// boolFlag is an optional bool: unset (nil) is distinct from explicit
// true/false, so up can warn that --keep-profile is fixed at container creation.
// `--keep-profile` sets true (NoOptDefVal); `--keep-profile=false` sets false.
// noOptDefTrue is the NoOptDefVal for tri-state bool flags, so a bare --flag
// reads as true while --flag=false still disables.
const noOptDefTrue = "true"

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
	defaultName    = backend.DefaultContainerName
	defaultCDPPort = 9222
	defaultVNCPort = 6080
	imageRepo      = "ghcr.io/glim-sh/cuttle"
	// localImageTag is the tag `just build-image` produces; a dev build defaults
	// to it (see defaultImage) instead of a published tag it has no match for.
	localImageTag = "cuttle:local"
)

func init() {
	AddCommand(newUpCmd(), newDownCmd(), newStatusCmd(), newOpenCmd(), newDownloadsCmd(), newLogsCmd(), newPurgeProfileCmd(), newContextCmd(), newSuperviseTunnelCmd())
}

// newSuperviseTunnelCmd is the hidden supervisor the backend re-execs (see
// backend.superviseCommand) to keep a standing ssh/kubectl forward alive with
// auto-reconnect and no autossh dependency. Flag parsing is off because its args
// are the forward's own argv (carrying -N/-o/-L), not cuttle flags.
func newSuperviseTunnelCmd() *cobra.Command {
	return &cobra.Command{
		Use:                backend.SuperviseTunnelSubcmd,
		Hidden:             true,
		DisableFlagParsing: true,
		Args:               cobra.MinimumNArgs(1),
		Run: func(_ *cobra.Command, args []string) {
			backend.SuperviseTunnel(args[0], args[1:])
		},
	}
}

// defaultImage is the image the CLI runs by default. A release build pins to its
// own version (repo:<version>) so the CLI never drives a `cuttle serve` from an
// image it was not shipped with. A dev build (version "dev", no matching published
// tag) uses the local-build tag `just build-image` produces, so `cuttle up` works
// from a source checkout once the image is built. It is never a floating :latest,
// which decouples the CLI from its daemon and once silently resolved to an
// unrelated image. --image overrides both.
func defaultImage() string {
	if cliVersion() == devVersion {
		return localImageTag
	}
	return imageRepo + ":" + cliVersion()
}

type commonFlags struct {
	cdpPort int
	vncPort int
}

func addCommonFlags(cmd *cobra.Command, cf *commonFlags) {
	f := cmd.Flags()
	f.IntVar(&cf.cdpPort, "cdp-port", defaultCDPPort, "host CDP port (verbs that reach an existing instance - up, status, open, downloads, secret, auth, grab - discover it; pass this only to pin ports when 'up' creates the container)")
	f.IntVar(&cf.vncPort, "vnc-port", defaultVNCPort, "host VNC viewer port (discovered like --cdp-port; pass this only to pin ports when 'up' creates the container)")
}

// instanceFlags is WHICH instance a verb acts on. Unlike the per-verb flags it
// is one selection for the whole invocation, registered once as root persistent
// flags, so every verb that reaches an instance honors it - `cuttle pw` and
// `cuttle jev-browse` included, which register no flags of their own.
type instanceFlags struct {
	contextName string
	name        string // docker container name override (--name); "" = default "cuttle"
}

// instance is that selection. It is global because the flags are: cobra binds
// root's persistent flags once, before any verb runs, and resolve is the single
// reader.
var instance instanceFlags

func addInstanceFlags(cmd *cobra.Command) {
	f := cmd.PersistentFlags()
	f.StringVar(&instance.contextName, "context", "", "context to use (default: "+config.EnvContext+", else config default_context, else local)")
	f.StringVar(&instance.name, "name", "", "container name for the docker (local/ssh) backends; run multiple isolated instances on one host by giving each its own --name and ports (default: "+config.EnvName+", else the context's name, else "+defaultName+")")
}

// containerName resolves which container a docker-backed context acts on:
// flag > env > the context's own `name` > the built-in default. k8s/direct are
// identified by their context name instead and ignore all of it.
func containerName(ctxName string, ctx config.Context, flag, env string) string {
	if ctx.Backend != config.BackendLocal && ctx.Backend != config.BackendSSH {
		return ctxName
	}
	return cmp.Or(flag, env, ctx.Name, defaultName)
}

// resolve loads the config, selects the active context, and builds its backend.
// It is the one place instance selection is decided: the context by
// [config.Config.Active], the container name by [containerName], each taking the
// root --context/--name flag first, then its env var, then the config.
func resolve(cf commonFlags, image string) (string, string, config.Context, backend.Backend, error) {
	cfg, err := config.Load()
	if err != nil {
		return "", "", config.Context{}, nil, err
	}
	ctxName, ctx, err := cfg.Active(instance.contextName, os.Getenv(config.EnvContext))
	if err != nil {
		return "", "", config.Context{}, nil, err
	}
	name := containerName(ctxName, ctx, instance.name, os.Getenv(config.EnvName))
	b, err := backend.New(name, ctxName, ctx, backend.ExecRunner{}, cf.cdpPort, cf.vncPort, image)
	if err != nil {
		return "", "", config.Context{}, nil, err
	}
	return name, ctxName, ctx, b, nil
}

// resolveRunning is resolve for verbs that target an EXISTING instance's ports
// (up, status; open, downloads, secret, auth and grab through resolveLive). When
// the user did not pin ports, it discovers the instance's own CDP/VNC ports -
// running or stopped - so only --context/--name is needed, and updates both cf
// and the backend to them. It keeps resolve's defaults only when the user passed
// explicit ports, the backend cannot discover (k8s/direct), or no container
// exists. An existing container whose ports cannot be read is an error: the
// default ports belong to whichever other instance holds them.
func resolveRunning(cmd *cobra.Command, cf *commonFlags, image string) (string, string, config.Context, backend.Backend, error) {
	name, ctxName, ctx, b, err := resolve(*cf, image)
	if err != nil {
		return name, ctxName, ctx, b, err
	}
	if cmd.Flags().Changed("cdp-port") || cmd.Flags().Changed("vnc-port") {
		return name, ctxName, ctx, b, nil
	}
	pd, ok := b.(backend.PortDiscoverer)
	if !ok {
		return name, ctxName, ctx, b, nil
	}
	cdpPort, vncPort, ok := pd.DiscoverPorts(cmd.Context())
	if !ok {
		state, err := b.State(cmd.Context())
		if err != nil {
			return name, ctxName, ctx, b, err
		}
		if state != backend.StateAbsent {
			return name, ctxName, ctx, b, fmt.Errorf("%s: cannot read its CDP/VNC ports - pass the --cdp-port/--vnc-port it was created with", locationLabel(ctxName, ctx, name)) //nolint:err113 // user-facing remedy
		}
		return name, ctxName, ctx, b, nil
	}
	if cdpPort == cf.cdpPort && vncPort == cf.vncPort {
		return name, ctxName, ctx, b, nil
	}
	cf.cdpPort, cf.vncPort = cdpPort, vncPort
	// Rebuild so the backend struct carries the discovered ports (Reach/EnsureTunnel
	// read them off it). New cannot fail here - the same context already built a
	// valid backend above and ports do not affect validity.
	b, _ = backend.New(name, ctxName, ctx, backend.ExecRunner{}, cdpPort, vncPort, image)
	return name, ctxName, ctx, b, nil
}

// resolveLive is resolveRunning for verbs that act on the live session (open,
// downloads, secret, auth, grab). The selected instance must be running: a
// stopped or absent one answers nothing, and whatever does answer on its ports
// is some other instance's browser.
func resolveLive(cmd *cobra.Command, cf *commonFlags) (string, string, config.Context, backend.Backend, error) {
	name, ctxName, ctx, b, err := resolveRunning(cmd, cf, defaultImage())
	if err != nil {
		return name, ctxName, ctx, b, err
	}
	state, err := b.State(cmd.Context())
	if err != nil {
		return name, ctxName, ctx, b, err
	}
	if state != backend.StateRunning {
		return name, ctxName, ctx, b, errNotRunning(ctxName, ctx, name, state)
	}
	return name, ctxName, ctx, b, nil
}

func errNotRunning(ctxName string, ctx config.Context, name string, state backend.State) error {
	return fmt.Errorf("%s: %s - run `%s up` first", locationLabel(ctxName, ctx, name), state, cuttleCmd(ctxName, ctx, name)) //nolint:err113 // user-facing remedy
}

func errCDPNotAnswering(ctxName string, ctx config.Context, name string) error {
	return fmt.Errorf("%s: CDP not answering - run `%s status` to triage", locationLabel(ctxName, ctx, name), cuttleCmd(ctxName, ctx, name)) //nolint:err113 // user-facing remedy
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

// localBackend reports whether the context runs the image in docker on this host -
// the case that has a container name and an image tail, as opposed to a
// remote/tunneled backend.
func localBackend(ctx config.Context) bool {
	return ctx.Backend == config.BackendLocal || ctx.Backend == ""
}

func locationLabel(ctxName string, ctx config.Context, name string) string {
	if localBackend(ctx) {
		return "container '" + name + "'"
	}
	// Remote (ssh/k8s): the context names where it runs. A non-default --name is a
	// separate instance on that context, so name it explicitly - otherwise two
	// instances print the same label and a message about one instance reads as if
	// it were about the whole context.
	if name != defaultName {
		return "container '" + name + "' on context '" + ctxName + "'"
	}
	return "context '" + ctxName + "'"
}

// cuttleCmd is the `cuttle` invocation that reaches this same instance again, for
// the next-step commands cuttle prints: a bare `cuttle pw` in a non-default
// instance's briefing drives the default container instead. Each flag rides along
// only when a bare invocation would not already land on that value.
func cuttleCmd(ctxName string, ctx config.Context, name string) string {
	cmd := "cuttle"
	if instance.contextName != "" || os.Getenv(config.EnvContext) != "" {
		cmd += " --context " + ctxName
	}
	if name != containerName(ctxName, ctx, "", "") {
		cmd += " --name " + name
	}
	return cmd
}

func endpointURLs(ep backend.Endpoint) (string, string) {
	cdpURL := "http://" + net.JoinHostPort(ep.CDPHost, strconv.Itoa(ep.CDPPort))
	viewer := ""
	if ep.VNCPort != 0 {
		viewer = "http://" + net.JoinHostPort(ep.VNCHost, strconv.Itoa(ep.VNCPort)) + "/"
	}
	return cdpURL, viewer
}

// daemonState is the multiplexer's health root: it reports the live browser
// count without launching one, which /json/version cannot do.
type daemonState struct {
	Active int `json:"active"`
}

// daemonHealth polls the daemon's root until it answers, or the timeout expires.
// nil means the daemon never answered. The whole poll is bound to the timeout -
// getJSON carries its own (longer) per-request deadline, so testing the clock
// only between attempts let a daemon that accepts the connection and then stalls
// blow the caller's budget.
func daemonHealth(ctx context.Context, host string, port int, timeout time.Duration) *daemonState {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	endpoint := "http://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/"
	for {
		var st daemonState
		if err := getJSON(ctx, endpoint, &st); err == nil {
			return &st
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func cdpReady(ctx context.Context, host string, port int, timeout time.Duration) map[string]any {
	// Only a live browser answers 200 here, and daemonRequest turns anything else
	// into an error - a launch error (e.g. serve's backoff 503) returns a JSON
	// {"error":...} body that would otherwise unmarshal fine and read as readiness.
	// daemonRequest rather than requestJSON: the timeout is the CALLER's here
	// (waitCDP hands over its whole remaining budget on purpose), and requestJSON
	// would cap it at daemonTimeout and cancel a cold launch mid-flight.
	endpoint := "http://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/json/version"
	data, err := daemonRequest(ctx, http.MethodGet, endpoint, nil, 1<<20, timeout)
	if err != nil {
		return nil
	}
	var v map[string]any
	if json.Unmarshal(data, &v) != nil {
		return nil
	}
	return v
}

func waitCDP(ctx context.Context, host string, port int, timeout time.Duration) map[string]any {
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil
		}
		// Give each poll the whole remaining budget, not a fixed few seconds: the
		// daemon holds /json/version open until Chrome's CDP is up, and a cold launch
		// under CPU emulation can take tens of seconds. A short per-poll timeout would
		// cancel the request mid-launch, and because the handler then reads Chrome over
		// that same (now-canceled) request context, a browser that IS ready gets
		// reported as "never came up". A fast non-200 (launch backoff / invalid seed)
		// still returns immediately, so we back off and retry.
		if v := cdpReady(ctx, host, port, remaining); v != nil {
			return v
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func browserOf(v map[string]any) string {
	if v == nil {
		return ""
	}
	b, _ := v["Browser"].(string)
	return b
}

// getJSON does a context-bound GET and decodes a JSON body. It is the CLI's
// read side of the daemon's state API.
func getJSON(ctx context.Context, endpoint string, out any) error {
	return requestJSON(ctx, http.MethodGet, endpoint, nil, out)
}

// ---------------------------------------------------------------------------
// up
// ---------------------------------------------------------------------------

type upFlags struct {
	common       commonFlags
	image        string
	keepProfile  boolFlag
	humanize     boolFlag
	ephemeral    bool
	purgeProfile bool
	recreate     bool
	idleTimeout  string
	screen       string

	allowContextCreation   bool
	blockThirdPartyCookies bool
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
	// --keep-profile is now the default and effectively a no-op kept for
	// compatibility; --keep-profile=false is a synonym for --ephemeral.
	cmd.Flags().Var(&uf.keepProfile, "keep-profile", "deprecated: the full Chrome profile is now persisted by default in a named volume; --keep-profile=false is a synonym for --ephemeral")
	cmd.Flags().Lookup("keep-profile").NoOptDefVal = noOptDefTrue
	_ = cmd.Flags().MarkHidden("keep-profile")
	cmd.Flags().BoolVar(&uf.ephemeral, "ephemeral", false, "use a disposable profile: no persistent volume, discarded on recreate/down --purge (opt out of the default persistent profile)")
	cmd.Flags().BoolVar(&uf.purgeProfile, "purge-profile", false, "remove the persistent profile (volume on local/ssh, PVC on k8s) before starting, so it comes up with a fresh profile (implies --recreate)")
	cmd.Flags().BoolVar(&uf.recreate, "recreate", false, "destroy any existing container and start fresh (the persistent profile survives; add --purge-profile to also reset it)")
	cmd.Flags().StringVar(&uf.idleTimeout, "idle-timeout", "", `seconds of no CDP client activity after which an idle per-seed browser is closed; "0" = off (default off)`)
	cmd.Flags().StringVar(&uf.screen, "screen", "", `screen size the browser claims and is sized to, "WxH" from the image persona's table (default: the context's "screen", else the persona's largest; cuttle serve --help in the image lists the choices)`)
	cmd.Flags().Var(&uf.humanize, "humanize", "rewrite CDP Input into human-like mouse/keyboard/scroll so interactions defeat behavioral detection (on by default; --humanize=false to disable)")
	cmd.Flags().Lookup("humanize").NoOptDefVal = noOptDefTrue
	cmd.Flags().BoolVar(&uf.allowContextCreation, "allow-context-creation", false, "let drivers call Target.createBrowserContext instead of rejecting it, for a stack whose browser.newContext() is not optional (off by default: one identity per seed)")
	cmd.Flags().BoolVar(&uf.blockThirdPartyCookies, "block-third-party-cookies", false, "block third-party cookies (off by default, matching stock Chrome). Blocking them breaks embedded SSO, silent token refresh and some payment/captcha challenges - those flows load but never finish")
	return cmd
}

// warnBakedFlags tells the user which of the flags they just passed cannot take
// effect on an existing docker-backed container, since its env is fixed at
// creation.
func warnBakedFlags(cmd *cobra.Command, name string, flags ...string) {
	for _, f := range flags {
		if cmd.Flags().Changed(f) {
			fmt.Fprintf(os.Stderr, "cuttle: --%s is fixed when the container is created; %q keeps its original setting (use --recreate to change it)\n", f, name)
		}
	}
}

func runUp(cmd *cobra.Command, uf *upFlags) error {
	// resolveRunning, not resolve: an existing container keeps its own ports on a
	// restart, an idempotent up and a --recreate, so those must be the ports
	// checked and probed - not the defaults another instance may hold. Unreadable
	// ports fail here, before a --recreate tears anything down.
	name, ctxName, ctx, b, err := resolveRunning(cmd, &uf.common, defaultImage())
	if err != nil {
		return err
	}
	before, err := b.State(cmd.Context())
	if err != nil {
		return err
	}

	if before != backend.StateAbsent {
		// --image only takes effect on a fresh container; a plain restart keeps the
		// image it was created with. --recreate (and --purge-profile, which implies
		// it) DO rebuild with the new image, so only warn when neither is set.
		if uf.image != "" && !uf.recreate && !uf.purgeProfile {
			fmt.Fprintf(os.Stderr, "cuttle: --image is fixed when the container is created; %q keeps the image it was created with (use --recreate to change it)\n", name)
		}
		// The persistence choice (volume + keep-profile env) is baked at container
		// creation, so flipping --ephemeral/--keep-profile on an existing container
		// only takes effect on a --recreate (--purge-profile also recreates).
		if (uf.ephemeral || uf.keepProfile.set) && !uf.recreate && !uf.purgeProfile {
			fmt.Fprintf(os.Stderr, "cuttle: profile persistence is fixed when the container is created; %q keeps its original setting (use --recreate to change it)\n", name)
		}
		// On docker-backed backends these are baked into the container env at
		// creation, so a restart via `docker start` ignores a new value. (k8s
		// re-applies them on every `helm upgrade`, so they are not fixed there.)
		if localBackend(ctx) || ctx.Backend == config.BackendSSH {
			warnBakedFlags(cmd, name, "idle-timeout", "screen", "humanize", "allow-context-creation", "block-third-party-cookies")
		}
	}

	opts := backend.StartOpts{
		Image:        uf.image,
		Recreate:     uf.recreate,
		Ephemeral:    uf.ephemeral,
		PurgeProfile: uf.purgeProfile,
		KeepProfile:  uf.keepProfile.value(),
		Proxy:        ctx.Proxy,
		IdleTimeout:  uf.idleTimeout,
		Screen:       cmp.Or(uf.screen, ctx.Screen),
		Humanize:     uf.humanize.value(),

		AllowContextCreation:   uf.allowContextCreation,
		BlockThirdPartyCookies: uf.blockThirdPartyCookies,
	}
	// Single source of truth for the persist decision - the backend derives the
	// volume/PVC choice from the same predicate, so the CLI never re-implements it.
	persistent := opts.Persistent()
	if err = b.Start(cmd.Context(), opts); err != nil {
		return err
	}

	ep, release, err := reachStable(cmd.Context(), b, uf.common)
	if err != nil {
		return err
	}
	defer release()

	// 60s: a fresh container must boot the X server + KasmVNC and cold-start Chrome.
	v := waitCDP(cmd.Context(), ep.CDPHost, ep.CDPPort, 60*time.Second)
	if v == nil {
		c := cuttleCmd(ctxName, ctx, name)
		if before == backend.StateRunning {
			return fmt.Errorf("%q is running but CDP is not answering - run `%s status` to triage, then `%s down` and retry", name, c, c) //nolint:err113
		}
		return fmt.Errorf("started but CDP never came up - run `%s status` to triage (it tails the Chrome launch failure reason)", c) //nolint:err113
	}

	recreated := uf.recreate || uf.purgeProfile
	// The profile is fresh only when it was disposable (--ephemeral) or explicitly
	// reset (--purge-profile). A plain --recreate re-attaches the persistent volume.
	freshProfile := uf.purgeProfile || !persistent
	verb, showImage := "ready", true
	switch {
	case recreated && before != backend.StateAbsent:
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
	printBriefingFor(cmd.OutOrStdout(), verb, name, ctxName, ctx, ep, browserOf(v), image, showImage,
		secretNames(cmd.Context(), ep))
	switch {
	case recreated && before != backend.StateAbsent && freshProfile:
		fmt.Fprintln(cmd.OutOrStdout(), "  note: the profile (cookies/logins) was reset - fresh identity")
	case recreated && before != backend.StateAbsent:
		fmt.Fprintln(cmd.OutOrStdout(), "  note: recreated the container; the persistent profile was re-attached (logins kept)")
	}
	return nil
}

func printBriefingFor(w io.Writer, verb, name, ctxName string, ctx config.Context, ep backend.Endpoint, engine, image string, showImage bool, secrets []string) {
	cdpURL, viewer := endpointURLs(ep)
	imageTail := ""
	if showImage && localBackend(ctx) {
		imageTail = ", image " + image
	}
	renderBriefing(w, briefing{
		verb:      verb,
		location:  locationLabel(ctxName, ctx, name),
		imageTail: imageTail,
		cuttle:    cuttleCmd(ctxName, ctx, name),
		version:   cliVersion(),
		cdpURL:    cdpURL,
		viewerURL: viewer,
		engine:    engine,
		secrets:   secrets,
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
			// Plain resolve: stopping needs no ports (the standing tunnel is keyed
			// by context), so down works even when an instance's ports are unreadable.
			name, ctxName, ctx, b, err := resolve(cf, defaultImage())
			if err != nil {
				return err
			}
			state, err := b.State(cmd.Context())
			if err != nil {
				return err
			}
			if state == backend.StateAbsent && !purge {
				// An absent container can still have a leftover forward from a prior
				// session; tear it down.
				if t, ok := b.(backend.Tunneler); ok {
					_ = t.StopTunnel()
				}
				fmt.Fprintf(cmd.OutOrStdout(), "cuttle: nothing to stop (%s)\n", locationLabel(ctxName, ctx, name))
				return nil
			}
			// --purge still runs even when nothing is running: the durable profile
			// (docker volume / k8s PVC + helm release) outlives the running instance,
			// so a `down --purge` after a plain `down` must still tear it down.
			if t, ok := b.(backend.Tunneler); ok {
				_ = t.StopTunnel()
			}
			if err := b.Stop(cmd.Context(), purge); err != nil {
				return err
			}
			if purge {
				fmt.Fprintf(cmd.OutOrStdout(), "cuttle: removed %s (profile discarded)\n", locationLabel(ctxName, ctx, name))
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "cuttle: stopped %s (profile kept; `%s up` to resume)\n", locationLabel(ctxName, ctx, name), cuttleCmd(ctxName, ctx, name))
			}
			return nil
		},
	}
	addCommonFlags(cmd, &cf)
	cmd.Flags().BoolVar(&purge, "purge", false, "also remove the container/release and discard the persistent profile (deletes its volume/PVC)")
	return cmd
}

// ---------------------------------------------------------------------------
// purge-profile
// ---------------------------------------------------------------------------

func newPurgeProfileCmd() *cobra.Command {
	var cf commonFlags
	cmd := &cobra.Command{
		Use:   "purge-profile",
		Short: "reset the persistent profile so the next `up` starts fresh",
		Long: `Remove the persistent profile's backing store - the named Docker volume, or
the PVC on the k8s backend - so the next 'cuttle up' starts from a clean profile
with all cookies and logins discarded.

The container/release is torn down so the volume can be removed; run 'cuttle up'
afterwards for a fresh session. To reset and start again in one step, use
'cuttle up --recreate --purge-profile'. Supported on the docker (local/ssh) and
k8s backends; the direct backend has no profile store cuttle manages.`,
		RunE: func(cmd *cobra.Command, _ []string) error { return runPurgeProfile(cmd, cf) },
	}
	addCommonFlags(cmd, &cf)
	return cmd
}

func runPurgeProfile(cmd *cobra.Command, cf commonFlags) error {
	name, ctxName, ctx, b, err := resolve(cf, defaultImage())
	if err != nil {
		return err
	}
	purger, ok := b.(backend.ProfilePurger)
	if !ok {
		return fmt.Errorf("%s: purge-profile is only supported on the docker (local/ssh) and k8s backends", locationLabel(ctxName, ctx, name)) //nolint:err113
	}
	state, err := b.State(cmd.Context())
	if err != nil {
		return err
	}
	if t, ok := b.(backend.Tunneler); ok {
		_ = t.StopTunnel()
	}
	// Tearing the container/release down with purge=true removes its volume/PVC.
	// If nothing is running, a volume may still linger from a prior session, so
	// drop it directly.
	if state != backend.StateAbsent {
		if err := b.Stop(cmd.Context(), true); err != nil {
			return err
		}
	} else if err := purger.PurgeProfileVolume(cmd.Context()); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "cuttle: purged the profile for %s - run `%s up` for a fresh session\n", locationLabel(ctxName, ctx, name), cuttleCmd(ctxName, ctx, name))
	return nil
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
	name, ctxName, ctx, b, err := resolveRunning(cmd, &cf, defaultImage())
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	c := cuttleCmd(ctxName, ctx, name)
	state, err := b.State(cmd.Context())
	if err != nil {
		return err
	}
	if state == backend.StateAbsent {
		return fmt.Errorf("%s: nothing running - run `%s up`", locationLabel(ctxName, ctx, name), c) //nolint:err113 // user-facing remedy
	}
	if state == backend.StateStopped {
		// Nothing of this instance answers, so probing its ports could only reach
		// someone else's browser.
		return fmt.Errorf("%s: stopped (profile kept) - run `%s up` to resume", locationLabel(ctxName, ctx, name), c) //nolint:err113 // user-facing remedy
	}

	// reachStable health-checks and re-establishes the standing tunnel for a
	// tunneled backend, so the endpoint below is the same stable one `up` printed.
	ep, release, err := reachStable(cmd.Context(), b, cf)
	if err != nil {
		return err
	}
	defer release()

	// Deliberately NOT /json/version anywhere in this command: that endpoint
	// launches a browser on demand, and a read-only status check must not start
	// one as a side effect. The daemon's own root answers without touching Chrome,
	// and its silence is what "not answering" means below.
	daemon := daemonHealth(cmd.Context(), ep.CDPHost, ep.CDPPort, 5*time.Second)
	if daemon != nil {
		engine := ""
		if daemon.Active > 0 {
			// A browser is already up, so asking its version cannot start one.
			engine = browserOf(waitCDP(cmd.Context(), ep.CDPHost, ep.CDPPort, 5*time.Second))
		}
		printBriefingFor(out, "running", name, ctxName, ctx, ep, engine, "", false, secretNames(cmd.Context(), ep))
		if img := localImage(cmd.Context(), b); img != "" {
			fmt.Fprintf(out, "  image   %s\n", img)
		}
		if daemon.Active == 0 {
			fmt.Fprintf(out, "  note: no browser is running right now - `%s open` starts it (the profile is kept)\n", c)
		}
		return nil
	}

	cdpURL, viewer := endpointURLs(ep)
	fmt.Fprintf(out, "%s: %s\n", locationLabel(ctxName, ctx, name), state)
	fmt.Fprintf(out, "  CDP     %s  (not answering)\n", cdpURL)
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
	fmt.Fprintf(out, "  fix: `%s down && %s up` (keeps the profile), or\n", c, c)
	fmt.Fprintf(out, "    `%s up --recreate` to rebuild from scratch (discards the profile).\n", c)
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
	var o openFlags
	cmd := &cobra.Command{
		Use:   "open [url]",
		Short: "navigate the running session to a URL and open the viewer (returns immediately, or --wait)",
		Long: `Point the running session at a URL, print the briefing and open the viewer, so
a human can sign in or clear a challenge in the browser the agent is driving.

By default it returns immediately. --wait (or --until) holds the terminal until
the page reaches a condition, then prints where it ended up - so the agent gets
a real return signal instead of asking the user "are you done yet?" three
times. It only ever LOOKS at the page: waiting never clicks anything.

  cuttle open https://example.com/login --wait     # until the URL leaves that origin
  cuttle open --until 'title:Dashboard'
  cuttle open --until 'url:https://app.example.com/home*'
  cuttle open --until 'js:!!document.querySelector("[data-signed-in]")'`,
		// login/connect are the pre-overhaul verbs; kept as aliases for one
		// release. They do not show in help, which is the intended "hidden".
		Aliases: []string{"login", "connect"},
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := ""
			if len(args) == 1 {
				target = args[0]
			}
			return runOpen(cmd, cf, target, o)
		},
	}
	addCommonFlags(cmd, &cf)
	cmd.Flags().BoolVar(&o.noOpen, "no-open", false, "print the viewer URL, don't open it in a browser")
	cmd.Flags().BoolVar(&o.wait, "wait", false, "hold the terminal until the page leaves the URL's origin")
	cmd.Flags().StringVar(&o.until, "until", "", "hold until a condition holds: url:<glob>, gone:<glob>, title:<substring> or js:<expression>")
	cmd.Flags().DurationVar(&o.timeout, "timeout", defaultWaitFor, "how long --wait/--until may block")
	return cmd
}

// openFlags groups open's own flags; cf carries the shared ones.
type openFlags struct {
	noOpen  bool
	wait    bool
	until   string
	timeout time.Duration
}

// runOpen navigates the already-running session's browser to target (when given),
// prints the briefing, and opens the viewer - then returns. It does not hold the
// terminal or check out any profile state: the session lives in the daemon, and
// its login persists in the profile volume on its own.
func runOpen(cmd *cobra.Command, cf commonFlags, target string, o openFlags) error {
	// Parsed FIRST: everything below navigates the session, raises a window and
	// opens a viewer on someone's desktop, and a typo in --until should not do
	// all three before it errors.
	var wait *predicate
	if o.wait || o.until != "" {
		p, perr := parsePredicate(o.until, target)
		if perr != nil {
			return perr
		}
		wait = &p
	}
	name, ctxName, ctx, b, err := resolveLive(cmd, &cf)
	if err != nil {
		return err
	}
	ep, release, err := reachStable(cmd.Context(), b, cf)
	if err != nil {
		return err
	}
	defer release()

	v := waitCDP(cmd.Context(), ep.CDPHost, ep.CDPPort, 30*time.Second)
	if v == nil {
		return errCDPNotAnswering(ctxName, ctx, name)
	}

	out := cmd.OutOrStdout()
	if target != "" {
		title, nerr := navigate(cmd.Context(), ep.CDPHost, ep.CDPPort, target, ep.VNCPort)
		if nerr != nil {
			return fmt.Errorf("navigation failed: %w", nerr)
		}
		line := "navigated to " + mask.Params(target)
		if title != "" {
			line += "  (" + title + ")"
		}
		fmt.Fprintln(out, line)
	}

	printBriefingFor(out, "open", name, ctxName, ctx, ep, browserOf(v), "", false, secretNames(cmd.Context(), ep))

	base, viewer := endpointURLs(ep)
	if viewer != "" {
		// Raise the session's window before handing over the link: with more than
		// one browser on the shared display the viewer shows whichever window the
		// window manager had on top, which is not necessarily the one just
		// navigated. Best effort - a headless daemon has no window and the link
		// still works.
		_ = requestJSON(cmd.Context(), http.MethodPost, base+"/window/raise", nil, nil)
		if !o.noOpen {
			openBrowser(viewer)
		}
	}
	if wait == nil {
		return nil
	}
	fmt.Fprintf(out, "waiting for %s (up to %s) - hand the viewer link to the user\n", wait, o.timeout)
	return waitUntil(cmd.Context(), out, ep.CDPHost, ep.CDPPort, ep.VNCPort, *wait, o.timeout)
}

// ---------------------------------------------------------------------------
// downloads
// ---------------------------------------------------------------------------

// downloadFlags groups downloads' own flags; cf carries the shared ones.
type downloadFlags struct {
	latest bool
	force  bool
	wait   time.Duration
}

func newDownloadsCmd() *cobra.Command {
	var cf commonFlags
	var o downloadFlags
	cmd := &cobra.Command{
		Use:   "downloads [name [dest]]",
		Short: "list the session's downloaded files, or pull one to a local path",
		Long: `Files downloaded in the browser land inside the container; this verb lists
them and pulls one out over the CDP endpoint (so it works on every backend).
With no arguments it lists the session's completed downloads; with a name it
saves that file locally (default: ./<name>) and prints only the local path -
the content is never written to stdout, so pulled secrets stay out of
terminal and agent transcripts.

--latest pulls the newest one, so a click-then-pull needs no name; --wait waits
for a download to finish first, which is one command instead of a sleep whose
length you would have to guess:

  cuttle downloads --latest --wait 30s ./creds.json`,
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error { return runDownloads(cmd, cf, args, o) },
	}
	addCommonFlags(cmd, &cf)
	cmd.Flags().BoolVar(&o.latest, "latest", false, "act on the newest download instead of naming it")
	cmd.Flags().BoolVar(&o.force, "force", false, "let the destination replace an existing file")
	cmd.Flags().DurationVar(&o.wait, "wait", 0, "wait up to this long for a download to finish first")
	return cmd
}

func runDownloads(cmd *cobra.Command, cf commonFlags, args []string, o downloadFlags) error {
	base, release, err := sessionEndpoint(cmd, &cf)
	if err != nil {
		return err
	}
	defer release()
	// A download the browser is still writing is not listed (the daemon hides
	// .crdownload partials), so "a new name appeared" IS "a download finished".
	// That is what --wait waits for, and it is what makes click-then-pull one
	// command instead of a sleep an agent has to guess the length of.
	before := map[string]bool{}
	if o.wait > 0 {
		known, err := listing(cmd.Context(), base)
		if err != nil {
			return err
		}
		for _, d := range known {
			before[d.Name] = true
		}
		if err := waitForDownload(cmd.Context(), base, before, o.wait); err != nil {
			return err
		}
	}
	if len(args) == 0 && !o.latest {
		return listDownloads(cmd.Context(), cmd.OutOrStdout(), base)
	}

	name, dest := "", ""
	if len(args) > 0 {
		name, dest = args[0], args[0]
	}
	if len(args) == 2 {
		dest = args[1]
	}
	if o.latest {
		newest, err := newestDownload(cmd.Context(), base, before)
		if err != nil {
			return err
		}
		if dest == "" {
			dest = newest
		}
		name = newest
	}
	return pullDownload(cmd.Context(), cmd.OutOrStdout(), base, name, dest, o.force)
}

// downloadPollGap is how often --wait re-lists. Downloads are a human-scale
// event; a tighter poll would only add HTTP round trips.
const downloadPollGap = 500 * time.Millisecond

var (
	errNoDownloads    = errors.New("no downloads in this session yet")
	errDownloadWait   = errors.New("no new download finished in time")
	errDownloadsEmpty = errors.New("nothing to pull")
)

type downloadEntry struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Modified string `json:"modified"`
}

// listing returns the session's completed downloads, newest first.
func listing(ctx context.Context, base string) ([]downloadEntry, error) {
	var payload struct {
		Downloads []downloadEntry `json:"downloads"`
	}
	if err := getJSON(ctx, base+"/downloads", &payload); err != nil {
		return nil, fmt.Errorf("listing downloads: %w", err)
	}
	return payload.Downloads, nil
}

// waitForDownload blocks until a name appears that was not there before.
func waitForDownload(ctx context.Context, base string, before map[string]bool, wait time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for {
		files, err := listing(ctx, base)
		if err == nil {
			for _, d := range files {
				if !before[d.Name] {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (waited %s) - check the browser: a download can be waiting on a save dialog", errDownloadWait, wait)
		case <-time.After(downloadPollGap):
		}
	}
}

// newestDownload names the most recent completed download, preferring one that
// appeared during this command's own --wait window.
func newestDownload(ctx context.Context, base string, before map[string]bool) (string, error) {
	files, err := listing(ctx, base)
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", errNoDownloads
	}
	for _, d := range files {
		if !before[d.Name] {
			return d.Name, nil
		}
	}
	return files[0].Name, nil
}

func listDownloads(ctx context.Context, out io.Writer, base string) error {
	files, err := listing(ctx, base)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		fmt.Fprintln(out, "no downloads")
		return nil
	}
	for _, d := range files {
		fmt.Fprintf(out, "%s\t%d\t%s\n", d.Name, d.Size, d.Modified)
	}
	return nil
}

// pullDownload streams one downloaded file from the daemon to dest (0600) and
// prints only the local path: the file's content - possibly a credential the
// user just exported in the browser - must never reach stdout.
func pullDownload(ctx context.Context, out io.Writer, base, name, dest string, force bool) error {
	if name == "" || dest == "" {
		return errDownloadsEmpty
	}
	// Under --latest the name is the BROWSER's, so without this a page can pick
	// which file in the working directory gets replaced.
	if err := checkDest(dest, force); err != nil {
		return err
	}
	endpoint := base + "/downloads/" + url.PathEscape(name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err //nolint:wrapcheck
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err //nolint:wrapcheck
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return fmt.Errorf("pull %q: %w", name, daemonError(resp.StatusCode, body))
	}
	// Streamed to a temp file beside the destination and renamed on success: the
	// download can be large, and a failed pull must not truncate whatever was at
	// dest, nor leave a partial credential file behind at 0600 or otherwise.
	tmp, err := os.CreateTemp(filepath.Dir(dest), "."+filepath.Base(dest)+".*.part")
	if err != nil {
		return fmt.Errorf("writing %s: %w", dest, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if cherr := tmp.Chmod(0o600); cherr != nil {
		_ = tmp.Close()
		return fmt.Errorf("securing %s: %w", dest, cherr)
	}
	n, cerr := io.Copy(tmp, resp.Body)
	if err := tmp.Close(); cerr == nil {
		cerr = err
	}
	if cerr == nil {
		cerr = os.Rename(tmpName, dest)
	}
	if cerr != nil {
		return fmt.Errorf("writing %s: %w", dest, cerr)
	}
	fmt.Fprintf(out, "saved %s (%d bytes)\n", dest, n)
	return nil
}

// ---------------------------------------------------------------------------
// logs
// ---------------------------------------------------------------------------

var errNoLogs = errors.New("logs are not available for the direct backend - read them where the browser runs")

func newLogsCmd() *cobra.Command {
	var cf commonFlags
	var follow bool
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "show the browser container's logs (docker logs / kubectl logs passthrough)",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return runLogs(cmd, cf, follow) },
	}
	addCommonFlags(cmd, &cf)
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "stream new log lines until interrupted")
	return cmd
}

// runLogs execs the backend's own log command with the terminal attached, so
// --follow streams and Ctrl-C behave exactly like docker/kubectl logs.
func runLogs(cmd *cobra.Command, cf commonFlags, follow bool) error {
	_, _, _, b, err := resolve(cf, defaultImage())
	if err != nil {
		return err
	}
	src, ok := b.(backend.LogSource)
	if !ok {
		return errNoLogs
	}
	exe, args := src.LogsCommand(follow)
	c := exec.CommandContext(cmd.Context(), exe, args...)
	c.Stdout = cmd.OutOrStdout()
	c.Stderr = cmd.ErrOrStderr()
	if err := c.Run(); err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				return nil // Ctrl-C on --follow is the normal way to leave
			}
			return fmt.Errorf("%s exited with %d", exe, ee.ExitCode()) //nolint:err113 // stderr already passed through
		}
		return err //nolint:wrapcheck
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
(flags-first, no hand-editing needed). To run the browser on a remote host (a
bigger box, a shared runner, or a fixed egress IP), add an ssh or k8s context and
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
	ls := &cobra.Command{
		Use:   "ls",
		Short: "list contexts and show the active one",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			active, _, err := cfg.Active(instance.contextName, os.Getenv(config.EnvContext))
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
	errAddWithName     = errors.New(`--name selects an instance, it is not saved - pin the context's container with name = "..." in its stanza`)
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
			if instance.name != "" {
				return errAddWithName
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
