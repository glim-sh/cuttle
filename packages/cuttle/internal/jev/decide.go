package jev

import (
	"context"
	"errors"
	"fmt"
	"math"
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
			"See every element on the page through the accessibility snapshot, so scrolling is never needed",
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

// pageState is the page's identity. Its body text is NOT here: the elements
// carry everything the decision needs, and the text is what must not be sent.
type pageState struct {
	URL   string `json:"url"`
	Title string `json:"title"`
}

// Step is one entry of the loop's memory: what it did, where, and whether the
// driver refused. It is fed back as state so the model can see that a route was
// already tried - the one thing a single-decision tool cannot know.
type Step struct {
	URL    string `json:"url"`
	Action string `json:"action"`
	Failed bool   `json:"failed,omitempty"`
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

// actionSpace turns a snapshot into the options the model may pick from. It
// prunes hard, because every option it does not prune splits probability with
// the one that matters: elements with no accessible name are unjudgeable,
// routes already taken on this page are noise, and two options with the same
// label are the same decision made twice.
func actionSpace(snap Snapshot, valueNames []string, history []Step) []candidate {
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
		if el.Label == "" || typableRoles[el.Role] {
			continue
		}
		label := el.Role + ": " + truncate(el.Label, maxLabel)
		if tried(history, snap.URL, label) {
			continue
		}
		add(el.Ref, label)
	}
	for _, el := range snap.Elements {
		if !typableRoles[el.Role] {
			continue
		}
		for _, name := range valueNames {
			add(typeKeyPrefix+el.Ref+":"+name,
				fmt.Sprintf("type `values.%s` into %s: %s", name, el.Role, truncate(el.Label, maxLabel)))
		}
	}
	// Typing into a plain textbox already ends with Tab, so Enter there would hit
	// whatever the focus moved to. After any other field it is how the form is
	// submitted.
	if last := lastStep(history); last != nil && last.URL == snap.URL &&
		strings.HasPrefix(last.Action, "type ") && !strings.Contains(last.Action, " into textbox:") {
		add(enterKey, "Press Enter to submit the text just typed")
	}
	add(backKey, "Go back to the previous page")
	return candidates
}

// maxLabel bounds one option's rubric. A label past this is a paragraph that
// someone gave an aria-label, and the first line of it is the part that says
// what the control does.
const maxLabel = 200

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

// tried reports whether this exact action already worked on this exact page. A
// FAILED attempt is deliberately not pruned: the ref went stale or an overlay
// swallowed the click, and the same action on a fresh snapshot is the fix.
func tried(history []Step, url, label string) bool {
	for _, h := range history {
		if h.URL == url && h.Action == label && !h.Failed {
			return true
		}
	}
	return false
}

func lastStep(history []Step) *Step {
	if len(history) == 0 {
		return nil
	}
	return &history[len(history)-1]
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
				"Prefer an element whose target is the task itself over elements that only relate to it.",
				"`history` lists what has already been done. Do not repeat an action that did not get closer.",
				"Type a value from `values` only into the box it belongs in. After typing, press Enter or click the submit button.",
				"If typing opened a list of suggestions, click the option matching the typed value before moving to another box, or the site discards the value.",
				"A field that already shows the right value is done. Do not type into it again.",
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
			Instructions: "Is `page` the state that `task` asks for, with everything `task` asks to be done already done?",
			Criteria: noulCriteria{
				True:  "The page is the target itself, and every value `task` names - a place, a date, a count, a sort order, a submitted form - shows on it",
				False: "The page only mentions or links to the target, is a different page, or shows a value that differs from one `task` names",
			},
		},
		questionBlocked: {
			Type:         typeNoul,
			Instructions: "Does reaching `task` from `page` require an action listed in `agent.cannot`?",
			Criteria: noulCriteria{
				True:  "Every remaining route needs an action the agent cannot do, such as solving a CAPTCHA or typing a value not listed in `values`",
				False: "A route of clicks, typing from `values`, and back steps could plausibly reach the task",
			},
		},
	}
	for i, g := range groups {
		questions[groupKey(i)] = pickQuestion(g)
	}
	return request{State: st, Model: model, Questions: questions}
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

// decide runs one step: one bundled request, then a runoff among the group
// winners when there was more than one group. The reported confidence is the
// WEAKEST of the answers the pick depends on - a certain group winner that the
// runoff was torn about is exactly the case a threshold exists to catch.
func decide(ctx context.Context, t transport, st state, groups [][]candidate) (decision, error) {
	first, err := t.evaluate(ctx, buildRequest(st, groups))
	if err != nil {
		return decision{}, err
	}
	dec := decision{
		Done:    first.Answers[questionDone].Noul,
		Blocked: first.Answers[questionBlocked].Noul,
	}

	finalists := make([]candidate, 0, len(groups))
	confidence := 1.0
	for i, g := range groups {
		ans := first.Answers[groupKey(i)]
		if ans.Choice == noneKey || ans.Choice == "" {
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
		dec.Key, dec.Confidence = noneKey, 1
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
		Model:     model,
		Questions: map[string]question{"pick": pickQuestion(finalists)},
	})
	if err != nil {
		return decision{}, err
	}
	dec.Key = runoff.Answers["pick"].Choice
	dec.Confidence = math.Min(confidence, runoff.Answers["pick"].Confidence)
	return dec, nil
}
