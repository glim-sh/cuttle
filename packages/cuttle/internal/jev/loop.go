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
// action. There are no fixed sleeps anywhere in the loop: a page that is already
// still costs two reads and the one poll between them, and one that never stops
// changing - a spinner, a polling widget - costs the deadline and then gets
// acted on anyway.
const (
	settlePoll     = 250 * time.Millisecond
	settleDeadline = 5 * time.Second
)

// extractBatch is how many page lines are judged in one request. Roughly this
// many short lines and their questions fit comfortably in one request.
const extractBatch = 15

// extractThreshold is the probability a line's noul must clear to be printed.
// Unlike ending a run, printing one line too many is cheap to see and ignore, so
// a lean toward yes is enough.
const extractThreshold = 0.5

var (
	errTaskRequired = errors.New("--task is required")
	errNoSteps      = errors.New("--max-steps must be at least 1")
	errNoStartPage  = errors.New("the session has no page - pass --url, or navigate first with cuttle pw")
	// The mock never answers done, which is the only way an extract is reached,
	// and it has no judgement to pick lines with even if it did.
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
	Driver Runner
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
	return &loop{Options: opts, valueNames: names, out: painterFor(opts.Out), err: painterFor(opts.Err)}, nil
}

func (l *loop) run(ctx context.Context) (int, error) {
	if l.URL != "" {
		if out, err := l.Driver(ctx, "goto", l.URL); err != nil {
			return ExitError, fmt.Errorf("goto %s: %w", l.URL, driverErr(out, err))
		}
	}

	var snap Snapshot
	for step := 1; step <= l.MaxSteps; step++ {
		var err error
		snap, err = l.settle(ctx)
		if err != nil {
			return ExitError, err
		}
		if snap.Modal != "" {
			// A dialog parks the renderer, so every read after it is a read of a
			// stale page. Recognizing that is worth more than any action the loop
			// could take next, and the modal block itself names the verb that clears
			// it (dialog-accept / dialog-dismiss).
			return l.stop(ExitBlocked, snap, "the page is parked behind a native dialog: "+snap.Modal), nil
		}
		// Without --url the run starts wherever the session already is, and a fresh
		// session is a blank tab: there is nothing to read, nothing to pick, and the
		// run would spend a step and a request to say so.
		if step == 1 && l.URL == "" && blankPage(snap.URL) {
			return ExitError, errNoStartPage
		}

		candidates := actionSpace(snap, l.valueNames, l.history)
		dec, err := decide(ctx, l.transport, l.state(snap, candidates), group(candidates))
		if err != nil {
			return ExitError, err
		}

		switch {
		case dec.Done >= doneThreshold:
			l.report(step, snap, dec, "done")
			if l.Extract != "" {
				// The task itself is done, and the session is sitting on the page it
				// was done on, so say so even though the extract is what failed:
				// otherwise the one useful fact - where to pick it up - is lost.
				if err := l.extract(ctx, snap); err != nil {
					return ExitError, fmt.Errorf("the task is done at <%s>, but the extract failed: %w", snap.URL, err)
				}
			}
			return l.stop(ExitDone, snap, "the task is done"), nil
		case dec.Blocked >= doneThreshold:
			l.report(step, snap, dec, "blocked")
			return l.stop(ExitBlocked, snap, "the task needs an action this loop cannot take"), nil
		case dec.Key == noneKey || dec.Key == "":
			l.report(step, snap, dec, noneKey)
			return l.stop(ExitBlocked, snap, "nothing on this page makes progress toward the task"+l.valueHint(snap)), nil
		}

		chosen, ok := find(candidates, dec.Key)
		if !ok {
			// The model is only ever offered this page's candidates, so a key that is
			// not one of them is a malformed answer aimed at an element nobody
			// vouched for - and acting on it would aim a prepared value at nothing.
			l.report(step, snap, dec, dec.Key)
			return l.stop(ExitBlocked, snap, fmt.Sprintf("chose %q, which this page did not offer", dec.Key)), nil
		}
		l.report(step, snap, dec, chosen.Label)
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
	if final, err := l.settle(ctx); err == nil {
		snap = final
	}
	return l.stop(ExitMaxSteps, snap, fmt.Sprintf("gave up after %d steps with the task unfinished", l.MaxSteps)), nil
}

func (l *loop) state(snap Snapshot, candidates []candidate) state {
	// Only the elements the model may actually pick reach the state: an element
	// pruned out of the action space is one it must not reason its way back to.
	elements := make([]Element, 0, len(candidates))
	for _, c := range candidates {
		if el, ok := snap.element(refOf(c.Key)); ok && !slices.Contains(elements, el) {
			elements = append(elements, el)
		}
	}
	return state{
		Task:     l.Task,
		Values:   l.valueNames,
		Agent:    factsFor(l.valueNames),
		Page:     pageState{URL: snap.URL, Title: snap.Title},
		History:  l.history,
		Elements: elements,
	}
}

// settle reads the page until it stops changing: two consecutive snapshots with
// the same identity and the same handles. It is the only thing between an action
// and the next decision, and it replaces the fixed sleep a loop would otherwise
// need after every click.
func (l *loop) settle(ctx context.Context) (Snapshot, error) {
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
		if settled && prev.signature() == snap.signature() {
			return snap, nil
		}
		prev, settled = snap, true
		if !l.now().Before(deadline) {
			return snap, nil
		}
		l.sleep(settlePoll)
	}
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
	if line := firstLine(out); line != "" {
		return fmt.Errorf("%w: %s", err, line)
	}
	return err
}

// act performs the chosen action and records it. A driver failure is not fatal:
// the page can re-render between the snapshot that mints a ref and the verb that
// uses it, and the symptom is a click timeout on an element the ref now resolves
// to something else. So a failure is answered with a fresh read and ONE retry,
// re-aimed at the same element by its label, and only then written down as
// failed - which is what stops the next step from pruning it away as done. That
// retry is skipped when the fresh read shows a page that moved: the action
// probably landed, and repeating it could take it twice.
func (l *loop) act(ctx context.Context, snap Snapshot, chosen candidate) error {
	entry := Step{URL: snap.URL, Action: chosen.Label}
	failure := l.perform(ctx, snap, chosen.Key)
	if failure != nil {
		fresh, err := l.settle(ctx)
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
		return l.remintAfterBack(ctx)
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
		// types per character never lets `{{cuttle:NAME}}` reassemble.
		if out, err := l.Driver(ctx, "fill", ref, value); err != nil {
			return driverErr(out, err)
		}
		// Date pickers and similar fields commit a typed value only on blur. Tab
		// blurs without submitting the form.
		if el, ok := snap.element(ref); ok && el.Role == roleTextbox {
			out, err := l.Driver(ctx, "press", "Tab")
			return driverErr(out, err)
		}
		return nil
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
func (l *loop) remintAfterBack(ctx context.Context) error {
	landed, err := l.settle(ctx)
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
// printed verbatim. This is the one place page TEXT leaves the process, and it
// only happens because the caller asked for it by name.
func (l *loop) extract(ctx context.Context, snap Snapshot) error {
	lines := pageLines(snap.Raw)
	var picked []string
	for start := 0; start < len(lines); start += extractBatch {
		batch := lines[start:min(start+extractBatch, len(lines))]
		questions := make(map[string]question, len(batch))
		for i := range batch {
			questions[fmt.Sprintf("l%d", i)] = question{
				Type:         typeNoul,
				Instructions: fmt.Sprintf("Is `lines[%d]` one complete item of the kind described by `wanted`?", i),
				Criteria: noulCriteria{
					True:  "The line names one item and carries every detail `wanted` asks for",
					False: "The line is a heading, control, label, rating, description, or link, or it is missing a detail `wanted` asks for",
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

// extractState is the whole state an extract call sees: what was asked for, and
// the page lines to judge against it. The questions address both by name, so it
// is a shape of its own rather than the browsing state with fields left empty.
type extractState struct {
	Wanted string   `json:"wanted"`
	Lines  []string `json:"lines"`
}

var (
	attrRE  = regexp.MustCompile(`\s\[[^\]]*\]`)
	namedRE = regexp.MustCompile(`^([a-z]+) "(.*)"(:.*)?$`)
	wordRE  = regexp.MustCompile(`\w`)
	bareRE  = regexp.MustCompile(`^\w+:?$`)
	roleRE  = regexp.MustCompile(`^[a-z]+:\s+`)
	propRE  = regexp.MustCompile(`^/[a-z]+:`)
	// valueRE matches the roles whose node line ends in a VALUE rather than in
	// the page's own words: `- textbox "Password" [ref=e8]: hunter2`. Playwright
	// renders that value in full, password fields included, and after a
	// `{{cuttle:NAME}}` fill it IS the substituted secret - so an extract, the one
	// path that sends page lines to the API, must never read one.
	valueRE = regexp.MustCompile(`^(?:textbox|searchbox|combobox|spinbutton|slider)\b`)
)

// maxLine bounds one extracted line. Past this it is a paragraph, not an item.
const maxLine = 300

// maxLines bounds how much of a page one extract judges. Every batch of lines is
// a request, and the page is the site's to write, so without this a long enough
// listing turns one `--extract` into an unbounded run of them. Past this many
// judgeable lines the page is one to page through, not one to extract from.
const maxLines = 300

// pageLines renders the snapshot as the plain lines a reader would see: the yaml
// scaffolding, the refs and the role names come off, and what is left is the
// page's own words.
func pageLines(raw string) []string {
	var lines []string
	seen := map[string]bool{}
	for line := range strings.SplitSeq(raw, "\n") {
		text := strings.TrimSpace(line)
		// The capture's own framing - section headers, the yaml fence, the page
		// identity block - is the driver talking, not the page.
		if strings.HasPrefix(text, "### ") || strings.HasPrefix(text, "```") ||
			strings.HasPrefix(text, "- Page URL:") || strings.HasPrefix(text, "- Page Title:") {
			continue
		}
		text = strings.TrimPrefix(text, "- ")
		// `/url: ...` and its kin are a node's properties: a link's target is not
		// words on the page, and element data deliberately leaves URLs out.
		if propRE.MatchString(text) {
			continue
		}
		text = attrRE.ReplaceAllString(text, "")
		if valueRE.MatchString(text) {
			continue
		}
		text = strings.TrimSpace(namedRE.ReplaceAllString(text, "$2$3"))
		// A trailing colon means the node has children, and a leading `role:` is
		// how the tree introduces a node's own text. Both are its syntax, not the
		// page's words - and `role: content` is a shape the page cannot produce,
		// because every line of the tree already has that shape.
		text = strings.TrimSpace(roleRE.ReplaceAllString(strings.TrimSuffix(text, ":"), ""))
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
	if l.JSON {
		_ = l.emit(map[string]any{
			"step": step, "url": snap.URL, "title": snap.Title,
			questionDone: dec.Done, questionBlocked: dec.Blocked,
			"action": action, "key": dec.Key, "confidence": dec.Confidence,
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

// note records what became of an action the driver refused. A machine reader
// has to learn it too: without this a `--json` consumer sees the step that was
// decided and never hears that the page refused it twice.
func (l *loop) note(entry Step, trouble string) {
	if l.JSON {
		// The step line printed just above already names the page, so this only
		// has to say which action, and what became of it.
		_ = l.emit(map[string]any{"action": entry.Action, "failed": entry.Failed, "trouble": trouble})
		return
	}
	fmt.Fprintf(l.Out, "    %s\n", l.out.paint(trouble, yellow))
}

// stop prints the handoff brief and returns the code. The browser session is
// still live at exactly this page, so the brief says where it is and the literal
// command that picks it up - which is what makes a blocked run something a
// person or an agent can continue rather than something they have to restart.
func (l *loop) stop(code int, snap Snapshot, reason string) int {
	url := l.stopURL(snap)
	if l.JSON {
		_ = l.emit(map[string]any{
			"outcome": outcomeName(code), "reason": reason,
			"url": url, "steps": len(l.history), "next": handoffCmd,
		})
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
	fmt.Fprintf(l.Err, "    %s\n", l.err.paint(handoffCmd, bold, cyan))
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
const handoffCmd = "cuttle pw snapshot"

// stopURL is where the brief says the session is. A capture parked behind a
// native dialog can carry no page identity at all - an alert prints the modal
// block and nothing else - and the page the last action was taken on is where
// that dialog was raised. With no history either, the brief says nothing rather
// than pointing at an empty page.
func (l *loop) stopURL(snap Snapshot) string {
	if snap.URL != "" {
		return snap.URL
	}
	if last := lastStep(l.history); last != nil {
		return last.URL
	}
	return ""
}

// valueHint names the one reason for a `none` that is the CALLER's to fix. With
// no --text values the action space holds no typable option at all, so a page
// whose only route forward is a search box or a form has genuinely nothing to
// offer - and the brief would otherwise read as the page's fault.
func (l *loop) valueHint(snap Snapshot) string {
	if len(l.valueNames) > 0 {
		return ""
	}
	if !slices.ContainsFunc(snap.Elements, func(el Element) bool { return typableRoles[el.Role] }) {
		return ""
	}
	return " - typable fields were not offered because no --text values were supplied; pass --text NAME=VALUE"
}

// blankPage reports whether a capture names no real page: the blank tab a fresh
// browser sits on, or a capture with no page identity at all.
func blankPage(url string) bool { return url == "" || url == "about:blank" }

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
