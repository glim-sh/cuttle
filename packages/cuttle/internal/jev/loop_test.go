package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

const signinURL = "http://127.0.0.1:8799/"

// errStaleRef is what a ref that went stale actually looks like coming out of
// the driver: a click timeout, not a "stale ref" of any kind.
var errStaleRef = errors.New("TimeoutError: Timeout 5000ms exceeded")

// errPoisonedRef is how the pinned driver rejects a ref minted before a go-back.
// The driver capitalizes "Ref"; only the case differs from the real message.
var errPoisonedRef = errors.New("ref e1 not found in the current page snapshot")

// fakeDriver stands in for the bundled playwright-cli. `snapshot` answers with
// the page the session is on; any other verb is recorded and moves it to the
// next page, which is what a click or a fill does to a browser. Repeating the
// same output is what a settled page looks like.
type fakeDriver struct {
	pages []string
	// reads, when set, answers each `snapshot` call from this script instead,
	// so a test can make the page change under the settle loop. The last entry
	// repeats.
	reads []string
	fail  func(args []string) error

	page  int
	read  int
	calls []string
}

func (d *fakeDriver) run(_ context.Context, args ...string) (string, error) {
	d.calls = append(d.calls, strings.Join(args, " "))
	if args[0] == "snapshot" {
		if d.reads != nil {
			out := d.reads[min(d.read, len(d.reads)-1)]
			d.read++
			return out, nil
		}
		return d.pages[min(d.page, len(d.pages)-1)], nil
	}
	if d.fail != nil {
		if err := d.fail(args); err != nil {
			return "", err
		}
	}
	d.page++
	return "### Result\n\"ok\"\n", nil
}

// actions is the driver log with the reads taken out: what the loop DID.
func (d *fakeDriver) actions() []string {
	var out []string
	for _, c := range d.calls {
		if !strings.HasPrefix(c, "snapshot") {
			out = append(out, c)
		}
	}
	return out
}

type runResult struct {
	code   int
	err    error
	stdout string
	stderr string
}

func runLoop(t *testing.T, driver *fakeDriver, opts Options) runResult {
	t.Helper()
	var out, errOut bytes.Buffer
	opts.Driver = driver.run
	opts.Out, opts.Err = &out, &errOut
	if opts.MaxSteps == 0 {
		opts.MaxSteps = 25
	}
	if opts.Task == "" {
		opts.Task = "sign in to the demo shop"
	}
	// The settle loop waits a poll interval between reads. On a fake clock that
	// costs nothing, and no test here is about how long a real one takes.
	if opts.now == nil {
		newFakeClock().wire(&opts)
	}
	code, err := Run(context.Background(), opts)
	return runResult{code: code, err: err, stdout: out.String(), stderr: errOut.String()}
}

func TestRunRequiresATask(t *testing.T) {
	code, err := Run(context.Background(), Options{Mock: true})
	if !errors.Is(err, errTaskRequired) {
		t.Errorf("got %v, want errTaskRequired", err)
	}
	if code != ExitError {
		t.Errorf("exit code: got %d, want %d", code, ExitError)
	}
}

// A budget below one is a usage error, not a run: the loop would never read the
// page, and the brief it printed named a page it had never seen.
func TestRunRequiresAStepBudget(t *testing.T) {
	d := &fakeDriver{pages: []string{readFixture(t, "signin.snapshot")}}
	res := runLoop(t, d, Options{Mock: true, MaxSteps: -1})
	if !errors.Is(res.err, errNoSteps) {
		t.Errorf("got %v, want errNoSteps", res.err)
	}
	if res.code != ExitError {
		t.Errorf("exit code: got %d, want %d", res.code, ExitError)
	}
	if d.calls != nil {
		t.Errorf("the loop touched the driver anyway: %q", d.calls)
	}
}

// Without --url the run starts wherever the session already is, and a fresh
// session is a blank tab. Spending a step and a request to discover that is
// worse than saying so, and the message names both ways out.
func TestRunRefusesToStartOnABlankPage(t *testing.T) {
	d := &fakeDriver{pages: []string{readFixture(t, "blank_tab.snapshot")}}
	res := runLoop(t, d, Options{Mock: true})
	if !errors.Is(res.err, errNoStartPage) {
		t.Fatalf("got %v, want errNoStartPage", res.err)
	}
	if res.code != ExitError {
		t.Errorf("exit code: got %d, want %d", res.code, ExitError)
	}
	if d.actions() != nil {
		t.Errorf("the loop acted on a blank page: %q", d.actions())
	}
}

// A failed navigation leaves the tab on the browser's own error page, which is
// no more a place to start from than a blank one.
func TestRunRefusesToStartOnAnErrorPage(t *testing.T) {
	d := &fakeDriver{pages: []string{"### Page\n- Page URL: chrome-error://chromewebdata/\n- Page Title: example.invalid\n"}}
	if res := runLoop(t, d, Options{Mock: true}); !errors.Is(res.err, errNoStartPage) {
		t.Fatalf("got %v, want errNoStartPage", res.err)
	}
}

// A page that raises a dialog while it loads never finishes loading, so the goto
// fails - but the dialog is what a person has to clear, and the run is blocked
// on it rather than broken.
func TestRunBlocksOnADialogRaisedWhileTheStartPageLoads(t *testing.T) {
	d := &fakeDriver{
		pages: []string{"### Modal state\n- [\"alert\" dialog with message \"hi\"]: can be handled by dialog-accept or dialog-dismiss\n"},
		fail: func(args []string) error {
			if args[0] == "goto" {
				return errStaleRef
			}
			return nil
		},
	}
	res := runLoop(t, d, Options{Mock: true, URL: signinURL})
	if res.err != nil {
		t.Fatalf("run: %v", res.err)
	}
	if res.code != ExitBlocked {
		t.Fatalf("exit code: got %d, want %d", res.code, ExitBlocked)
	}
	if !strings.Contains(res.stderr, `"alert" dialog with message "hi"`) {
		t.Errorf("the brief does not name the dialog:\n%s", res.stderr)
	}
}

// The driver prints its failures under an `### Error` header, and that header is
// the first line of its output. The message is the line after it.
func TestDriverErrKeepsTheDriversMessage(t *testing.T) {
	out := "### Error\nError: page.goto: net::ERR_NAME_NOT_RESOLVED at https://example.invalid/\nCall log:\n  - navigating to \"https://example.invalid/\"\n"
	got := driverErr(out, errStaleRef).Error()
	if want := errStaleRef.Error() + ": Error: page.goto: net::ERR_NAME_NOT_RESOLVED at https://example.invalid/"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := driverErr("no header here\nsecond", errStaleRef).Error(); got != errStaleRef.Error()+": no header here" {
		t.Errorf("output with no error block: got %q", got)
	}
}

// The read after the last action is the first look at where it landed, so a
// budget that ran out ON the goal page is a task done, not one given up on.
func TestRunJudgesThePageTheLastStepLandedOn(t *testing.T) {
	tr := &scriptedTransport{rounds: []map[string]answer{
		{"pick0": {Choice: "f2e11", Confidence: 0.9}},
		{questionDone: {Noul: 0.95}},
	}}
	d := &fakeDriver{pages: []string{readFixture(t, "signin.snapshot")}}
	res := runLoop(t, d, Options{transport: tr, MaxSteps: 1})
	if res.err != nil {
		t.Fatalf("run: %v", res.err)
	}
	if res.code != ExitDone {
		t.Fatalf("exit code: got %d, want %d:\n%s", res.code, ExitDone, res.stderr)
	}
	if got := d.actions(); !slices.Equal(got, []string{"click f2e11"}) {
		t.Errorf("driver calls: got %q, want the one step the budget allowed", got)
	}
}

// The guard is about a run with nowhere to start, not about the blank tab a
// --url is on its way off.
func TestRunStartsOnABlankPageWhenAURLWasGiven(t *testing.T) {
	d := &fakeDriver{pages: []string{readFixture(t, "blank_tab.snapshot")}}
	res := runLoop(t, d, Options{Mock: true, URL: signinURL, MaxSteps: 1})
	if res.err != nil {
		t.Fatalf("run: %v", res.err)
	}
	if res.code == ExitError {
		t.Errorf("exit code: got %d, want the run to have gone ahead", res.code)
	}
}

// The mock has no judgement - it is the loop, the snapshot parsing, the driver
// shell-out and the exit codes that are under test. What it guarantees is that
// it never repeats an action, so the loop walks the page instead of pressing one
// button until the budget runs out.
func TestRunWalksThePageWithTheMockTransport(t *testing.T) {
	d := &fakeDriver{pages: []string{readFixture(t, "signin.snapshot")}}
	res := runLoop(t, d, Options{Mock: true, Values: map[string]string{"pass": "{{cuttle:DEMO_PASS}}"}})
	if res.err != nil {
		t.Fatalf("run: %v", res.err)
	}
	// Every action is distinct, and the page eventually runs out of them.
	seen := map[string]bool{}
	for _, a := range d.actions() {
		if strings.HasPrefix(a, "press ") {
			continue
		}
		if seen[a] {
			t.Errorf("the loop repeated an action: %q", a)
		}
		seen[a] = true
	}
	if res.code != ExitBlocked {
		t.Errorf("exit code: got %d, want %d once nothing is left to do", res.code, ExitBlocked)
	}
	if !strings.Contains(res.stderr, "the model found no useful action on this page") {
		t.Errorf("stderr does not say why it stopped:\n%s", res.stderr)
	}
}

// The value reaches the page exactly as it was given. cuttle substitutes its
// sentinels inside its own CDP frame, on the fill path, so anything this loop
// did to the string on the way past would break the substitution - and `fill` is
// the only verb it survives, since typing per character never lets it reassemble.
func TestRunPassesTheSecretSentinelThroughToFill(t *testing.T) {
	d := &fakeDriver{pages: []string{readFixture(t, "signin.snapshot")}}
	res := runLoop(t, d, Options{Mock: true, MaxSteps: 1, Values: map[string]string{"pass": "{{cuttle:DEMO_PASS}}"}})
	if res.err != nil {
		t.Fatalf("run: %v", res.err)
	}
	want := []string{"fill f2e7 {{cuttle:DEMO_PASS}}"}
	got := d.actions()
	if !slices.Equal(got, want) {
		t.Errorf("driver calls: got %q, want %q", got, want)
	}
	if strings.Contains(res.stdout+res.stderr, "DEMO_PASS=") {
		t.Error("the loop echoed a prepared value")
	}
}

// A dialog parks the renderer, so every read after it is a read of a stale page.
// Recognizing that is worth more than any action the loop could take next.
func TestRunBlocksOnAModalState(t *testing.T) {
	d := &fakeDriver{pages: []string{readFixture(t, "modal_filechooser.snapshot")}}
	res := runLoop(t, d, Options{Mock: true})
	if res.err != nil {
		t.Fatalf("run: %v", res.err)
	}
	if res.code != ExitBlocked {
		t.Fatalf("exit code: got %d, want %d", res.code, ExitBlocked)
	}
	if !strings.Contains(res.stderr, "File chooser") {
		t.Errorf("the brief does not name the dialog:\n%s", res.stderr)
	}
	if d.actions() != nil {
		t.Errorf("the loop acted on a page behind a dialog: %q", d.actions())
	}
}

// An alert capture carries no page identity at all - the modal block and nothing
// else - so the brief has to fall back to the page the action that raised the
// dialog was taken on. Pointing a handoff at an empty page is worse than useless.
func TestRunBriefNamesThePageADialogWasRaisedOn(t *testing.T) {
	signin := readFixture(t, "signin.snapshot")
	d := &fakeDriver{reads: []string{signin, signin, readFixture(t, "modal_beforeunload.snapshot")}}
	res := runLoop(t, d, Options{Mock: true, MaxSteps: 3})
	if res.code != ExitBlocked {
		t.Fatalf("exit code: got %d, want %d", res.code, ExitBlocked)
	}
	if strings.Contains(res.stderr, "page: <>") {
		t.Errorf("the brief pointed at an empty page:\n%s", res.stderr)
	}
	if !strings.Contains(res.stderr, "page: <"+signinURL+">") {
		t.Errorf("the brief does not name the page the dialog was raised on:\n%s", res.stderr)
	}
}

// The session stays live at exactly the page it stopped on, so a stop is a
// handoff: the reason, where it is, and the command that picks it up.
func TestRunPrintsAHandoffBrief(t *testing.T) {
	d := &fakeDriver{pages: []string{readFixture(t, "signin.snapshot")}}
	res := runLoop(t, d, Options{Mock: true, MaxSteps: 1})
	if res.code != ExitMaxSteps {
		t.Fatalf("exit code: got %d, want %d", res.code, ExitMaxSteps)
	}
	for _, want := range []string{"gave up after 1 step with", signinURL, "cuttle pw snapshot"} {
		if !strings.Contains(res.stderr, want) {
			t.Errorf("the brief is missing %q:\n%s", want, res.stderr)
		}
	}
}

// On a named instance a bare `cuttle pw snapshot` reaches the default container,
// so the handoff - in the brief and in the JSON outcome - carries the invocation
// that reaches the instance the run drove.
func TestRunHandsOffToTheInstanceItDrove(t *testing.T) {
	const want = "cuttle --context box --name scraper pw snapshot"
	d := &fakeDriver{pages: []string{readFixture(t, "signin.snapshot")}}
	res := runLoop(t, d, Options{Mock: true, MaxSteps: 1, Cuttle: "cuttle --context box --name scraper"})
	if !strings.Contains(res.stderr, want) {
		t.Errorf("the brief does not hand off to the named instance:\n%s", res.stderr)
	}
	d = &fakeDriver{pages: []string{readFixture(t, "signin.snapshot")}}
	res = runLoop(t, d, Options{Mock: true, MaxSteps: 1, JSON: true, Cuttle: "cuttle --context box --name scraper"})
	if !strings.Contains(res.stdout, `"next":"`+want+`"`) {
		t.Errorf("the JSON outcome does not hand off to the named instance:\n%s", res.stdout)
	}
}

// The budget runs out by ACTING, so the last step navigated after the snapshot
// it was decided from. The brief promises the session is live at exactly the
// page it names, which only holds if it names the page the action landed on.
func TestRunNamesThePageTheLastActionLandedOn(t *testing.T) {
	d := &fakeDriver{pages: []string{
		readFixture(t, "signin.snapshot"),
		"### Page\n- Page URL: http://127.0.0.1:8799/account\n### Snapshot\n- button \"Sign out\" [ref=e1]\n",
	}}
	res := runLoop(t, d, Options{Mock: true, MaxSteps: 1})
	if res.code != ExitMaxSteps {
		t.Fatalf("exit code: got %d, want %d", res.code, ExitMaxSteps)
	}
	if !strings.Contains(res.stderr, "/account") {
		t.Errorf("the brief names the page the last action left, not the one it reached:\n%s", res.stderr)
	}
}

// The model is only ever offered this page's candidates, so a key that is not
// one of them is an answer aimed at an element nobody vouched for.
func TestRunRefusesAKeyThePageDidNotOffer(t *testing.T) {
	tr := &scriptedTransport{rounds: []map[string]answer{{"pick0": {Choice: "e404", Confidence: 0.99}}}}
	d := &fakeDriver{pages: []string{readFixture(t, "signin.snapshot")}}
	res := runLoop(t, d, Options{transport: tr})
	if res.code != ExitBlocked {
		t.Fatalf("exit code: got %d, want %d", res.code, ExitBlocked)
	}
	if !strings.Contains(res.stderr, `chose "e404"`) {
		t.Errorf("the brief does not name the refused key:\n%s", res.stderr)
	}
	if d.actions() != nil {
		t.Errorf("the loop acted on an unoffered key: %q", d.actions())
	}
}

// cuttle's mux hands out frame-prefixed refs; an answer that echoes the bare
// form back names the same element.
func TestRunAcceptsABareRefForAFramePrefixedElement(t *testing.T) {
	tr := &scriptedTransport{rounds: []map[string]answer{{"pick0": {Choice: "e11", Confidence: 0.99}}}}
	d := &fakeDriver{pages: []string{readFixture(t, "signin.snapshot")}}
	res := runLoop(t, d, Options{transport: tr, MaxSteps: 1})
	if res.err != nil {
		t.Fatalf("run: %v", res.err)
	}
	if got := d.actions(); len(got) != 1 || got[0] != "click f2e11" {
		t.Errorf("driver calls: got %q, want the Login button clicked by its real ref", got)
	}
}

func TestRunReportsDone(t *testing.T) {
	tr := &scriptedTransport{rounds: []map[string]answer{{questionDone: {Noul: 0.95}}}}
	d := &fakeDriver{pages: []string{readFixture(t, "signin.snapshot")}}
	res := runLoop(t, d, Options{transport: tr})
	if res.err != nil {
		t.Fatalf("run: %v", res.err)
	}
	if res.code != ExitDone {
		t.Fatalf("exit code: got %d, want %d", res.code, ExitDone)
	}
	if !strings.Contains(res.stdout, "the task is done") {
		t.Errorf("stdout: %s", res.stdout)
	}
	if d.actions() != nil {
		t.Errorf("the loop kept acting after the task was done: %q", d.actions())
	}
}

// A near coin flip must not be allowed to end a run or hand one to a human.
func TestRunHoldsTheNoulsToTheThreshold(t *testing.T) {
	tr := &scriptedTransport{rounds: []map[string]answer{{
		questionDone: {Noul: 0.6}, questionBlocked: {Noul: 0.6}, "pick0": {Choice: "f2e11", Confidence: 0.9},
	}}}
	d := &fakeDriver{pages: []string{readFixture(t, "signin.snapshot")}}
	res := runLoop(t, d, Options{transport: tr, MaxSteps: 1})
	if res.code != ExitMaxSteps {
		t.Errorf("exit code: got %d, want the loop to have acted anyway (%d)", res.code, ExitMaxSteps)
	}
	if got := d.actions(); len(got) != 1 || got[0] != "click f2e11" {
		t.Errorf("driver calls: got %q", got)
	}
}

// A `none` ends the run blocked with the confidence the model gave it. It used
// to be stamped 1.00 - and a `none` at even odds on a page rated 0.4 done used
// to be read as done.
func TestRunReportsANoneAtItsOwnConfidence(t *testing.T) {
	tr := &scriptedTransport{rounds: []map[string]answer{{
		questionDone: {Noul: 0.4}, "pick0": {Choice: noneKey, Confidence: 0.55},
	}}}
	d := &fakeDriver{pages: []string{readFixture(t, "article_toc.snapshot")}}
	res := runLoop(t, d, Options{transport: tr, Task: "reach the Geography section"})
	if res.err != nil || res.code != ExitBlocked {
		t.Fatalf("run: code %d, err %v, want %d:\n%s", res.code, res.err, ExitBlocked, res.stderr)
	}
	if !strings.Contains(res.stdout, "(confidence 0.55)") || strings.Contains(res.stdout, "1.00") {
		t.Errorf("the step line does not carry the model's own confidence:\n%s", res.stdout)
	}
	if d.actions() != nil {
		t.Errorf("the loop acted on a none: %q", d.actions())
	}
}

// fakeClock makes the settle deadline decidable without waiting for it: sleeping
// is what moves time here, so a test that never sleeps never ages.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time               { return c.t }
func (c *fakeClock) sleep(d time.Duration)        { c.t = c.t.Add(d) }
func newFakeClock() *fakeClock                    { return &fakeClock{t: time.Unix(0, 0)} }
func (c *fakeClock) wire(opts *Options)           { opts.now, opts.sleep = c.now, c.sleep }
func (c *fakeClock) elapsed() time.Duration       { return c.t.Sub(time.Unix(0, 0)) }
func (c *fakeClock) String() string               { return c.elapsed().String() }
func (c *fakeClock) reached(d time.Duration) bool { return c.elapsed() >= d }

// The loop never sleeps for a fixed time. It re-reads until two reads agree,
// which is what makes an action's effect safe to decide against.
func TestSettleWaitsUntilTwoReadsAgree(t *testing.T) {
	loading := readFixture(t, "blank_tab.snapshot")
	loaded := readFixture(t, "signin.snapshot")
	clock := newFakeClock()
	opts := Options{Task: "t", MaxSteps: 1, transport: &scriptedTransport{}, Driver: (&fakeDriver{}).run}
	clock.wire(&opts)

	d := &fakeDriver{reads: []string{loading, loading, loaded, loaded}}
	opts.Driver = d.run
	l, err := newLoop(opts)
	if err != nil {
		t.Fatalf("newLoop: %v", err)
	}
	snap, err := l.settle(context.Background(), Snapshot{})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	// Reads 1+2 agree on the loading page, so that is where it stops - the point
	// is that it stops on agreement, not on a timer.
	if snap.URL != "about:blank" {
		t.Errorf("settled on %q, want the first pair of agreeing reads", snap.URL)
	}
	if d.read != 2 {
		t.Errorf("took %d reads, want 2", d.read)
	}
	if clock.elapsed() != settlePoll {
		t.Errorf("waited %s, want one poll interval", clock)
	}
}

// A read that has left the page the action was taken on is a navigation - a full
// load or an SPA route change - and is taken as it is: a confirming second read
// is a whole driver spawn per step. A read still on that page takes the two.
func TestSettleTakesOneReadAfterANavigation(t *testing.T) {
	signin := readFixture(t, "signin.snapshot")
	from := ParseSnapshot(signin)
	other := "### Page\n- Page URL: http://x/next\n### Snapshot\n- button \"b\" [ref=e1]\n"
	// The sign-in page's own elements under the next page's address and title:
	// what a client-side transition shows between changing the URL and swapping
	// the body in.
	stale := strings.NewReplacer(from.URL, "http://x/next", from.Title, "Next").Replace(signin)
	for _, tc := range []struct {
		name      string
		from      Snapshot
		reads     []string
		wantReads int
		wantURL   string
	}{
		{"navigated", from, []string{other, signin}, 1, "http://x/next"},
		{"navigation lands on the second read", from, []string{signin, other, signin}, 2, "http://x/next"},
		{"same page", from, []string{signin, signin}, 2, from.URL},
		{"no page to compare with", Snapshot{}, []string{other, other}, 2, "http://x/next"},
		{"the address moved before the body", from, []string{stale, stale, other}, 3, "http://x/next"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newFakeClock()
			d := &fakeDriver{reads: tc.reads}
			opts := Options{Task: "t", MaxSteps: 1, transport: &scriptedTransport{}, Driver: d.run}
			clock.wire(&opts)
			l, err := newLoop(opts)
			if err != nil {
				t.Fatalf("newLoop: %v", err)
			}
			snap, err := l.settle(context.Background(), tc.from)
			if err != nil {
				t.Fatalf("settle: %v", err)
			}
			if d.read != tc.wantReads || snap.URL != tc.wantURL {
				t.Errorf("took %d reads to settle on %q, want %d on %q", d.read, snap.URL, tc.wantReads, tc.wantURL)
			}
		})
	}
}

// A client-side transition that changes the URL and the title and then never
// swaps the body in is judged anyway - at the deadline, the way a spinner is.
func TestSettleGivesUpOnABodyThatNeverFollowsItsAddress(t *testing.T) {
	signin := readFixture(t, "signin.snapshot")
	from := ParseSnapshot(signin)
	stale := strings.NewReplacer(from.URL, "http://x/next", from.Title, "Next").Replace(signin)
	clock := newFakeClock()
	d := &fakeDriver{reads: []string{stale}}
	opts := Options{Task: "t", MaxSteps: 1, transport: &scriptedTransport{}, Driver: d.run}
	clock.wire(&opts)
	l, err := newLoop(opts)
	if err != nil {
		t.Fatalf("newLoop: %v", err)
	}
	snap, err := l.settle(context.Background(), from)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if snap.URL != "http://x/next" || !clock.reached(settleDeadline) {
		t.Errorf("settled on %q after %s, want the moved page at the %s deadline", snap.URL, clock, settleDeadline)
	}
}

// A read taken while a client-side transition has changed the url and the
// title and not yet the body would hand the judgement the previous page's
// elements and text under the target's address, and the address is what it
// would believe. The judgement has to receive the page that landed.
func TestRunJudgesDoneOnTheElementsThatLanded(t *testing.T) {
	signin := readFixture(t, "signin.snapshot")
	from := ParseSnapshot(signin)
	stale := strings.NewReplacer(from.URL, "http://x/next", from.Title, "Next").Replace(signin)
	landed := "### Page\n- Page URL: http://x/next\n- Page Title: Next\n### Snapshot\n- button \"b\" [ref=e1]\n"
	tr := &scriptedTransport{rounds: []map[string]answer{
		{"pick0": {Choice: "f2e11", Confidence: 0.9}},
		{questionDone: {Noul: 0.95}},
	}}
	d := &fakeDriver{reads: []string{signin, signin, stale, landed}}
	res := runLoop(t, d, Options{transport: tr})
	if res.err != nil || res.code != ExitDone {
		t.Fatalf("run: code %d, err %v:\n%s", res.code, res.err, res.stderr)
	}
	if len(tr.requests) != 3 {
		t.Fatalf("requests: got %d, want the pick, its write guard and the judgement of where it landed", len(tr.requests))
	}
	st, _ := tr.requests[2].State.(state)
	if st.Page.URL != "http://x/next" || len(st.Elements) != 1 || st.Elements[0].Label != "b" {
		t.Errorf("the judgement received page %q with elements %+v, want the landed page's own button", st.Page.URL, st.Elements)
	}
	if d.read != 4 {
		t.Errorf("took %d reads, want the two that settled the start page, the stale one and the one that landed", d.read)
	}
}

// A caller that counts the processes behind each Driver call - a lease renew
// rides inside a click - gets that count as the step's spawns, not one per call.
func TestRunCountsSpawnsFromTheCaller(t *testing.T) {
	d := &fakeDriver{pages: []string{followPage}}
	var spawned int64
	driver := func(ctx context.Context, args ...string) (string, error) {
		spawned++
		if args[0] != "snapshot" {
			spawned++ // the renew in front of a driving verb
		}
		return d.run(ctx, args...)
	}
	var out bytes.Buffer
	opts := Options{
		Task: "t", transport: usageTransport{}, MaxSteps: 2, JSON: true,
		Driver: driver, Spawns: func() int64 { return spawned }, Out: &out, Err: &out,
	}
	newFakeClock().wire(&opts)
	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatalf("run: %v", err)
	}
	var steps []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(out.String()), "\n") {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("not JSON: %q: %v", line, err)
		}
		if _, ok := obj["step"]; ok {
			steps = append(steps, obj)
		}
	}
	// Step 2's line carries step 1's click (a renew and the verb) and the two
	// reads of a page whose URL the click did not change.
	if len(steps) != 2 || steps[0]["spawns"] != 2.0 || steps[1]["spawns"] != 4.0 {
		t.Errorf("step lines: %v", steps)
	}
	// verbs counts the Driver calls themselves, whatever they cost in processes.
	if len(steps) == 2 && (steps[0]["verbs"] != 2.0 || steps[1]["verbs"] != 3.0) {
		t.Errorf("step verbs: %v, %v", steps[0]["verbs"], steps[1]["verbs"])
	}
}

// A page that never stops changing - a spinner, a polling widget - must not stop
// the loop. The deadline is what makes that decidable.
func TestSettleGivesUpAtTheDeadline(t *testing.T) {
	churn := make([]string, 0, 100)
	for i := range 100 {
		churn = append(churn, "### Page\n- Page URL: http://x/\n### Snapshot\n- button \"b\" [ref=e"+string(rune('0'+i%10))+"]\n")
	}
	clock := newFakeClock()
	d := &fakeDriver{reads: churn}
	opts := Options{Task: "t", MaxSteps: 1, transport: &scriptedTransport{}, Driver: d.run}
	clock.wire(&opts)
	l, err := newLoop(opts)
	if err != nil {
		t.Fatalf("newLoop: %v", err)
	}
	if _, err := l.settle(context.Background(), Snapshot{}); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if !clock.reached(settleDeadline) {
		t.Errorf("gave up after %s, before the %s deadline", clock, settleDeadline)
	}
	if want := int(settleDeadline/settlePoll) + 1; d.read > want+1 {
		t.Errorf("took %d reads for a %s deadline at a %s poll", d.read, settleDeadline, settlePoll)
	}
}

// Refs go stale between the snapshot that mints them and the verb that uses
// them, and the symptom is a click timeout. One fresh read and one retry is the
// fix; writing the step down as failed is what stops the next step from pruning
// it away as done.
func TestRunRetriesOnceThenMarksTheStepFailed(t *testing.T) {
	tr := &scriptedTransport{rounds: []map[string]answer{
		{"pick0": {Choice: "f2e11", Confidence: 0.9}},
		{"pick0": {Choice: "f2e12", Confidence: 0.9}},
	}}
	clicks := 0
	d := &fakeDriver{
		pages: []string{readFixture(t, "signin.snapshot")},
		fail: func(args []string) error {
			if args[0] == "click" && args[1] == "f2e11" {
				clicks++
				return errStaleRef
			}
			return nil
		},
	}
	res := runLoop(t, d, Options{transport: tr, MaxSteps: 2})
	if res.err != nil {
		t.Fatalf("run: %v", res.err)
	}
	if clicks != 2 {
		t.Errorf("tried the failing click %d times, want 2 (the first plus one retry)", clicks)
	}
	if !strings.Contains(res.stdout, "action failed: TimeoutError") {
		t.Errorf("the failure was not reported:\n%s", res.stdout)
	}
	// The second step's state carries the failed entry, and the page it did not
	// move: the model reads it as a route that did not work, not one taken.
	if len(tr.requests) < 3 {
		t.Fatal("the loop stopped after the failure instead of trying again")
	}
	st, _ := tr.requests[2].State.(state)
	if len(st.History) != 1 || !st.History[0].Failed || st.History[0].Changed {
		t.Errorf("history: got %+v, want one entry marked failed and unchanged", st.History)
	}
}

// A verb can fail AFTER its action landed - a submit that went through and then
// timed out reading what came back looks exactly like one that never happened.
// The page having moved is the only evidence available, and it is enough to stop
// the retry: submitting twice is worse than leaving the model to judge the new
// page.
func TestRunDoesNotRepeatAnActionThePageMovedUnder(t *testing.T) {
	const page = "### Page\n- Page URL: http://127.0.0.1:8799/review\n### Snapshot\n- button \"Submit\" [ref=e11]\n"
	const after = page + "- link \"Receipt\" [ref=e12]\n"
	tr := &scriptedTransport{rounds: []map[string]answer{{"pick0": {Choice: "e11", Confidence: 0.9}}}}
	clicks := 0
	d := &fakeDriver{
		reads: []string{page, page, after},
		fail: func(args []string) error {
			if args[0] == "click" {
				clicks++
				return errStaleRef
			}
			return nil
		},
	}
	res := runLoop(t, d, Options{transport: tr, MaxSteps: 1})
	if res.err != nil {
		t.Fatalf("run: %v", res.err)
	}
	if clicks != 1 {
		t.Errorf("clicked %d times, want 1 - the retry must not repeat an action the page moved under", clicks)
	}
	if !strings.Contains(res.stdout, "taken as done rather than repeated") {
		t.Errorf("the skipped retry was not reported:\n%s", res.stdout)
	}
}

// A failed action has to reach a machine reader too: the step line is written
// before the action is taken, so without this the JSON log says an action was
// decided and never says the page refused it.
func TestRunWritesAFailedActionToTheJSONLog(t *testing.T) {
	tr := &scriptedTransport{rounds: []map[string]answer{{"pick0": {Choice: "f2e11", Confidence: 0.9}}}}
	d := &fakeDriver{
		pages: []string{readFixture(t, "signin.snapshot")},
		fail: func(args []string) error {
			if args[0] == "click" {
				return errStaleRef
			}
			return nil
		},
	}
	res := runLoop(t, d, Options{transport: tr, MaxSteps: 1, JSON: true})
	if res.err != nil {
		t.Fatalf("run: %v", res.err)
	}
	failed := false
	for line := range strings.SplitSeq(strings.TrimSpace(res.stdout), "\n") {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("not a JSON line: %q", line)
		}
		if obj["failed"] == true {
			failed = true
		}
	}
	if !failed {
		t.Errorf("no JSON line says the action failed:\n%s", res.stdout)
	}
}

// The pinned driver keeps handing out refs from before a `go-back`, and every
// click on one fails. A `goto` to where the back landed re-mints them, and the
// next step has to act on a ref from AFTER that goto.
func TestRunReGotosAfterBackAndUsesTheFreshRefs(t *testing.T) {
	const landed = "http://127.0.0.1:8799/list"
	poisoned := "### Page\n- Page URL: " + landed + "\n### Snapshot\n- link \"Next\" [ref=e1]\n"
	fresh := "### Page\n- Page URL: " + landed + "\n### Snapshot\n- link \"Next\" [ref=e5]\n"
	tr := &scriptedTransport{rounds: []map[string]answer{
		{"pick0": {Choice: backKey, Confidence: 0.9}},
		{"pick0": {Choice: "e5", Confidence: 0.9}},
	}}
	d := &fakeDriver{
		pages: []string{readFixture(t, "signin.snapshot"), poisoned, fresh},
		fail: func(args []string) error {
			if args[0] == "click" && args[1] == "e1" {
				return errPoisonedRef
			}
			return nil
		},
	}
	res := runLoop(t, d, Options{transport: tr, MaxSteps: 2})
	if res.err != nil {
		t.Fatalf("run: %v", res.err)
	}
	want := []string{"go-back", "goto " + landed, "click e5"}
	if got := d.actions(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("driver calls: got %q, want %q", got, want)
	}
	st, _ := tr.requests[1].State.(state)
	if len(st.Elements) != 1 || st.Elements[0].Ref != "e5" {
		t.Errorf("the step after back was offered %+v, want the ref the goto minted", st.Elements)
	}
}

// usageTransport is the mock with the model and usage a real API reports, so the
// --json totals have something to sum.
type usageTransport struct{ mockTransport }

func (u usageTransport) evaluate(ctx context.Context, req request) (response, error) {
	resp, err := u.mockTransport.evaluate(ctx, req)
	resp.Model, resp.Usage = "typesafe/jev-1.13-test", usage{InputTokens: 100, OutputTokens: 3}
	return resp, err
}

const followPage = "### Page\n- Page URL: https://example.test/company\n- Page Title: Acme\n### Snapshot\n" +
	"- button \"Follow\" [ref=e1]\n- link \"People\" [ref=e2]\n"

// --json is the machine-readable half: one object per step, then the outcome,
// both carrying where the time went.
func TestRunWritesJSONLines(t *testing.T) {
	d := &fakeDriver{pages: []string{followPage}}
	res := runLoop(t, d, Options{transport: usageTransport{}, MaxSteps: 1, JSON: true})
	if res.stderr != "" {
		t.Errorf("--json wrote prose to stderr:\n%s", res.stderr)
	}
	var lines []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(res.stdout), "\n") {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("not JSON: %q: %v", line, err)
		}
		lines = append(lines, obj)
	}
	step, last := lines[0], lines[len(lines)-1]
	for _, key := range []string{"model_ms", "api_calls", "driver_ms", "spawns", "verbs", "input_tokens"} {
		if v, ok := step[key].(float64); !ok || v < 0 {
			t.Errorf("step line %s: got %v", key, step[key])
		}
	}
	if step["api_calls"] != 1.0 || step["input_tokens"] != 100.0 {
		t.Errorf("step line: %v", step)
	}
	if step["spawns"] != 2.0 {
		t.Errorf("step spawns: got %v, want the two reads that settled the page", step["spawns"])
	}
	if last["outcome"] != "max-steps" {
		t.Errorf("outcome: got %v, want max-steps", last["outcome"])
	}
	if last["next"] != "cuttle pw snapshot" {
		t.Errorf("the outcome does not carry the handoff command: %v", last)
	}
	// The step's write guard and the final judgement after the budget are not
	// steps, but they are spent.
	if last["api_calls"] != 3.0 || last["input_tokens"] != 300.0 || last["output_tokens"] != 9.0 {
		t.Errorf("outcome totals: %v", last)
	}
	for _, key := range []string{"model_ms", "driver_ms", "spawns", "verbs", "elapsed_ms"} {
		if v, ok := last[key].(float64); !ok || v < 0 {
			t.Errorf("outcome %s: got %v", key, last[key])
		}
	}
	if last["model"] != "typesafe/jev-1.13-test" {
		t.Errorf("outcome model: got %v", last["model"])
	}
}

// extractTransport ends the run (done, unless browse says otherwise), then
// answers the per-line nouls: yes for the lines that name a product, no for
// everything else.
type extractTransport struct {
	wanted string
	// browse overrides the browsing step's answers, so the same fake can script a
	// run that ends blocked or out of budget instead of done.
	browse map[string]answer
	// broken is what the extract call fails with, for the endings where a failed
	// extract must not become the outcome.
	broken error
}

func (e extractTransport) evaluate(_ context.Context, req request) (response, error) {
	st, ok := req.State.(extractState)
	if !ok {
		// The browsing step: done on the first read unless browse scripts another
		// ending, so the run is only its extract.
		answers := map[string]answer{questionDone: {Noul: 0.99}}
		maps.Copy(answers, e.browse)
		for id, q := range req.Questions {
			if _, scripted := answers[id]; scripted {
				continue
			}
			answers[id] = answer{}
			if q.Type == typeChoice {
				answers[id] = answer{Choice: noneKey, Confidence: 1}
			}
		}
		return response{Answers: answers}, nil
	}
	if e.broken != nil {
		return response{}, e.broken
	}
	answers := make(map[string]answer, len(st.Lines))
	for i, line := range st.Lines {
		noul := 0.0
		if strings.Contains(line, e.wanted) {
			noul = 1
		}
		answers[fmt.Sprintf("l%d", i)] = answer{Noul: noul}
	}
	return response{Answers: answers}, nil
}

// The model cannot write, so an extract is the model SELECTING page lines and
// this code printing them verbatim.
func TestRunExtractsLinesVerbatim(t *testing.T) {
	d := &fakeDriver{pages: []string{readFixture(t, "signin.snapshot")}}
	res := runLoop(t, d, Options{transport: extractTransport{wanted: "Cart"}, Extract: "the cart link"})
	if res.err != nil {
		t.Fatalf("run: %v", res.err)
	}
	if res.code != ExitDone {
		t.Fatalf("exit code: got %d, want %d", res.code, ExitDone)
	}
	if !strings.Contains(res.stdout, "- Cart (0)") {
		t.Errorf("the chosen line was not printed verbatim:\n%s", res.stdout)
	}
	if strings.Contains(res.stdout, "- Sign in") {
		t.Errorf("a line the model said no to was printed:\n%s", res.stdout)
	}
}

// An extract's lifetime is the page the run ended on, not the ending that got
// there. A blocked run stopped on a page, and the lines the caller asked for are
// on it - which is also why the brief that follows still names that page.
func TestRunExtractsOnABlockedEnding(t *testing.T) {
	tr := extractTransport{wanted: "Cart", browse: map[string]answer{
		questionDone: {}, questionBlocked: {Noul: 0.99},
	}}
	d := &fakeDriver{pages: []string{readFixture(t, "signin.snapshot")}}
	res := runLoop(t, d, Options{transport: tr, Extract: "the cart link", MaxSteps: 1})
	if res.err != nil {
		t.Fatalf("run: %v", res.err)
	}
	if res.code != ExitBlocked {
		t.Fatalf("exit code: got %d, want %d - an extract must not change the outcome", res.code, ExitBlocked)
	}
	if !strings.Contains(res.stdout, "- Cart (0)") {
		t.Errorf("the blocked ending skipped the extract:\n%s", res.stdout)
	}
	if !strings.Contains(res.stderr, "page: <"+signinURL+">") {
		t.Errorf("the brief no longer names the page the lines came from:\n%s", res.stderr)
	}
}

// The budget running out is the ending most worth extracting from: the run got
// somewhere, it just ran out of steps to finish. --json carries the same extract
// object here as on a done run, ahead of the outcome.
func TestRunExtractsWhenTheBudgetRunsOut(t *testing.T) {
	tr := extractTransport{wanted: "Cart", browse: map[string]answer{
		questionDone: {}, "pick0": {Choice: "f2e11", Confidence: 0.9},
	}}
	d := &fakeDriver{pages: []string{readFixture(t, "signin.snapshot")}}
	res := runLoop(t, d, Options{transport: tr, Extract: "the cart link", MaxSteps: 1, JSON: true})
	if res.err != nil {
		t.Fatalf("run: %v", res.err)
	}
	if res.code != ExitMaxSteps {
		t.Fatalf("exit code: got %d, want %d", res.code, ExitMaxSteps)
	}
	var extracted, last map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(res.stdout), "\n") {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("not JSON: %q: %v", line, err)
		}
		if _, ok := obj["lines"]; ok {
			extracted = obj
		}
		last = obj
	}
	if extracted == nil {
		t.Fatalf("no extract object was emitted:\n%s", res.stdout)
	}
	if got := fmt.Sprint(extracted["lines"]); !strings.Contains(got, "Cart (0)") {
		t.Errorf("extracted lines: got %v, want the line the model picked", got)
	}
	if last["outcome"] != "max-steps" {
		t.Errorf("the extract replaced the outcome: %v", last)
	}
}

// errExtractRefused stands in for an extract call the API would not answer.
var errExtractRefused = errors.New("the API said no")

// An extract that fails on an ending that was already non-zero must not mask it:
// the caller branches on 3 and 4, and an exit 1 would cost it the one fact the
// run produced.
func TestRunExtractFailureKeepsTheExitCode(t *testing.T) {
	tr := extractTransport{
		browse: map[string]answer{questionDone: {}, questionBlocked: {Noul: 0.99}},
		broken: errExtractRefused,
	}
	d := &fakeDriver{pages: []string{readFixture(t, "signin.snapshot")}}
	res := runLoop(t, d, Options{transport: tr, Extract: "the cart link", MaxSteps: 1})
	if res.err != nil {
		t.Fatalf("run: %v, want the blocked outcome to stand", res.err)
	}
	if res.code != ExitBlocked {
		t.Fatalf("exit code: got %d, want %d", res.code, ExitBlocked)
	}
	if !strings.Contains(res.stderr, "the extract failed: the API said no") {
		t.Errorf("the failed extract was not reported:\n%s", res.stderr)
	}
	if !strings.Contains(res.stderr, "cuttle pw snapshot") {
		t.Errorf("the handoff brief was lost to the failed extract:\n%s", res.stderr)
	}
}

// The mock reaches an extract on every ending, and it has no judgement to pick
// lines with. Saying so up front beats a run that silently skips it.
func TestRunRefusesExtractWithTheMock(t *testing.T) {
	d := &fakeDriver{pages: []string{readFixture(t, "signin.snapshot")}}
	res := runLoop(t, d, Options{Mock: true, Extract: "the cart link"})
	if !errors.Is(res.err, errMockExtract) {
		t.Errorf("got %v, want errMockExtract", res.err)
	}
	if d.calls != nil {
		t.Errorf("the loop touched the driver anyway: %q", d.calls)
	}
}

// A filled field's value is not the page's words. Playwright renders it in full,
// password fields included, and after a `{{cuttle:NAME}}` fill it IS the
// substituted secret - so the one path that sends page lines to the API must
// never carry one.
func TestPageLinesDropFilledFieldValues(t *testing.T) {
	lines := pageLines(snapshotOf(
		`- textbox "User" [ref=e2]: alice@example.com`,
		`- textbox "Pass" [ref=e3]: topsecret999`,
		`- searchbox [ref=e4]: widgets`,
		`- combobox "Country" [ref=e5]: Germany`,
		`- paragraph [ref=e6]: Starter 10 USD`,
	).tree)
	for _, line := range lines {
		for _, value := range []string{"alice@example.com", "topsecret999", "widgets", "Germany"} {
			if strings.Contains(line, value) {
				t.Errorf("a filled field's value became a page line: %q", line)
			}
		}
	}
	if len(lines) != 1 || lines[0] != "Starter 10 USD" {
		t.Errorf("page lines: got %q, want only the page's own words", lines)
	}
}

func TestPageLinesStripTheYamlScaffolding(t *testing.T) {
	lines := pageLines(parseFixture(t, "signin.snapshot").tree)
	want := map[string]bool{"Sign in": true, "Cart (0)": true, "Item one": true}
	for _, line := range lines {
		delete(want, line)
		if strings.Contains(line, "[ref=") || strings.HasPrefix(line, "- ") {
			t.Errorf("a line kept its scaffolding: %q", line)
		}
		// A bare single word is a field label, not an item, and it would otherwise
		// cost a question per form field on every extract.
		if line == "Username" || line == "Password" {
			t.Errorf("a bare label was offered as a page line: %q", line)
		}
		// The capture's framing and a link's target are the driver talking.
		for _, framing := range []string{"###", "```", "Page URL", "Page Title", "/url"} {
			if strings.Contains(line, framing) {
				t.Errorf("the capture's framing was offered as a page line: %q", line)
			}
		}
	}
	if len(want) != 0 {
		t.Errorf("these page lines were lost: %v", want)
	}
}

// Piped output is what agents parse, so color must never reach it, and on a
// terminal it must only ever add styling to the same words.
func TestRunColorsOnlyWhenForced(t *testing.T) {
	ansiRE := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	run := func() runResult {
		tr := &scriptedTransport{rounds: []map[string]answer{{questionDone: {Noul: 0.95}}}}
		d := &fakeDriver{pages: []string{readFixture(t, "signin.snapshot")}}
		return runLoop(t, d, Options{transport: tr})
	}
	// The handoff brief carries the other half of the styling, and it prints to
	// stderr - which no other test would catch color leaking onto, since they all
	// look for substrings that survive being wrapped in escapes.
	brief := func() runResult {
		d := &fakeDriver{pages: []string{readFixture(t, "signin.snapshot")}}
		return runLoop(t, d, Options{Mock: true, MaxSteps: 1})
	}
	t.Setenv("NO_COLOR", "")
	t.Setenv("CLICOLOR_FORCE", "")
	plain, plainBrief := run(), brief()
	if want := "[1] Cuttle demo shop <http://127.0.0.1:8799/> done=0.95 blocked=0.00\n" +
		"    -> done (confidence 1.00)\n" +
		"the task is done at <http://127.0.0.1:8799/>\n"; plain.stdout != want {
		t.Errorf("plain stdout:\ngot  %q\nwant %q", plain.stdout, want)
	}
	if strings.Contains(plainBrief.stderr, "\x1b") {
		t.Errorf("plain stderr carries an escape: %q", plainBrief.stderr)
	}

	t.Setenv("CLICOLOR_FORCE", "1")
	colored, coloredBrief := run(), brief()
	if !strings.Contains(colored.stdout, "\x1b[") {
		t.Fatalf("CLICOLOR_FORCE did not color the output: %q", colored.stdout)
	}
	if !strings.Contains(coloredBrief.stderr, "\x1b[") {
		t.Fatalf("CLICOLOR_FORCE did not color the brief: %q", coloredBrief.stderr)
	}
	if got := strings.Replace(ansiRE.ReplaceAllString(colored.stdout, ""), "✓ ", "", 1); got != plain.stdout {
		t.Errorf("color changed the words:\ngot  %q\nwant %q", got, plain.stdout)
	}
	if got := strings.Replace(ansiRE.ReplaceAllString(coloredBrief.stderr, ""), "X ", "", 1); got != plainBrief.stderr {
		t.Errorf("color changed the brief:\ngot  %q\nwant %q", got, plainBrief.stderr)
	}

	t.Setenv("NO_COLOR", "1")
	if res := run(); res.stdout != plain.stdout {
		t.Errorf("NO_COLOR did not win over CLICOLOR_FORCE: %q", res.stdout)
	}
}

// The extract is the one path that sends page lines to the API, so everything
// that is not the page's own words has to stay out of it no matter how the
// driver quoted it: a field whose label holds ": " is single-quoted, a value
// holding one is double-quoted, a field with a placeholder renders its value as
// a child line, and the sections around the tree carry every tab's full URL.
func TestPageLinesNeverCarryValuesOrTabURLs(t *testing.T) {
	capture := strings.Join([]string{
		"### Open tabs",
		"- 0: (current) [Form](https://shop.example/form?token=tab-token-123)",
		"- 1: [Other](https://other.example/?session=tab-session-456)",
		"### Page",
		"- Page URL: https://shop.example/form?token=tab-token-123",
		"- Page Title: Form",
		"- Console: 0 errors, 1 warnings",
		"### Snapshot",
		"```yaml",
		`- 'textbox "Password: required" [ref=e8]': hunter2`,
		`- 'textbox "Notes: private" [ref=e9]': "secret note: xyz"`,
		`- textbox "Secret" [ref=e10]:`,
		`  - /placeholder: Enter it`,
		`  - text: placeholder-child-secret`,
		`- 'searchbox "Find: anything" [ref=e11]': searched-secret`,
		`- 'link "flate: avoid FMA in EstimatedBits" [ref=e40]':`,
		`  - /url: /golang/go/pull/81591?token=link-token-789`,
		`- textbox "Card [required" [ref=e13]:`,
		`  - /placeholder: 1234`,
		`  - text: bracket-child-secret`,
		`- textbox / [ref=e14]:`,
		`  - /placeholder: Search`,
		`  - text: slash-child-secret`,
		`- widget "an unparseable node" [ref=e15] [weird attr]:`,
		`  - text: opaque-child-secret`,
		`- paragraph [ref=e12]: "Price: 10 USD"`,
		"```",
		"### Events",
		"- console warning: https://shop.example/?token=event-token",
	}, "\n")
	lines := pageLines(ParseSnapshot(capture).tree)
	for _, line := range lines {
		for _, leak := range []string{"hunter2", "secret note", "placeholder-child-secret", "bracket-child-secret", "slash-child-secret", "opaque-child-secret", "searched-secret", "token", "session", "Console", "warning", "'", `"`} {
			if strings.Contains(line, leak) {
				t.Errorf("page line %q carries %q", line, leak)
			}
		}
	}
	want := []string{"flate: avoid FMA in EstimatedBits", "Price: 10 USD"}
	if !slices.Equal(lines, want) {
		t.Errorf("page lines: got %q, want %q", lines, want)
	}
}

// A "reach its X section" task is judged from the headings, and the table of
// contents is the route: the anchor link is offered, the model picks it, the
// next read carries the fragment - without waiting out the settle deadline,
// since the body it scrolls is the same body - and the model calls it done.
func TestRunReachesASectionThroughTheTableOfContents(t *testing.T) {
	article := readFixture(t, "article_toc.snapshot")
	section := strings.Replace(article, "/wiki/Lisbon\n", "/wiki/Lisbon#Geography\n", 1)
	tr := &scriptedTransport{rounds: []map[string]answer{
		{"pick0": {Choice: "e12", Confidence: 0.9}},
		{questionDone: {Noul: 0.9}},
	}}
	d := &fakeDriver{pages: []string{article, section}}
	res := runLoop(t, d, Options{transport: tr, Task: "reach the Geography section of the Lisbon article"})
	if res.err != nil || res.code != ExitDone {
		t.Fatalf("run: code %d, err %v, want %d:\n%s%s", res.code, res.err, ExitDone, res.stdout, res.stderr)
	}
	if got := d.actions(); !slices.Equal(got, []string{"click e12"}) {
		t.Errorf("driver calls: got %q, want the TOC link clicked", got)
	}
	if reads := len(d.calls) - len(d.actions()); reads != 4 {
		t.Errorf("took %d reads, want two per page: a fragment change is not a navigation to wait out", reads)
	}
	st, _ := tr.requests[0].State.(state)
	if !slices.Contains(st.Page.Headings, heading{Level: 2, Text: "Geography"}) {
		t.Errorf("the headings did not reach the request: %+v", st.Page.Headings)
	}
	if !slices.Contains(st.Page.Text, "Lisbon is the capital and largest city of Portugal.") {
		t.Errorf("the page text did not reach the request: %q", st.Page.Text)
	}
	options, _ := tr.requests[0].Questions["pick0"].Criteria.(map[string]string)
	for key, want := range map[string]string{
		"e12": "[navigation: Contents] link: Geography",
		"e13": "[navigation: Contents] button: Toggle Geography subsection",
		"e20": "[main] link: Lisbon",
	} {
		if options[key] != want {
			t.Errorf("option %s: got %q, want %q", key, options[key], want)
		}
	}
	landed, _ := tr.requests[2].State.(state)
	if landed.Page.URL != "https://example.test/wiki/Lisbon#Geography" {
		t.Errorf("the judgement received %q, want the page with the fragment", landed.Page.URL)
	}
	if len(landed.History) != 1 || !landed.History[0].Changed {
		t.Errorf("history: got %+v, want the click marked as having changed the page", landed.History)
	}
}

// Everything in the filters dialog is offered - the checkboxes, the saved
// filter, the Easy Apply toggle, the apply button - and the guard judges the
// one that was picked.
func TestRunGuardsTheChosenActionNotTheOffer(t *testing.T) {
	panel := readFixture(t, "filters_panel.snapshot")
	t.Run("applying filters is a filter", func(t *testing.T) {
		tr := &scriptedTransport{rounds: []map[string]answer{
			{"pick0": {Choice: "e19", Confidence: 0.9}},
			{questionWrite: {Noul: 0.05}},
		}}
		d := &fakeDriver{pages: []string{panel}}
		res := runLoop(t, d, Options{transport: tr, Task: "show only remote jobs", MaxSteps: 1})
		if res.err != nil {
			t.Fatalf("run: %v", res.err)
		}
		if got := d.actions(); !slices.Equal(got, []string{"click e19"}) {
			t.Errorf("driver calls: got %q, want the apply button clicked", got)
		}
		options, _ := tr.requests[0].Questions["pick0"].Criteria.(map[string]string)
		for key, want := range map[string]string{
			"e12": "[dialog: All filters] checkbox: Entry level",
			"e16": "[dialog: All filters] switch: Easy Apply filter.",
			"e17": "[dialog: All filters] button: Save filter",
			"e19": "[dialog: All filters] button: Apply current filters to show results",
		} {
			if options[key] != want {
				t.Errorf("option %s: got %q, want %q", key, options[key], want)
			}
		}
		if _, ok := options["e8"]; ok {
			t.Error("the page behind the open dialog was offered")
		}
		if asked := fmt.Sprint(tr.requests[1].Questions[questionWrite].Instructions); !strings.Contains(asked, `"[dialog: All filters] button: Apply current filters to show results"`) {
			t.Errorf("the guard did not ask about the pick by name: %s", asked)
		}
	})
	t.Run("saving a filter is a write", func(t *testing.T) {
		tr := &scriptedTransport{rounds: []map[string]answer{
			{"pick0": {Choice: "e17", Confidence: 0.9}},
			{questionWrite: {Noul: 0.9}},
			{"pick0": {Choice: "e17", Confidence: 0.9}},
			{questionWrite: {Noul: 0.9}},
		}}
		d := &fakeDriver{pages: []string{panel}}
		res := runLoop(t, d, Options{transport: tr, Task: "save this search"})
		if res.err != nil || res.code != ExitBlocked {
			t.Fatalf("run: code %d, err %v, want %d:\n%s", res.code, res.err, ExitBlocked, res.stderr)
		}
		if d.actions() != nil {
			t.Errorf("the loop took the write: %q", d.actions())
		}
		if !strings.Contains(res.stdout, "refused: it would change something on the site (write 0.90)") {
			t.Errorf("the refusal was not reported:\n%s", res.stdout)
		}
		// The re-pick sees the refusal; the second one hands the write to a person.
		st, _ := tr.requests[2].State.(state)
		if len(st.History) != 1 || !st.History[0].Refused || st.History[0].Action != "[dialog: All filters] button: Save filter" {
			t.Errorf("history: got %+v, want the refused pick", st.History)
		}
		if !strings.Contains(res.stderr, "the task needs a write action: [dialog: All filters] button: Save filter") {
			t.Errorf("the brief does not name the write the task needs:\n%s", res.stderr)
		}
	})
	t.Run("pay is refused without asking", func(t *testing.T) {
		tr := &scriptedTransport{rounds: []map[string]answer{{"pick0": {Choice: "e20", Confidence: 0.9}}}}
		d := &fakeDriver{pages: []string{panel}}
		res := runLoop(t, d, Options{transport: tr, Task: "unlock premium filters", MaxSteps: 1})
		if res.err != nil {
			t.Fatalf("run: %v", res.err)
		}
		if d.actions() != nil {
			t.Errorf("the loop took the write: %q", d.actions())
		}
		for _, req := range tr.requests {
			if _, asked := req.Questions[questionWrite]; asked {
				t.Error("the hard list was put to the model, whose answer must not override it")
			}
		}
		if !strings.Contains(res.stdout, `refused: "pay" is a write this loop never takes`) {
			t.Errorf("the refusal was not reported:\n%s", res.stdout)
		}
	})
}
