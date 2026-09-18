package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

const signinURL = "http://127.0.0.1:8799/"

// errStaleRef is what a ref that went stale actually looks like coming out of
// the driver: a click timeout, not a "stale ref" of any kind.
var errStaleRef = errors.New("TimeoutError: Timeout 5000ms exceeded")

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
	fail  func(call int, args []string) error

	page  int
	read  int
	calls []string
}

func (d *fakeDriver) run(_ context.Context, args ...string) (string, error) {
	call := len(d.calls)
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
		if err := d.fail(call, args); err != nil {
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
		fail: func(_ int, args []string) error {
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
type extractTransport struct{ wanted string }

func (e extractTransport) evaluate(_ context.Context, req request) (response, error) {
	answers := map[string]answer{}
	st, ok := req.State.(extractState)
	if !ok {
		return response{Answers: map[string]answer{questionDone: {Noul: 0.99}}}, nil
	}
	for i, line := range st.Lines {
		noul := 0.0
		if strings.Contains(line, e.wanted) {
			noul = 1
		}
		answers["l"+string(rune('0'+i))] = answer{Noul: noul}
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
	}
	if len(want) != 0 {
		t.Errorf("these page lines were lost: %v", want)
	}
}
