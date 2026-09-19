package jev

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Option keys that are not an element. They are answers about the page as a
// whole, so they ride in every group rather than in one of them.
const (
	noneKey  = "none"
	backKey  = "back"
	enterKey = "enter"
	// typeKeyPrefix builds `type:<ref>:<name>`: the element to type into and the
	// NAME of the prepared value. The value itself is looked up here, after the
	// answer comes back, and never reaches the API.
	typeKeyPrefix = "type:"
)

// groupSize is how many elements go into one Choice question. TypeSafe caps a
// Choice at 255 options, but a wide choice takes a slower internal two-stage
// path, so the groups are kept far below the cap and their winners meet in a
// runoff. One slot per group is reserved for `none`, which is what lets a group
// that holds nothing useful say so instead of nominating its least-bad option.
const groupSize = 50

// maxChoiceOptions is TypeSafe's hard cap on one Choice question. The groups sit
// far below it, but the runoff offers one option per group, so a wide enough
// action space would build a request past the cap. That fails here, by count:
// dropping options to fit would silently discard the one that mattered.
const maxChoiceOptions = 255

var errTooManyOptions = errors.New("choice question exceeds the API's option cap")

// doneThreshold is the probability a `done` or `blocked` answer must clear.
// Ending a run, or handing it to a human, are the two most consequential calls
// this loop makes and a coin flip must not be allowed to make either.
const doneThreshold = 0.8

// candidate is one option in the action space: the key the model answers with,
// and the label it judges by. Labels are authored by the page and are treated as
// data throughout - quoted into the log, sent as option rubrics, never followed
// as instructions.
type candidate struct {
	Key   string
	Label string
}

// agentFacts tells the model what this loop can and cannot do, so every question
// is judged against the same limits rather than against a browser in general. It
// is derived from the options, not configured: with no prepared values there is
// no way to type, and a question that assumes there is produces a pick at a
// search box that can never be filled.
type agentFacts struct {
	Can    []string `json:"can"`
	Cannot []string `json:"cannot"`
}

func factsFor(valueNames []string) agentFacts {
	facts := agentFacts{
		Can: []string{
			"Click one link, button, tab, menu item, checkbox, radio button, switch, or list option per step",
			"Go back to the previous page",
			"See the page's headings, text and every element through the accessibility snapshot, so scrolling is never needed",
		},
		Cannot: []string{
			"Hover, drag, upload, or download files",
			"Write answers or summaries; it only navigates, and success means ending on the right page",
		},
	}
	if len(valueNames) > 0 {
		facts.Can = append(facts.Can, "Type any value listed in `values` into a text box, then press Enter")
		facts.Cannot = append(facts.Cannot,
			"Type anything that is not listed in `values`, or enter URLs",
			"Log in without a username and password in `values`, create accounts, or solve CAPTCHAs")
		return facts
	}
	facts.Cannot = append(facts.Cannot,
		"Type text, so it cannot use search boxes, fill forms, or enter URLs",
		"Log in, create accounts, or solve CAPTCHAs")
	return facts
}

// pageState is the page as the model sees it: its identity, its outline and
// its text - the first stateLines of what a reader would see, with field
// values kept out of it by pageLines.
type pageState struct {
	URL      string    `json:"url"`
	Title    string    `json:"title"`
	Headings []heading `json:"headings,omitempty"`
	Text     []string  `json:"text,omitempty"`
}

// stateLines bounds the page text a decision sees. A step's whole state has to
// fit the model's context beside up to maxElements elements, and past this many
// lines the page is one to read with --extract, which judges every line.
const stateLines = 120

// Step is one entry of the loop's memory: what it did, where, and what became
// of it. It is fed back as state so the model can see that a route was already
// tried, whether it moved the page, and that the loop would not take it - the
// things a single-decision tool cannot know.
type Step struct {
	URL    string `json:"url"`
	Action string `json:"action"`
	Failed bool   `json:"failed,omitempty"`
	// Changed says the page's signature moved after the step, which is what
	// tells an action that got somewhere from one that did nothing.
	Changed bool `json:"changed,omitempty"`
	// Refused says the write guard would not take the step.
	Refused bool `json:"refused,omitempty"`
}

// state is what the model gets to see, as a structured object rather than a
// rendered string: the model reads `history` and `values` by name in the
// question instructions, which only works while they are addressable fields.
type state struct {
	Task string `json:"task"`
	// Values holds the NAMES of the prepared values, never the values. Which
	// field a value belongs in is a judgement; what the value is, is not.
	Values   []string   `json:"values,omitempty"`
	Agent    agentFacts `json:"agent"`
	Page     pageState  `json:"page"`
	History  []Step     `json:"history"`
	Elements []Element  `json:"elements"`
}

// hardDenyRE names the controls the run never takes, whatever the model says:
// the irreversible writes, matched as whole words in the control's own name.
// The loop runs on real signed-in accounts, and no judgement call is worth a
// payment or a deleted account. It is an English word list that errs toward
// refusing, not a guarantee; the write noul in the loop judges the rest.
var hardDenyRE = regexp.MustCompile(`(?i)\b(pay|buy|purchase|checkout|place order|transfer|donate|delete|remove|send|post|publish|sign out|unsubscribe)\b`)

// hardDenied is the verb that puts el on the hard list, or empty. A link or a
// tab moves around the site unless its name leads with the verb - an icon or
// punctuation before it aside: a job titled "Social Media Post Coordinator" is
// somewhere to go, a "Sign out" link is not.
func hardDenied(el Element) string {
	m := hardDenyRE.FindStringIndex(el.Label)
	if m == nil || ((el.Role == "link" || el.Role == "tab") && wordRE.MatchString(el.Label[:m[0]])) {
		return ""
	}
	return strings.ToLower(el.Label[m[0]:m[1]])
}

// hardDeniedPick is the verb that puts the picked action on the hard list. A
// box is named by what goes in it - "Post code" - not by what it does, so a
// fill is the noul's alone to judge. Enter submits the form that holds the
// focus, so the verbs to check are on that form's own buttons: the "Send"
// beside the box Enter would send from. Only a form or a dialog groups a box
// with its submit - a landmark as wide as main would put every "Delete" on a
// page beside its search box - so elsewhere Enter too is the noul's to judge.
func hardDeniedPick(snap Snapshot, key string) string {
	if key != enterKey {
		if el, ok := snap.element(refOf(key)); ok && !typableRoles[el.Role] {
			return hardDenied(el)
		}
		return ""
	}
	i := slices.IndexFunc(snap.Elements, func(el Element) bool {
		return typableRoles[el.Role] && strings.Contains(el.State, "active")
	})
	if i < 0 {
		return ""
	}
	if landmark, _, _ := strings.Cut(snap.Elements[i].Section, ":"); landmark != "form" && !dialogRoles[landmark] {
		return ""
	}
	for _, el := range snap.Elements {
		if el.Role == "button" && el.Section == snap.Elements[i].Section {
			if verb := hardDenied(el); verb != "" {
				return verb
			}
		}
	}
	return ""
}

// actionSpace turns a snapshot into the options the model may pick from: every
// element with a name, every prepared value into every box, Enter when a box
// holds the focus, and back. Elements with no accessible name are unjudgeable,
// and two options with the same label are the same decision made twice, so
// those are left out; everything else is the model's to judge, with `history`
// saying what was already tried.
func actionSpace(snap Snapshot, valueNames []string) []candidate {
	var candidates []candidate
	seen := map[string]bool{}
	add := func(key, label string) {
		if seen[label] {
			return
		}
		seen[label] = true
		candidates = append(candidates, candidate{Key: key, Label: label})
	}
	for _, el := range snap.Elements {
		if el.Label != "" && !typableRoles[el.Role] {
			add(el.Ref, el.rubric())
		}
	}
	for _, el := range snap.Elements {
		if !typableRoles[el.Role] {
			continue
		}
		// A box with no accessible name is still addressable, and often the only
		// one on the page - but the dedupe above keys on the rubric, so two unnamed
		// boxes would collapse into one option and the second would be unreachable
		// for the whole run. Its handle is what tells them apart.
		into := el
		if into.Label == "" {
			into.Label = el.Ref
		}
		filled := ""
		if strings.Contains(el.State, "filled") {
			filled = filledMark
		}
		for _, name := range valueNames {
			add(typeKeyPrefix+el.Ref+":"+name,
				fmt.Sprintf("type `values.%s` into %s%s", name, into.rubric(), filled))
		}
		// The focus is where Enter lands: after a fill it is still in the box. A
		// page that focuses an empty box as it loads has nothing to submit yet.
		if filled != "" && strings.Contains(el.State, "active") {
			add(enterKey, "Press Enter to submit the text just typed")
		}
	}
	add(backKey, "Go back to the previous page")
	return candidates
}

// filledMark ends the option for a box that already holds text. Only that it is
// filled is said, never what it holds.
const filledMark = " (filled)"

// truncate cuts s to at most n bytes, backing off to a rune boundary: a label cut
// mid-rune is invalid UTF-8 and reaches the API as a replacement character.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// group splits the action space into Choice-sized questions. Every group gets
// its own `none`, so a group of unrelated options can decline rather than
// nominate its least-bad member into the runoff.
func group(candidates []candidate) [][]candidate {
	var groups [][]candidate
	for start := 0; start < len(candidates); start += groupSize {
		groups = append(groups, candidates[start:min(start+groupSize, len(candidates))])
	}
	if len(groups) == 0 {
		groups = append(groups, nil)
	}
	return groups
}

func pickQuestion(candidates []candidate) question {
	criteria := make(map[string]string, len(candidates)+1)
	for _, c := range candidates {
		criteria[c.Key] = c.Label
	}
	criteria[noneKey] = "None of these actions makes progress toward the task"
	return question{
		Type: typeChoice,
		Instructions: map[string]any{
			"task": "Pick the action that makes the most progress toward `task`.",
			"rules": []string{
				"Only pick actions allowed by `agent.can`. An element that leads to something in `agent.cannot`, such as a search button with nothing to type, does not help.",
				"`history` lists what has already been done: `changed` says whether the page changed after it, `refused` that the loop would not take it. Do not repeat an action that did not get closer, or one that was refused.",
				"Type a value from `values` only into the box it belongs in, in the part of the page the task is about - the bracket before an element names that part - then press Enter or click the submit control.",
			},
		},
		Criteria: criteria,
	}
}

// buildRequest assembles one step's whole call: the two page-level nouls and
// one Choice per group, all against the same state, all evaluated in parallel.
func buildRequest(st state, groups [][]candidate) request {
	questions := map[string]question{
		questionDone: {
			Type:         typeNoul,
			Instructions: "Is `page` the state that `task` asks for, with everything `task` asks to be done already done? When `task` asks to find, read or list something, the page that holds it is the state `task` asks for.",
			Criteria: noulCriteria{
				True:  "The page is the target itself - for a task that finds, reads or lists something, the page that holds it - and every value `task` names - a place, a section, a sort order, a submitted form - shows in its url, title, headings, text or elements",
				False: "The page only mentions or links to the target, is a different page, or shows a value that differs from one `task` names",
			},
		},
		questionBlocked: {
			Type:         typeNoul,
			Instructions: "Is `page` asking for something listed in `agent.cannot` - credentials not in `values`, a CAPTCHA, a file upload, a payment - before it lets a visitor go on?",
			Criteria: noulCriteria{
				True:  "The page asks for something the agent cannot provide, such as a login without a username and password in `values`, a CAPTCHA, a file upload or a payment",
				False: "The page asks for nothing the agent cannot do, so clicks, typing from `values` and back steps can go on",
			},
		},
	}
	for i, g := range groups {
		questions[groupKey(i)] = pickQuestion(g)
	}
	return request{State: st, Questions: questions}
}

// writeThreshold is the probability at which a chosen action is refused as a
// write. It is low on purpose: a follow or a save taken on a signed-in account
// is not undone by the next step, and a refusal only costs a re-pick.
const writeThreshold = 0.2

// writeQuestion asks whether taking the chosen action changes something on the
// site. The label is the page's own words, quoted in as data the way an extract
// line is.
func writeQuestion(label string) question {
	return question{
		Type:         typeNoul,
		Instructions: fmt.Sprintf("Does taking %q on `page` change something on the site - send, submit, apply, follow, save, purchase - rather than navigate, open, sort or filter?", label),
		Criteria: noulCriteria{
			True:  "Taking it sends, submits, applies, follows, saves, purchases, deletes or otherwise changes something on the site or the account",
			False: "Taking it only navigates, opens, expands, sorts, filters or searches, and changes nothing on the site",
		},
	}
}

func groupKey(i int) string { return "pick" + strconv.Itoa(i) }

func find(candidates []candidate, key string) (candidate, bool) {
	for _, c := range candidates {
		if c.Key == key || (isRef(c.Key) && sameRef(c.Key, key)) {
			return c, true
		}
	}
	return candidate{}, false
}

// isRef reports whether a key names an element rather than a page-level action,
// which is what decides whether the frame-qualifier equivalence applies to it.
func isRef(key string) bool {
	return key != noneKey && key != backKey && key != enterKey && !strings.HasPrefix(key, typeKeyPrefix)
}

// decision is one step's worth of judgement, already collapsed from the
// parallel answers.
type decision struct {
	Done       float64
	Blocked    float64
	Key        string
	Confidence float64
}

// answered reads one question's answer. A question id that came back missing is
// a protocol failure, not a verdict: read as a zero value it says "not done, not
// blocked, nothing worth clicking" and would end the run as though the page had
// nothing to offer.
func answered(r response, id string) (answer, error) {
	a, ok := r.Answers[id]
	if !ok {
		return answer{}, fmt.Errorf("%w: no answer came back for %q", errAPI, id)
	}
	return a, nil
}

// decide runs one step: one bundled request, then a runoff among the group
// winners when there was more than one group. The reported confidence is the
// WEAKEST of the answers the pick depends on - a group winner the runoff was
// then torn about is a pick worth reading as the weaker of the two, and a
// `none` is as sure as the least sure group that declined.
func decide(ctx context.Context, t transport, st state, groups [][]candidate) (decision, error) {
	first, err := t.evaluate(ctx, buildRequest(st, groups))
	if err != nil {
		return decision{}, err
	}
	done, err := answered(first, questionDone)
	if err != nil {
		return decision{}, err
	}
	blocked, err := answered(first, questionBlocked)
	if err != nil {
		return decision{}, err
	}
	dec := decision{Done: done.Noul, Blocked: blocked.Noul}

	finalists := make([]candidate, 0, len(groups))
	confidence, declined := 1.0, 1.0
	for i, g := range groups {
		ans, missing := answered(first, groupKey(i))
		if missing != nil {
			return decision{}, missing
		}
		if ans.Choice == noneKey || ans.Choice == "" {
			declined = math.Min(declined, ans.Confidence)
			continue
		}
		won, ok := find(g, ans.Choice)
		if !ok {
			// A key this group never offered. Hand it straight back rather than let
			// it into a runoff it could win. The caller checks it against the WHOLE
			// action space, so a key another group offered is still acted on; only a
			// key no group offered is refused.
			dec.Key, dec.Confidence = ans.Choice, ans.Confidence
			return dec, nil
		}
		finalists = append(finalists, won)
		confidence = math.Min(confidence, ans.Confidence)
	}

	switch len(finalists) {
	case 0:
		dec.Key, dec.Confidence = noneKey, declined
		return dec, nil
	case 1:
		dec.Key, dec.Confidence = finalists[0].Key, confidence
		return dec, nil
	}

	// +1 for the `none` every pick question carries.
	if n := len(finalists) + 1; n > maxChoiceOptions {
		return decision{}, fmt.Errorf("%w: the runoff would offer %d options, the cap is %d", errTooManyOptions, n, maxChoiceOptions)
	}
	runoff, err := t.evaluate(ctx, request{
		State:     st,
		Questions: map[string]question{runoffKey: pickQuestion(finalists)},
	})
	if err != nil {
		return decision{}, err
	}
	pick, err := answered(runoff, runoffKey)
	if err != nil {
		return decision{}, err
	}
	dec.Key = pick.Choice
	dec.Confidence = math.Min(confidence, pick.Confidence)
	return dec, nil
}

// runoffKey names the one question a runoff asks.
const runoffKey = "pick"
