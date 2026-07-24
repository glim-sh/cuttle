package backend

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// sshExe is the local OpenSSH client every remote call goes through.
const sshExe = "ssh"

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
	return requireExe(s.runner, sshExe, "install an OpenSSH client and configure the host in ~/.ssh/config.")
}

// controlPath is a deterministic ControlMaster socket per host, so State/Stop
// reuse the multiplexed connection the forwarding session established.
func (s *SSH) controlPath() string {
	return filepath.Join(os.TempDir(), "cuttle-ssh-"+safeToken(s.host)+".sock")
}

// remoteArgs runs a command on the ssh host, reusing the ControlMaster socket.
// ssh appends the remote-command args separated by spaces into one string and
// runs it under the host's login shell, so each token is shell-quoted first: a
// token carrying shell metacharacters would otherwise be re-parsed remotely
// (splitting on `;`/`|`, expanding `$`/globs, dropping quotes) instead of reaching
// the remote command as one argument. The ssh client options before the host are
// not quoted (they are consumed by the local ssh client, not the remote shell).
func (s *SSH) remoteArgs(cmd ...string) []string {
	out := make([]string, 0, 5+len(cmd))
	out = append(out, "-o", sshControlMaster, "-o", "ControlPath="+s.controlPath(), s.host)
	for _, c := range cmd {
		out = append(out, shellQuote(c))
	}
	return out
}

// shellQuote returns tok unchanged when it holds only characters the remote
// shell treats literally, else wraps it in single quotes (with any embedded
// single quote escaped as '\”). Leaving already-safe tokens verbatim keeps the
// common docker argv - and the tests that assert it - readable, while any token
// with a metacharacter survives ssh's remote re-parse as a single argument.
func shellQuote(tok string) string {
	if tok == "" {
		return "''"
	}
	safe := true
	for _, r := range tok {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("_-.:/=@%+,{}", r):
		default:
			safe = false
		}
		if !safe {
			break
		}
	}
	if safe {
		return tok
	}
	return "'" + strings.ReplaceAll(tok, "'", `'\''`) + "'"
}

// container wraps the docker argv to run it on the ssh host, reusing the
// ControlMaster socket.
func (s *SSH) container() containerHost {
	return containerHost{
		runner: s.runner,
		name:   s.name,
		label:  "remote docker",
		wrap: func(args ...string) (string, []string) {
			return sshExe, s.remoteArgs(append([]string{dockerExe}, args...)...)
		},
	}
}

// LogsCommand returns the ssh argv that streams the remote container's docker
// logs, reusing the ControlMaster socket like every other remote call.
func (s *SSH) LogsCommand(follow bool) (string, []string) {
	return sshExe, s.remoteArgs(append([]string{dockerExe}, dockerLogsArgs(follow, s.name)...)...)
}

// DiscoverPorts reads the remote container's published CDP/VNC host ports (the
// ports ssh -L then forwards), so a caller need only pass --context/--name.
func (s *SSH) DiscoverPorts(ctx context.Context) (int, int, bool) {
	if err := s.check(); err != nil {
		return 0, 0, false
	}
	return discoverPorts(ctx, s.container())
}

func (s *SSH) State(ctx context.Context) (State, error) {
	if err := s.check(); err != nil {
		return "", err
	}
	res, err := s.runner.Output(ctx, sshExe, s.remoteArgs(dockerExe, "inspect", "-f", "{{.State.Status}}", s.name)...)
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
	status, err := s.container().status(ctx)
	if err != nil {
		return err
	}
	// A plain stop on an absent container has nothing to do; on --purge we still
	// fall through to drop a profile volume that outlived the container.
	if status == "" && !purge {
		return nil
	}
	if status == string(StateRunning) {
		if err := runOK(ctx, s.runner, "remote docker stop", sshExe, s.remoteArgs(dockerExe, "stop", "-t", stopGrace, s.name)...); err != nil {
			return err
		}
	}
	if purge {
		if status != "" {
			if err := runOK(ctx, s.runner, "remote docker rm", sshExe, s.remoteArgs(dockerExe, "rm", "-f", s.name)...); err != nil {
				return err
			}
		}
		// Full teardown: drop the persistent profile volume too (best-effort). A
		// plain `down` (purge=false) never touches it, so the profile survives.
		s.container().volumeRm(ctx)
	}
	return nil
}

// PurgeProfileVolume removes the persistent profile's named volume on the ssh
// host. The caller (`cuttle purge-profile`) removes the container first so the
// volume is detached.
func (s *SSH) PurgeProfileVolume(ctx context.Context) error {
	if err := s.check(); err != nil {
		return err
	}
	s.container().volumeRm(ctx)
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
	proc, err := s.runner.Start(ctx, sshExe, args...)
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
	return ensureTunnel(ctx, tunnelSpec{context: s.tunnelContext, name: sshExe, args: args, cdpPort: cdpPort, vncPort: vncPort})
}

func (s *SSH) StopTunnel() error { return stopTunnel(s.tunnelContext) }
