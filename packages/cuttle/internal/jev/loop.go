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
)

// Exit codes. Anything a caller has to branch on is a code, not a parsed line of
// output. 1 and 3 keep the meanings the jev-step experiment gave them.
const (
	ExitDone     = 0 // the task is done
	ExitError    = 1 // a usage or infrastructure error; the message says which
	ExitBlocked  = 3 // progress needs a person: a dialog, a captcha, a dead end
	ExitMaxSteps = 4 // --max-steps ran out with the task unfinished
)

// settlePoll and settleDeadline bound how long the page is re-read for after an
// action. There are no fixed sleeps anywhere in the loop: a page that is already
// still costs two reads and no waiting, and one that never stops changing - a
// spinner, a polling widget - costs the deadline and then gets acted on anyway.
const (
	settlePoll     = 250 * time.Millisecond
	settleDeadline = 5 * time.Second
)

// extractBatch is how many page lines are judged in one request. Roughly this
// many short lines and their questions fit comfortably in one request.
const extractBatch = 15

var errTaskRequired = errors.New("--task is required")

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
}

func newLoop(opts Options) (*loop, error) {
	if strings.TrimSpace(opts.Task) == "" {
		return nil, errTaskRequired
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
	return &loop{Options: opts, valueNames: names}, nil
}

func (l *loop) run(ctx context.Context) (int, error) {
	if l.URL != "" {
		if _, err := l.Driver(ctx, "goto", l.URL); err != nil {
			return ExitError, fmt.Errorf("goto %s: %w", l.URL, err)
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

		candidates := actionSpace(snap, l.valueNames, l.history)
		dec, err := decide(ctx, l.transport, l.state(snap, candidates), group(candidates))
		if err != nil {
			return ExitError, err
		}

		switch {
		case dec.Done >= doneThreshold:
			l.report(step, snap, dec, "done")
			if l.Extract != "" {
				if err := l.extract(ctx, snap); err != nil {
					return ExitError, err
				}
			}
			return l.stop(ExitDone, snap, "the task is done"), nil
		case dec.Blocked >= doneThreshold:
			l.report(step, snap, dec, "blocked")
			return l.stop(ExitBlocked, snap, "the task needs an action this loop cannot take"), nil
		case dec.Key == noneKey || dec.Key == "":
			l.report(step, snap, dec, noneKey)
			return l.stop(ExitBlocked, snap, "nothing on this page makes progress toward the task"), nil
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
		return Snapshot{}, fmt.Errorf("playwright-cli snapshot: %w", err)
	}
	return snap, nil
}

// act performs the chosen action and records it. A driver failure is not fatal:
// the page can re-render between the snapshot that mints a ref and the verb that
// uses it, and the symptom is a click timeout on an element the ref now resolves
// to something else. So a failure is answered with a fresh read and ONE retry,
// re-aimed at the same element by its label, and only then written down as
// failed - which is what stops the next step from pruning it away as done.
func (l *loop) act(ctx context.Context, snap Snapshot, chosen candidate) error {
	entry := Step{URL: snap.URL, Action: chosen.Label}
	failure := l.perform(ctx, snap, chosen.Key)
	if failure != nil {
		fresh, err := l.settle(ctx)
		if err != nil {
			return err
		}
		retry, ok := reaim(chosen, snap, fresh)
		if !ok || l.perform(ctx, fresh, retry) != nil {
			entry.Failed = true
			l.note("    action failed: %s", firstLine(failure.Error()))
		}
	}
	l.history = append(l.history, entry)
	return nil
}

func (l *loop) perform(ctx context.Context, snap Snapshot, key string) error {
	switch {
	case key == backKey:
		_, err := l.Driver(ctx, "go-back")
		return err
	case key == enterKey:
		_, err := l.Driver(ctx, "press", "Enter")
		return err
	case strings.HasPrefix(key, typeKeyPrefix):
		ref, name, ok := strings.Cut(strings.TrimPrefix(key, typeKeyPrefix), ":")
		if !ok {
			return fmt.Errorf("%w: malformed type key %q", errDriver, key)
		}
		value, ok := l.Values[name]
		if !ok {
			return fmt.Errorf("%w: chose the value %q, which was not supplied", errDriver, name)
		}
		// `fill` is the only verb cuttle's secret sentinels survive: anything that
		// types per character never lets `{{cuttle:NAME}}` reassemble.
		if _, err := l.Driver(ctx, "fill", ref, value); err != nil {
			return err
		}
		// Date pickers and similar fields commit a typed value only on blur. Tab
		// blurs without submitting the form.
		if el, ok := snap.element(ref); ok && el.Role == roleTextbox {
			_, err := l.Driver(ctx, "press", "Tab")
			return err
		}
		return nil
	default:
		_, err := l.Driver(ctx, "click", key)
		return err
	}
}

var errDriver = errors.New("playwright-cli")

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
			Model:     model,
			Questions: questions,
		})
		if err != nil {
			return err
		}
		for i, line := range batch {
			if resp.Answers[fmt.Sprintf("l%d", i)].Noul >= 0.5 {
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
	// valueRE matches the roles whose node line ends in a VALUE rather than in
	// the page's own words: `- textbox "Password" [ref=e8]: hunter2`. Playwright
	// renders that value in full, password fields included, and after a
	// `{{cuttle:NAME}}` fill it IS the substituted secret - so an extract, the one
	// path that sends page lines to the API, must never read one.
	valueRE = regexp.MustCompile(`^(?:textbox|searchbox|combobox|spinbutton|slider)\b`)
)

// maxLine bounds one extracted line. Past this it is a paragraph, not an item.
const maxLine = 300

// pageLines renders the snapshot as the plain lines a reader would see: the yaml
// scaffolding, the refs and the role names come off, and what is left is the
// page's own words.
func pageLines(raw string) []string {
	var lines []string
	seen := map[string]bool{}
	for line := range strings.SplitSeq(raw, "\n") {
		text := strings.TrimSpace(line)
		text = strings.TrimPrefix(text, "- ")
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
	fmt.Fprintf(l.Out, "[%d] %s <%s> done=%.2f blocked=%.2f\n", step, snap.Title, snap.URL, dec.Done, dec.Blocked)
	fmt.Fprintf(l.Out, "    -> %s (confidence %.2f)\n", action, dec.Confidence)
}

func (l *loop) note(format string, args ...any) {
	if !l.JSON {
		fmt.Fprintf(l.Out, format+"\n", args...)
	}
}

// stop prints the handoff brief and returns the code. The browser session is
// still live at exactly this page, so the brief says where it is and the literal
// command that picks it up - which is what makes a blocked run something a
// person or an agent can continue rather than something they have to restart.
func (l *loop) stop(code int, snap Snapshot, reason string) int {
	if l.JSON {
		_ = l.emit(map[string]any{
			"outcome": outcomeName(code), "reason": reason,
			"url": snap.URL, "steps": len(l.history), "next": "cuttle pw snapshot",
		})
		return code
	}
	if code == ExitDone {
		fmt.Fprintf(l.Out, "%s at <%s>\n", reason, snap.URL)
		return code
	}
	fmt.Fprintf(l.Err, "cuttle jev-browse: stopped: %s\n", reason)
	fmt.Fprintf(l.Err, "  page: <%s>\n", snap.URL)
	fmt.Fprint(l.Err, "  the browser session is live at exactly this page - pick it up with:\n")
	fmt.Fprint(l.Err, "    cuttle pw snapshot\n")
	return code
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
