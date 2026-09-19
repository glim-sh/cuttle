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
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestPlaywrightArgv(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want []string
		err  error
	}{
		{
			name: "attach gets the in-container endpoint",
			args: []string{"attach"},
			want: []string{"playwright-cli", "attach", "--cdp=http://127.0.0.1:9222"},
		},
		{
			name: "an explicit --cdp is left alone",
			args: []string{"attach", "--cdp=http://127.0.0.1:9333"},
			want: []string{"playwright-cli", "attach", "--cdp=http://127.0.0.1:9333"},
		},
		{
			name: "--cdp as a separate token counts too",
			args: []string{"attach", "--cdp", "http://127.0.0.1:9333"},
			want: []string{"playwright-cli", "attach", "--cdp", "http://127.0.0.1:9333"},
		},
		{
			name: "other verbs pass through untouched",
			args: []string{"screenshot", "--filename=shot.png"},
			want: []string{"playwright-cli", "screenshot", "--filename=shot.png"},
		},
		{
			name: "--cdp is only injected for attach",
			args: []string{"navigate", "https://example.com"},
			want: []string{"playwright-cli", "navigate", "https://example.com"},
		},
		// The image's CDP endpoint makes `open` attach too, so it passes through.
		{
			name: "open passes through",
			args: []string{"open", "https://example.com"},
			want: []string{"playwright-cli", "open", "https://example.com"},
		},
		{name: "--endpoint redirects the driver", args: []string{"attach", "--endpoint=ws://elsewhere"}, err: errPlaywrightRedirect},
		{name: "--endpoint as a bare flag", args: []string{"attach", "--endpoint", "ws://elsewhere"}, err: errPlaywrightRedirect},
		{name: "--extension redirects the driver", args: []string{"attach", "--extension"}, err: errPlaywrightRedirect},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := playwrightArgv(tt.args)
			if tt.err != nil {
				if !errors.Is(err, tt.err) {
					t.Fatalf("err=%v want %v", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("argv=%v want %v", got, tt.want)
			}
		})
	}
}

func TestPlaywrightNeedsAttach(t *testing.T) {
	t.Parallel()
	const notOpen = "Error: The browser 'cuttle' is not open, please run open first"
	const browserDied = "The browser 'cuttle' is not open, please run open first\n\n  playwright-cli -s=cuttle open [params]\n"
	const staleSession = "Error: Browser 'cuttle' is not open. Run\n\n  playwright-cli -s=cuttle open\n\nto start the browser session.\n    at Session.run (session.js:60:13)"
	tests := []struct {
		name     string
		args     []string
		combined string
		want     bool
	}{
		{name: "a verb with no session", args: []string{"snapshot"}, combined: notOpen, want: true},
		// Observed, not assumed: kill /opt/browser/chrome under a live session and
		// the daemon exits with it, so the next verb reports exactly this - stdout,
		// exit 1, no distinct "browser closed" wording to match. Re-attach is the
		// right answer because cuttle has a replacement browser up by then.
		{name: "a browser killed mid-session reports the same marker", args: []string{"snapshot"}, combined: browserDied, want: true},
		// A session file that outlived its daemon (`docker kill`, the driver process
		// killed) gets the other wording, as an uncaught Node error on stderr.
		{name: "a stale session file after the daemon died", args: []string{"snapshot"}, combined: staleSession, want: true},
		{name: "the marker on stdout counts", args: []string{"goto", "https://example.com"}, combined: notOpen, want: true},
		{name: "any other failure is real", args: []string{"click", "e17"}, combined: "Error: no element e17", want: false},
		{name: "attach failing is real", args: []string{"attach"}, combined: notOpen, want: false},
		{name: "open failing is real", args: []string{"open", "https://example.com"}, combined: notOpen, want: false},
		{name: "attach failing on a stale session is real", args: []string{"attach"}, combined: staleSession, want: false},
		{name: "no args", args: nil, combined: notOpen, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := playwrightNeedsAttach(tt.args, tt.combined); got != tt.want {
				t.Fatalf("playwrightNeedsAttach=%v want %v", got, tt.want)
			}
		})
	}
}

func TestExitCodeErrorCarriesCode(t *testing.T) {
	t.Parallel()
	var err error = &ExitCodeError{Code: 42}
	ec, ok := errors.AsType[*ExitCodeError](err)
	if !ok || ec.Code != 42 {
		t.Fatalf("AsType gave %v %v", ec, ok)
	}
}

// `cuttle pw` parses its own --context/--name/--takeover because
// DisableFlagParsing means cobra parses nothing for it, root's persistent flags
// included.
func TestSplitCuttleFlags(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		args         []string
		wantSel      instanceFlags
		wantTakeover bool
		wantArgs     []string
		wantErr      bool
	}{
		{name: "no cuttle flags", args: []string{"snapshot"}, wantArgs: []string{"snapshot"}},
		{
			name:    "separate value",
			args:    []string{"--name", "scraper", "snapshot"},
			wantSel: instanceFlags{name: "scraper"}, wantArgs: []string{"snapshot"},
		},
		{
			name:    "--flag=value",
			args:    []string{"--context=box", "--name=scraper", "goto", "https://example.com"},
			wantSel: instanceFlags{contextName: "box", name: "scraper"}, wantArgs: []string{"goto", "https://example.com"},
		},
		{
			name:    "only the leading run is cuttle's",
			args:    []string{"--name", "scraper", "click", "--name", "ref"},
			wantSel: instanceFlags{name: "scraper"}, wantArgs: []string{"click", "--name", "ref"},
		},
		{name: "a driver flag ends the run", args: []string{"snapshot", "--name", "x"}, wantArgs: []string{"snapshot", "--name", "x"}},
		{name: "missing value", args: []string{"--name"}, wantErr: true},
		// An empty selector would pick the default instance, not refuse.
		{name: "empty =value", args: []string{"--takeover", "--name=", "click", "e1"}, wantErr: true},
		{name: "empty separate value", args: []string{"--context", "", "snapshot"}, wantErr: true},
		{name: "takeover with a value", args: []string{"--takeover=true", "--name", "x", "click", "e5"}, wantErr: true},
		{name: "no verb left", args: []string{"--name", "scraper"}, wantSel: instanceFlags{name: "scraper"}, wantArgs: []string{}},
		{
			name:    "takeover after the instance flags",
			args:    []string{"--name", "scraper", "--takeover", "press", "Tab"},
			wantSel: instanceFlags{name: "scraper"}, wantTakeover: true, wantArgs: []string{"press", "Tab"},
		},
		// The stress-run shape: the instance flag after --takeover once went to the
		// driver while the lease was taken from the default instance.
		{
			name:    "takeover before the instance flags",
			args:    []string{"--takeover", "--name", "scraper", "--context=box", "press", "Tab"},
			wantSel: instanceFlags{contextName: "box", name: "scraper"}, wantTakeover: true, wantArgs: []string{"press", "Tab"},
		},
		{name: "takeover after the verb is the driver's", args: []string{"press", "--takeover"}, wantArgs: []string{"press", "--takeover"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sel, takeover, args, err := splitCuttleFlags(instanceFlags{}, tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("args %q: want an error, got sel %+v args %q", tc.args, sel, args)
				}
				return
			}
			if err != nil {
				t.Fatalf("args %q: %v", tc.args, err)
			}
			if sel != tc.wantSel || takeover != tc.wantTakeover {
				t.Fatalf("selection = %+v takeover=%v, want %+v takeover=%v", sel, takeover, tc.wantSel, tc.wantTakeover)
			}
			if !slices.Equal(args, tc.wantArgs) {
				t.Fatalf("passthrough = %q, want %q", args, tc.wantArgs)
			}
		})
	}
}

// echoExecer runs the argv it is handed through echo, so a test sees exactly
// what would be execed in the container.
type echoExecer struct{}

func (echoExecer) ExecCommand(_ string, argv []string) (string, []string) { return "echo", argv }

// `cuttle pw --help` is what an agent reaches for first, so it must end with the
// driver's own verb list rather than only the wrapper's.
func TestWriteDriverHelpRunsTheBundledDriversHelp(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	writeDriverHelp(context.Background(), echoExecer{}, &out)
	for _, want := range []string{"cuttle pw <verb>", "cuttle pw --help <verb>", "playwright-cli --help\n"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("driver help missing %q:\n%s", want, out.String())
		}
	}
}

type failExecer struct{}

func (failExecer) ExecCommand(string, []string) (string, []string) { return "false", nil }

// An image that predates the bundled driver fails the exec; --help still
// succeeds and says why the driver's half is missing.
func TestWriteDriverHelpWithoutTheDriver(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	writeDriverHelp(context.Background(), failExecer{}, &out)
	if !strings.Contains(out.String(), "is not available") || strings.Contains(out.String(), "--- the bundled driver") {
		t.Errorf("driver help failure not reported as a note:\n%s", out.String())
	}
}

// sessionlessDriver stands in for a driver with no live session: every verb
// fails with the given wording until `attach` has run once.
type sessionlessDriver struct{ dir string }

func (d sessionlessDriver) ExecCommand(_ string, argv []string) (string, []string) {
	const script = `cd "$0" || exit 2
echo "$*" >> calls
case "$*" in *" attach --cdp="*) touch attached; exit 0 ;; esac
if [ -e attached ]; then echo "ran $2"; exit 0; fi
cat marker >&2; exit 1`
	return "sh", append([]string{"-c", script, d.dir}, argv...)
}

// The jev-browse runner re-attaches on both "not open" wordings and retries the
// verb once, so a stale session left by a restart never reaches the loop.
func TestPlaywrightRunnerReattaches(t *testing.T) {
	t.Parallel()
	for name, marker := range map[string]string{
		"no session file": "The browser 'cuttle' is not open, please run open first\n",
		"stale session":   "Error: Browser 'cuttle' is not open. Run\n\n  playwright-cli -s=cuttle open\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "marker"), []byte(marker), 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := newPlaywrightRunner(sessionlessDriver{dir})(context.Background(), "snapshot")
			if err != nil || out != "ran snapshot\n" {
				t.Fatalf("runner = %q, %v; want the retried verb's output", out, err)
			}
			calls, err := os.ReadFile(filepath.Join(dir, "calls"))
			if err != nil {
				t.Fatal(err)
			}
			attach := strings.Join(playwrightAttachArgv(), " ")
			if !strings.Contains(attach, "playwright-cli attach --cdp=http://127.0.0.1:9222") {
				t.Fatalf("auto-attach %q does not attach to the container's CDP endpoint", attach)
			}
			want := "playwright-cli snapshot\n" + attach + "\nplaywright-cli snapshot\n"
			if string(calls) != want {
				t.Fatalf("driver calls:\n%s\nwant:\n%s", calls, want)
			}
		})
	}
}

// With no instance to exec into, the wrapper help still prints and says why the
// driver's own help is missing, instead of failing the --help.
func TestPlaywrightHelpWithoutAnInstance(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	withInstance(t, instanceFlags{contextName: "no-such-context"})
	cmd := newPlaywrightCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())
	if err := playwrightHelp(cmd); err != nil {
		t.Fatalf("playwrightHelp: %v", err)
	}
	for _, want := range []string{"cuttle pw --help <verb>", "is not available: "} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("help missing %q:\n%s", want, out.String())
		}
	}
}

// noDriverExecer stands in for an image with or without the bundled driver:
// the probe's shell runs for real, with PATH narrowed to what the image has.
type noDriverExecer struct{ path string }

func (e noDriverExecer) ExecCommand(_ string, argv []string) (string, []string) {
	return "env", append([]string{"PATH=" + e.path, "/bin/sh"}, argv[1:]...) // argv is ["sh", "-c", probe]
}

func TestBundledDriverAbsent(t *testing.T) {
	t.Parallel()
	withDriver := t.TempDir()
	if err := os.WriteFile(filepath.Join(withDriver, driverPlaywright), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if bundledDriverAbsent(context.Background(), noDriverExecer{path: withDriver}) {
		t.Error("an image with the driver on PATH reported it absent")
	}
	if !bundledDriverAbsent(context.Background(), noDriverExecer{path: t.TempDir()}) {
		t.Error("an image without the driver reported it present")
	}
	// An exec that fails outright says nothing about the image.
	if bundledDriverAbsent(context.Background(), failExecer{}) {
		t.Error("a failed exec was read as a missing driver")
	}
}

// The exec of a driver the image does not have fails 127 with docker's (or the
// shell's) "not found" - and only that reads as a missing driver.
func TestDriverMissing(t *testing.T) {
	t.Parallel()
	notFound := exec.Command("sh", "-c", "exit 127").Run()
	other := exec.Command("sh", "-c", "exit 1").Run()
	const dockerSays = `OCI runtime exec failed: exec failed: unable to start container process: exec: "playwright-cli": executable file not found in $PATH: unknown`
	if !driverMissing(notFound, dockerSays) {
		t.Error("docker's not-found was not recognized")
	}
	if !driverMissing(notFound, "sh: 1: exec: playwright-cli: not found") {
		t.Error("the k8s shell's not-found was not recognized")
	}
	if driverMissing(other, dockerSays) || driverMissing(notFound, "Error: element not found") || driverMissing(nil, dockerSays) {
		t.Error("a driver failure was read as a missing driver")
	}
}

var errSnapshotFailed = errors.New("exit status 1")

// A snapshot with a focused dialog yields one line naming it and a button that
// only dismisses it; an unfocused dialog, no dialog, or a failed snapshot yields
// nothing.
func TestDialogHint(t *testing.T) {
	t.Parallel()
	const head = "### Snapshot\n```yaml\n"
	const modal = head + `- generic [ref=e1]:
  - heading "Behind the modal" [level=1] [ref=e2]
  - button "Apply now" [ref=e3]
  - dialog [ref=e4]:
    - heading "Sign in to continue" [level=2] [ref=e5]
    - paragraph [ref=e6]: Join to see more.
    - button "Cancel subscription" [ref=e7]
    - button "Dismiss" [active] [ref=e8]: X
  - button "Close chat" [ref=e9]
` + "```\n"
	fake := func(out string, err error) playwrightRunner {
		return func(_ context.Context, args ...string) (string, error) {
			if len(args) != 1 || args[0] != "snapshot" {
				t.Errorf("hint ran %q, want a single snapshot", args)
			}
			return out, err
		}
	}
	for name, tt := range map[string]struct {
		out  string
		err  error
		want string
	}{
		"modal with close button": {modal, nil, "cuttle: an open dialog covers the page: Sign in to continue - dismiss it first (e.g. `cuttle pw click e8`)"},
		"quoted name, write-shaped buttons only": {
			head + "- 'alertdialog \"Step 1: Close account\" [ref=e2]':\n  - button \"Close account\" [active] [ref=e3]\n  - button \"Cancel\" [ref=e4]\n", nil,
			"cuttle: an open dialog covers the page: Step 1: Close account - dismiss it first (e.g. `cuttle pw press Escape`)",
		},
		"quoted key with escaped quotes of both kinds": {
			head + "- 'dialog \"Don''t miss: \\\"Step 1\\\"\" [ref=e2]':\n  - textbox \"Email\" [active] [ref=e3]\n  - button \"No, thanks\" [ref=e4]\n", nil,
			"cuttle: an open dialog covers the page: Don't miss: \"Step 1\" - dismiss it first (e.g. `cuttle pw click e4`)",
		},
		"last focused dialog wins": {
			head + "- dialog \"Outer\" [ref=e1]:\n  - button \"Close\" [ref=e2]\n  - dialog \"Inner\" [ref=e3]:\n    - button \"×\" [active] [ref=e4]\n", nil,
			"cuttle: an open dialog covers the page: Inner - dismiss it first (e.g. `cuttle pw click e4`)",
		},
		"only the last snapshot section counts": {
			head + "- dialog \"Gone\" [ref=e1]:\n  - button \"Close\" [active] [ref=e2]\n### Snapshot\n- button \"Go\" [active] [ref=e3]\n", nil, "",
		},
		"dialog without focus":  {head + "- dialog \"Cookies\" [ref=e2]:\n  - button \"Close\" [ref=e3]\n- button \"Go\" [active] [ref=e4]\n", nil, ""},
		"no dialog":             {head + "- button \"Go\" [active] [ref=e4]\n", nil, ""},
		"snapshot itself fails": {modal, errSnapshotFailed, ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := dialogHint(context.Background(), fake(tt.out, tt.err), "cuttle"); got != tt.want {
				t.Errorf("hint = %q\nwant  %q", got, tt.want)
			}
		})
	}
}

func TestPlaywrightPointerIntercepted(t *testing.T) {
	t.Parallel()
	const intercepted = "### Error\nTimeoutError: Timeout 5000ms exceeded.\n  - <dialog open> intercepts pointer events\n"
	for _, tt := range []struct {
		args     []string
		combined string
		want     bool
	}{
		{[]string{"click", "e3"}, intercepted, true},
		{[]string{"--raw", "hover", "e3"}, intercepted, true},
		{[]string{"click", "e3"}, "### Error\nTimeoutError: Timeout 5000ms exceeded.\n  - element is not enabled\n", false},
		{[]string{"-s=other", "click", "e3"}, intercepted, false},
		{[]string{"--session", "other", "click", "e3"}, intercepted, false},
	} {
		if got := playwrightPointerIntercepted(tt.args, tt.combined); got != tt.want {
			t.Errorf("playwrightPointerIntercepted(%q) = %v, want %v", tt.args, got, tt.want)
		}
	}
}

// A driving verb's exec also fetches the snapshot it linked, through the
// daemon, behind a marker; the verb's output and exit status are untouched, and
// a refused fetch appends nothing.
func TestSnapshotArgv(t *testing.T) {
	t.Parallel()
	const link = "- [Snapshot](.playwright-cli/page-2026-09-19T10-27-05-037Z.yml)"
	stub := &leaseStub{reply: func(r *http.Request) (int, string) {
		if r.URL.Path != "/snapshot" || r.URL.Query().Get("file") != ".playwright-cli/page-2026-09-19T10-27-05-037Z.yml" {
			return http.StatusBadRequest, "{}"
		}
		return http.StatusOK, "- textbox: {{cuttle:T}}\n"
	}}
	ex := stub.start(t)
	run := func(ex hostCurl, script string) (string, error) {
		var out bytes.Buffer
		err := execIn(context.Background(), nil, ex, "/", snapshotArgv([]string{"sh", "-c", script}), &out, io.Discard)
		return out.String(), err
	}

	got, err := run(ex, "echo '### Snapshot'; echo '"+link+"'")
	if want := "### Snapshot\n" + link + "\n\n" + snapshotMarker + "\n- textbox: {{cuttle:T}}\n"; err != nil || got != want {
		t.Fatalf("wrapped verb = %q, %v; want %q", got, err, want)
	}
	if got, err := run(ex, "echo '"+link+"'; exit 3"); got != link+"\n" || playwrightExit(err) == nil {
		t.Errorf("failed verb = %q, %v; want its own output and exit status only", got, err)
	}
	refusing := (&leaseStub{reply: func(*http.Request) (int, string) { return http.StatusNotFound, "{}" }}).start(t)
	if got, err := run(refusing, "echo '"+link+"'"); err != nil || got != link+"\n" {
		t.Errorf("refused fetch = %q, %v; want the verb's output alone", got, err)
	}
}

// A k8s context name is never validated, so one that would leave cuttle/ gets no
// host snapshot dir at all: nothing is written there and nothing purged.
func TestInstanceSnapshotDirStaysUnderCuttle(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	if got, want := instanceSnapshotDir("fs-x"), filepath.Join(state, "cuttle", "fs-x", "snapshots"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	for _, name := range []string{"..", "../../x", "a/../.."} {
		if got := instanceSnapshotDir(name); got != "" {
			t.Fatalf("%q: got %q, want none", name, got)
		}
	}
}

// The appended snapshot is kept on the host with owner-only modes and a bounded
// history, and the printed link points at the copy; anything short of that
// leaves the driver's output as it printed it.
func TestHostSnapshot(t *testing.T) {
	t.Parallel()
	const name = "page-2026-09-19T10-27-05-037Z.yml"
	verb := "### Snapshot\n- [Snapshot](.playwright-cli/" + name + ")\n### Events\n"
	out := []byte(verb + "\n" + snapshotMarker + "\n- textbox: {{cuttle:T}}\n")

	dir := filepath.Join(t.TempDir(), "snapshots")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := range snapshotsKept + 5 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("page-2026-01-01T00-00-%02dZ.yml", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	res, saved := hostSnapshot(dir, out, 0)
	got, path := string(res), filepath.Join(dir, name)
	if want := "### Snapshot\n- [Snapshot](" + path + ")\n### Events\n"; got != want || !saved {
		t.Fatalf("output (saved=%v):\n%s\nwant:\n%s", saved, got, want)
	}
	body, err := os.ReadFile(path)
	if err != nil || string(body) != "- textbox: {{cuttle:T}}\n" {
		t.Fatalf("host copy = %q, %v", body, err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("file mode %v, want 0600", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != snapshotsKept || entries[0].Name() != "page-2026-01-01T00-00-06Z.yml" {
		t.Errorf("kept %d, oldest %q: want the newest %d", len(entries), entries[0].Name(), snapshotsKept)
	}

	fresh := filepath.Join(t.TempDir(), "new")
	if res, _ := hostSnapshot(fresh, out, 0); string(res) == verb {
		t.Fatal("a missing snapshot dir should be created, not skipped")
	}
	if info, _ := os.Stat(fresh); info.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v, want 0700", info.Mode().Perm())
	}

	for label, tc := range map[string]struct{ out, want string }{
		"nothing appended":  {verb, verb},
		"no link":           {"### Ran\n\n" + snapshotMarker + "\nx\n", "### Ran\n"},
		"unwritable target": {string(out), verb},
	} {
		target := filepath.Join(t.TempDir(), "s")
		if label == "unwritable target" {
			if err := os.WriteFile(target, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if res, saved := hostSnapshot(target, []byte(tc.out), 0); string(res) != tc.want || saved {
			t.Errorf("%s: output %q (saved=%v), want %q unsaved", label, res, saved, tc.want)
		}
	}
}

// A `snapshot` that would print the whole tree inline is sent to a host file
// instead, named as the driver names an action's; the head under its link is the
// first lines only, and what the file holds beyond them is counted. A `find`
// printed inline is compacted the same way; a script's --raw/--json output is not.
func TestInlineVerb(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"snapshot"}, "snapshot"},
		{[]string{"snapshot", "e5"}, "snapshot"},
		{[]string{"-s=other", "snapshot", "--depth", "2"}, "snapshot"},
		{[]string{"find", "Sign in"}, "find"},
		{[]string{"find", "--regex", "Popul[a-z]+"}, "find"},
		{[]string{"snapshot", "--filename=after.yml"}, ""},
		{[]string{"snapshot", "--filename", "after.yml"}, ""},
		{[]string{"--raw", "snapshot"}, ""},
		{[]string{"snapshot", "--raw"}, ""},
		{[]string{"--json", "snapshot"}, ""},
		{[]string{"--raw", "find", "x"}, ""},
		{[]string{"--help", "snapshot"}, ""},
		{[]string{"snapshot", "--", "e5"}, ""},
		{[]string{"find", "snapshot"}, "find"},
		{[]string{"console"}, ""},
		{[]string{"--version"}, ""},
	} {
		if got := inlineVerb(tt.args); got != tt.want {
			t.Errorf("inlineVerb(%q) = %q, want %q", tt.args, got, tt.want)
		}
	}
	name := snapshotFileName(time.Date(2026, 9, 19, 10, 27, 5, 37_000_000, time.UTC))
	if want := ".playwright-cli/page-2026-09-19T10-27-05-037Z.yml"; name != want {
		t.Errorf("snapshotFileName = %q, want %q", name, want)
	}
	if !snapshotLinkRE.MatchString("[Snapshot](" + name + ")") {
		t.Errorf("%q would not be rewritten to a host path", name)
	}
}

func TestHostSnapshotHead(t *testing.T) {
	t.Parallel()
	const name = "page-2026-09-19T10-27-05-037Z.yml"
	var tree strings.Builder
	for i := range snapshotHeadLines + 3 {
		fmt.Fprintf(&tree, "- line %d\n", i)
	}
	dir := t.TempDir()
	verb := "### Snapshot\n- [Snapshot](.playwright-cli/" + name + ")\n### Events\n- x\n"
	res, _ := hostSnapshot(dir, []byte(verb+"\n"+snapshotMarker+"\n"+tree.String()), snapshotHeadLines)
	got := string(res)
	lines := strings.Split(tree.String(), "\n")
	want := "### Snapshot\n- [Snapshot](" + filepath.Join(dir, name) + ")\n```yaml\n" +
		strings.Join(lines[:snapshotHeadLines], "\n") + "\n```\n... 3 more lines in the file\n### Events\n- x\n"
	if got != want {
		t.Fatalf("output:\n%s\nwant:\n%s", got, want)
	}
	res, _ = hostSnapshot(dir, []byte(verb+"\n"+snapshotMarker+"\n- one\n"), snapshotHeadLines)
	short := string(res)
	if !strings.Contains(short, "```yaml\n- one\n```\n### Events") || strings.Contains(short, "more lines") {
		t.Errorf("a snapshot within the head should print whole, with no remainder note:\n%s", short)
	}
}

// A file-backed `snapshot` whose file never reached the host prints only a
// container path, so stderr says how to see the tree; an action's link left in
// the container gets no such hint.
func TestReplayHostHintsAStrandedSnapshot(t *testing.T) {
	t.Parallel()
	const link = "### Snapshot\n- [Snapshot](.playwright-cli/page-2026-09-19T10-27-05-037Z.yml)\n"
	for inline, want := range map[string]string{verbSnapshot: "cuttle: the snapshot stayed in the container - `cuttle --name p pw --raw snapshot` prints it inline; `cuttle --name p up --recreate` brings an older container up to this CLI's image\n", "": ""} {
		var stdout, stderr bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		replayHost(cmd, []byte(link), []byte("warn\n"), inline, "cuttle --name p")
		if stdout.String() != link || stderr.String() != "warn\n"+want {
			t.Errorf("inline %q: stdout %q stderr %q, want %q and %q", inline, stdout.String(), stderr.String(), link, "warn\n"+want)
		}
	}
}

// The console log link is a container path; the host gets the verb that reads it.
func TestHostConsoleLine(t *testing.T) {
	t.Parallel()
	in := "### Events\n- New console entries: .playwright-cli/console-2026-09-19T15-47-19-217Z.log#L1-L3\n- Downloading file x ...\n"
	want := "### Events\n- New console entries: in the container - `cuttle --name fixb pw console` prints them\n- Downloading file x ...\n"
	if got := string(hostConsoleLine([]byte(in), "cuttle --name fixb")); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if got := string(hostConsoleLine([]byte("### Page\n- Console: 2 errors, 0 warnings\n"), "cuttle")); got != "### Page\n- Console: 2 errors, 0 warnings\n" {
		t.Errorf("the count line should pass through, got %q", got)
	}
}

// findSample is a `find "Population"` result captured from the bundled driver on
// an encyclopedia article, cut to three of its snippets: each is the ancestor
// chain plus a window of siblings, with elisions, and no match is marked.
var findSample = `### Result
Found 6 matches for "Population":

- generic [active] [ref=e1]:
  - generic [ref=e38]:
    - main [ref=e122]:
      - generic "Lisbon" [ref=e172]:
        - table [ref=e193]:
          - rowgroup [ref=e194]:
            - row [ref=e411]:
              - columnheader "Population (2025)[1]" [ref=e412]:
                - text: Population
                - generic [ref=e413]:
                  - text: (2025)

----

- generic [active] [ref=e1]:
  - generic [ref=e38]:
    - main [ref=e122]:
      - generic "Lisbon" [ref=e172]:
        - region [ref=e2423]:
          - generic [ref=e2424]:
            ...
            - generic [ref=e2426]:
              - link "edit":
                - /url: /w/index.php?title=Lisbon&action=edit&section=27
          - generic [ref=e2427]: Historical population
          - table [ref=e2428]:

----

- generic [active] [ref=e1]:
  - generic [ref=e38]:
    - main [ref=e122]:
      - generic "Lisbon" [ref=e172]:
        - region [ref=e3674]:
          - listitem [ref=e4679]:
            - link "Jump up" [ref=e5874] [cursor=pointer]:
              ...
              - text: ^
            - generic [ref=e4682]:
              - link "World Urbanization Prospects" [ref=e4683] [cursor=pointer]:
                - /url: https://www.un.org/esa/population/publications/wup2007/2007WUP_Highlights_web.pdf
              - link "` + longName + `" [ref=e4690] [cursor=pointer]:
`

var longName = "Population by sex and age groups on 1 January, " + strings.Repeat("x", findClipRunes)

// `find` compacts to one line per matching snapshot line, each behind its
// nearest referenced ancestor, so a text or url hit names the node holding it
// and a node hit names the one enclosing it; a hit longer than the clip keeps
// its ref; an ancestor line that matches is one hit however many snippets
// repeat it. --raw output never reaches this (TestInlineVerb).
func TestCompactFind(t *testing.T) {
	t.Parallel()
	got := string(compactFind([]byte(findSample)))
	want := "### Result\nFound 6 matches for \"Population\":\n" +
		"- row [ref=e411] > columnheader \"Population (2025)[1]\" [ref=e412]\n" +
		"- columnheader \"Population (2025)[1]\" [ref=e412] > text: Population\n" +
		"- region [ref=e2423] > generic [ref=e2427]: Historical population\n" +
		"- link \"World Urbanization Prospects\" [ref=e4683] > /url: https://www.un.org/esa/population/publications/wup2007/2007WUP_Highlights_web.pdf\n" +
		"- generic [ref=e4682] > link \"" + longName[:findClipRunes-6] + "... [ref=e4690]\n"
	if got != want {
		t.Fatalf("compacted:\n%s\nwant:\n%s", got, want)
	}

	// The article's own name is an ancestor in every snippet: one hit, and the
	// url that carries it names the link it belongs to, through a ref-less one.
	lisbon := strings.Replace(findSample, `Found 6 matches for "Population":`, `Found 2 matches for "lisbon":`, 1)
	got = string(compactFind([]byte(lisbon)))
	want = "### Result\nFound 2 matches for \"lisbon\":\n" +
		"- main [ref=e122] > generic \"Lisbon\" [ref=e172]\n" +
		"- generic [ref=e2426] > /url: /w/index.php?title=Lisbon&action=edit&section=27\n"
	if got != want {
		t.Fatalf("compacted:\n%s\nwant:\n%s", got, want)
	}

	// A --regex query is re-run with its flags; one Go cannot compile, and a
	// query that re-matches nothing, pass the driver's output through.
	re := strings.Replace(findSample, `Found 6 matches for "Population":`, `Found 1 match for /historical POP\w+/i:`, 1)
	if got = string(compactFind([]byte(re))); got != "### Result\nFound 1 match for /historical POP\\w+/i:\n- region [ref=e2423] > generic [ref=e2427]: Historical population\n" {
		t.Errorf("regex:\n%s", got)
	}
	for _, header := range []string{`Found 1 match for /(?<=a)b/:`, `Found 1 match for "not on this page":`} {
		in := strings.Replace(findSample, `Found 6 matches for "Population":`, header, 1)
		if got = string(compactFind([]byte(in))); got != in {
			t.Errorf("%s: should pass through, got:\n%s", header, got)
		}
	}
	none := "### Result\nNo matches found for \"x\".\n"
	if got = string(compactFind([]byte(none))); got != none {
		t.Errorf("no matches: got %q", got)
	}

	// Past the cap, the rest is a count: a common word wants a narrower search.
	var many strings.Builder
	many.WriteString("Found 50 matches for \"item\":\n\n- list [ref=e1]:\n")
	for i := range findMaxHits + 10 {
		fmt.Fprintf(&many, "  - listitem \"item %d\" [ref=e%d]\n", i, i+2)
	}
	got = string(compactFind([]byte(many.String())))
	if lines := strings.Split(strings.TrimSpace(got), "\n"); len(lines) != findMaxHits+2 ||
		lines[1] != "- list [ref=e1] > listitem \"item 0\" [ref=e2]" ||
		lines[findMaxHits+1] != "... 10 more matches - narrow the text, or `--raw find` prints the driver's full output" {
		t.Errorf("capped:\n%s", got)
	}
}
