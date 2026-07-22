package cli

import (
	"bytes"
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
	"runtime"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/glim-sh/cuttle/internal/backend"
	"github.com/glim-sh/cuttle/internal/cdp"
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
	defaultName    = backend.DefaultContainerName
	defaultCDPPort = 9222
	defaultVNCPort = 6080
	imageRepo      = "ghcr.io/glim-sh/cuttle"
	// localImageTag is the tag `just build-image` produces; a dev build defaults
	// to it (see defaultImage) instead of a published tag it has no match for.
	localImageTag = "cuttle:local"
)

var errCDPNotAnswering = errors.New("CDP not answering - run `cuttle up` first")

func init() {
	AddCommand(newUpCmd(), newDownCmd(), newStatusCmd(), newOpenCmd(), newPurgeProfileCmd(), newContextCmd())
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
	contextName string
	name        string // docker container name override (--name); "" = default "cuttle"
	cdpPort     int
	vncPort     int
	profile     string // seed; set only on verbs that take --profile (open)
}

func addCommonFlags(cmd *cobra.Command, cf *commonFlags) {
	f := cmd.Flags()
	f.StringVar(&cf.contextName, "context", "", "context to use (default: config default_context, else local)")
	f.StringVar(&cf.name, "name", "", "container name for the docker (local/ssh) backends; run multiple isolated instances on one host by giving each its own --name and ports (default: cuttle)")
	f.IntVar(&cf.cdpPort, "cdp-port", defaultCDPPort, "host CDP port")
	f.IntVar(&cf.vncPort, "vnc-port", defaultVNCPort, "host VNC viewer port (docker/local backend only)")
}

// addProfileFlag wires --profile on the verbs that drive a session with a named
// profile, routing to the profile's seed. Its local auth state is synced at the
// session lifecycle edges (up/status), not checked out per open.
func addProfileFlag(cmd *cobra.Command, p *string) {
	cmd.Flags().StringVar(p, "profile", "", "profile name (= seed); routes this session to the profile's seed")
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
// persistent storage. A remote profile lives durably on the browser host, so the
// local-canonical restore skips it. An unconfigured profile is local.
func profileRemote(cfg *config.Config, name string) bool {
	p, ok := cfg.Profiles[name]
	return ok && p.Storage == config.StorageRemote
}

// injectLocalCanonicalState restores each local profile's saved login into a box
// that lacks it: for every profile with saved state, it seeds the daemon only
// when the daemon has none (GET 404), so a fresh or --recreated box gets the
// login back while a normal restart keeps the daemon's own (newer) snapshot
// untouched. Best-effort throughout - a failed profile is skipped, never fatal.
func injectLocalCanonicalState(ctx context.Context, w io.Writer, ep backend.Endpoint) {
	names := profile.ListLocal()
	if len(names) == 0 {
		return
	}
	cfg, err := config.Load()
	if err != nil {
		return
	}
	base := "http://" + net.JoinHostPort(ep.CDPHost, strconv.Itoa(ep.CDPPort))
	var restored []string
	for _, name := range names {
		if profileRemote(cfg, name) {
			continue // durable on the host; nothing to restore
		}
		st, lerr := profile.LoadLocal(name)
		if lerr != nil || !profile.HasState(st) {
			continue
		}
		endpoint := base + "/profile/" + url.PathEscape(name) + "/state"
		// Restore ONLY on a definitive 404. A 200 means the daemon holds this
		// seed's state (authoritative); any other outcome (timeout, 5xx,
		// unreachable) is ambiguous, and clobbering a possibly-newer remote
		// snapshot with the local one is worse than skipping the restore.
		if !daemonStateAbsent(ctx, endpoint) {
			continue
		}
		if putJSON(ctx, endpoint, st) != nil {
			continue
		}
		restored = append(restored, name)
	}
	if len(restored) > 0 {
		fmt.Fprintf(w, "cuttle: restored local-canonical login for: %v\n", restored)
	}
}

// resolve loads the config, selects the active context, and builds its backend.
// The docker-container backends (local, ssh) name the container "cuttle" by
// default, or the --name override for running several isolated instances on one
// host; k8s/direct are identified by their context name instead and ignore --name.
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
		if cf.name != "" {
			name = cf.name
		}
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
	cdpURL := "http://" + net.JoinHostPort(ep.CDPHost, strconv.Itoa(ep.CDPPort))
	viewer := ""
	if ep.VNCPort != 0 {
		viewer = "http://" + net.JoinHostPort(ep.VNCHost, strconv.Itoa(ep.VNCPort)) + "/"
	}
	return cdpURL, viewer
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
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
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
		return fmt.Errorf("%s: HTTP %d", endpoint, resp.StatusCode) //nolint:err113
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err //nolint:wrapcheck
	}
	return json.Unmarshal(body, out) //nolint:wrapcheck
}

// daemonStateAbsent reports whether the daemon DEFINITIVELY has no state for the
// seed - an explicit 404. A 200 (has state) or any error (timeout, 5xx,
// unreachable) returns false, so the restore only writes when it is certain the
// daemon is stateless and a transient blip can never clobber a newer snapshot.
func daemonStateAbsent(ctx context.Context, endpoint string) bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == http.StatusNotFound
}

// putJSON PUTs a JSON body to the daemon's state API (the write side, used by the
// local-canonical restore). It is the mirror of getJSON.
func putJSON(ctx context.Context, endpoint string, body any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	data, err := json.Marshal(body)
	if err != nil {
		return err //nolint:wrapcheck
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(data))
	if err != nil {
		return err //nolint:wrapcheck
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err //nolint:wrapcheck
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", endpoint, resp.StatusCode) //nolint:err113
	}
	return nil
}

// runningSeeds returns the daemon's live seed keys (the process-table keys from
// the multiplexer's health endpoint).
func runningSeeds(ctx context.Context, base string) []string {
	var status struct {
		Processes map[string]json.RawMessage `json:"processes"`
	}
	if err := getJSON(ctx, base+"/", &status); err != nil {
		return nil
	}
	seeds := make([]string, 0, len(status.Processes))
	for seed := range status.Processes {
		seeds = append(seeds, seed)
	}
	return seeds
}

// pullLocalCanonicalState copies every running, named seed's current auth state
// from the daemon into the local profile store. It is the local-canonical safety
// net: a `down` (or a flip away from durable remote profiles) never strands a
// login that was created against a seed, because the state is captured locally
// before the container stops. The reserved default seed is skipped - it has no
// name to key a local profile by, so its state stays container-side only.
// Best-effort throughout: a failed seed is logged and does not abort the stop.
func pullLocalCanonicalState(ctx context.Context, w io.Writer, ep backend.Endpoint) {
	base := "http://" + net.JoinHostPort(ep.CDPHost, strconv.Itoa(ep.CDPPort))
	var saved []string
	for _, seed := range runningSeeds(ctx, base) {
		if !profile.ValidName(seed) {
			continue // reserved/invalid keys have no local profile
		}
		var st cdp.StorageState
		if err := getJSON(ctx, base+"/profile/"+url.PathEscape(seed)+"/state", &st); err != nil {
			continue
		}
		if err := profile.SaveState(seed, &st); err != nil {
			continue
		}
		saved = append(saved, seed)
	}
	if len(saved) > 0 {
		fmt.Fprintf(w, "cuttle: saved local-canonical auth state for: %v\n", saved)
	}
}

// ---------------------------------------------------------------------------
// up
// ---------------------------------------------------------------------------

type upFlags struct {
	common       commonFlags
	image        string
	keepProfile  boolFlag
	ephemeral    bool
	purgeProfile bool
	recreate     bool
	idleTimeout  string
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
	cmd.Flags().Lookup("keep-profile").NoOptDefVal = "true"
	_ = cmd.Flags().MarkHidden("keep-profile")
	cmd.Flags().BoolVar(&uf.ephemeral, "ephemeral", false, "use a disposable profile: no persistent volume, discarded on recreate/down --purge (opt out of the default persistent profile)")
	cmd.Flags().BoolVar(&uf.purgeProfile, "purge-profile", false, "remove the persistent profile (volume on local/ssh, PVC on k8s) before starting, so it comes up with a fresh profile (implies --recreate)")
	cmd.Flags().BoolVar(&uf.recreate, "recreate", false, "destroy any existing container and start fresh (the persistent profile survives; add --purge-profile to also reset it)")
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
		// The persistence choice (volume + keep-profile env) is baked at container
		// creation, so flipping --ephemeral/--keep-profile on an existing container
		// only takes effect on a --recreate (--purge-profile also recreates).
		if (uf.ephemeral || uf.keepProfile.set) && !uf.recreate && !uf.purgeProfile {
			fmt.Fprintf(os.Stderr, "cuttle: profile persistence is fixed when the container is created; %q keeps its original setting (use --recreate to change it)\n", name)
		}
		// On docker-backed backends --idle-timeout is baked into the container env
		// at creation, so a restart via `docker start` ignores a new value. (k8s
		// re-applies it on every `helm upgrade`, so it is not fixed there.)
		dockerBaked := localBackend(ctx) || ctx.Backend == config.BackendSSH
		if dockerBaked && cmd.Flags().Changed("idle-timeout") {
			fmt.Fprintf(os.Stderr, "cuttle: --idle-timeout is fixed when the container is created; %q keeps its original setting (use --recreate to change it)\n", name)
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

	// 60s: a fresh container must boot the X server + KasmVNC and cold-start Chrome,
	// which is slow under CPU emulation (an amd64 image on an arm64 host).
	v := waitCDP(cmd.Context(), ep.CDPHost, ep.CDPPort, 60*time.Second)
	if v == nil {
		if before == backend.StateRunning {
			return fmt.Errorf("%q is running but CDP is not answering - run `cuttle status` to triage, then `cuttle down` and retry", name) //nolint:err113
		}
		return errors.New("started but CDP never came up - run `cuttle status` to triage (it tails the Chrome launch failure reason)") //nolint:err113
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
	printBriefingFor(cmd.OutOrStdout(), verb, name, ctxName, ctx, uf.common.profile, ep, browserOf(v), image, showImage)
	switch {
	case recreated && before != backend.StateAbsent && freshProfile:
		fmt.Fprintln(cmd.OutOrStdout(), "  note: the profile (cookies/logins) was reset - fresh identity")
	case recreated && before != backend.StateAbsent:
		fmt.Fprintln(cmd.OutOrStdout(), "  note: recreated the container; the persistent profile was re-attached (logins kept)")
	}

	// Local-canonical sync at the lifecycle edge: restore each saved login into a
	// box that lacks it (fresh / --recreated), then refresh the local mirror from
	// whatever the daemon already holds. Both best-effort - a fresh box has
	// nothing to pull, a normal restart has nothing to restore.
	injectLocalCanonicalState(cmd.Context(), cmd.OutOrStdout(), ep)
	pullLocalCanonicalState(cmd.Context(), cmd.OutOrStdout(), ep)
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

func printBriefingFor(w io.Writer, verb, name, ctxName string, ctx config.Context, profileName string, ep backend.Endpoint, engine, image string, showImage bool) {
	cdpURL, viewer := endpointURLs(ep)
	cdpURL = withFingerprint(cdpURL, profileName)
	imageTail := ""
	if showImage && localBackend(ctx) {
		imageTail = ", image " + image
	}
	renderBriefing(w, briefing{
		verb:      verb,
		location:  locationLabel(ctxName, ctx, name),
		imageTail: imageTail,
		version:   cliVersion(),
		cdpURL:    cdpURL,
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
			// Local-canonical pull: while the browser is still up, copy every running
			// named seed's auth state into the local store so stopping (or a later
			// --recreate / box loss) never strands a login. Skipped on --purge, which
			// is an explicit discard.
			if state == backend.StateRunning && !purge {
				if ep, release, rerr := reachStable(cmd.Context(), b, cf); rerr == nil {
					pullLocalCanonicalState(cmd.Context(), cmd.OutOrStdout(), ep)
					release()
				}
			}
			if t, ok := b.(backend.Tunneler); ok {
				_ = t.StopTunnel()
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
	fmt.Fprintf(cmd.OutOrStdout(), "cuttle: purged the profile for %s - run `cuttle up` for a fresh session\n", locationLabel(ctxName, ctx, name))
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
		printBriefingFor(out, "running", name, ctxName, ctx, cf.profile, ep, browserOf(v), "", false)
		if img := localImage(cmd.Context(), b); img != "" {
			fmt.Fprintf(out, "  image   %s\n", img)
		}
		// Opportunistic refresh of the local mirror while we are talking to a
		// healthy daemon anyway - keeps saved logins fresh without a background
		// process. Best-effort and silent when nothing changed.
		pullLocalCanonicalState(cmd.Context(), out, ep)
		return nil
	}

	cdpURL, viewer := endpointURLs(ep)
	fmt.Fprintf(out, "%s: %s\n", locationLabel(ctxName, ctx, name), state)
	if v == nil {
		fmt.Fprintf(out, "  CDP     %s  (not answering)\n", cdpURL)
	} else {
		fmt.Fprintf(out, "  CDP     %s\n", cdpURL)
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
		Short: "navigate the running session to a URL and open the viewer (returns immediately)",
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

// runOpen navigates the already-running session's browser to target (when given),
// prints the briefing, and opens the viewer - then returns. It does not hold the
// terminal, inject, or check out any profile state: the session lives in the
// daemon and its login persists on its own (up restores it, down/status pull it
// back locally). --profile only selects which seed to drive.
func runOpen(cmd *cobra.Command, cf commonFlags, target string, noOpen bool) error {
	name, ctxName, ctx, b, err := resolve(cf, defaultImage())
	if err != nil {
		return err
	}
	if cf.profile != "" && !profile.ValidName(cf.profile) {
		return fmt.Errorf("%w: %q", errInvalidProfile, cf.profile)
	}
	ep, release, err := reachStable(cmd.Context(), b, cf)
	if err != nil {
		return err
	}
	defer release()

	v := waitCDP(cmd.Context(), ep.CDPHost, ep.CDPPort, 30*time.Second)
	if v == nil {
		return errCDPNotAnswering
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

	printBriefingFor(out, "open", name, ctxName, ctx, cf.profile, ep, browserOf(v), "", false)

	if _, viewer := endpointURLs(ep); viewer != "" && !noOpen {
		openBrowser(viewer)
	}
	return nil
}

var errInvalidProfile = errors.New("invalid profile name")

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
