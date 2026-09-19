package jev

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"golang.org/x/term"
)

// Exit codes. Anything a caller has to branch on is a code, not a parsed line of
// output. 2 is deliberately skipped: it is what a Go binary exits with on a
// panic, and no caller should read a crash as one of these outcomes.
const (
	ExitDone     = 0 // the task is done
	ExitError    = 1 // a usage or infrastructure error; the message says which
	ExitBlocked  = 3 // progress needs a person: a dialog, a captcha, a dead end
	ExitMaxSteps = 4 // --max-steps ran out with the task unfinished
)

// settlePoll and settleDeadline bound how long the page is re-read for after an
// action. There are no fixed sleeps anywhere in the loop: a navigation costs one
// read, an in-page change that is already still costs two reads and the one poll
// between them, and one that never stops changing - a spinner, a polling widget -
// or a navigation whose body never follows its address costs the deadline and
// then gets acted on anyway.
const (
	settlePoll     = 250 * time.Millisecond
	settleDeadline = 5 * time.Second
)

// extractBatch is how many page lines are judged in one request. Roughly this
// many short lines and their questions fit comfortably in one request.
const extractBatch = 15

// extractThreshold is the probability a line's noul must clear to be printed.
// Unlike ending a run, printing one line too many is cheap to see and ignore, so
// it sits below even odds: on a real 25-item results list every item scored
// 0.41 or more and every other line 0.32 or less, and at 0.5 a third of the
// items were lost.
const extractThreshold = 0.4

var (
	errTaskRequired = errors.New("--task is required")
	errNoSteps      = errors.New("--max-steps must be at least 1")
	errNoStartPage  = errors.New("the session has no page - pass --url, or navigate first with cuttle pw")
	// An extract runs on whatever page the run ended on, so the mock reaches one
	// on every ending - and it has no judgement to pick lines with.
	errMockExtract = errors.New("--extract needs the model's judgement and cannot run with --mock")
	// errBadAnswer covers the two ways an answer is unusable once it is back: a
	// key that does not parse, and a value name nobody supplied. Neither is the
	// driver's doing, so neither is reported as a driver failure.
	errBadAnswer = errors.New("the model's answer")
)

// Runner performs one bundled-driver verb and returns its combined output. The
// output must come back even on a non-zero exit: a pending native dialog makes
// `snapshot` an error whose body carries the block naming the dialog.
type Runner func(ctx context.Context, args ...string) (string, error)

// Options is one run of the loop.
type Options struct {
	Task     string
	URL      string // optional page to start from
	MaxSteps int
	Extract  string // what to read off the final page, or empty
	JSON     bool
	Mock     bool
	// Values are the prepared values that may be typed, by name. Only the NAMES
	// are ever sent: which field a value belongs in is a judgement, what the
	// value is, is not. Values pass to the driver verbatim, which is what makes
	// cuttle's `{{cuttle:NAME}}` secret sentinels work - they are substituted
	// inside cuttle, on the fill path, so the real value never enters this
	// process at all.
	Values map[string]string
	// Cuttle is the invocation that reaches the instance being driven, such as
	// "cuttle --name scraper", so the handoff command lands on the same browser.
	// Empty means the default instance.
	Cuttle string
	Driver Runner
	// Spawns, when set, is a running count of the processes started for the run -
	// a lease renew or a re-attach rides inside one Driver call - read around each
	// call, so the per-step `spawns` includes a heartbeat renew that overlaps it.
	// Without it every Driver call counts as one.
	Spawns func() int64
	Out    io.Writer
	Err    io.Writer

	// transport and clock are test seams. Production always takes the defaults:
	// the mock or the HTTP transport, and the real clock.
	transport transport
	now       func() time.Time
	sleep     func(time.Duration)
}

// Run drives the browser until the task is done, progress is impossible, or the
// step budget runs out. The browser session is left exactly where it stopped, so
// whatever the outcome a person or an agent can pick it up with `cuttle pw`.
func Run(ctx context.Context, opts Options) (int, error) {
	l, err := newLoop(opts)
	if err != nil {
		return ExitError, err
	}
	return l.run(ctx)
}

type loop struct {
	Options
	valueNames []string
	history    []Step
	out, err   painter

	// step counts what was spent since the previous step line, total what the
	// whole run spent. A step line is printed before its action, so the action's
	// driver calls land on the next line - and in the total either way.
	step, total runStats
	model       string
	started     time.Time
}

// runStats is the time and usage behind a run, so a bench can split a step into
// model time and driver time without instrumenting either from outside.
type runStats struct {
	modelMS, driverMS         int64
	apiCalls, spawns, verbs   int
	inputTokens, outputTokens int
}

func (l *loop) count(f func(*runStats)) {
	f(&l.step)
	f(&l.total)
}

// timedTransport is the transport with its calls counted and timed. A runoff is
// a second call and counts as one; the HTTP transport's own retries do not.
type timedTransport struct {
	inner transport
	l     *loop
}

func (t timedTransport) evaluate(ctx context.Context, req request) (response, error) {
	start := t.l.now()
	resp, err := t.inner.evaluate(ctx, req)
	ms := t.l.now().Sub(start).Milliseconds()
	t.l.count(func(s *runStats) {
		s.modelMS += ms
		s.apiCalls++
		s.inputTokens += resp.Usage.InputTokens
		s.outputTokens += resp.Usage.OutputTokens
	})
	if resp.Model != "" {
		t.l.model = resp.Model
	}
	return resp, err
}

func newLoop(opts Options) (*loop, error) {
	if strings.TrimSpace(opts.Task) == "" {
		return nil, errTaskRequired
	}
	// A budget of zero used to be accepted: the loop never read the page at all
	// and still printed a handoff brief for a page it had never seen.
	if opts.MaxSteps < 1 {
		return nil, errNoSteps
	}
	if opts.Mock && opts.Extract != "" {
		return nil, errMockExtract
	}
	if opts.Cuttle == "" {
		opts.Cuttle = "cuttle"
	}
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	if opts.Err == nil {
		opts.Err = os.Stderr
	}
	if opts.now == nil {
		opts.now = time.Now
	}
	if opts.sleep == nil {
		opts.sleep = time.Sleep
	}
	if opts.transport == nil {
		if opts.Mock {
			opts.transport = mockTransport{}
		} else {
			t, err := newHTTPTransport()
			if err != nil {
				return nil, err
			}
			opts.transport = t
		}
	}
	names := make([]string, 0, len(opts.Values))
	for name := range opts.Values {
		names = append(names, name)
	}
	slices.Sort(names)
	l := &loop{Options: opts, valueNames: names, out: painterFor(opts.Out), err: painterFor(opts.Err)}
	l.transport = timedTransport{inner: opts.transport, l: l}
	driver := opts.Driver
	l.Driver = func(ctx context.Context, args ...string) (string, error) {
		start, spawned := l.now(), l.spawned()
		out, err := driver(ctx, args...)
		ms := l.now().Sub(start).Milliseconds()
		spawns := int(l.spawned() - spawned)
		if l.Spawns == nil {
			spawns = 1
		}
		l.count(func(s *runStats) {
			s.driverMS += ms
			s.spawns += spawns
			s.verbs++
		})
		return out, err
	}
	return l, nil
}

func (l *loop) spawned() int64 {
	if l.Spawns == nil {
		return 0
	}
	return l.Spawns()
}

func (l *loop) run(ctx context.Context) (int, error) {
	l.started = l.now()
	if l.URL != "" {
		if out, err := l.Driver(ctx, "goto", l.URL); err != nil {
			// A page that raises a dialog while it loads never finishes loading, so
			// the goto fails - but the dialog is the ending, and a fresh read is what
			// can name it.
			if snap, serr := l.snapshot(ctx); serr == nil && snap.Modal != "" {
				return l.parked(ctx, snap), nil
			}
			return ExitError, fmt.Errorf("goto %s: %w", l.URL, driverErr(out, err))
		}
	}

	var snap Snapshot
	for step := 1; step <= l.MaxSteps; step++ {
		// The page the last action was taken on; the first step has none, and
		// settles the long way.
		from := snap
		var err error
		snap, err = l.settle(ctx, snap)
		if err != nil {
			return ExitError, err
		}
		if snap.Modal != "" {
			// Recognizing a dialog is worth more than any action the loop could
			// take next.
			return l.parked(ctx, snap), nil
		}
		// Without --url the run starts wherever the session already is, and a fresh
		// session is a blank tab: there is nothing to read, nothing to pick, and the
		// run would spend a step and a request to say so.
		if step == 1 && l.URL == "" && blankPage(snap.URL) {
			return ExitError, errNoStartPage
		}
		// This read is the first look at what the last action did to the page.
		if n := len(l.history); n > 0 && !l.history[n-1].Refused {
			l.history[n-1].Changed = snap.signature() != from.signature()
		}

		candidates := actionSpace(snap, l.valueNames)
		st := l.state(snap)
		dec, err := decide(ctx, l.transport, st, group(candidates))
		if err != nil {
			return ExitError, err
		}

		switch {
		case dec.Done >= doneThreshold:
			l.report(step, snap, dec, "done")
			return l.done(ctx, snap)
		case dec.Blocked >= doneThreshold:
			l.report(step, snap, dec, "blocked")
			return l.stop(ctx, ExitBlocked, snap, "the task needs an action this loop cannot take"), nil
		case dec.Key == noneKey || dec.Key == "":
			l.report(step, snap, dec, noneKey)
			return l.stop(ctx, ExitBlocked, snap, "the model found no useful action on this page"), nil
		}

		chosen, ok := find(candidates, dec.Key)
		if !ok {
			// The model is only ever offered this page's candidates, so a key that is
			// not one of them is a malformed answer aimed at an element nobody
			// vouched for - and acting on it would aim a prepared value at nothing.
			l.report(step, snap, dec, dec.Key)
			return l.stop(ctx, ExitBlocked, snap, fmt.Sprintf("chose %q, which this page did not offer", dec.Key)), nil
		}
		l.report(step, snap, dec, chosen.Label)
		refusal, err := l.guard(ctx, st, snap, chosen)
		if err != nil {
			return ExitError, err
		}
		if refusal != "" {
			// The model re-picks with the refusal in `history`. A second refusal on
			// the same page means the task itself needs the write, and that is a
			// person's to take: the brief hands the session over at this page.
			if slices.ContainsFunc(l.history, func(h Step) bool { return h.Refused && h.URL == snap.URL }) {
				return l.stop(ctx, ExitBlocked, snap, "the task needs a write action: "+chosen.Label), nil
			}
			entry := Step{URL: snap.URL, Action: chosen.Label, Refused: true}
			l.history = append(l.history, entry)
			l.note(entry, refusal)
			continue
		}
		if err := l.act(ctx, snap, chosen); err != nil {
			return ExitError, err
		}
	}
	// Every other exit is decided from a fresh read, but this one is reached by
	// acting: the last step navigated after the snapshot above was taken, so that
	// snapshot names the page the session has already left. The brief promises the
	// session is live at exactly the page it names, so read once more before
	// making that promise - and keep the last known page if the read fails, since
	// a brief naming the wrong page still beats no brief at all.
	//
	// That read is also the first look at where the last action landed, and a
	// budget that ran out ON the goal page is a task done, not one given up on.
	if final, err := l.settle(ctx, snap); err == nil {
		snap = final
		if snap.Modal != "" {
			return l.parked(ctx, snap), nil
		}
		// A failed judgement here costs only the upgrade: the budget did run out.
		// It is not a step of its own, so it prints none: the outcome says done.
		if dec, err := decide(ctx, l.transport, l.state(snap), group(actionSpace(snap, l.valueNames))); err == nil && dec.Done >= doneThreshold {
			return l.done(ctx, snap)
		}
	}
	return l.stop(ctx, ExitMaxSteps, snap, fmt.Sprintf("gave up after %s with the task unfinished", plural(l.MaxSteps, "step"))), nil
}

// guard is the write guard on the chosen action. A control whose own name
// carries an irreversible verb is refused outright; anything else - Enter
// included, since it submits whatever holds the focus - is put to the model as
// one more noul against the same state, and refused past writeThreshold. Back
// only ever navigates, so it is not asked about. The reason comes back worded
// for the step log, empty when the action may be taken.
func (l *loop) guard(ctx context.Context, st state, snap Snapshot, chosen candidate) (string, error) {
	if chosen.Key == backKey {
		return "", nil
	}
	// A pick refused on this page once is refused again without asking: the
	// noul is a probability, and a second draw that lands under the threshold
	// would take the very write the first one refused.
	if slices.ContainsFunc(l.history, func(h Step) bool { return h.Refused && h.URL == snap.URL && h.Action == chosen.Label }) {
		return "refused: already refused on this page", nil
	}
	if verb := hardDeniedPick(snap, chosen.Key); verb != "" {
		return fmt.Sprintf("refused: %q is a write this loop never takes", verb), nil
	}
	resp, err := l.transport.evaluate(ctx, request{State: st, Questions: map[string]question{questionWrite: writeQuestion(chosen.Label)}})
	if err != nil {
		return "", err
	}
	a, err := answered(resp, questionWrite)
	if err != nil {
		return "", err
	}
	if a.Noul >= writeThreshold {
		return fmt.Sprintf("refused: it would change something on the site (write %.2f)", a.Noul), nil
	}
	return "", nil
}

// parked ends a run on a native dialog. It parks the renderer, so every read
// after it is a read of a stale page, and the modal block itself names the verb
// that clears it (dialog-accept / dialog-dismiss).
func (l *loop) parked(ctx context.Context, snap Snapshot) int {
	return l.stop(ctx, ExitBlocked, snap, "the page is parked behind a native dialog: "+snap.Modal)
}

// done ends a run whose task is done, extracting first when asked to.
func (l *loop) done(ctx context.Context, snap Snapshot) (int, error) {
	if l.Extract != "" {
		// The task itself is done, and the session is sitting on the page it
		// was done on, so say so even though the extract is what failed:
		// otherwise the one useful fact - where to pick it up - is lost.
		if err := l.extract(ctx, snap); err != nil {
			return ExitError, fmt.Errorf("the task is done at <%s>, but the extract failed: %w", snap.URL, err)
		}
	}
	return l.stop(ctx, ExitDone, snap, "the task is done"), nil
}

func (l *loop) state(snap Snapshot) state {
	text := pageLines(snap.tree)
	return state{
		Task:     l.Task,
		Values:   l.valueNames,
		Agent:    factsFor(l.valueNames),
		Page:     pageState{URL: snap.URL, Title: snap.Title, Headings: snap.Headings, Text: text[:min(len(text), stateLines)]},
		History:  l.history,
		Elements: snap.Elements,
	}
}

// settle reads the page until it stops changing: two consecutive snapshots with
// the same identity and the same handles. It is the only thing between an action
// and the next decision, and it replaces the fixed sleep a loop would otherwise
// need after every click. from is the page the action was taken on: a read that
// has left its URL is a navigation, and the driver only answers one once the new
// document - or an SPA's new route - is there, so it is taken without a second,
// confirming read - unless it still carries from's own handles. A client-side
// transition between two apps of a site changes the URL and the title first and
// swaps the body in seconds later, and a read in between is the old page under
// the new address: judged as it stands, its url and title say the target is
// reached about elements that belong to the page before it. Such a read is not
// settled, and is re-read until the handles move or the deadline passes. A zero
// from always takes the two reads. A link to a section of the page changes only
// the fragment, and the body it scrolls is the same body, so that is not a move
// - unless the fragment is a hash router's route, which swaps the body like any
// navigation.
func (l *loop) settle(ctx context.Context, from Snapshot) (Snapshot, error) {
	deadline := l.now().Add(settleDeadline)
	var prev Snapshot
	settled := false
	for {
		snap, err := l.snapshot(ctx)
		if err != nil {
			return Snapshot{}, err
		}
		// A dialog is a state, not a transient: it will not settle on its own, and
		// nothing behind it can be read until it is cleared.
		if snap.Modal != "" {
			return snap, nil
		}
		moved := from.URL != "" && snap.URL != "" && beforeFragment(snap.URL) != beforeFragment(from.URL)
		switch {
		case moved && snap.handles() != from.handles():
			return snap, nil
		case !moved && settled && prev.signature() == snap.signature():
			return snap, nil
		}
		prev, settled = snap, true
		if !l.now().Before(deadline) {
			return snap, nil
		}
		l.sleep(settlePoll)
	}
}

// beforeFragment is the address without its section fragment. A `#/` or `#!`
// fragment is a single-page app's route, not a section, and stays.
func beforeFragment(url string) string {
	base, fragment, _ := strings.Cut(url, "#")
	if strings.HasPrefix(fragment, "/") || strings.HasPrefix(fragment, "!") {
		return url
	}
	return base
}

func (l *loop) snapshot(ctx context.Context) (Snapshot, error) {
	out, err := l.Driver(ctx, "snapshot")
	snap := ParseSnapshot(out)
	// The driver exits non-zero on a modal snapshot, and the block naming the
	// dialog rides along with that error. Treating the modal as the result is
	// what keeps the recovery path readable.
	if err != nil && snap.Modal == "" {
		return Snapshot{}, fmt.Errorf("playwright-cli snapshot: %w", driverErr(out, err))
	}
	return snap, nil
}

// driverErr keeps the driver's own words on the error. A failed exec carries
// nothing but "exit status 1"; what actually went wrong - a host that does not
// resolve, a ref that no longer does - is in the output the runner captured,
// which is why the Runner contract returns it even on a non-zero exit.
func driverErr(out string, err error) error {
	if err == nil {
		return nil
	}
	if msg := errorSection(out); msg != "" {
		return fmt.Errorf("%w: %s", err, msg)
	}
	if line := firstLine(out); line != "" {
		return fmt.Errorf("%w: %s", err, line)
	}
	return err
}

// errorSection reads the message out of the driver's `### Error` block: the
// header alone says only that something failed, and the call log after the
// message is playwright retracing its steps.
func errorSection(out string) string {
	_, body, ok := strings.Cut(out, "### Error\n")
	if !ok {
		return ""
	}
	var msg []string
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "Call log:" || strings.HasPrefix(line, "### ") {
			break
		}
		msg = append(msg, line)
	}
	return strings.Join(msg, " ")
}

// act performs the chosen action and records it. A driver failure is not fatal:
// the page can re-render between the snapshot that mints a ref and the verb that
// uses it, and the symptom is a click timeout on an element the ref now resolves
// to something else. So a failure is answered with a fresh read and ONE retry,
// re-aimed at the same element by its label, and only then written down as
// failed, so the model reads it as a route that did not work rather than one
// that was taken. That retry is skipped when the fresh read shows a page that
// moved: the action probably landed, and repeating it could take it twice.
func (l *loop) act(ctx context.Context, snap Snapshot, chosen candidate) error {
	entry := Step{URL: snap.URL, Action: chosen.Label}
	failure := l.perform(ctx, snap, chosen.Key)
	if failure != nil {
		fresh, err := l.settle(ctx, snap)
		if err != nil {
			return err
		}
		retry, ok := reaim(chosen, snap, fresh)
		switch {
		case fresh.signature() != snap.signature():
			// The page moved under the failed verb, so the action may well have
			// landed - a submit that went through and then timed out reading what
			// came back is exactly this shape. A second attempt could submit twice,
			// which is not a risk a retry may take, so the step is written down as
			// taken and the next one judges wherever that left the session.
			l.note(entry, "the action failed but the page changed - taken as done rather than repeated")
		case !ok || l.perform(ctx, fresh, retry) != nil:
			entry.Failed = true
			l.note(entry, "action failed: "+firstLine(failure.Error()))
		}
	}
	l.history = append(l.history, entry)
	return nil
}

func (l *loop) perform(ctx context.Context, snap Snapshot, key string) error {
	switch {
	case key == backKey:
		if out, err := l.Driver(ctx, "go-back"); err != nil {
			return driverErr(out, err)
		}
		return l.remintAfterBack(ctx, snap)
	case key == enterKey:
		out, err := l.Driver(ctx, "press", "Enter")
		return driverErr(out, err)
	case strings.HasPrefix(key, typeKeyPrefix):
		ref, name, ok := strings.Cut(strings.TrimPrefix(key, typeKeyPrefix), ":")
		if !ok {
			return fmt.Errorf("%w: malformed type key %q", errBadAnswer, key)
		}
		value, ok := l.Values[name]
		if !ok {
			return fmt.Errorf("%w: chose the value %q, which was not supplied", errBadAnswer, name)
		}
		// `fill` is the only verb cuttle's secret sentinels survive: anything that
		// types per character never lets `{{cuttle:NAME}}` reassemble. Focus stays
		// in the field, so the Enter offered next submits what was typed.
		out, err := l.Driver(ctx, "fill", ref, value)
		return driverErr(out, err)
	default:
		out, err := l.Driver(ctx, "click", key)
		return driverErr(out, err)
	}
}

// remintAfterBack works around a defect in @playwright/cli 0.1.20, the driver
// pinned as cli.BundledPlaywrightCLIVersion: after `go-back`, `snapshot` keeps
// emitting refs from the pre-navigation frame generation and `click` rejects
// every one ("Ref ... not found in the current page snapshot"). Another snapshot
// does not recover; only a fresh `goto` re-mints working refs. `back` is offered
// on every page, so without this one back would brick every later click. Re-check
// the defect when that pin moves, and delete this once it is fixed upstream.
func (l *loop) remintAfterBack(ctx context.Context, from Snapshot) error {
	landed, err := l.settle(ctx, from)
	if err != nil {
		return err
	}
	// A dialog the back raised cannot be navigated past; the next step reads it
	// and stops on it.
	if landed.Modal != "" || landed.URL == "" {
		return nil
	}
	out, err := l.Driver(ctx, "goto", landed.URL)
	return driverErr(out, err)
}

// reaim points a retry at the same element in a fresh snapshot. Refs are minted
// per snapshot, so after a re-render the element is still there under a new one;
// the label is what survives.
func reaim(chosen candidate, from, fresh Snapshot) (string, bool) {
	was, ok := from.element(refOf(chosen.Key))
	if !ok {
		return "", false
	}
	now, ok := fresh.element(was.Ref)
	if !ok || now.Role != was.Role || now.Label != was.Label {
		i := slices.IndexFunc(fresh.Elements, func(el Element) bool {
			return el.Role == was.Role && el.Label == was.Label
		})
		if i < 0 {
			return "", false
		}
		now = fresh.Elements[i]
	}
	return strings.Replace(chosen.Key, was.Ref, now.Ref, 1), true
}

// refOf reads the element ref out of an option key, which is the key itself for
// a click and the middle field of `type:<ref>:<name>`.
func refOf(key string) string {
	if rest, ok := strings.CutPrefix(key, typeKeyPrefix); ok {
		ref, _, _ := strings.Cut(rest, ":")
		return ref
	}
	return key
}

// ---------------------------------------------------------------- the extract

// extract answers `--extract` without generating a word: the model judges each
// line of the page against what was asked for, and the lines it says yes to are
// printed verbatim.
func (l *loop) extract(ctx context.Context, snap Snapshot) error {
	lines := pageLines(snap.tree)
	var picked []string
	for start := 0; start < len(lines); start += extractBatch {
		batch := lines[start:min(start+extractBatch, len(lines))]
		questions := make(map[string]question, len(batch))
		for i, line := range batch {
			questions[fmt.Sprintf("l%d", i)] = question{
				Type: typeNoul,
				// The line is quoted into its own question, not only addressed by
				// index: judged by index alone, the model lost track of which line of
				// the batch it was asked about, and recall on a real results list
				// was a quarter of what it is quoted.
				Instructions: fmt.Sprintf("Is the line %q (`lines[%d]`) one individual item of the kind `wanted` describes? The lines are the page's text with its markup removed, so an item's title often stands on a line of its own.", line, i),
				Criteria: noulCriteria{
					True:  "The line names one individual item of that kind - one entry of the page's list, table or results - and does not lack a detail `wanted` asks for",
					False: "The line is not one such item: it is navigation, a heading or section name, a button or form label, a count, a sort or filter control, or prose about the page - or it lacks a detail `wanted` asks for",
				},
			}
		}
		resp, err := l.transport.evaluate(ctx, request{
			State:     extractState{Wanted: l.Extract, Lines: batch},
			Questions: questions,
		})
		if err != nil {
			return err
		}
		for i, line := range batch {
			a, err := answered(resp, fmt.Sprintf("l%d", i))
			if err != nil {
				return err
			}
			if a.Noul >= extractThreshold {
				picked = append(picked, line)
			}
		}
	}
	if l.JSON {
		return l.emit(map[string]any{"extract": l.Extract, "lines": picked})
	}
	fmt.Fprintf(l.Out, "extracted %d of %d lines\n", len(picked), len(lines))
	for _, line := range picked {
		fmt.Fprintf(l.Out, "- %s\n", line)
	}
	return nil
}

// noteExtractFailed reports an extract that failed on an ending that was already
// going to be non-zero. Masking a blocked or out-of-budget run with an exit 1
// would cost the caller the outcome it branches on, so this says what was lost
// and nothing else changes.
func (l *loop) noteExtractFailed(err error) {
	if l.JSON {
		_ = l.emit(map[string]any{"extract": l.Extract, "failed": true, "trouble": firstLine(err.Error())})
		return
	}
	fmt.Fprintf(l.Err, "%s %s\n", l.err.paint("cuttle jev-browse: the extract failed:", yellow), firstLine(err.Error()))
}

// extractState is the whole state an extract call sees: what was asked for, and
// the page lines to judge against it. The questions address both by name, so it
// is a shape of its own rather than the browsing state with fields left empty.
type extractState struct {
	Wanted string   `json:"wanted"`
	Lines  []string `json:"lines"`
}

var (
	wordRE = regexp.MustCompile(`\w`)
	bareRE = regexp.MustCompile(`^\w+:?$`)
)

// holdsValue reports whether a role's text is a VALUE rather than the page's
// own words: `- textbox "Password" [ref=e8]: hunter2`. Playwright renders that
// value in full, password fields included - as a child `- text:` line when the
// field also has a placeholder - and after a `{{cuttle:NAME}}` fill it IS the
// substituted secret. So pageLines, the one path that sends page text to the
// API, reads nothing of such a node or of anything nested under it - which
// costs a combobox's options too, page words that are not worth that risk.
func holdsValue(role string) bool {
	return typableRoles[role] || role == "spinbutton" || role == "slider"
}

// maxLine bounds one extracted line. Past this it is a paragraph, not an item.
const maxLine = 300

// maxLines bounds how much of a page one extract judges. Every batch of lines is
// a request, and the page is the site's to write, so without this a long enough
// listing turns one `--extract` into an unbounded run of them. Past this many
// judgeable lines the page is one to page through, not one to extract from.
const maxLines = 300

// pageLines renders the aria tree as the plain lines a reader would see: a
// node's name and its text, without the roles, refs and yaml quoting around
// them. Link targets and other properties never parse as nodes, so they never
// get here.
func pageLines(tree []node) []string {
	var lines []string
	seen := map[string]bool{}
	skipBelow := -1
	for _, n := range tree {
		if skipBelow >= 0 && n.Depth > skipBelow {
			continue
		}
		skipBelow = -1
		if n.opaque || holdsValue(n.Role) {
			skipBelow = n.Depth
			continue
		}
		text := n.Name
		if n.Value != "" {
			if text != "" {
				text += ": "
			}
			text += n.Value
		}
		text = strings.TrimSpace(text)
		// A bare single word is a field label or a control, not an item, and it
		// would otherwise cost a question per form field on every extract.
		if text == "" || !wordRE.MatchString(text) || bareRE.MatchString(text) || seen[text] {
			continue
		}
		seen[text] = true
		lines = append(lines, truncate(text, maxLine))
		if len(lines) == maxLines {
			break
		}
	}
	return lines
}

// ------------------------------------------------------------------- the log

func (l *loop) report(step int, snap Snapshot, dec decision, action string) {
	defer func() { l.step = runStats{} }()
	if l.JSON {
		_ = l.emit(map[string]any{
			"step": step, "url": snap.URL, "title": snap.Title,
			questionDone: dec.Done, questionBlocked: dec.Blocked,
			"action": action, "key": dec.Key, "confidence": dec.Confidence,
			"model_ms": l.step.modelMS, "api_calls": l.step.apiCalls,
			"driver_ms": l.step.driverMS, "spawns": l.step.spawns, "verbs": l.step.verbs,
			"input_tokens": l.step.inputTokens, "output_tokens": l.step.outputTokens,
		})
		return
	}
	p := l.out
	fmt.Fprintf(l.Out, "%s %s %s %s\n", p.paint(fmt.Sprintf("[%d]", step), faint), snap.Title,
		p.paint("<"+snap.URL+">", faint), p.paint(fmt.Sprintf("done=%.2f blocked=%.2f", dec.Done, dec.Blocked), faint))
	verb, target, found := strings.Cut(action, ": ")
	if found {
		target = ": " + target
	}
	color := faint
	if dec.Confidence < doneThreshold {
		color = yellow
	}
	fmt.Fprintf(l.Out, "    %s %s%s %s\n", p.paint("->", faint), p.paint(verb, bold), target,
		p.paint(fmt.Sprintf("(confidence %.2f)", dec.Confidence), color))
}

// note records what became of an action that was not taken as decided: the
// driver refused it, or the write guard did. A machine reader has to learn it
// too: without this a `--json` consumer sees the step that was decided and
// never hears that the page refused it twice.
func (l *loop) note(entry Step, trouble string) {
	if l.JSON {
		// The step line printed just above already names the page, so this only
		// has to say which action, and what became of it.
		_ = l.emit(map[string]any{"action": entry.Action, "failed": entry.Failed, "refused": entry.Refused, "trouble": trouble})
		return
	}
	fmt.Fprintf(l.Out, "    %s\n", l.out.paint(trouble, yellow))
}

// stop prints the handoff brief and returns the code. The browser session is
// still live at exactly this page, so the brief says where it is and the literal
// command that picks it up - which is what makes a blocked run something a
// person or an agent can continue rather than something they have to restart.
func (l *loop) stop(ctx context.Context, code int, snap Snapshot, reason string) int {
	// An extract's lifetime is the page the run ended on, not the ending that got
	// there: a run that stopped blocked or out of budget still stopped on a page,
	// and the caller asked for those lines by name. The done path extracts before
	// this, because there a failed extract is the whole result and turns the run
	// into an error; here the outcome being reported is the real one, so a failed
	// extract is a line of its own and the code stands.
	if l.Extract != "" && code != ExitDone {
		if err := l.extract(ctx, snap); err != nil {
			l.noteExtractFailed(err)
		}
	}
	url := l.stopURL(snap)
	if l.JSON {
		outcome := map[string]any{
			"outcome": outcomeName(code), "reason": reason,
			"url": url, "steps": len(l.history), "next": l.handoffCmd(),
			"model_ms": l.total.modelMS, "api_calls": l.total.apiCalls,
			"driver_ms": l.total.driverMS, "spawns": l.total.spawns, "verbs": l.total.verbs,
			"input_tokens": l.total.inputTokens, "output_tokens": l.total.outputTokens,
			"elapsed_ms": l.now().Sub(l.started).Milliseconds(),
		}
		if l.model != "" {
			outcome["model"] = l.model
		}
		_ = l.emit(outcome)
		return code
	}
	if code == ExitDone {
		fmt.Fprintf(l.Out, "%s%s at <%s>\n", l.out.mark("✓ ", green), l.out.paint(reason, bold, green), url)
		return code
	}
	// Running out of steps is a failure; every other stop is a page asking for a hand.
	sym, color := "! ", yellow
	if code == ExitMaxSteps {
		sym, color = "X ", red
	}
	fmt.Fprintf(l.Err, "%s%s %s\n", l.err.mark(sym, color), l.err.paint("cuttle jev-browse: stopped:", bold, color), reason)
	if url != "" {
		fmt.Fprintf(l.Err, "  page: <%s>\n", url)
	}
	fmt.Fprint(l.Err, "  the browser session is live at exactly this page - pick it up with:\n")
	fmt.Fprintf(l.Err, "    %s\n", l.err.paint(l.handoffCmd(), bold, cyan))
	return code
}

// ANSI 16 SGR codes rather than exact colors, so the terminal's own theme picks
// the shades.
const (
	bold   = "1"
	faint  = "2"
	red    = "31"
	green  = "32"
	yellow = "33"
	cyan   = "36"
)

// painter styles text only when its stream is a terminal. Piped or captured
// output stays the exact plain text an agent parses.
type painter bool

func painterFor(w io.Writer) painter {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if force := os.Getenv("CLICOLOR_FORCE"); force != "" && force != "0" {
		return true
	}
	f, ok := w.(*os.File)
	return painter(ok && os.Getenv("TERM") != "dumb" && term.IsTerminal(int(f.Fd())))
}

func (p painter) paint(s string, sgr ...string) string {
	if !p || s == "" {
		return s
	}
	return "\x1b[" + strings.Join(sgr, ";") + "m" + s + "\x1b[0m"
}

// mark is a status symbol, shown only alongside color so the plain text stays
// unchanged - and so color is never the only thing carrying the outcome.
func (p painter) mark(sym, color string) string {
	if !p {
		return ""
	}
	return p.paint(sym, bold, color)
}

// handoffCmd is what the brief tells a person or an agent to run next, and the
// same string the JSON outcome carries as `next`. One owner, so the two cannot
// drift apart.
func (l *loop) handoffCmd() string { return l.Cuttle + " pw snapshot" }

// stopURL is where the brief says the session is. A capture parked behind a
// native dialog can carry no page identity at all - an alert prints the modal
// block and nothing else - and the page the last action was taken on is where
// that dialog was raised. With no history either, the brief says nothing rather
// than pointing at an empty page.
func (l *loop) stopURL(snap Snapshot) string {
	if snap.URL != "" {
		return snap.URL
	}
	if len(l.history) > 0 {
		return l.history[len(l.history)-1].URL
	}
	return ""
}

// blankPage reports whether a capture names no real page: the blank tab a fresh
// browser sits on, the error page a failed navigation leaves, or a capture with
// no page identity at all.
func blankPage(url string) bool {
	return url == "" || url == "about:blank" || strings.HasPrefix(url, "chrome-error://")
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func (l *loop) emit(v map[string]any) error {
	enc := json.NewEncoder(l.Out)
	return enc.Encode(v) //nolint:wrapcheck // a write error to stdout is already the whole message
}

func outcomeName(code int) string {
	switch code {
	case ExitDone:
		return "done"
	case ExitBlocked:
		return "blocked"
	case ExitMaxSteps:
		return "max-steps"
	default:
		return "error"
	}
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}
