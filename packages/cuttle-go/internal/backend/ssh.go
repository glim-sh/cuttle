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
	runner  Runner
	host    string
	name    string
	cdpPort int // remote host-published CDP port
	vncPort int // remote host-published VNC port
	image   string
	proxy   string
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

func (s *SSH) State(ctx context.Context) (State, error) {
	if err := s.check(); err != nil {
		return "", err
	}
	res, err := s.runner.Output(ctx, "ssh", s.remoteArgs(dockerExe, "inspect", "-f", "{{.State.Status}}", s.name)...)
	if err != nil {
		return "", err
	}
	status := strings.TrimSpace(res.Stdout)
	switch {
	case res.Code != 0 || status == "":
		return StateAbsent, nil
	case status == string(StateRunning):
		return StateRunning, nil
	default:
		return StateStopped, nil
	}
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
	run := dockerRunArgs(s.name, s.cdpPort, s.vncPort, opts, image)
	return runOK(ctx, s.runner, "remote docker run", "ssh", s.remoteArgs(append([]string{dockerExe}, run...)...)...)
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
// deterministic and mcp can target it); 0 auto-picks free ports for the
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
