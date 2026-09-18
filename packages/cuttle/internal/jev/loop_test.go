package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
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

// A `none` with no prepared values is the CALLER's to fix: the action space held
// no typable option at all, so a page whose only route forward is a form had
// nothing to offer and the brief would otherwise read as the page's fault.
func TestRunBlockedBriefExplainsAnEmptyActionSpace(t *testing.T) {
	d := &fakeDriver{pages: []string{readFixture(t, "signin.snapshot")}}
	res := runLoop(t, d, Options{transport: &scriptedTransport{}, MaxSteps: 1})
	if res.code != ExitBlocked {
		t.Fatalf("exit code: got %d, want %d", res.code, ExitBlocked)
	}
	if !strings.Contains(res.stderr, "no --text values were supplied") {
		t.Errorf("the brief does not explain why nothing typable was offered:\n%s", res.stderr)
	}
}

// The hint answers one diagnosis only. With values supplied, or with nothing
// typable on the page, `none` means what it says.
func TestRunBlockedBriefOmitsTheHintWhenItWouldNotHelp(t *testing.T) {
	withValues := runLoop(t, &fakeDriver{pages: []string{readFixture(t, "signin.snapshot")}},
		Options{transport: &scriptedTransport{}, MaxSteps: 1, Values: map[string]string{"pass": "x"}})
	if strings.Contains(withValues.stderr, "--text") {
		t.Errorf("the hint fired with values supplied:\n%s", withValues.stderr)
	}
	const noFields = "### Page\n- Page URL: " + signinURL + "\n### Snapshot\n- button \"Pay\" [ref=e1]\n"
	noTypables := runLoop(t, &fakeDriver{pages: []string{noFields}},
		Options{transport: &scriptedTransport{}, MaxSteps: 1})
	if strings.Contains(noTypables.stderr, "--text") {
		t.Errorf("the hint fired on a page with nothing typable:\n%s", noTypables.stderr)
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
	if !strings.Contains(res.stderr, "nothing on this page makes progress") {
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
	want := []string{"fill f2e7 {{cuttle:DEMO_PASS}}", "press Tab"}
	got := d.actions()
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
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
	for _, want := range []string{"gave up after 1 steps", signinURL, "cuttle pw snapshot"} {
		if !strings.Contains(res.stderr, want) {
			t.Errorf("the brief is missing %q:\n%s", want, res.stderr)
		}
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
	snap, err := l.settle(context.Background())
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
	if _, err := l.settle(context.Background()); err != nil {
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
	// The second step's state carries the failed entry, which is what keeps the
	// route in the action space instead of pruning it as already done.
	if len(tr.requests) < 2 {
		t.Fatal("the loop stopped after the failure instead of trying again")
	}
	st, _ := tr.requests[1].State.(state)
	if len(st.History) != 1 || !st.History[0].Failed {
		t.Errorf("history: got %+v, want one entry marked failed", st.History)
	}
}

// A verb can fail AFTER its action landed - a submit that went through and then
// timed out reading what came back looks exactly like one that never happened.
// The page having moved is the only evidence available, and it is enough to stop
// the retry: pressing Pay twice is worse than leaving the model to judge the new
// page.
func TestRunDoesNotRepeatAnActionThePageMovedUnder(t *testing.T) {
	const page = "### Page\n- Page URL: http://127.0.0.1:8799/pay\n### Snapshot\n- button \"Pay\" [ref=e11]\n"
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

// --json is the machine-readable half: one object per step, then the outcome.
func TestRunWritesJSONLines(t *testing.T) {
	d := &fakeDriver{pages: []string{readFixture(t, "signin.snapshot")}}
	res := runLoop(t, d, Options{Mock: true, MaxSteps: 1, JSON: true})
	if res.stderr != "" {
		t.Errorf("--json wrote prose to stderr:\n%s", res.stderr)
	}
	var last map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(res.stdout), "\n") {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("not JSON: %q: %v", line, err)
		}
		last = obj
	}
	if last["outcome"] != "max-steps" {
		t.Errorf("outcome: got %v, want max-steps", last["outcome"])
	}
	if last["next"] != "cuttle pw snapshot" {
		t.Errorf("the outcome does not carry the handoff command: %v", last)
	}
}

// extractTransport says the task is done, then answers the per-line nouls: yes
// for the lines that name a product, no for everything else.
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
		// The browsing step: done on the first read, so the run is only its extract.
		answers := map[string]answer{questionDone: {Noul: 0.99}, questionBlocked: {}}
		maps.Copy(answers, e.browse)
		for id, q := range req.Questions {
			if _, scripted := answers[id]; !scripted && q.Type == typeChoice {
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

// The mock reaches an extract on every ending now, and it has no judgement to
// pick lines with. Saying so up front beats a run that silently skips it.
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
	lines := pageLines(strings.Join([]string{
		`- textbox "User" [ref=e2]: alice@example.com`,
		`- textbox "Pass" [ref=e3]: topsecret999`,
		`- searchbox [ref=e4]: widgets`,
		`- combobox "Country" [ref=e5]: Germany`,
		`- paragraph [ref=e6]: Starter 10 USD`,
	}, "\n"))
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
	lines := pageLines(readFixture(t, "signin.snapshot"))
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
