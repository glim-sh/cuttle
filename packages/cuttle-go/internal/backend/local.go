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
	stopGrace        = "15" // > cuttleserve's 5s Chrome drain, so the clean exit completes
	serveCommand     = "cuttleserve"
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
	return requireExe(l.runner, "docker", "install Docker (or OrbStack) first.")
}

// dockerStatus returns the container's raw docker state ("running", "exited",
// "created", ...) or "" if the container does not exist.
func (l *Local) dockerStatus(ctx context.Context) (string, error) {
	res, err := l.runner.Output(ctx, "docker", "inspect", "-f", "{{.State.Status}}", l.name)
	if err != nil {
		return "", err
	}
	if res.Code != 0 {
		return "", nil
	}
	return strings.TrimSpace(res.Stdout), nil
}

func (l *Local) State(ctx context.Context) (State, error) {
	if err := l.check(); err != nil {
		return "", err
	}
	status, err := l.dockerStatus(ctx)
	if err != nil {
		return "", err
	}
	switch status {
	case "":
		return StateAbsent, nil
	case "running":
		return StateRunning, nil
	default:
		return StateStopped, nil
	}
}

func (l *Local) rm(ctx context.Context) {
	_, _ = l.runner.Output(ctx, "docker", "rm", "-f", l.name)
}

// Start ensures the container is up, idempotently. A stopped container is
// restarted (profile preserved); a zombie (a run that died before a clean exit)
// is removed and re-run; --recreate forces a fresh container.
func (l *Local) Start(ctx context.Context, opts StartOpts) error {
	if err := l.check(); err != nil {
		return err
	}
	status, err := l.dockerStatus(ctx)
	if err != nil {
		return err
	}

	if opts.Recreate && status != "" {
		l.rm(ctx)
		status = ""
	}
	// A container that never ran cleanly ("created" from a run that died at
	// network setup, or "dead") has no live host port binding to restart into.
	// Only "exited" is safe to restart (a clean `cuttle down`).
	if status != "" && status != "running" && status != "exited" {
		l.rm(ctx)
		status = ""
	}

	switch {
	case status == "running":
		return nil
	case status != "": // exited -> restart, keeping the profile
		res, err := l.runner.Output(ctx, "docker", "start", l.name)
		if err != nil {
			return err
		}
		if res.Code != 0 {
			return fmt.Errorf("docker start failed:\n%s", strings.TrimSpace(res.Stderr)) //nolint:err113
		}
		return nil
	}

	image := opts.Image
	if image == "" {
		image = l.image
	}
	args := dockerRunArgs(l.name, l.cdpPort, l.vncPort, opts, image)
	res, err := l.runner.Output(ctx, "docker", args...)
	if err != nil {
		return err
	}
	if res.Code != 0 {
		// A run that fails at network setup leaves a half-created container
		// behind; remove it so the next `up` does not mistake it for restartable.
		l.rm(ctx)
		return fmt.Errorf("docker run failed:\n%s", strings.TrimSpace(res.Stderr)) //nolint:err113
	}
	return nil
}

// dockerRunArgs builds the `docker run ...` argv (without the leading "docker")
// shared by the local and ssh backends. The container always binds host
// 127.0.0.1: locally that is this machine, over ssh it is the remote host that
// ssh -L then tunnels to.
func dockerRunArgs(name string, cdpPort, vncPort int, opts StartOpts, image string) []string {
	args := []string{
		"run", "-d",
		"--name", name,
		"-p", "127.0.0.1:" + portStr(cdpPort) + ":" + containerCDPPort,
		shmSize,
	}
	if !opts.NoVNC {
		args = append(args, "-p", "127.0.0.1:"+portStr(vncPort)+":"+containerVNCPort, "-e", "CUTTLE_VNC=1")
	}
	if opts.Proxy != "" {
		args = append(args, "-e", "CUTTLESERVE_PROXY="+opts.Proxy)
	}
	// cuttleserve defaults to port 9222 and auto-binds 0.0.0.0 in a container, so
	// pass neither; it only accepts the `=` form and has no --host flag.
	args = append(args, image, serveCommand)
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
	if status == "running" {
		res, err := l.runner.Output(ctx, "docker", "stop", "-t", stopGrace, l.name)
		if err != nil {
			return err
		}
		if res.Code != 0 {
			return fmt.Errorf("docker stop failed:\n%s", strings.TrimSpace(res.Stderr)) //nolint:err113
		}
	}
	if purge {
		res, err := l.runner.Output(ctx, "docker", "rm", "-f", l.name)
		if err != nil {
			return err
		}
		if res.Code != 0 {
			return fmt.Errorf("docker rm failed:\n%s", strings.TrimSpace(res.Stderr)) //nolint:err113
		}
	}
	return nil
}

// Image reports the image an existing container was created with, or "".
func (l *Local) Image(ctx context.Context) string {
	res, err := l.runner.Output(ctx, "docker", "inspect", "-f", "{{.Config.Image}}", l.name)
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
	if res, err := l.runner.Output(ctx, "docker", "port", l.name); err == nil && res.Code == 0 {
		if ports := strings.TrimSpace(res.Stdout); ports != "" {
			lines = append(lines, "actual port bindings (start `up` with THESE ports, do not --recreate):")
			for _, line := range strings.Split(ports, "\n") {
				lines = append(lines, "  "+line)
			}
		}
	}
	if res, err := l.runner.Output(ctx, "docker", "logs", "--tail", "20", l.name); err == nil {
		out := strings.TrimSpace(res.Stdout + res.Stderr)
		if out != "" {
			lines = append(lines, "last 20 log lines:")
			for _, line := range strings.Split(out, "\n") {
				lines = append(lines, "  "+line)
			}
		}
	}
	return lines
}

// Reach for local is a direct loopback endpoint on the host-mapped ports; no
// tunnel, so release is a no-op.
func (l *Local) Reach(_ context.Context) (Endpoint, func(), error) {
	return Endpoint{
		CDPHost: "127.0.0.1", CDPPort: l.cdpPort,
		VNCHost: "127.0.0.1", VNCPort: l.vncPort,
	}, func() {}, nil
}
