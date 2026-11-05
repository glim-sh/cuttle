package serve

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testChildTable(t *testing.T) *childTable {
	t.Helper()
	table := newChildTable(context.Background(), t.TempDir(), 9222, func() (int, error) { return 19000, nil })
	table.command = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sleep", "60")
	}
	t.Cleanup(table.close)
	return table
}

func TestChildTableConcurrentSessions(t *testing.T) {
	table := testChildTable(t)
	var starts atomic.Int32
	table.command = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		starts.Add(1)
		return exec.CommandContext(ctx, "sleep", "60")
	}
	const n = 20
	children := make([]*seedChild, n)
	releases := make([]func(), n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			var err error
			children[i], releases[i], err = table.childFor(context.Background(), "alpha", "alpha")
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if starts.Load() != 1 {
		t.Fatalf("starts = %d", starts.Load())
	}
	for i := range n {
		if children[i] != children[0] {
			t.Fatal("different children")
		}
	}
	for _, release := range releases[:n-1] {
		release()
		release()
	}
	if !table.busy("alpha") {
		t.Fatal("live sessions not busy")
	}
	releases[n-1]()
	if table.busy("alpha") {
		t.Fatal("last release did not remove child")
	}
	_, release, err := table.childFor(context.Background(), "alpha", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if starts.Load() != 2 {
		t.Fatal("child not restarted after release")
	}
	if len(table.pruners) != 1 {
		t.Fatal("pruner not retained across children")
	}
}

func TestChildTableArguments(t *testing.T) {
	table := testChildTable(t)
	var commands []*exec.Cmd
	var arguments [][]string
	table.command = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name != "node" {
			t.Errorf("executable = %q", name)
		}
		arguments = append(arguments, slices.Clone(args))
		cmd := exec.CommandContext(ctx, "sleep", "60")
		commands = append(commands, cmd)
		return cmd
	}
	main, releaseMain, err := table.childFor(context.Background(), reservedSeed, "main")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseMain()
	other, releaseOther, err := table.childFor(context.Background(), "alpha", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseOther()
	if main.Host == other.Host || main.Host == "" {
		t.Fatal("hosts not independent")
	}
	for i, seed := range []string{reservedSeed, "alpha"} {
		port, endpoint, child := playwrightMCPPort, "http://127.0.0.1:9222", main
		if i == 1 {
			port, endpoint, child = 19000, endpoint+"/fingerprint/alpha", other
		}
		dir := filepath.Join(table.dataDir, seed, "Downloads")
		want := []string{playwrightMCPScript, "mcp", "--host", "127.0.0.1", "--port", strconv.Itoa(port), "--cdp-endpoint", endpoint, "--shared-browser-context", "--no-webmcp", "--output-dir", dir, "--allowed-hosts", child.Host}
		if !slices.Equal(arguments[i], want) || commands[i].Dir != dir {
			t.Fatalf("seed %s arguments or directory mismatch", seed)
		}
		if child.Endpoint != "http://127.0.0.1:"+strconv.Itoa(port)+"/mcp" {
			t.Fatal("wrong endpoint")
		}
	}
}

func TestChildTableWaitsForStop(t *testing.T) {
	table := testChildTable(t)
	stopping, allowStop := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	defer unblock.Do(func() { close(allowStop) })
	var starts atomic.Int32
	table.command = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, "sleep", "60")
		if starts.Add(1) == 1 {
			cmd.Cancel = func() error { close(stopping); <-allowStop; return cmd.Process.Kill() }
		}
		return cmd
	}
	_, release, err := table.childFor(context.Background(), "alpha", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() { release(); close(released) }()
	<-stopping
	if !table.busy("alpha") {
		t.Fatal("stopping child not busy")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, waitErr := table.childFor(ctx, "alpha", "alpha"); !errors.Is(waitErr, context.DeadlineExceeded) {
		t.Fatalf("waiting caller: %v", waitErr)
	}
	if starts.Load() != 1 {
		t.Fatal("started during stop")
	}
	unblock.Do(func() { close(allowStop) })
	<-released
	_, release, err = table.childFor(context.Background(), "alpha", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if starts.Load() != 2 {
		t.Fatal("no replacement after stop")
	}
}

func TestChildTableFailuresAndClose(t *testing.T) {
	table := testChildTable(t)
	table.allocPort = func() (int, error) { return 0, exec.ErrNotFound }
	if _, _, err := table.childFor(context.Background(), "alpha", "alpha"); err == nil {
		t.Fatal("allocation succeeded")
	}
	table.command = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "/nonexistent-cuttle-test-command")
	}
	if _, _, err := table.childFor(context.Background(), reservedSeed, "main"); err == nil {
		t.Fatal("start succeeded")
	}
	if table.busy(reservedSeed) || table.busy("alpha") {
		t.Fatal("failed start retained")
	}
	table.command = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sleep", "60")
	}
	_, release, err := table.childFor(context.Background(), reservedSeed, "main")
	if err != nil {
		t.Fatal(err)
	}
	table.close()
	release()
	if table.busy(reservedSeed) {
		t.Fatal("close retained child")
	}
	if _, _, err := table.childFor(context.Background(), reservedSeed, "main"); !errors.Is(err, context.Canceled) {
		t.Fatalf("closed table: %v", err)
	}
}

func TestChildTableRestartsExitedChild(t *testing.T) {
	table := testChildTable(t)
	commands := make(chan *exec.Cmd, 2)
	table.command = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, "sleep", "60")
		commands <- cmd
		return cmd
	}
	child, release, err := table.childFor(context.Background(), "alpha", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	first := <-commands
	if killErr := first.Process.Kill(); killErr != nil {
		t.Fatal(killErr)
	}
	select {
	case <-commands:
	case <-time.After(5 * time.Second):
		t.Fatal("child not restarted")
	}
	again, releaseAgain, err := table.childFor(context.Background(), "alpha", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseAgain()
	if child != again {
		t.Fatal("restart changed session endpoint")
	}
}

func TestChildTableDaemonCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	table := newChildTable(ctx, t.TempDir(), 9222, nil)
	table.command = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sleep", "60")
	}
	t.Cleanup(table.close)
	_, release, err := table.childFor(context.Background(), reservedSeed, "main")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	table.mu.Lock()
	done := table.entries[reservedSeed].done
	table.mu.Unlock()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("daemon cancellation did not stop child")
	}
	if _, _, err := table.childFor(context.Background(), reservedSeed, "main"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled daemon: %v", err)
	}
}
