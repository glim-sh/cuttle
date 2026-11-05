package serve

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"
)

const mcpChildHost = "127.0.0.1"

type seedChild struct {
	Endpoint string
	Host     string
}

type childEntry struct {
	child  seedChild
	refs   int
	cancel context.CancelFunc
	done   chan struct{}
}

type childTable struct {
	mu         sync.Mutex
	ctx        context.Context
	cancel     context.CancelFunc
	dataDir    string
	daemonPort int
	allocPort  func() (int, error)
	entries    map[string]*childEntry
	pruners    map[string]bool
	closed     bool
	command    func(context.Context, string, ...string) *exec.Cmd
}

func newChildTable(ctx context.Context, dataDir string, daemonPort int, allocPort func() (int, error)) *childTable {
	ctx, cancel := context.WithCancel(ctx)
	t := &childTable{
		ctx: ctx, cancel: cancel, dataDir: dataDir, daemonPort: daemonPort,
		allocPort: allocPort, entries: make(map[string]*childEntry), pruners: make(map[string]bool), command: exec.CommandContext,
	}
	go func() { <-ctx.Done(); t.close() }()
	return t
}

func (t *childTable) childFor(ctx context.Context, seedKey, seedName string) (*seedChild, func(), error) {
	for {
		t.mu.Lock()
		if err := ctx.Err(); err != nil {
			t.mu.Unlock()
			return nil, nil, fmt.Errorf("get MCP child: %w", err)
		}
		if t.closed || t.ctx.Err() != nil {
			t.mu.Unlock()
			return nil, nil, context.Canceled
		}
		e := t.entries[seedKey]
		if e != nil && e.refs == 0 {
			t.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, nil, fmt.Errorf("wait for MCP child: %w", ctx.Err())
			case <-e.done:
			}
			continue
		}
		if e == nil {
			var err error
			e, err = t.start(seedKey, seedName)
			if err != nil {
				t.mu.Unlock()
				return nil, nil, err
			}
			t.entries[seedKey] = e
		}
		e.refs++
		t.mu.Unlock()
		var once sync.Once
		return &e.child, func() { once.Do(func() { t.release(e) }) }, nil
	}
}

// start holds mu so first callers cannot create competing children or pruners.
func (t *childTable) start(seedKey, seedName string) (*childEntry, error) {
	port := playwrightMCPPort
	endpoint := "http://127.0.0.1:" + strconv.Itoa(t.daemonPort)
	if seedKey != reservedSeed {
		var err error
		port, err = t.allocPort()
		if err != nil {
			return nil, fmt.Errorf("allocate MCP port: %w", err)
		}
		endpoint += "/fingerprint/" + seedName
	}
	dir := seedDownloadsDir(t.dataDir, seedKey)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create MCP downloads: %w", err)
	}
	ctx, cancel := context.WithCancel(t.ctx)
	e := &childEntry{child: seedChild{Endpoint: "http://127.0.0.1:" + strconv.Itoa(port) + "/mcp", Host: "cuttle-" + newLeaseToken()}, cancel: cancel, done: make(chan struct{})}
	args := []string{
		playwrightMCPScript, "mcp", "--host", mcpChildHost, "--port", strconv.Itoa(port),
		"--cdp-endpoint", endpoint, "--shared-browser-context", "--no-webmcp", "--output-dir", dir, "--allowed-hosts", e.child.Host,
	}
	command := func() *exec.Cmd {
		cmd := t.command(ctx, "node", args...)
		cmd.Dir = dir
		out := &lineLogger{prefix: "playwright-mcp: "}
		cmd.Stdout, cmd.Stderr = out, out
		cmd.WaitDelay = 5 * time.Second
		return cmd
	}
	cmd := command()
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start MCP child: %w", err)
	}
	if !t.pruners[seedKey] {
		t.pruners[seedKey] = true
		go pruneArtifacts(t.ctx, dir)
	}
	go func() {
		defer func() {
			t.mu.Lock()
			delete(t.entries, seedKey)
			close(e.done)
			t.mu.Unlock()
		}()
		backoff := time.Second
		for {
			started := time.Now()
			err := cmd.Wait()
			if ctx.Err() != nil {
				return
			}
			if time.Since(started) > time.Minute {
				backoff = time.Second
			}
			logWarn("remote: Playwright MCP child exited (%v); restarting in %s", err, backoff)
			for {
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}
				cmd = command()
				err = cmd.Start()
				backoff = min(backoff*2, launchBackoffMax)
				if err == nil {
					break
				}
				logWarn("remote: restart Playwright MCP child: %v", err)
			}
		}
	}()
	return e, nil
}

func (t *childTable) release(e *childEntry) {
	t.mu.Lock()
	e.refs--
	last := e.refs == 0
	if last {
		e.cancel()
	}
	t.mu.Unlock()
	if !last {
		return
	}
	<-e.done
}

func (t *childTable) busy(seedKey string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.entries[seedKey] != nil
}

func (t *childTable) close() {
	t.mu.Lock()
	t.closed = true
	t.cancel()
	entries := make([]*childEntry, 0, len(t.entries))
	for _, e := range t.entries {
		e.cancel()
		entries = append(entries, e)
	}
	t.mu.Unlock()
	for _, e := range entries {
		<-e.done
	}
	t.mu.Lock()
	clear(t.entries)
	t.mu.Unlock()
}
