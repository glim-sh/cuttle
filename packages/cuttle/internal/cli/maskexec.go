package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
)

const (
	maskEndpoint        = "http://127.0.0.1:9222/mask"
	maskResponseTimeout = 10 * time.Second
)

func init() { AddCommand(newMaskExecCmd()) }

func newMaskExecCmd() *cobra.Command {
	var hint, seed string
	cmd := &cobra.Command{
		Use: "mask-exec -- <argv>", Hidden: true, Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, argv []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGPIPE)
			defer stop()
			return runMaskExec(ctx, seedURL(maskEndpoint, seed), hint, argv, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringVar(&seed, "seed", "", "browser seed")
	cmd.Flags().StringVar(&hint, "hint", "upgrade the container image", "remedy when masking is unavailable")
	return cmd
}

func maskUpgradeHint(self string, external bool) string {
	self = strings.TrimSuffix(self, " --seed "+instance.seed)
	if external {
		return "the release's image predates this CLI - bump its chart pin"
	}
	return "run `" + self + " up --recreate` to upgrade the image"
}

func maskExecArgv(argv []string) []string {
	name, ctxName, cfg, _, err := resolve(commonFlags{}, defaultImage())
	self := defaultName
	if err == nil {
		self = cuttleCmd(ctxName, cfg, name)
	}
	hint := maskUpgradeHint(self, cfg.External)
	// Old images must still preserve the command's status without exposing its output.
	script := `hint=$1; shift
if cuttle mask-exec --help >/dev/null 2>&1; then exec cuttle mask-exec --hint "$hint" -- "$@"; fi
"$@" >/dev/null 2>&1; rc=$?
printf 'cuttle: driver output withheld: masking unavailable; %s\n' "$hint" >&2
exit "$rc"`
	if seed := selectedSeed(); seed != "" {
		script = strings.Replace(script, `--hint "$hint" --`, `--hint "$hint" --seed `+seed+` --`, 1)
	}
	return append([]string{"sh", "-c", script, "cuttle-mask", hint}, argv...)
}

func runMaskExec(ctx context.Context, endpoint, hint string, argv []string, stdin io.Reader, stdout, stderr io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for _, dst := range []io.Writer{stdout, stderr} {
					if f, ok := dst.(*os.File); ok {
						fd := f.Fd()
						if fd > 1<<31-1 {
							continue
						}
						fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT}}
						if _, err := unix.Poll(fds, 0); err == nil && fds[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
							cancel()
							return
						}
					}
				}
			}
		}
	}()
	child := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // argv is the explicitly requested driver command
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	child.Cancel = func() error { return syscall.Kill(-child.Process.Pid, syscall.SIGKILL) }
	child.WaitDelay = time.Second
	child.Stdin = stdin
	maskCtx, stopMask := context.WithCancel(ctx)
	defer stopMask()
	var wg sync.WaitGroup
	var failed bool
	var mu sync.Mutex
	pipe := func(dst io.Writer) *io.PipeWriter {
		r, w := io.Pipe()
		wg.Go(func() {
			defer r.Close()
			if !streamMasked(maskCtx, endpoint, r, dst, cancel) {
				stopMask()
				mu.Lock()
				failed = true
				mu.Unlock()
			}
		})
		return w
	}
	out, errOut := pipe(stdout), pipe(stderr)
	child.Stdout, child.Stderr = maskInputWriter{out}, maskInputWriter{errOut}
	err := child.Run()
	_ = out.Close()
	_ = errOut.Close()
	drainTimer := time.AfterFunc(maskResponseTimeout, cancel)
	defer drainTimer.Stop()
	wg.Wait()
	if failed {
		_, _ = fmt.Fprintf(stderr, "cuttle: driver output withheld: masking unavailable; %s\n", hint)
	}
	if err == nil {
		return nil
	}
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		code := ee.ExitCode()
		if code < 0 {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
				code = 128 + int(ws.Signal())
			} else {
				code = 1
			}
		}
		return &ExitCodeError{Code: code}
	}
	// Start errors can contain command arguments, so never replay them unmasked.
	return &ExitCodeError{Code: execNotFound}
}

func streamMasked(ctx context.Context, endpoint string, src io.ReadCloser, dst io.Writer, cancel context.CancelFunc) bool {
	requestCtx, stop := context.WithCancel(ctx)
	defer stop()
	// Transport waits for its body writer, even when the request is canceled.
	unblock := context.AfterFunc(requestCtx, func() { _ = src.Close() })
	defer unblock()
	defer src.Close()
	headerTimer := time.AfterFunc(maskResponseTimeout, stop)
	defer headerTimer.Stop()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, src)
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, dialErr := (&net.Dialer{Timeout: maskResponseTimeout}).DialContext(ctx, network, address)
		if dialErr != nil {
			return nil, fmt.Errorf("connect mask stream: %w", dialErr)
		}
		return maskConn{conn}, nil
	}}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	headerTimer.Stop()
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false
	}
	_, err = io.Copy(maskOutputWriter{dst, cancel}, resp.Body)
	return err == nil
}

// Once masking fails, the driver must still finish with its own exit status.
type maskInputWriter struct{ *io.PipeWriter }

func (w maskInputWriter) Write(p []byte) (int, error) {
	_, _ = w.PipeWriter.Write(p)
	return len(p), nil
}

type maskConn struct{ net.Conn }

func (c maskConn) Write(p []byte) (int, error) {
	if err := c.SetWriteDeadline(time.Now().Add(maskResponseTimeout)); err != nil {
		return 0, fmt.Errorf("set mask write deadline: %w", err)
	}
	return c.Conn.Write(p) //nolint:wrapcheck // preserve the transport error
}

type maskOutputWriter struct {
	io.Writer
	cancel context.CancelFunc
}

func (w maskOutputWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if err != nil {
		w.cancel()
	}
	return n, err //nolint:wrapcheck // io.Copy must see the output failure
}

func olderDaemon(daemon, cli string) bool {
	parse := func(v string) ([3]int, bool) {
		var parts [3]int
		v = strings.TrimPrefix(v, "v")
		if strings.ContainsAny(v, "-+") {
			return parts, false
		}
		n, err := fmt.Sscanf(v, "%d.%d.%d", &parts[0], &parts[1], &parts[2])
		return parts, n == 3 && err == nil
	}
	a, okA := parse(daemon)
	b, okB := parse(cli)
	if !okA || !okB {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}
