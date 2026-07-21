package backend

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const sshControlMaster = "ControlMaster=auto"

// SSH runs the browser in docker on a remote host reached over ssh, tunneled to
// this machine with ssh -L. It inherits ~/.ssh/config (keys, jump hosts, and any
// routing the user provides), so cuttle needs no ssh setup of its own.
type SSH struct {
	runner        Runner
	host          string
	name          string
	cdpPort       int // remote host-published CDP port
	vncPort       int // remote host-published VNC port
	image         string
	proxy         string
	tunnelContext string // resolved context name; standing-tunnel pidfile identity
}

func (s *SSH) check() error {
	return requireExe(s.runner, "ssh", "install an OpenSSH client and configure the host in ~/.ssh/config.")
}

// controlPath is a deterministic ControlMaster socket per host, so State/Stop
// reuse the multiplexed connection the forwarding session established.
func (s *SSH) controlPath() string {
	safe := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, s.host)
	return filepath.Join(os.TempDir(), "cuttle-ssh-"+safe+".sock")
}

// remoteArgs runs a command on the ssh host, reusing the ControlMaster socket.
func (s *SSH) remoteArgs(cmd ...string) []string {
	out := make([]string, 0, 5+len(cmd))
	out = append(out, "-o", sshControlMaster, "-o", "ControlPath="+s.controlPath(), s.host)
	return append(out, cmd...)
}

// container wraps the docker argv to run it on the ssh host, reusing the
// ControlMaster socket.
func (s *SSH) container() containerHost {
	return containerHost{
		runner: s.runner,
		name:   s.name,
		label:  "remote docker",
		wrap: func(args ...string) (string, []string) {
			return "ssh", s.remoteArgs(append([]string{dockerExe}, args...)...)
		},
	}
}

func (s *SSH) State(ctx context.Context) (State, error) {
	if err := s.check(); err != nil {
		return "", err
	}
	res, err := s.runner.Output(ctx, "ssh", s.remoteArgs(dockerExe, "inspect", "-f", "{{.State.Status}}", s.name)...)
	if err != nil {
		return "", err
	}
	return dockerStatusState(res.Stdout, res.Code), nil
}

func (s *SSH) Start(ctx context.Context, opts StartOpts) error {
	if err := s.check(); err != nil {
		return err
	}
	if opts.Proxy == "" {
		opts.Proxy = s.proxy
	}
	image := opts.Image
	if image == "" {
		image = s.image
	}
	return s.container().start(ctx, s.cdpPort, s.vncPort, opts, image, s.portConflict)
}

// portConflict turns a fresh remote-run host-port bind clash into an honest
// remedy: the ports are the remote host's, so the fix is to free whatever else is
// bound there (another container - `docker ps` on the host), not a local flag.
func (s *SSH) portConflict(err error) error {
	return fmt.Errorf("remote host port %d (CDP) or %d (VNC) is already bound - another container is using it; run `docker ps` on the host and stop it (or `cuttle down`)\n%w",
		s.cdpPort, s.vncPort, err)
}

func (s *SSH) Stop(ctx context.Context, purge bool) error {
	if err := s.check(); err != nil {
		return err
	}
	if err := runOK(ctx, s.runner, "remote docker stop", "ssh", s.remoteArgs(dockerExe, "stop", "-t", stopGrace, s.name)...); err != nil {
		return err
	}
	if purge {
		if err := runOK(ctx, s.runner, "remote docker rm", "ssh", s.remoteArgs(dockerExe, "rm", "-f", s.name)...); err != nil {
			return err
		}
	}
	return nil
}

// Reach opens an ssh -L tunnel from local ports to the remote container's
// published ports, establishing the ControlMaster the other calls reuse.
// cdpPort/vncPort pin the local ports (so a held `cuttle connect` forward is
// deterministic and a driver can attach to it); 0 auto-picks free ports for the
// ephemeral status/login forwards.
func (s *SSH) Reach(ctx context.Context, cdpPort, vncPort int) (Endpoint, func(), error) {
	if err := s.check(); err != nil {
		return Endpoint{}, nil, err
	}
	cdpLocal, err := chooseLocalPort(cdpPort)
	if err != nil {
		return Endpoint{}, nil, err
	}
	vncLocal, err := chooseLocalPort(vncPort)
	if err != nil {
		return Endpoint{}, nil, err
	}
	args := []string{
		"-N",
		"-o", sshControlMaster,
		"-o", "ControlPath=" + s.controlPath(),
		"-o", "ControlPersist=60",
		"-L", portStr(cdpLocal) + ":127.0.0.1:" + portStr(s.cdpPort),
		"-L", portStr(vncLocal) + ":127.0.0.1:" + portStr(s.vncPort),
		s.host,
	}
	proc, err := s.runner.Start(ctx, "ssh", args...)
	if err != nil {
		return Endpoint{}, nil, fmt.Errorf("starting ssh tunnel: %w", err)
	}
	ep := Endpoint{CDPHost: loopbackHost, CDPPort: cdpLocal, VNCHost: loopbackHost, VNCPort: vncLocal}
	return ep, func() { _ = proc.Stop() }, nil
}

// EnsureTunnel establishes (or reuses) a detached `ssh -N -L` forward on the
// fixed cdp/vnc ports that outlives the CLI. It carries no ControlPersist: the
// standing -N master is itself the long-lived connection State/Stop reuse.
func (s *SSH) EnsureTunnel(ctx context.Context, cdpPort, vncPort int) (Endpoint, error) {
	if err := s.check(); err != nil {
		return Endpoint{}, err
	}
	args := []string{
		"-N",
		"-o", sshControlMaster,
		"-o", "ControlPath=" + s.controlPath(),
		// Without this, ssh stays alive when a -L bind fails and the health
		// check would false-positive on whatever else holds the local port.
		"-o", "ExitOnForwardFailure=yes",
		"-L", portStr(cdpPort) + ":127.0.0.1:" + portStr(s.cdpPort),
		"-L", portStr(vncPort) + ":127.0.0.1:" + portStr(s.vncPort),
		s.host,
	}
	return ensureTunnel(ctx, tunnelSpec{context: s.tunnelContext, name: "ssh", args: args, cdpPort: cdpPort, vncPort: vncPort})
}

func (s *SSH) TunnelHealthy(ctx context.Context, cdpPort int) bool {
	return tunnelHealthy(ctx, s.tunnelContext, cdpPort)
}

func (s *SSH) StopTunnel() error { return stopTunnel(s.tunnelContext) }
