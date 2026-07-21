package backend

import (
	"context"
	"fmt"
	"strings"
)

const (
	containerCDPPort = "9222"
	containerVNCPort = "6080"
	shmSize          = "--shm-size=2g"
	stopGrace        = "15" // > cuttle serve's 5s Chrome drain, so the clean exit completes
	dockerRunSub     = "run"
	dockerNameFlag   = "--name"
)

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
	if opts.Recreate && status != "" {
		h.rm(ctx)
		status = ""
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
// ssh -L then tunnels to.
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
	}
	if !opts.NoVNC {
		args = append(args, "-p", loopbackHost+":"+portStr(vncPort)+":"+containerVNCPort, "-e", "CUTTLE_VNC=1")
	}
	if opts.Proxy != "" {
		args = append(args, "-e", "CUTTLE_PROXY="+opts.Proxy)
	}
	// cuttle serve defaults to port 9222 and auto-binds 0.0.0.0 in a container, so
	// pass neither; it only accepts the `=` form and has no --host flag.
	args = append(args, image, "cuttle", "serve")
	if opts.IdleTimeout != "" {
		args = append(args, "--idle-timeout="+opts.IdleTimeout)
	}
	if opts.KeepProfile == nil || *opts.KeepProfile {
		args = append(args, "--keep-profile")
	}
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
	if status == "" {
		return nil
	}
	if status == string(StateRunning) {
		if err := runOK(ctx, l.runner, "docker stop", dockerExe, "stop", "-t", stopGrace, l.name); err != nil {
			return err
		}
	}
	if purge {
		if err := runOK(ctx, l.runner, "docker rm", dockerExe, "rm", "-f", l.name); err != nil {
			return err
		}
	}
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
