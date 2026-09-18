package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/glim-sh/cuttle/internal/config"
)

// runContextAdd executes `context add` against a temp XDG config home and returns
// its stdout, the reloaded config, and any error.
func runContextAdd(t *testing.T, args ...string) (string, *config.Config, error) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cmd := newContextAddCmd()
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	cfg, lerr := config.Load()
	if lerr != nil {
		t.Fatalf("reload config: %v", lerr)
	}
	return out.String(), cfg, err
}

func TestContextAddNew(t *testing.T) {
	out, cfg, err := runContextAdd(t, "box", "--backend", "ssh", "--host", "user@box.example", "--proxy", "http://p:1")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if !strings.Contains(out, "added context \"box\"") {
		t.Fatalf("output: %q", out)
	}
	ctx := cfg.Contexts["box"]
	if ctx.Backend != config.BackendSSH || ctx.Host != "user@box.example" || ctx.Proxy != "http://p:1" {
		t.Fatalf("persisted context: %+v", ctx)
	}
	if cfg.DefaultContext != "" {
		t.Fatalf("default should be unset, got %q", cfg.DefaultContext)
	}
}

func TestContextAddUpdateExisting(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	// First add.
	first := newContextAddCmd()
	first.SetArgs([]string{"box", "--backend", "ssh", "--host", "old@host"})
	if err := first.Execute(); err != nil {
		t.Fatalf("first add: %v", err)
	}
	// Update the same name.
	second := newContextAddCmd()
	var out bytes.Buffer
	second.SetOut(&out)
	second.SetArgs([]string{"box", "--backend", "ssh", "--host", "new@host", "--default"})
	if err := second.Execute(); err != nil {
		t.Fatalf("update: %v", err)
	}
	if !strings.Contains(out.String(), "updated context \"box\"") {
		t.Fatalf("output: %q", out.String())
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if cfg.Contexts["box"].Host != "new@host" {
		t.Fatalf("host not updated: %+v", cfg.Contexts["box"])
	}
	if cfg.DefaultContext != "box" {
		t.Fatalf("--default not applied: %q", cfg.DefaultContext)
	}
}

func TestContextAddK8sDefaultsAndDirect(t *testing.T) {
	_, cfg, err := runContextAdd(t, "cluster", "--backend", "k8s")
	if err != nil {
		t.Fatalf("k8s add: %v", err)
	}
	// namespace/release omitted: persisted empty, backend applies its defaults.
	if c := cfg.Contexts["cluster"]; c.Backend != config.BackendK8s || c.Namespace != "" || c.Release != "" {
		t.Fatalf("k8s context: %+v", c)
	}

	_, cfg2, err := runContextAdd(t, "tailnet", "--backend", "direct", "--cdp-url", "http://cuttle.example:9222")
	if err != nil {
		t.Fatalf("direct add: %v", err)
	}
	if c := cfg2.Contexts["tailnet"]; c.CDPURL != "http://cuttle.example:9222" {
		t.Fatalf("direct context: %+v", c)
	}
}

func TestContextAddValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"reserved name local", []string{"local", "--backend", "ssh", "--host", "h"}},
		{"invalid backend", []string{"c", "--backend", "podman"}},
		{"ssh without host", []string{"c", "--backend", "ssh"}},
		{"direct without url", []string{"c", "--backend", "direct"}},
		{"ssh with k8s flag", []string{"c", "--backend", "ssh", "--host", "h", "--namespace", "x"}},
		{"k8s with host", []string{"c", "--backend", "k8s", "--host", "h"}},
		{"missing backend", []string{"c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := runContextAdd(t, tc.args...); err == nil {
				t.Fatalf("expected error for %v", tc.args)
			}
		})
	}
}

// The global --name reaches `context add` too, where it would read as setting the
// stanza's `name` yet save nothing - so it is refused, and nothing is written.
func TestContextAddRefusesName(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	withInstance(t, instanceFlags{})
	rootCmd.SetOut(&bytes.Buffer{})
	rootCmd.SetArgs([]string{"context", "add", "box", "--backend", "ssh", "--host", "h", "--name", "scraper"})
	if err := rootCmd.Execute(); !errors.Is(err, errAddWithName) {
		t.Fatalf("error = %v, want errAddWithName", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, ok := cfg.Contexts["box"]; ok {
		t.Fatalf("a refused add must not write the context: %+v", cfg.Contexts["box"])
	}
}

// TestDefaultImageNeverLatest locks the image contract: the CLI must never
// default to a floating :latest (which decoupled the CLI from its daemon and once
// resolved to an unrelated image). A release build pins to repo:<version>; a dev
// build uses the local-build tag `just build-image` produces.
func TestDefaultImageNeverLatest(t *testing.T) {
	img := defaultImage()
	if strings.HasSuffix(img, ":latest") {
		t.Fatalf("defaultImage() must never be :latest, got %q", img)
	}
	// A `go test` build carries no release ldflags, so the default is the local-
	// build tag; a release build pins to its version.
	if cliVersion() == devVersion {
		if img != localImageTag {
			t.Fatalf("dev build defaultImage() = %q, want %q", img, localImageTag)
		}
		return
	}
	if img != imageRepo+":"+cliVersion() {
		t.Fatalf("release build defaultImage() = %q, want %s:%s", img, imageRepo, cliVersion())
	}
}

// withInstance points the global instance selection at sel for one test. The
// flags live on the root command, which a test driving a single subcommand -
// or resolve directly - never goes through.
func withInstance(t *testing.T, sel instanceFlags) {
	t.Helper()
	prev := instance
	instance = sel
	t.Cleanup(func() { instance = prev })
}

func TestContainerNamePrecedence(t *testing.T) {
	pinned := config.Context{Backend: config.BackendSSH, Name: "from-config"}
	cases := []struct {
		name string
		ctx  config.Context
		flag string
		env  string
		want string
	}{
		{"flag wins over env and config", pinned, "from-flag", "from-env", "from-flag"},
		{"env wins over config", pinned, "", "from-env", "from-env"},
		{"config wins over the built-in default", pinned, "", "", "from-config"},
		{"built-in default when nothing selects", config.Context{Backend: config.BackendLocal}, "", "", defaultName},
		{"k8s is named by its context, ignoring all three", config.Context{Backend: config.BackendK8s}, "from-flag", "from-env", "cluster"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := containerName("cluster", tc.ctx, tc.flag, tc.env); got != tc.want {
				t.Fatalf("containerName = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestResolveInstanceSelection walks the same precedence through resolve, the
// one place it is decided, so the config file and the env var are exercised as
// the CLI actually reads them.
func TestResolveInstanceSelection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "cuttle"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := "default_context = \"box\"\n\n[context.box]\nbackend = \"local\"\nname = \"from-config\"\n\n[context.plain]\nbackend = \"local\"\n"
	if err := os.WriteFile(filepath.Join(home, "cuttle", "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cases := []struct {
		name        string
		sel         instanceFlags
		envContext  string
		envName     string
		wantCtx     string
		wantCtnName string
	}{
		{"config default_context and its pinned name", instanceFlags{}, "", "", "box", "from-config"},
		{"CUTTLE_CONTEXT selects the context", instanceFlags{}, "plain", "", "plain", defaultName},
		{"--context beats CUTTLE_CONTEXT", instanceFlags{contextName: "plain"}, "box", "", "plain", defaultName},
		{"CUTTLE_NAME beats the context's name", instanceFlags{}, "", "from-env", "box", "from-env"},
		{"--name beats CUTTLE_NAME", instanceFlags{name: "from-flag"}, "", "from-env", "box", "from-flag"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(config.EnvContext, tc.envContext)
			t.Setenv(config.EnvName, tc.envName)
			withInstance(t, tc.sel)
			name, ctxName, _, _, err := resolve(commonFlags{}, defaultImage())
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if ctxName != tc.wantCtx || name != tc.wantCtnName {
				t.Fatalf("resolve = context %q / container %q, want %q / %q", ctxName, name, tc.wantCtx, tc.wantCtnName)
			}
		})
	}
}

// TestInstanceFlagsAreGlobal drives the real command tree: --context/--name are
// root persistent flags, so every verb that reaches an instance honors them -
// `pw`, whose DisableFlagParsing means cobra hands it those flags unparsed,
// included. A context that cannot resolve fails in resolve, before any docker,
// ssh or network call, so seeing its name back proves the selection arrived.
func TestInstanceFlagsAreGlobal(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"pw", []string{"--context", "no-such-context", "pw", "snapshot"}},
		{"pw with --context=value", []string{"--context=no-such-context", "pw", "snapshot"}},
		{"pw with the flag after the subcommand", []string{"pw", "--context", "no-such-context", "snapshot"}},
		{"jev-browse", []string{"--context", "no-such-context", "jev-browse", "--mock", "a task"}},
		{"status", []string{"--context", "no-such-context", "status"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv(config.EnvContext, "")
			t.Setenv(config.EnvName, "")
			withInstance(t, instanceFlags{})
			var out bytes.Buffer
			rootCmd.SetOut(&out)
			rootCmd.SetErr(&out)
			rootCmd.SetArgs(tc.args)
			err := rootCmd.Execute()
			if err == nil || !strings.Contains(err.Error(), `unknown context "no-such-context"`) {
				t.Fatalf("error = %v, want the unknown-context error naming the selected context", err)
			}
		})
	}
}

// fakeInstanceDocker is a `docker` holding one container whose state lives in a
// file, so `start`/`run`/`stop`/`rm` change what the next inspect sees. `docker
// port` answers only while it runs, as the real one does; the configured
// bindings answer whenever the container exists.
const fakeInstanceDocker = `#!/bin/sh
echo "$*" >> "$FAKE_DOCKER_LOG"
state=$(cat "$FAKE_DOCKER_STATE")
case "$1" in
inspect)
	[ -n "$state" ] || exit 1
	case "$*" in
	*State.Status*) echo "$state" ;;
	*PortBindings*) echo "$FAKE_DOCKER_BINDINGS" ;;
	*) echo img:1 ;;
	esac ;;
port) [ "$state" = running ] || exit 1; printf '%s\n' "$FAKE_DOCKER_PORTS" ;;
exec) [ -z "$FAKE_DOCKER_NODRIVER" ] || echo cuttle-no-driver ;;
start|run) echo running > "$FAKE_DOCKER_STATE" ;;
stop) echo exited > "$FAKE_DOCKER_STATE" ;;
rm) : > "$FAKE_DOCKER_STATE" ;;
esac
`

// freePort returns a loopback port nothing listens on right now, for a port
// that must be free when `up` checks it and bound only later.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// daemonServer answers as a cuttle daemon and counts the requests it sees.
func daemonServer(hits *atomic.Int32) *http.Server {
	return &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/json/version":
			_, _ = io.WriteString(w, `{"Browser":"FakeChrome/1"}`)
		case "/secret":
			_, _ = io.WriteString(w, `{"secrets":[]}`)
		default:
			_, _ = io.WriteString(w, `{"active":1}`)
		}
	})}
}

// serveDaemon runs a daemonServer on a fresh loopback port for the test's
// lifetime, and returns the port and its request count.
func serveDaemon(t *testing.T) (int, *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int32
	srv := daemonServer(&hits)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().(*net.TCPAddr).Port, &hits
}

// serveDaemonAfterStart brings a daemon up on port once the fake docker logs a
// `docker start`: before it, `up` requires the stopped container's ports free.
func serveDaemonAfterStart(t *testing.T, port int, log string) {
	t.Helper()
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			if raw, _ := os.ReadFile(log); strings.Contains(string(raw), "start fs-x") {
				break
			}
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			return // `up` then reports CDP never came up, which fails the test
		}
		srv := daemonServer(new(atomic.Int32))
		go func() { _ = srv.Serve(ln) }()
		<-stop
		_ = srv.Close()
	}()
	t.Cleanup(func() { close(stop); <-done })
}

type fakeInstance struct {
	state      string // docker state: "running", "exited", or "" for no container
	cdp, vnc   int    // the container's bindings
	unreadable bool   // the container publishes no readable CDP/VNC ports
}

// runFakeInstance runs the real command tree against fakeInstanceDocker holding
// the container "fs-x", and returns the output, the docker log and the error.
// serve, when set, runs against the docker log path before the command does.
func runFakeInstance(t *testing.T, fi fakeInstance, serve func(log string), args ...string) (string, string, error) {
	t.Helper()
	bin, dir := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(fakeInstanceDocker), 0o755); err != nil {
		t.Fatal(err)
	}
	log, state := filepath.Join(dir, "docker.log"), filepath.Join(dir, "state")
	if err := os.WriteFile(state, []byte(fi.state+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(log, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	bindings := fmt.Sprintf(`{"6080/tcp":[{"HostIp":"127.0.0.1","HostPort":"%d"}],"9222/tcp":[{"HostIp":"127.0.0.1","HostPort":"%d"}]}`, fi.vnc, fi.cdp)
	ports := fmt.Sprintf("6080/tcp -> 127.0.0.1:%d\n9222/tcp -> 127.0.0.1:%d", fi.vnc, fi.cdp)
	if fi.unreadable {
		bindings, ports = "{}", ""
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_DOCKER_LOG", log)
	t.Setenv("FAKE_DOCKER_STATE", state)
	t.Setenv("FAKE_DOCKER_BINDINGS", bindings)
	t.Setenv("FAKE_DOCKER_PORTS", ports)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(config.EnvContext, "")
	t.Setenv(config.EnvName, "")
	withInstance(t, instanceFlags{})
	if serve != nil {
		serve(log)
	}
	t.Cleanup(func() { resetChangedFlags(rootCmd) })
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs(append([]string{"--name", "fs-x"}, args...))
	err := rootCmd.Execute()
	raw, rerr := os.ReadFile(log)
	if rerr != nil {
		t.Fatal(rerr)
	}
	return out.String(), string(raw), err
}

// resetChangedFlags undoes the flags an Execute set: the verbs are built once,
// so a --recreate would otherwise leak into the next test's `up`.
func resetChangedFlags(cmd *cobra.Command) {
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		if f.Changed {
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		}
	})
	for _, c := range cmd.Commands() {
		resetChangedFlags(c)
	}
}

// A stopped or absent selected instance must be refused by name, with the
// command that resumes it - never served by whatever answers on its ports or on
// the default ones.
func TestVerbsRefuseAStoppedSelectedInstance(t *testing.T) {
	const stopped = "container 'fs-x': stopped - run `cuttle --name fs-x up` first"
	for _, tc := range []struct {
		name    string
		state   string
		args    []string
		wantErr string
	}{
		{name: "status stopped", state: "exited", args: []string{"status"}, wantErr: stopped},
		{name: "open stopped", state: "exited", args: []string{"open", "--no-open"}, wantErr: stopped},
		{name: "secret ls stopped", state: "exited", args: []string{"secret", "ls"}, wantErr: stopped},
		{name: "downloads stopped", state: "exited", args: []string{"downloads"}, wantErr: stopped},
		{name: "open absent", state: "", args: []string{"open", "--no-open"}, wantErr: "container 'fs-x': absent - run `cuttle --name fs-x up` first"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cdp, hits := serveDaemon(t) // the instance's own port answering proves nothing was probed
			_, _, err := runFakeInstance(t, fakeInstance{state: tc.state, cdp: cdp, vnc: freePort(t)}, nil, tc.args...)
			if err == nil || err.Error() != tc.wantErr {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			if n := hits.Load(); n != 0 {
				t.Fatalf("the endpoint saw %d requests for a stopped instance", n)
			}
		})
	}
}

// `up` on a stopped instance created with its own ports restarts it on those
// ports, and reports them - not the defaults, which another instance may hold,
// nor a --cdp-port that a restart cannot apply.
func TestUpRestartsAStoppedInstanceOnItsOwnPorts(t *testing.T) {
	for _, args := range [][]string{{"up"}, {"up", "--cdp-port", "9"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			cdp, vnc := freePort(t), freePort(t)
			out, log, err := runFakeInstance(t, fakeInstance{state: "exited", cdp: cdp, vnc: vnc},
				func(log string) { serveDaemonAfterStart(t, cdp, log) }, args...)
			if err != nil {
				t.Fatalf("up: %v\n%s", err, out)
			}
			if !strings.Contains(log, "start fs-x") || strings.Contains(log, "run ") {
				t.Fatalf("want a plain docker start, got:\n%s", log)
			}
			if want := "127.0.0.1:" + strconv.Itoa(cdp); !strings.Contains(out, "restarted") || !strings.Contains(out, want) {
				t.Fatalf("briefing lacks restarted/%s:\n%s", want, out)
			}
		})
	}
}

// An idempotent `up` reports a running instance's real ports, and `up
// --recreate` rebuilds it on them.
func TestUpKeepsARunningInstancesPorts(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		wantVerb string
	}{
		{name: "plain", args: []string{"up"}, wantVerb: "already running"},
		{name: "recreate", args: []string{"up", "--recreate"}, wantVerb: "recreated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cdp, _ := serveDaemon(t)
			out, log, err := runFakeInstance(t, fakeInstance{state: "running", cdp: cdp, vnc: freePort(t)}, nil, tc.args...)
			if err != nil {
				t.Fatalf("%v: %v\n%s", tc.args, err, out)
			}
			if !strings.Contains(out, tc.wantVerb) || !strings.Contains(out, "127.0.0.1:"+strconv.Itoa(cdp)) {
				t.Fatalf("briefing lacks %q on port %d:\n%s", tc.wantVerb, cdp, out)
			}
			if tc.wantVerb == "recreated" && !strings.Contains(log, fmt.Sprintf("-p 127.0.0.1:%d:9222", cdp)) {
				t.Fatalf("recreate did not rerun on port %d:\n%s", cdp, log)
			}
		})
	}
}

// When an existing container's ports cannot be read, a rebuild must fail before
// it tears the container or its profile down, not after, on a default port it
// never had. `down` needs no ports and still stops it.
func TestUnreadablePortsFailBeforeTeardown(t *testing.T) {
	for _, args := range [][]string{{"up", "--recreate"}, {"up", "--purge-profile"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, log, err := runFakeInstance(t, fakeInstance{state: "running", unreadable: true}, nil, args...)
			if err == nil || !strings.Contains(err.Error(), "cannot read its CDP/VNC ports") {
				t.Fatalf("err = %v, want the unreadable-ports refusal", err)
			}
			for line := range strings.Lines(log) {
				if f := strings.Fields(line); f[0] == "stop" || f[0] == "rm" || f[0] == "run" || f[0] == "volume" {
					t.Fatalf("docker %q ran before the refusal:\n%s", strings.TrimSpace(line), log)
				}
			}
		})
	}
	t.Run("down", func(t *testing.T) {
		_, log, err := runFakeInstance(t, fakeInstance{state: "running", unreadable: true}, nil, "down")
		if err != nil || !strings.Contains(log, "stop -t") {
			t.Fatalf("down: err=%v, docker log:\n%s", err, log)
		}
	})
}

// `up` refuses ports docker would publish somewhere other than asked, before any
// container or volume exists: 0 is a random port the CLI never polls, and one
// port for both fails the run only after the volume was made.
func TestUpRejectsBadPortsBeforeDocker(t *testing.T) {
	for _, args := range [][]string{{"up", "--cdp-port", "0"}, {"up", "--cdp-port", "9300", "--vnc-port", "9300"}, {"up", "--vnc-port", "70000"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, log, err := runFakeInstance(t, fakeInstance{}, nil, args...)
			if !errors.Is(err, errBadPorts) {
				t.Fatalf("err = %v, want errBadPorts", err)
			}
			if log != "" {
				t.Fatalf("docker ran before the refusal:\n%s", log)
			}
		})
	}
}

// A name docker would reject is refused before any docker call, so no hint ever
// prints it unquoted.
func TestInvalidInstanceNameIsRefused(t *testing.T) {
	_, log, err := runFakeInstance(t, fakeInstance{}, nil, "--name", "bad name", "status")
	if !errors.Is(err, errInvalidName) || !strings.Contains(err.Error(), `"bad name"`) {
		t.Fatalf("err = %v, want errInvalidName quoting the name", err)
	}
	if log != "" {
		t.Fatalf("docker ran for an invalid name:\n%s", log)
	}
}

// A repeated `down --purge` finds no container, and must not claim to remove one.
func TestDownPurgeOnAnAbsentInstance(t *testing.T) {
	out, _, err := runFakeInstance(t, fakeInstance{}, nil, "down", "--purge")
	if err != nil || !strings.Contains(out, "was already gone") || strings.Contains(out, "removed") {
		t.Fatalf("down --purge on nothing: err=%v out=%q", err, out)
	}
}

// `up --recreate` without --image moves the container to this CLI's image - the
// upgrade path - and says so rather than switching silently.
func TestUpRecreateNamesAnImageChange(t *testing.T) {
	cdp, _ := serveDaemon(t)
	out, _, err := runFakeInstance(t, fakeInstance{state: "running", cdp: cdp, vnc: freePort(t)}, nil, "up", "--recreate")
	if want := "image changed img:1 -> " + defaultImage(); err != nil || !strings.Contains(out, want) {
		t.Fatalf("err=%v; output lacks %q:\n%s", err, want, out)
	}
}

// Against an image that predates the bundled driver, the briefing must not
// advertise `pw` or the loop, and must name the upgrade instead.
func TestBriefingForAnImageWithoutTheDriver(t *testing.T) {
	t.Setenv("FAKE_DOCKER_NODRIVER", "1")
	cdp, _ := serveDaemon(t)
	out, _, err := runFakeInstance(t, fakeInstance{state: "running", cdp: cdp, vnc: freePort(t)}, nil, "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	if !strings.Contains(out, "predates the bundled") || !strings.Contains(out, "`cuttle --name fs-x up --recreate`") {
		t.Fatalf("briefing lacks the upgrade hint:\n%s", out)
	}
	for _, advert := range []string{" pw <command>", "jev-browse", "dialog-accept"} {
		if strings.Contains(out, advert) {
			t.Fatalf("briefing still advertises %q:\n%s", advert, out)
		}
	}
}

// cuttle does not manage a direct context's browser, so its not-running error
// must not tell anyone to run `up`, which the direct backend refuses.
func TestDirectNotRunningHint(t *testing.T) {
	dir := t.TempDir()
	cfg := fmt.Sprintf("[context.d]\nbackend = \"direct\"\ncdp_url = \"http://127.0.0.1:%d\"\n", freePort(t))
	if err := os.MkdirAll(filepath.Join(dir, "cuttle"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cuttle", "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv(config.EnvContext, "")
	t.Setenv(config.EnvName, "")
	withInstance(t, instanceFlags{})
	t.Cleanup(func() { resetChangedFlags(rootCmd) })
	for _, args := range [][]string{{"status"}, {"open", "--no-open"}, {"pw", "snapshot"}} {
		var out bytes.Buffer
		rootCmd.SetOut(&out)
		rootCmd.SetErr(&out)
		rootCmd.SetArgs(append([]string{"--context", "d"}, args...))
		err := rootCmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "start it yourself") || strings.Contains(err.Error(), " up") {
			t.Fatalf("%v: err = %v, want the direct backend's hint", args, err)
		}
	}
}
