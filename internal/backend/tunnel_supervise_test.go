//go:build !windows

package backend

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// superviseCommand must re-exec cuttle itself into the hidden supervisor subcommand
// with the forward's argv appended, so the tunnel gets dependency-free reconnect.
func TestSuperviseCommandReExecs(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Skipf("no os.Executable: %v", err)
	}
	name, args := superviseCommand(tunnelSpec{name: "ssh", args: []string{"-N", "-L", "9222:127.0.0.1:9222", "box"}})
	if name != self {
		t.Fatalf("re-exec target = %q, want the cuttle binary %q", name, self)
	}
	want := []string{SuperviseTunnelSubcmd, "ssh", "-N", "-L", "9222:127.0.0.1:9222", "box"}
	if len(args) != len(want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args = %v, want %v", args, want)
		}
	}
}

// superviseLoop must restart a forward that exits on its own, and stop promptly
// when its ctx is cancelled (the teardown path).
func TestSuperviseLoopRestartsThenStops(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "runs")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		superviseLoop(ctx, "sh", []string{"-c", "printf x >> " + marker + "; exit 1"})
		close(done)
	}()

	// The forward exits immediately; a restart is proof the loop re-ran it (min
	// backoff is 1s, so >=2 runs means at least one restart cycle happened).
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if b, _ := os.ReadFile(marker); len(b) >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if b, _ := os.ReadFile(marker); len(b) < 2 {
		t.Fatalf("supervisor did not restart the forward: %d runs", len(b))
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("superviseLoop did not stop after ctx cancel")
	}
}
