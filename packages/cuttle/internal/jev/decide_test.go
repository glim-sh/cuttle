package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

var update = flag.Bool("update", false, "rewrite the request-shape golden")

// scriptedTransport replays answers per question id, so a test can express the
// exact thing the model would have to say for a given outcome. A round is spent
// on the first request that asks a question it answers, which is how a runoff
// or a write noul is scripted separately from the groups - and how a script of
// picks alone leaves the write nouls between them at their default "no". A
// round the run never asks for stays unspent, and runLoop fails the test on it:
// a question asked in the wrong order, or never, must not pass on defaults.
type scriptedTransport struct {
	rounds   []map[string]answer
	requests []request
}

func (s *scriptedTransport) evaluate(_ context.Context, req request) (response, error) {
	s.requests = append(s.requests, req)
	round := map[string]answer{}
	if len(s.rounds) > 0 {
		for id := range s.rounds[0] {
			if _, asked := req.Questions[id]; asked {
				round, s.rounds = s.rounds[0], s.rounds[1:]
				break
			}
		}
	}
	// The API answers every question it was asked, so the fake does too: a script
	// names only the answers its test is about, and the rest come back as the "no"
	// the model would otherwise have sent.
	answers := make(map[string]answer, len(req.Questions))
	for id, q := range req.Questions {
		a := answer{}
		if q.Type == typeChoice {
			a = answer{Choice: noneKey, Confidence: 1}
		}
		if scripted, ok := round[id]; ok {
			a = scripted
		}
		answers[id] = a
	}
	return response{Answers: answers}, nil
}

func signinState(t *testing.T, history []Step) (state, []candidate) {
	t.Helper()
	snap := parseFixture(t, "signin.snapshot")
	l := &loop{Options: Options{Task: "sign in to the demo shop"}, valueNames: []string{"password", "username"}, history: history}
	return l.state(snap), actionSpace(snap, []string{"password", "username"})
}

func TestActionSpaceDedupsAndOffersTheValuesByName(t *testing.T) {
	_, candidates := signinState(t, nil)

	labels := map[string]string{}
	for _, c := range candidates {
		if _, dup := labels[c.Label]; dup {
			t.Errorf("two options share the label %q, which splits the same decision in two", c.Label)
		}
		labels[c.Label] = c.Key
	}
	if _, ok := labels["button: Login"]; !ok {
		t.Error("the Login button is not in the action space")
	}
	if _, ok := labels["type `values.password` into textbox: Password"]; !ok {
		t.Error("no option types the prepared password into the password box")
	}
	if labels["Go back to the previous page"] != backKey {
		t.Error("going back is always an option")
	}
	// The unlabelled combobox is unjudgeable: an option whose rubric is empty
	// only ever takes probability away from one that says something.
	for _, c := range candidates {
		if c.Key == "f2e9" {
			t.Error("an element with no accessible name was offered as a click")
		}
	}
}

func manyCandidates(n int) []candidate {
	out := make([]candidate, 0, n)
	for i := range n {
		out = append(out, candidate{Key: "e" + strconv.Itoa(i), Label: "link: item " + strconv.Itoa(i)})
	}
	return out
}

func TestGroupSplitsTheActionSpaceWithoutLosingAnything(t *testing.T) {
	// An empty action space still asks one question: the two page-level nouls
	// ride in the same request and must not be skipped because nothing is
	// clickable.
	cases := map[int]int{0: 1, 1: 1, groupSize - 1: 1, groupSize: 1, groupSize + 1: 2, 3*groupSize + 7: 4}
	for n, wantGroups := range cases {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			candidates := manyCandidates(n)
			groups := group(candidates)
			if len(groups) != wantGroups {
				t.Errorf("%d candidates made %d groups, want %d", n, len(groups), wantGroups)
			}
			seen := 0
			for i, g := range groups {
				if len(g) > groupSize {
					t.Errorf("group %d holds %d options, over the %d cap", i, len(g), groupSize)
				}
				// Every group carries its own `none`, which is what lets one holding
				// nothing useful decline instead of nominating its least-bad option.
				criteria, _ := pickQuestion(g).Criteria.(map[string]string)
				if len(criteria) != len(g)+1 {
					t.Errorf("group %d: %d options for %d candidates - the none slot is missing", i, len(criteria), len(g))
				}
				if _, ok := criteria[noneKey]; !ok {
					t.Errorf("group %d has no %q option", i, noneKey)
				}
				seen += len(g)
			}
			if seen != n {
				t.Errorf("grouping lost candidates: %d of %d", seen, n)
			}
		})
	}
}

// A real page that is dense with controls: the whole point of grouping is that
// it never produces a Choice the API would reject or answer down the slow path.
func TestGroupHandlesADenseRealPage(t *testing.T) {
	candidates := actionSpace(parseFixture(t, "consent_register.snapshot"), []string{"taxpayer_id"})
	if len(candidates) < 30 {
		t.Fatalf("the dense fixture yielded only %d candidates", len(candidates))
	}
	for i, g := range group(candidates) {
		if len(g) > groupSize {
			t.Errorf("group %d holds %d options, over the %d cap", i, len(g), groupSize)
		}
	}
}

func TestDecideRunsOffAmongTheGroupWinners(t *testing.T) {
	groups := [][]candidate{
		{{Key: "e1", Label: "link: one"}, {Key: "e2", Label: "link: two"}},
		{{Key: "e3", Label: "link: three"}},
		{{Key: "e4", Label: "link: four"}},
	}
	tr := &scriptedTransport{rounds: []map[string]answer{
		{
			questionDone:    {Noul: 0.1},
			questionBlocked: {Noul: 0.05},
			"pick0":         {Choice: "e2", Confidence: 0.9},
			"pick1":         {Choice: noneKey, Confidence: 0.99},
			"pick2":         {Choice: "e4", Confidence: 0.6},
		},
		{"pick": {Choice: "e4", Confidence: 0.95}},
	}}

	dec, err := decide(context.Background(), tr, state{}, groups)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if dec.Key != "e4" {
		t.Errorf("pick: got %q, want e4", dec.Key)
	}
	// The weakest answer the pick depends on is the one that counts: a runoff it
	// was sure about, won by a group winner it was torn over, is not a sure pick.
	if dec.Confidence != 0.6 {
		t.Errorf("confidence: got %v, want the weakest dependent answer (0.60)", dec.Confidence)
	}
	if len(tr.requests) != 2 {
		t.Fatalf("made %d requests, want a bundled one plus the runoff", len(tr.requests))
	}
	runoff, _ := tr.requests[1].Questions["pick"].Criteria.(map[string]string)
	if len(runoff) != 3 {
		t.Errorf("the runoff offered %d options, want the two group winners plus none", len(runoff))
	}
	if _, ok := runoff["e3"]; ok {
		t.Error("a group that answered none put its option into the runoff anyway")
	}
}

func TestDecideSkipsTheRunoffForASingleWinner(t *testing.T) {
	tr := &scriptedTransport{rounds: []map[string]answer{{
		questionDone: {Noul: 0.2}, "pick0": {Choice: "e1", Confidence: 0.8}, "pick1": {Choice: noneKey},
	}}}
	dec, err := decide(context.Background(), tr, state{},
		[][]candidate{{{Key: "e1", Label: "link: one"}}, {{Key: "e2", Label: "link: two"}}})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if dec.Key != "e1" || dec.Confidence != 0.8 {
		t.Errorf("got %q at %v, want e1 at 0.80", dec.Key, dec.Confidence)
	}
	if len(tr.requests) != 1 {
		t.Errorf("made %d requests; one winner needs no runoff", len(tr.requests))
	}
}

// A `none` is as sure as the least sure group that declined - never a stamped
// 1.0, which read as certainty the model had not expressed.
func TestDecideReportsNoneWhenEveryGroupDeclines(t *testing.T) {
	tr := &scriptedTransport{rounds: []map[string]answer{{"pick0": {Choice: noneKey, Confidence: 0.9}, "pick1": {Choice: "", Confidence: 0.55}}}}
	dec, err := decide(context.Background(), tr, state{},
		[][]candidate{{{Key: "e1"}}, {{Key: "e2"}}})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if dec.Key != noneKey {
		t.Errorf("got %q, want %q", dec.Key, noneKey)
	}
	if dec.Confidence != 0.55 {
		t.Errorf("confidence: got %v, want the weakest declining group (0.55)", dec.Confidence)
	}
}

// A key no group offered is a malformed answer. It must not win a runoff by
// being the only "winner" - it is handed straight back for the caller to refuse.
func TestDecideHandsBackAKeyNoGroupOffered(t *testing.T) {
	tr := &scriptedTransport{rounds: []map[string]answer{{
		"pick0": {Choice: "e99", Confidence: 0.99}, "pick1": {Choice: "e2", Confidence: 0.9},
	}}}
	dec, err := decide(context.Background(), tr, state{},
		[][]candidate{{{Key: "e1"}}, {{Key: "e2"}}})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if dec.Key != "e99" {
		t.Errorf("got %q, want the unoffered key handed back verbatim", dec.Key)
	}
	if len(tr.requests) != 1 {
		t.Error("an unoffered key must not reach a runoff")
	}
}

// A runoff among more group winners than a Choice can hold must fail by count,
// never reach the API and never quietly drop the options past the cap.
// silentTransport answers every question but one. The API is supposed to answer
// all of them, so the missing one is a protocol failure.
type silentTransport struct{ omit string }

func (s silentTransport) evaluate(_ context.Context, req request) (response, error) {
	answers := make(map[string]answer, len(req.Questions))
	for id, q := range req.Questions {
		if id == s.omit {
			continue
		}
		if q.Type == typeChoice {
			answers[id] = answer{Choice: noneKey, Confidence: 1}
		} else {
			answers[id] = answer{}
		}
	}
	return response{Answers: answers}, nil
}

// An answer that never came back must not be read as the zero it unmarshals to:
// that says "not done, not blocked, nothing worth clicking", and the run would
// stop with a verdict about the PAGE for what is a fault in the call.
func TestDecideFailsOnAnAnswerThatDidNotComeBack(t *testing.T) {
	for _, id := range []string{questionDone, questionBlocked, groupKey(0)} {
		t.Run(id, func(t *testing.T) {
			_, err := decide(context.Background(), silentTransport{omit: id}, state{}, group(manyCandidates(2)))
			if err == nil || !strings.Contains(err.Error(), "no answer came back") {
				t.Errorf("got %v, want the missing answer %q reported", err, id)
			}
		})
	}
}

func TestDecideRefusesARunoffPastTheOptionCap(t *testing.T) {
	winners := maxChoiceOptions // plus `none` is one past the cap
	groups := make([][]candidate, winners)
	round := map[string]answer{}
	for i := range winners {
		key := "e" + strconv.Itoa(i)
		groups[i] = []candidate{{Key: key}}
		round[groupKey(i)] = answer{Choice: key, Confidence: 0.9}
	}
	tr := &scriptedTransport{rounds: []map[string]answer{round}}
	_, err := decide(context.Background(), tr, state{}, groups)
	if !errors.Is(err, errTooManyOptions) {
		t.Fatalf("got %v, want errTooManyOptions", err)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(winners+1)) {
		t.Errorf("the error does not name the option count: %v", err)
	}
	if len(tr.requests) != 1 {
		t.Errorf("made %d requests, want the oversized runoff never sent", len(tr.requests))
	}
}

// Labels are cut to a byte budget, and a cut inside a multibyte rune would send
// invalid UTF-8.
func TestTruncateNeverSplitsARune(t *testing.T) {
	got := truncate("abéé", 3) // a, b, then a two-byte rune straddling the cut
	if got != "ab" {
		t.Errorf("got %q, want the cut backed off to the rune boundary", got)
	}
	if !utf8.ValidString(truncate(strings.Repeat("日", 100), maxLabel)) {
		t.Error("truncate produced invalid UTF-8")
	}
}

// The model picks WHICH field a value belongs in. What the value IS never leaves
// this process - it is looked up after the answer comes back - not on the step
// that types it, and not on the next read, where the page echoes it back as
// the box's value and as the suggestions under it.
func TestBuildRequestSendsNamesNotValues(t *testing.T) {
	st, candidates := signinState(t, nil)
	body, err := json.Marshal(buildRequest(st, group(candidates)))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(body), `"values":["password","username"]`) {
		t.Error("the request does not offer the prepared values by name")
	}
	// The page's own words are sent; the value that was typed into it is not.
	if !strings.Contains(string(body), "Welcome to the demo shop") {
		t.Error("the page text did not reach the request body")
	}

	tr := &scriptedTransport{rounds: []map[string]answer{{"pick0": {Choice: "type:e7:query", Confidence: 0.9}}}}
	d := &fakeDriver{pages: []string{readFixture(t, "jobs_landing.snapshot"), readFixture(t, "jobs_landing_typed.snapshot")}}
	res := runLoop(t, d, Options{transport: tr, Task: "search for jobs by the keyword", Values: map[string]string{"query": "golang"}})
	if res.err != nil {
		t.Fatalf("run: %v", res.err)
	}
	if got := d.actions(); !slices.Equal(got, []string{"fill e7 golang"}) {
		t.Errorf("driver calls: got %q, want the one fill", got)
	}
	for i, req := range tr.requests {
		body, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if strings.Contains(string(body), "golang") {
			t.Errorf("request %d carries the typed value:\n%s", i, body)
		}
	}
	st, _ = tr.requests[2].State.(state)
	options, _ := tr.requests[2].Questions["pick0"].Criteria.(map[string]string)
	if _, ok := options[enterKey]; !ok {
		t.Error("Enter was not offered while the filled box holds the focus")
	}
	if want := "[main] option: " + typedValueMark + " developer"; options["e11"] != want {
		t.Errorf("the suggestion was offered as %q, want %q", options["e11"], want)
	}
	if !slices.ContainsFunc(st.Elements, func(el Element) bool { return el.Ref == "e7" && el.State == "filled, active" }) {
		t.Errorf("the box is not shown filled and active: %+v", st.Elements)
	}
}

// The request shape is the contract with the API. Pinning it turns any change
// into a diff someone has to regenerate and read, the way the fingerprint golden
// does for the stealth args. It goes through the transport, because the body on
// the wire is what the API sees - the model line included, which is the
// transport's to stamp.
func TestRequestShapeGolden(t *testing.T) {
	st, candidates := signinState(t, []Step{
		{URL: "http://127.0.0.1:8799/", Action: "[banner] link: Cart (0)"},
		{URL: "http://127.0.0.1:8799/", Action: "button: Help", Failed: true},
	})
	body, err := transportFor(t, "ts-live-whatever").body(buildRequest(st, group(candidates)))
	if err != nil {
		t.Fatalf("body: %v", err)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, body, "", "  "); err != nil {
		t.Fatalf("indent: %v", err)
	}
	pretty.WriteByte('\n')
	checkGolden(t, "request.golden.json", pretty.Bytes())
}

func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v (regenerate with `go test ./internal/jev/ -update`)", err)
	}
	if string(got) != string(want) {
		t.Errorf("the request shape drifted from testdata/%s.\n"+
			"Regenerate with `go test ./internal/jev/ -update` and read the diff.\ngot:\n%s", name, got)
	}
}

// The hard list is the floor under the write guard: an irreversible verb in a
// control's own name is refused whatever the model says, as a whole word, so a
// "Payments" tab is somewhere to go and "Pay" is not. A link or tab is on it
// only when its name leads with the verb: a job titled "Social Media Post
// Coordinator" is a link to follow, a "Sign out" link is not.
func TestHardDenyMatchesIrreversibleVerbsAsWholeWords(t *testing.T) {
	for _, label := range []string{
		"Pay", "Pay now", "Buy", "Purchase", "Checkout", "Place order", "Transfer funds", "Donate",
		"Delete account", "Remove connection", "Send", "Post", "Publish", "Sign out", "Unsubscribe",
		"Now: delete it",
	} {
		if hardDenied(Element{Role: "button", Label: label}) == "" {
			t.Errorf("the button %q is not on the hard list", label)
		}
	}
	for _, label := range []string{
		"Payments", "Posts", "Sent items", "Apply current filters to show results", "Save filter",
		"Follow", "Sign in", "Submit", "Search", "Next", "Checkouts help",
	} {
		if verb := hardDenied(Element{Role: "button", Label: label}); verb != "" {
			t.Errorf("the button %q is on the hard list for %q; the write noul is what judges it", label, verb)
		}
	}
	for label, want := range map[string]string{"Sign out": "sign out", "- Delete item": "delete", "Social Media Post Coordinator": "", "Data Transfer Engineer": ""} {
		if got := hardDenied(Element{Role: "link", Label: label}); got != want {
			t.Errorf("the link %q: got %q, want %q", label, got, want)
		}
	}
}

// The hard list judges the picked action, not the box a value goes into - a
// "Post code" field is named by its content - and Enter by the buttons of the
// form or dialog it would submit: the "Send" beside a composer's box, not a
// "Delete" somewhere else in main.
func TestHardDenyJudgesThePickNotTheBox(t *testing.T) {
	page := func(focus string) Snapshot {
		return snapshotOf(
			`- main [ref=e1]:`,
			`  - searchbox "Search"`+focus+` [ref=e2]: golang`,
			`  - button "Delete" [ref=e3]`,
			`  - textbox "Post code" [ref=e4]`,
			`  - dialog "New message" [ref=e5]:`,
			`    - textbox "Write a message" [ref=e6]: hi`,
			`    - button "Send" [ref=e7]`,
		)
	}
	searching := page(" [active]")
	if got := hardDeniedPick(searching, "type:e4:postcode"); got != "" {
		t.Errorf("typing into the post code box was hard-denied for %q", got)
	}
	if got := hardDeniedPick(searching, "e3"); got != "delete" {
		t.Errorf("the Delete button: got %q, want delete", got)
	}
	if got := hardDeniedPick(searching, enterKey); got != "" {
		t.Errorf("Enter in the main search box was hard-denied for %q: the Delete button is not its submit", got)
	}
	composing := page("")
	composing.Elements[slices.IndexFunc(composing.Elements, func(el Element) bool { return el.Ref == "e6" })].State = "filled, active"
	if got := hardDeniedPick(composing, enterKey); got != "send" {
		t.Errorf("Enter in the composer: got %q, want send", got)
	}
}
