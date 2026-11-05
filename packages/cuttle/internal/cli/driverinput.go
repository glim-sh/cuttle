package cli

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

const driverInputChunk = 32 * 1024

// A zero-length frame closes the driver's stdin, but leaves the transport open
// until the host exits. Transport EOF therefore means the host has gone away.
type driverInputWriter struct{ io.WriteCloser }

func (w driverInputWriter) Close() error {
	if err := binary.Write(w.WriteCloser, binary.BigEndian, uint32(0)); err != nil {
		return fmt.Errorf("end driver stdin: %w", err)
	}
	return nil
}

func (w driverInputWriter) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		n := min(len(p), driverInputChunk)
		if err := binary.Write(w.WriteCloser, binary.BigEndian, uint32(n)); err != nil {
			return total, fmt.Errorf("write stdin frame: %w", err)
		}
		written, err := w.WriteCloser.Write(p[:n])
		total += written
		if err != nil {
			return total, fmt.Errorf("write driver stdin: %w", err)
		}
		p = p[n:]
	}
	return total, nil
}

func driverInput(ctx context.Context, src io.Reader) (*os.File, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create driver stdin: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { _ = w.Close() })
	go func() {
		defer stop()
		defer w.Close()
		framed := driverInputWriter{w}
		// A failed stdin read only ends the driver's input; cancellation alone means
		// the host is going away.
		if src != nil && copyDriverInput(ctx, framed, src) != nil && ctx.Err() != nil {
			return
		}
		if framed.Close() != nil {
			return
		}
		<-ctx.Done()
	}()
	return r, nil
}

func copyDriverInput(ctx context.Context, framed driverInputWriter, src io.Reader) error {
	buf := make([]byte, driverInputChunk)
	for {
		if ctx.Err() != nil {
			return fmt.Errorf("copy driver stdin: %w", ctx.Err())
		}
		if f, ok := src.(*os.File); ok {
			events, err := pollFile(f, unix.POLLIN, int(maskPollInterval.Milliseconds()))
			if errors.Is(err, unix.EINTR) || events == 0 && err == nil {
				continue
			}
			if err != nil {
				return fmt.Errorf("poll driver stdin: %w", err)
			}
		}
		if ctx.Err() != nil {
			return fmt.Errorf("copy driver stdin: %w", ctx.Err())
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			if _, err := framed.Write(buf[:n]); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return fmt.Errorf("read driver stdin: %w", readErr)
		}
	}
}

func runMaskExecStream(ctx context.Context, endpoint, hint string, argv []string, stdin io.Reader, stdout, stderr io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if closer, ok := stdin.(io.Closer); ok {
		defer closer.Close()
	}
	r, w, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create driver stdin: %w", err)
	}
	defer r.Close()
	defer w.Close()
	// Polling catches a disconnected transport even while forwarding is blocked
	// on a driver that does not consume its input.
	if f, ok := stdin.(*os.File); ok {
		go func() {
			ticker := time.NewTicker(maskPollInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
				events, pollErr := pollFile(f, unix.POLLIN, 0)
				if pollErr != nil && !errors.Is(pollErr, unix.EINTR) || events&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0 {
					cancel()
					return
				}
			}
		}()
	}
	go func() {
		defer cancel()
		defer w.Close()
		buf := make([]byte, driverInputChunk)
		var size uint32
		for {
			if err := binary.Read(stdin, binary.BigEndian, &size); err != nil || size > driverInputChunk {
				return
			}
			if size == 0 {
				break
			}
			if _, err := io.ReadFull(stdin, buf[:size]); err != nil {
				return
			}
			if _, err := w.Write(buf[:size]); err != nil {
				// A driver may close stdin early; keep watching the transport anyway.
				if ctx.Err() != nil {
					return
				}
			}
		}
		_ = w.Close()
		// Anything after the driver's EOF, data or disconnect, ends the run.
		_ = binary.Read(stdin, binary.BigEndian, &size)
	}()
	return runMaskExec(ctx, endpoint, hint, argv, r, stdout, stderr)
}

func pollFile(f *os.File, events int16, timeout int) (int16, error) {
	raw, err := f.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("access descriptor: %w", err)
	}
	var revents int16
	var pollErr error
	err = raw.Control(func(fd uintptr) {
		fds := []unix.PollFd{{Fd: int32(fd), Events: events}} //nolint:gosec // OS file descriptor
		_, pollErr = unix.Poll(fds, timeout)
		revents = fds[0].Revents
	})
	if err != nil {
		return 0, fmt.Errorf("control descriptor: %w", err)
	}
	if pollErr != nil {
		return 0, fmt.Errorf("poll descriptor: %w", pollErr)
	}
	return revents, nil
}
