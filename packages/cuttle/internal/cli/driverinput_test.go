package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"testing/iotest"
	"time"
)

func TestDriverInputEOFIsNotDisconnect(t *testing.T) {
	t.Parallel()
	s := fakeMaskServer(t)
	for _, input := range []string{"", "test-sensitive-value\n", strings.Repeat("x\n", 50000)} {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		r, err := driverInput(ctx, strings.NewReader(input))
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		var out bytes.Buffer
		err = runMaskExecStream(ctx, s.URL+"/mask", "upgrade", []string{"sh", "-c", "cat; sleep 0.1; exit 23"}, r, &out, io.Discard)
		cancel()
		var ec *ExitCodeError
		if !errors.As(err, &ec) || ec.Code != 23 {
			t.Fatalf("exit = %v", err)
		}
		if got, want := out.String(), strings.ReplaceAll(input, "test-sensitive-value", "<masked>"); got != want {
			t.Fatalf("output length = %d, want %d", len(got), len(want))
		}
	}
}

// A host stdin that fails to read ends the driver's input, not the driver.
func TestDriverInputReadErrorIsNotDisconnect(t *testing.T) {
	t.Parallel()
	s := fakeMaskServer(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	r, err := driverInput(ctx, io.MultiReader(strings.NewReader("partial\n"), iotest.ErrReader(syscall.EIO)))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = runMaskExecStream(ctx, s.URL+"/mask", "upgrade", []string{"sh", "-c", "cat; exit 23"}, r, &out, io.Discard)
	if ec, ok := errors.AsType[*ExitCodeError](err); !ok || ec.Code != 23 || out.String() != "partial\n" {
		t.Fatalf("exit = %v, output = %q", err, out.String())
	}
}

func TestDriverInputDisconnectKillsQuietGroup(t *testing.T) {
	t.Parallel()
	s := fakeMaskServer(t)
	for _, blocked := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		r, w, err := os.Pipe()
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		done := make(chan error, 1)
		ready := make(chan struct{}, 1)
		go func() {
			done <- runMaskExecStream(ctx, s.URL+"/mask", "upgrade", []string{"sh", "-c", "echo ready; sleep 30 & wait"}, r, signalWriter{ready}, io.Discard)
		}()
		select {
		case <-ready:
		case <-ctx.Done():
			t.Fatal("driver did not start")
		}
		if blocked {
			go func() { _, _ = driverInputWriter{w}.Write(bytes.Repeat([]byte("x"), 1024*1024)) }()
			time.Sleep(100 * time.Millisecond)
		}
		_ = w.Close()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("disconnected driver succeeded")
			}
		case <-time.After(2 * time.Second):
			cancel()
			t.Fatal("driver group survived transport EOF")
		}
		cancel()
	}
}
