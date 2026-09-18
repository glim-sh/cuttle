package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// `cuttle __mask-exec` is the in-container half of driver-output masking. The
// host's `cuttle pw` does not run the driver directly: it runs this wrapper
// BESIDE the daemon, inside the container, and the wrapper streams the driver's
// stdout and stderr through the daemon's /mask route.
//
// That placement is the whole design. Masking on the host would mean the host
// process must hold every secret value to compare against - which is exactly
// what daemon-owned secrets exist to avoid. Here the batch travels one hop over
// container loopback, the values stay in daemon memory, and what comes back is
// already masked, so a value never reaches the host, a file, or an agent's
// context.
//
// It covers what the daemon HOLDS - the same boundary playwright-mcp's
// redactSecrets and browser-use's sensitive-data handling draw - plus the
// credentials the daemon recognizes by their issuer's prefix, which it CAPTURES
// rather than destroys: a one-time token the page showed once stays usable as
// {{cuttle:TOKEN_1}}. Anything else the page shows is not masked.

const (
	// maskBatchLimit forces a flush on a stream that never emits a newline, so a
	// whole page never accumulates in the wrapper.
	maskBatchLimit = 512 << 10
	// partialLineWait is how long a tail with no newline waits for one before it
	// is printed anyway. A driver that prompts and waits must not be held back by
	// its own missing newline.
	partialLineWait = 150 * time.Millisecond
	maskRequestWait = 30 * time.Second
	// maskReplyLimit bounds one reply. Masking can lengthen a batch (a 4-byte
	// value becomes <secret:NAME>), but never by anywhere near this much.
	maskReplyLimit = 32 << 20
	// maskCheckFlag is how the host probes an image for this wrapper: an older
	// image's cuttle has no such command and exits non-zero (see maskExecScript).
	maskCheckFlag = "--check"
)

// autoNameRE finds the auto-capture names in already-masked output; each one is
// worth a line to the caller, who otherwise sees a placeholder for a value they
// never named.
var autoNameRE = regexp.MustCompile(`<secret:(TOKEN_[0-9]+)>`)

func init() { AddCommand(newMaskExecCmd()) }

func newMaskExecCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "__mask-exec -- command [args...]",
		Short:  "run a command inside the container with its output masked by the daemon (internal)",
		Hidden: true, // `cuttle pw` runs this in the container; it is not a user verb
		// The args belong to the wrapped command, not to cuttle.
		DisableFlagParsing: true,
		RunE:               runMaskExec,
	}
}

func runMaskExec(cmd *cobra.Command, args []string) error {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 1 && args[0] == maskCheckFlag {
		return nil // the host's probe: this image masks
	}
	if len(args) == 0 {
		return errPlaywrightNoVerb
	}
	// The argv is the one `cuttle pw` built from the caller's own command line,
	// running inside the container the caller already drives.
	child := exec.CommandContext(cmd.Context(), args[0], args[1:]...) //nolint:gosec // G204: the caller's own argv
	child.Stdin = cmd.InOrStdin()
	stdout, err := child.StdoutPipe()
	if err != nil {
		return fmt.Errorf("wiring the driver's stdout: %w", err)
	}
	stderr, err := child.StderrPipe()
	if err != nil {
		return fmt.Errorf("wiring the driver's stderr: %w", err)
	}
	if err = child.Start(); err != nil {
		return fmt.Errorf("running %s: %w", args[0], err)
	}
	stop := forwardSignals(child)
	defer stop()

	m := &outputMasker{ctx: cmd.Context(), errW: cmd.ErrOrStderr(), noted: map[string]bool{}}
	var wg sync.WaitGroup
	wg.Go(func() { m.pump(stdout, cmd.OutOrStdout()) })
	wg.Go(func() { m.pump(stderr, cmd.ErrOrStderr()) })
	// Both pipes must be drained before Wait closes them.
	wg.Wait()
	err = playwrightExit(child.Wait())
	if err == nil && m.withheld {
		return errMaskWithheld
	}
	return err
}

var errMaskWithheld = errors.New("some driver output was withheld because the daemon could not mask it")

// forwardSignals relays a terminating signal to the driver, so a Ctrl-C reaches
// the process doing the work rather than only the wrapper around it.
func forwardSignals(child *exec.Cmd) func() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for sig := range ch {
			if child.Process != nil {
				_ = child.Process.Signal(sig)
			}
		}
	}()
	return func() { signal.Stop(ch); close(ch) }
}

// outputMasker filters one process's two streams. Its mutex serializes the
// daemon calls and the writes, so stdout and stderr interleave at batch
// boundaries rather than mid-line, and each auto name is noted once.
type outputMasker struct {
	ctx  context.Context //nolint:containedctx // one per invocation, alongside the streams it bounds
	errW io.Writer

	mu       sync.Mutex
	noted    map[string]bool
	withheld bool
}

// pump streams one pipe through the masker in line batches. It never buffers a
// whole run: a batch leaves as soon as it ends in a newline, and a tail without
// one leaves after partialLineWait, so a streaming verb stays streaming.
func (m *outputMasker) pump(r io.Reader, w io.Writer) {
	chunks := make(chan []byte)
	go func() {
		defer close(chunks)
		for {
			buf := make([]byte, 32<<10)
			n, err := r.Read(buf)
			if n > 0 {
				chunks <- buf[:n]
			}
			if err != nil {
				return
			}
		}
	}()
	var pending []byte
	idle := time.NewTimer(time.Hour)
	idle.Stop()
	for {
		select {
		case chunk, ok := <-chunks:
			if !ok {
				m.emit(w, pending)
				return
			}
			pending = append(pending, chunk...)
			if cut := batchEnd(pending); cut > 0 {
				m.emit(w, pending[:cut])
				pending = slices.Clone(pending[cut:])
			}
			idle.Reset(partialLineWait)
		case <-idle.C:
			m.emit(w, pending)
			pending = nil
		}
	}
}

// batchEnd is how much of the buffer may be masked now: everything up to the
// last complete line, so a value split across two reads is never half-matched.
func batchEnd(pending []byte) int {
	if end := bytes.LastIndexByte(pending, '\n') + 1; end > 0 {
		return end
	}
	if len(pending) >= maskBatchLimit {
		// A stream with no newlines at all: cut after the last space rather than
		// through whatever token the limit happens to land in.
		if end := bytes.LastIndexAny(pending, " \t\r") + 1; end > 0 {
			return end
		}
		return len(pending)
	}
	return 0
}

// emit masks one batch, writes it, and names any credential the daemon captured
// out of it - once per name per invocation, so a page full of the same token
// costs one line.
func (m *outputMasker) emit(w io.Writer, batch []byte) {
	if len(batch) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	masked, ok := m.mask(batch)
	if !ok {
		return
	}
	_, _ = w.Write(masked)
	for _, match := range autoNameRE.FindAllSubmatch(masked, -1) {
		if name := string(match[1]); !m.noted[name] {
			m.noted[name] = true
			fmt.Fprintf(m.errW, "cuttle: masked a credential as %s - `cuttle secret ls` to see it,"+
				" fill it elsewhere as {{cuttle:%s}}\n", name, name)
		}
	}
}

// mask is one round trip to the daemon. It fails CLOSED: this wrapper and the
// daemon ship in the same image (an older image is caught by the host's probe
// before this runs), so an error here is a failing daemon, not a missing route -
// and printing the batch anyway would hand a page that can slow the daemon down a
// way to switch masking off. A withheld batch is said once on stderr and turns a
// successful run into an error.
func (m *outputMasker) mask(batch []byte) ([]byte, bool) {
	masked, err := postMask(m.ctx, batch)
	if err != nil {
		if !m.withheld {
			fmt.Fprintf(m.errW, "cuttle: driver output withheld - the daemon could not mask it (%v)\n", err)
		}
		m.withheld = true
		return nil, false
	}
	return masked, true
}

// maskEndpoint is the daemon's masker as seen from inside the container. A var
// only so a test can point it at a stub; nothing changes it at runtime.
var maskEndpoint = playwrightCDPEndpoint + "/mask"

func postMask(ctx context.Context, batch []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, maskRequestWait)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, maskEndpoint, bytes.NewReader(batch))
	if err != nil {
		return nil, err //nolint:wrapcheck
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err //nolint:wrapcheck
	}
	defer func() { _ = resp.Body.Close() }()
	masked, err := io.ReadAll(io.LimitReader(resp.Body, maskReplyLimit))
	if err != nil {
		return nil, err //nolint:wrapcheck
	}
	if resp.StatusCode != http.StatusOK {
		return nil, daemonError(resp.StatusCode, masked)
	}
	return masked, nil
}
