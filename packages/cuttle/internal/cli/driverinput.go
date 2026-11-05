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

func (w *driverInputWriter) Close() error {
	if err := binary.Write(w.WriteCloser, binary.BigEndian, uint32(0)); err != nil {
		return fmt.Errorf("end driver stdin: %w", err)
	}
	return nil
}

func (w *driverInputWriter) Write(p []byte) (int, error) {
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
		if written != n {
			return total, io.ErrShortWrite
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
		if src != nil && copyDriverInput(ctx, w, src) != nil {
			return
		}
		if err := binary.Write(w, binary.BigEndian, uint32(0)); err != nil {
			return
		}
		<-ctx.Done()
	}()
	return r, nil
}

func copyDriverInput(ctx context.Context, dst io.WriteCloser, src io.Reader) error {
	framed := &driverInputWriter{WriteCloser: dst}
	buf := make([]byte, driverInputChunk)
	for {
		if ctx.Err() != nil {
			return fmt.Errorf("copy driver stdin: %w", ctx.Err())
		}
		if f, ok := src.(*os.File); ok {
			events, err := pollDriverInput(f, 100)
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
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
				events, pollErr := pollDriverInput(f, 0)
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
		eof := false
		buf := make([]byte, driverInputChunk)
		for {
			var size uint32
			if err := binary.Read(stdin, binary.BigEndian, &size); err != nil {
				return
			}
			if size == 0 && !eof {
				_ = w.Close()
				eof = true
				continue
			}
			if eof || size > driverInputChunk || size == 0 {
				return
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
	}()
	return runMaskExec(ctx, endpoint, hint, argv, r, stdout, stderr)
}

func pollDriverInput(f *os.File, timeout int) (int16, error) {
	raw, err := f.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("access stdin descriptor: %w", err)
	}
	var events int16
	var pollErr error
	err = raw.Control(func(fd uintptr) {
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}} //nolint:gosec // OS file descriptor
		_, pollErr = unix.Poll(fds, timeout)
		events = fds[0].Revents
	})
	if err != nil {
		return 0, fmt.Errorf("control stdin descriptor: %w", err)
	}
	if pollErr != nil {
		return 0, fmt.Errorf("poll stdin descriptor: %w", pollErr)
	}
	return events, nil
}
