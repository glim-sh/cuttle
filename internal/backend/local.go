package backend

import (
	"context"
	"fmt"
	"strings"
)

const (
	containerCDPPort = "9222"
	containerVNCPort = "6080"
	// containerDataDir is the in-container profile dir (cuttle serve's default
	// data-dir in a container), shared with the k8s chart's dataDir so both docker
	// and k8s mount durable storage at the same path. The persistent named volume
	// mounts here so the default seed's Chrome profile outlives the container. It
	// must NOT be /tmp: the entrypoint touches /tmp/.X99-lock, which a volume there
	// would shadow.
	containerDataDir = "/data"
	shmSize          = "--shm-size=2g"
	stopGrace        = "15" // > cuttle serve's 5s Chrome drain, so the clean exit completes
	dockerRunSub     = "run"
	dockerNameFlag   = "--name"
)

// profileVolumeName is the stable, per-container Docker volume that backs the
// persistent profile. Deriving it from the container name keeps distinct
// contexts/containers from colliding, and reuses the SAME volume across
// up/recreate so the profile re-attaches. safeToken keeps it a valid volume name.
func profileVolumeName(containerName string) string {
	return "cuttle-" + safeToken(containerName) + "-profile"
}

// Local runs the browser in a docker container on this host. It is a faithful
// port of the Python cuttle CLI's docker lifecycle, so existing behavior does
// not regress when no config file is present.
type Local struct {
	runner  Runner
	name    string
	cdpPort int
	vncPort int
	image   string // resolved default image, used when StartOpts.Image is empty
}

func (l *Local) check() error {
	return requireExe(l.runner, dockerExe, "install Docker (or OrbStack) first.")
}

// dockerStatus returns the container's raw docker state ("running", "exited",
// "created", ...) or "" if the container does not exist.
func (l *Local) dockerStatus(ctx context.Context) (string, error) {
	return l.container().status(ctx)
}

// container wraps the docker argv to run it directly on this machine.
func (l *Local) container() containerHost {
	return containerHost{
		runner: l.runner,
		name:   l.name,
		label:  "docker",
		wrap:   func(args ...string) (string, []string) { return dockerExe, args },
	}
}

func (l *Local) State(ctx context.Context) (State, error) {
	if err := l.check(); err != nil {
		return "", err
	}
	status, err := l.dockerStatus(ctx)
	if err != nil {
		return "", err
	}
	// dockerStatus already folds a non-zero exit into an empty status.
	return dockerStatusState(status, 0), nil
}

// Start ensures the container is up, idempotently. A stopped container is
// restarted (profile preserved); a zombie (a run that died before a clean exit)
// is removed and re-run; --recreate forces a fresh container.
func (l *Local) Start(ctx context.Context, opts StartOpts) error {
	if err := l.check(); err != nil {
		return err
	}
	image := opts.Image
	if image == "" {
		image = l.image
	}
	return l.container().start(ctx, l.cdpPort, l.vncPort, opts, image, l.portConflict)
}

// portConflict turns a fresh-run host-port bind clash into an operator-facing
// remedy: the ports are this machine's, so --cdp-port/--vnc-port can dodge them.
func (l *Local) portConflict(err error) error {
	return fmt.Errorf("host port %d (CDP) or %d (VNC) is already in use - stop whatever is bound there (another cuttle? `docker ps`), or pass --cdp-port/--vnc-port to pick free ports\n%w",
		l.cdpPort, l.vncPort, err)
}

// containerHost drives the idempotent container state machine shared by the local
// and ssh backends. wrap maps a docker argv to the executable+argv that actually
// runs it - directly (`docker ...`) or over ssh (`ssh ... docker ...`) - and label
// prefixes error messages ("docker" / "remote docker").
type containerHost struct {
	runner Runner
	name   string
	label  string
	wrap   func(dockerArgs ...string) (string, []string)
}

// status returns the container's raw docker state ("running", "exited",
// "created", ...) or "" if the container does not exist.
func (h containerHost) status(ctx context.Context) (string, error) {
	name, full := h.wrap("inspect", "-f", "{{.State.Status}}", h.name)
	res, err := h.runner.Output(ctx, name, full...)
	if err != nil {
		return "", err
	}
	if res.Code != 0 {
		return "", nil
	}
	return strings.TrimSpace(res.Stdout), nil
}

func (h containerHost) rm(ctx context.Context) {
	name, full := h.wrap("rm", "-f", h.name)
	_, _ = h.runner.Output(ctx, name, full...)
}

// stop gracefully stops a running container (`docker stop -t`), giving cuttle
// serve time to drain Chrome so the persistent profile (cookies, localStorage,
// IndexedDB) is flushed to the volume before the container is torn down. Used
// before a --recreate/--purge-profile teardown; a plain `docker rm -f` would
// SIGKILL Chrome and lose unflushed state. Best-effort.
func (h containerHost) stop(ctx context.Context) {
	name, full := h.wrap("stop", "-t", stopGrace, h.name)
	_, _ = h.runner.Output(ctx, name, full...)
}

// teardown gracefully stops a running container, then removes it. status is the
// container's current docker state so a non-running container skips the drain.
func (h containerHost) teardown(ctx context.Context, status string) {
	if status == string(StateRunning) {
		h.stop(ctx)
	}
	h.rm(ctx)
}

// volumeRm removes the persistent profile volume, best-effort. `docker volume rm`
// errors if the volume is still attached to a container, so callers remove the
// container first. A missing volume is not an error worth surfacing (the goal -
// no volume - already holds), so this ignores the exit code.
func (h containerHost) volumeRm(ctx context.Context) {
	name, full := h.wrap("volume", "rm", "-f", profileVolumeName(h.name))
	_, _ = h.runner.Output(ctx, name, full...)
}

// docker runs a docker subcommand (its argv leads with the subcommand) and
// checks the exit code, labelling any failure with the host prefix.
func (h containerHost) docker(ctx context.Context, args ...string) error {
	name, full := h.wrap(args...)
	return runOK(ctx, h.runner, h.label+" "+args[0], name, full...)
}

// start is the state machine: running -> no-op; exited -> `docker start` (profile
// kept); a zombie ("created"/"dead" - no live host-port binding to restart into)
// -> rm + fresh run; --recreate -> rm + fresh run. A run that fails at network
// setup leaves a half-created container behind, so it is removed; a host-port
// clash is turned into the caller's remedy hint.
func (h containerHost) start(ctx context.Context, cdpPort, vncPort int, opts StartOpts, image string, conflict func(error) error) error {
	status, err := h.status(ctx)
	if err != nil {
		return err
	}
	// --recreate (and --purge-profile, which implies it) tears the container down
	// and runs a fresh one. Stop it gracefully first so Chrome flushes the
	// persistent profile to the volume, then remove it. --purge-profile then drops
	// the volume so the fresh container starts on a clean profile.
	if (opts.Recreate || opts.PurgeProfile) && status != "" {
		h.teardown(ctx, status)
		status = ""
	}
	// Drop the profile volume when --purge-profile resets it, or when a --recreate
	// switches the container to a disposable (--ephemeral) profile: the old named
	// volume would otherwise linger unreferenced. A plain --recreate (still
	// persistent) keeps the volume so the profile re-attaches.
	if opts.PurgeProfile || (opts.Recreate && !opts.Persistent()) {
		h.volumeRm(ctx)
	}
	if status != "" && status != string(StateRunning) && status != "exited" {
		h.rm(ctx)
		status = ""
	}
	switch {
	case status == string(StateRunning):
		return nil
	case status != "": // exited -> restart, keeping the profile
		return h.docker(ctx, "start", h.name)
	}
	if err := h.docker(ctx, dockerRunArgs(h.name, cdpPort, vncPort, opts, image)...); err != nil {
		h.rm(ctx)
		if isPortConflict(err) {
			return conflict(err)
		}
		return err
	}
	return nil
}

// isPortConflict reports whether a `docker run` failure was a host-port bind
// clash, so the caller can surface a targeted remedy instead of a raw docker
// error.
func isPortConflict(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "port is already allocated") ||
		strings.Contains(msg, "address already in use") ||
		strings.Contains(msg, "Bind for")
}

// dockerRunArgs builds the `docker run ...` argv (without the leading "docker")
// shared by the local and ssh backends. The container always binds host
// 127.0.0.1: locally that is this machine, over ssh it is the remote host that
// ssh -L then tunnels to. Per-invocation daemon settings are passed as CUTTLE_*
// envs, not trailing `cuttle serve` flags: env is the one channel serve reads, so
// this argv is decoupled from the daemon's flag surface.
func dockerRunArgs(name string, cdpPort, vncPort int, opts StartOpts, image string) []string {
	args := []string{
		dockerRunSub, "-d",
		// The image ships linux/amd64 only (clark/clearcote are linux-x64
		// prebuilts). Pin the platform so an arm64 host pulls and runs it under
		// emulation instead of failing with "no matching manifest for arm64".
		"--platform", "linux/amd64",
		// --init runs tini as PID 1 so Chrome's orphaned helper processes (zygote,
		// crashpad, GPU) are reaped instead of piling up as <defunct> zombies.
		"--init",
		dockerNameFlag, name,
		"-p", loopbackHost + ":" + portStr(cdpPort) + ":" + containerCDPPort,
		shmSize,
		"-p", loopbackHost + ":" + portStr(vncPort) + ":" + containerVNCPort,
		"-e", "CUTTLE_VNC=1",
	}
	if opts.Proxy != "" {
		args = append(args, "-e", "CUTTLE_PROXY="+opts.Proxy)
	}
	if opts.IdleTimeout != "" {
		args = append(args, "-e", "CUTTLE_IDLE_TIMEOUT="+opts.IdleTimeout)
	}
	// Humanization is on by the daemon default, so only an explicit opt-out needs an
	// env; the common (enabled) case passes nothing.
	if opts.Humanize != nil && !*opts.Humanize {
		args = append(args, "-e", "CUTTLE_HUMANIZE=0")
	}
	// The default (unnamed) profile is durable by default: a named Docker volume
	// mounted at the container's data dir outlives the container, so the full
	// Chrome profile (cookies + localStorage + IndexedDB + service workers)
	// survives `cuttle up` restarts, `cuttle up --recreate`, and image upgrades.
	// CUTTLE_KEEP_PROFILE=1 tells the daemon the profile dir is the durable source
	// of truth (a stable per-seed path, preserved on terminate) rather than a
	// scratch dir. --ephemeral (or the legacy --keep-profile=false) opts out for a
	// disposable session. The volume + env are baked at container creation, so an
	// existing container keeps whatever it was created with; changing this needs a
	// --recreate.
	if opts.Persistent() {
		args = append(
			args,
			"-v", profileVolumeName(name)+":"+containerDataDir,
			"-e", "CUTTLE_KEEP_PROFILE=1",
		)
	}
	// cuttle serve defaults to port 9222 and auto-binds 0.0.0.0 in a container, so
	// pass neither. This command overrides the image CMD, so the CMD's
	// --headless=false is not applied - harmless, because headed Chrome comes from
	// the container's X server (the entrypoint starts Xvfb), not a serve flag. The
	// command is a plain, shell-metacharacter-free argv so the ssh backend (which
	// re-parses the remote command through the login shell) forwards it intact;
	// the daemon clears any stale Chrome SingletonLock itself on launch.
	args = append(args, image, "cuttle", "serve")
	return args
}

func (l *Local) Stop(ctx context.Context, purge bool) error {
	if err := l.check(); err != nil {
		return err
	}
	status, err := l.dockerStatus(ctx)
	if err != nil {
		return err
	}
	// A plain stop on an absent container has nothing to do. On --purge we still
	// fall through: the profile volume can outlive the container (e.g. a `down
	// --purge` after a plain `down` removed the container earlier), so it must be
	// removed even when the container is already gone.
	if status == "" && !purge {
		return nil
	}
	if status == string(StateRunning) {
		if err := runOK(ctx, l.runner, "docker stop", dockerExe, "stop", "-t", stopGrace, l.name); err != nil {
			return err
		}
	}
	if purge {
		if status != "" {
			if err := runOK(ctx, l.runner, "docker rm", dockerExe, "rm", "-f", l.name); err != nil {
				return err
			}
		}
		// Full teardown: drop the persistent profile volume too (best-effort). A
		// plain `down` (purge=false) never touches it, so the profile survives.
		l.container().volumeRm(ctx)
	}
	return nil
}

// PurgeProfileVolume removes the persistent profile's named volume. The caller
// (`cuttle purge-profile`) removes the container first so the volume is detached.
func (l *Local) PurgeProfileVolume(ctx context.Context) error {
	if err := l.check(); err != nil {
		return err
	}
	l.container().volumeRm(ctx)
	return nil
}

// Image reports the image an existing container was created with, or "".
func (l *Local) Image(ctx context.Context) string {
	res, err := l.runner.Output(ctx, dockerExe, "inspect", "-f", "{{.Config.Image}}", l.name)
	if err != nil || res.Code != 0 {
		return ""
	}
	return strings.TrimSpace(res.Stdout)
}

// Diagnostics returns human-readable triage lines for an unhealthy container:
// the real host<-container port bindings and a log tail, so triage never needs a
// raw docker command. It is used by `status` via an optional interface.
func (l *Local) Diagnostics(ctx context.Context) []string {
	var lines []string
	if res, err := l.runner.Output(ctx, dockerExe, "port", l.name); err == nil && res.Code == 0 {
		if ports := strings.TrimSpace(res.Stdout); ports != "" {
			lines = append(lines, "actual port bindings (start `up` with THESE ports, do not --recreate):")
			for line := range strings.SplitSeq(ports, "\n") {
				lines = append(lines, "  "+line)
			}
		}
	}
	if res, err := l.runner.Output(ctx, dockerExe, "logs", "--tail", "20", l.name); err == nil {
		out := strings.TrimSpace(res.Stdout + res.Stderr)
		if out != "" {
			lines = append(lines, "last 20 log lines:")
			for line := range strings.SplitSeq(out, "\n") {
				lines = append(lines, "  "+line)
			}
		}
	}
	return lines
}

// Reach for local is a direct loopback endpoint on the host-mapped ports; no
// tunnel, so release is a no-op.
func (l *Local) Reach(_ context.Context, _, _ int) (Endpoint, func(), error) {
	return Endpoint{
		CDPHost: loopbackHost, CDPPort: l.cdpPort,
		VNCHost: loopbackHost, VNCPort: l.vncPort,
	}, func() {}, nil
}
