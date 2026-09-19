package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glim-sh/cuttle/internal/jev"
)

func TestParseTextValues(t *testing.T) {
	values, err := parseTextValues([]string{"user=qa@example.com", "pass={{cuttle:QA_PASS}}", "blank="})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// The sentinel has to survive byte for byte: cuttle substitutes it inside its
	// own CDP frame, on the fill path, so anything that rewrites it here types a
	// literal into the field instead.
	if got := values["pass"]; got != "{{cuttle:QA_PASS}}" {
		t.Errorf("sentinel: got %q", got)
	}
	// A value can legitimately contain '=' - only the FIRST one separates.
	more, err := parseTextValues([]string{"token=a=b=c"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := more["token"]; got != "a=b=c" {
		t.Errorf("value: got %q, want a=b=c", got)
	}
	if _, ok := values["blank"]; !ok {
		t.Error("an empty value is still a value")
	}
}

// The error for a malformed pair must not echo the half that may be a password,
// so it says which half is missing instead.
func TestParseTextValuesRejectsAMalformedPairWithoutEchoingIt(t *testing.T) {
	for pair, want := range map[string]string{"hunter2": "no =", "=hunter2": "no name"} {
		_, err := parseTextValues([]string{pair})
		if !errors.Is(err, errJevTextPair) {
			t.Fatalf("%s: got %v, want errJevTextPair", pair, err)
		}
		if !strings.HasSuffix(err.Error(), want) {
			t.Errorf("%s: got %v, want it to say it has %s", pair, err, want)
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("the error echoed the value: %v", err)
		}
	}
}

// The task is the one argument every run needs, so it is takeable the way a
// shell user reaches for it first - as the argument - with --task still canonical.
func TestJevBrowseTakesTheTaskAsAnArgument(t *testing.T) {
	got, err := jevTask("", []string{"find the support phone number"})
	if err != nil {
		t.Fatalf("jevTask: %v", err)
	}
	if got != "find the support phone number" {
		t.Errorf("task: got %q", got)
	}
	cmd := newJevBrowseCmd()
	if err := cmd.Args(cmd, []string{"one task", "another"}); err == nil {
		t.Error("a second positional argument was accepted")
	}
}

// Both spellings at once is a typo, and silently preferring one of them would
// run a task the caller did not mean to run.
func TestJevBrowseRefusesTheTaskTwice(t *testing.T) {
	if _, err := jevTask("sign in", []string{"find the support phone number"}); !errors.Is(err, errJevTaskTwice) {
		t.Errorf("got %v, want errJevTaskTwice", err)
	}
	if _, err := jevTask("", nil); !errors.Is(err, errJevTaskAbsent) {
		t.Errorf("got %v, want errJevTaskAbsent", err)
	}
}

// The help is where an agent learns the contract, so it has to carry the two
// things that are not guessable: where the key comes from, and what the exit
// codes mean.
func TestJevBrowseHelpNamesTheKeyEnvAndTheExitCodes(t *testing.T) {
	long := newJevBrowseCmd().Long
	for _, want := range []string{jev.APIKeyEnv, "Exit codes:", "cuttle pw", "EXPERIMENTAL", "--url is the page to start from"} {
		if !strings.Contains(long, want) {
			t.Errorf("the help does not mention %q", want)
		}
	}
	if !strings.Contains(newJevBrowseCmd().Short, "experimental") {
		t.Error("the short help does not mark the loop experimental")
	}
	if strings.Contains(long, "--endpoint") {
		t.Error("the help must not suggest pointing the driver at another browser")
	}
}

// jev re-gotos after every `go-back` to work around a ref-poisoning defect in
// the pinned driver (see remintAfterBack in internal/jev/loop.go). A pin bump
// must decide consciously whether that workaround still earns its place.
func TestJevBackWorkaroundTracksTheDriverPin(t *testing.T) {
	if BundledPlaywrightCLIVersion != "0.1.20" {
		t.Errorf("the bundled playwright-cli moved to %s: re-check the go-back ref defect that "+
			"jev's remintAfterBack works around, drop the workaround if it is fixed, then update this test",
			BundledPlaywrightCLIVersion)
	}
}

type argvExecer struct{}

func (argvExecer) ExecCommand(_ string, argv []string) (string, []string) { return argv[0], argv[1:] }

func TestCountingExecerCountsEveryExec(t *testing.T) {
	t.Parallel()
	c := &countingExecer{Execer: argvExecer{}}
	for range 3 {
		if err := execIn(context.Background(), nil, c, "/", []string{"true"}, io.Discard, io.Discard); err != nil {
			t.Fatalf("exec: %v", err)
		}
	}
	if n := c.spawns.Load(); n != 3 {
		t.Errorf("counted %d spawns, want 3", n)
	}
}

// fakeClientDriver stands in for the container: `node` is a persistent client
// that answers per the request line, anything else is an exec of the driver.
// Both log to dir/calls, and each client start to dir/starts.
type fakeClientDriver struct{ dir string }

func (d fakeClientDriver) ExecCommand(_ string, argv []string) (string, []string) {
	if argv[0] == "node" {
		const client = `cd "$0" || exit 2
echo start >> starts
[ -e nostart ] && exit 1
echo '{"ready":true}'
while IFS= read -r line; do
  echo "$line" >> calls
  case "$line" in
    *'"hang"'*) read -r _ ;;
    *'"url"'*) [ -e leasedown ] && echo '{"isError":true,"text":"fetch failed"}' || echo '{"status":409,"text":"{\\"owner\\":\\"cuttle pw 9@me\\"}"}' ;;
    *'"list"'*) echo '{"fallback":true}' ;;
    *'"modal"'*) echo '{"isError":true,"text":"### Modal state\\n"}' ;;
    *) if [ -e sessionless ] && [ ! -e attached ]; then
         echo '{"isError":true,"text":"The browser '"'cuttle'"' is not open, please run open first\\n"}'
       else echo '{"text":"ok\\n"}'; fi ;;
  esac
done`
		return "sh", []string{"-c", client, d.dir}
	}
	const execed = `cd "$0" || exit 2
echo "exec $*" >> calls
case "$*" in *" attach --cdp="*) touch attached ;; esac
echo "exec ran"`
	return "sh", append([]string{"-c", execed, d.dir}, argv...)
}

func (d fakeClientDriver) read(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(d.dir, name))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(b)
}

func (d fakeClientDriver) touch(t *testing.T, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(d.dir, name), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func newFakeClient(t *testing.T) (fakeClientDriver, playwrightRunner) {
	t.Helper()
	fake := fakeClientDriver{t.TempDir()}
	drv := &persistentDriver{ex: fake}
	t.Cleanup(drv.close)
	return fake, attachingRunner(fake, drv.run)
}

// Every verb of a run goes through the one client start, not an exec each.
func TestPersistentDriverKeepsOneClient(t *testing.T) {
	t.Parallel()
	fake, run := newFakeClient(t)
	for _, args := range [][]string{{"snapshot"}, {"click", "e1"}, {"fill", "e2", "a \"b\""}} {
		if out, err := run(context.Background(), args...); err != nil || out != "ok\n" {
			t.Fatalf("%v = %q, %v", args, out, err)
		}
	}
	if got := fake.read(t, "starts"); got != "start\n" {
		t.Errorf("client starts = %q, want one", got)
	}
	want := `["snapshot"]` + "\n" + `["click","e1"]` + "\n" + `["fill","e2","a \"b\""]` + "\n"
	if got := fake.read(t, "calls"); got != want {
		t.Errorf("calls:\n%s\nwant:\n%s", got, want)
	}
}

// A failed verb keeps its output beside the error, as the exec path does: the
// modal block rides on snapshot's error exit. The error is not an ExitCodeError,
// which main would exit on without printing why.
func TestPersistentDriverErrorKeepsOutput(t *testing.T) {
	t.Parallel()
	_, run := newFakeClient(t)
	out, err := run(context.Background(), "modal")
	if !errors.Is(err, errDriverVerbFailed) || out != "### Modal state\n" {
		t.Fatalf("modal = %q, %v; want its output and the failed-verb error", out, err)
	}
	if _, ok := errors.AsType[*ExitCodeError](err); ok {
		t.Error("a failed verb is an ExitCodeError, which main exits on silently")
	}
}

// A canceled run sends no further verb, not even through the exec fallback.
func TestPersistentDriverCanceledSendsNothing(t *testing.T) {
	t.Parallel()
	fake, run := newFakeClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := run(ctx, "click", "e1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("click = %v, want the cancellation", err)
	}
	if got := fake.read(t, "calls"); got != "" {
		t.Errorf("calls = %q, want none", got)
	}
}

// The guard's renew goes through the client, so a takeover is seen before every
// driving verb with no process start; a fetch the client cannot make falls back
// to the exec'd curl.
func TestPersistentDriverRenewsTheLease(t *testing.T) {
	t.Parallel()
	fake := fakeClientDriver{t.TempDir()}
	drv := &persistentDriver{ex: fake}
	t.Cleanup(drv.close)
	l := &sessionLease{ex: fake, owner: "jev-browse 7@me", token: "abc", ttl: time.Hour}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	drive := l.guard(attachingRunner(fake, drv.run), drv.lease, cancel)
	if _, err := drive(ctx, "click", "e1"); !errors.Is(err, errSessionTakenOver) || !strings.Contains(err.Error(), "cuttle pw 9@me") {
		t.Fatalf("click = %v, want the takeover by its taker", err)
	}
	calls := fake.read(t, "calls")
	if !strings.Contains(calls, `"method":"POST"`) || !strings.Contains(calls, "token=abc") || strings.Contains(calls, "exec") {
		t.Errorf("calls:\n%s\nwant one renew through the client and no exec", calls)
	}

	fake.touch(t, "leasedown")
	if _, _, err := drv.lease(context.Background(), "POST", nil); err != nil {
		t.Fatalf("lease: %v", err)
	}
	if !strings.Contains(fake.read(t, "calls"), "exec curl") {
		t.Error("a fetch the client could not make was not made again by curl")
	}
}

// What the client leaves to the driver's own process - and attach/open, which
// never reach it - is execed, so it keeps the driver's behavior.
func TestPersistentDriverFallsBackToExec(t *testing.T) {
	t.Parallel()
	fake, run := newFakeClient(t)
	for _, args := range [][]string{{"list"}, {"attach"}} {
		if out, err := run(context.Background(), args...); err != nil || out != "exec ran\n" {
			t.Fatalf("%v = %q, %v; want the exec's output", args, out, err)
		}
	}
	want := `["list"]` + "\nexec playwright-cli list\nexec playwright-cli attach --cdp=http://127.0.0.1:9222\n"
	if got := fake.read(t, "calls"); got != want {
		t.Errorf("calls:\n%s\nwant:\n%s", got, want)
	}
}

// A client that cannot start is given up on for the run: every verb execs, and
// no verb pays for another start attempt.
func TestPersistentDriverUnstartableExecs(t *testing.T) {
	t.Parallel()
	fake, run := newFakeClient(t)
	fake.touch(t, "nostart")
	for range 2 {
		if out, err := run(context.Background(), "snapshot"); err != nil || out != "exec ran\n" {
			t.Fatalf("snapshot = %q, %v; want the exec's output", out, err)
		}
	}
	if got := fake.read(t, "starts"); got != "start\n" {
		t.Errorf("client starts = %q, want one attempt", got)
	}
}

// No session re-attaches through the locked exec and retries on the client.
func TestPersistentDriverReattaches(t *testing.T) {
	t.Parallel()
	fake, run := newFakeClient(t)
	fake.touch(t, "sessionless")
	if out, err := run(context.Background(), "snapshot"); err != nil || out != "ok\n" {
		t.Fatalf("snapshot = %q, %v; want the retried verb's output", out, err)
	}
	calls := fake.read(t, "calls")
	if !strings.Contains(calls, "attach --cdp=") || strings.Count(calls, `["snapshot"]`) != 2 {
		t.Errorf("calls:\n%s\nwant snapshot, the exec attach, snapshot", calls)
	}
}

// A canceled verb kills the client and is never re-run - it may already have
// acted - and the next verb starts a fresh client.
func TestPersistentDriverCancelRestarts(t *testing.T) {
	t.Parallel()
	fake, run := newFakeClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := run(ctx, "hang"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("hang = %v, want the deadline", err)
	}
	if out, err := run(context.Background(), "snapshot"); err != nil || out != "ok\n" {
		t.Fatalf("snapshot after cancel = %q, %v", out, err)
	}
	if got := fake.read(t, "starts"); got != "start\nstart\n" {
		t.Errorf("client starts = %q, want a restart", got)
	}
	if strings.Contains(fake.read(t, "calls"), "exec") {
		t.Error("the canceled verb was re-run through exec")
	}
}
