package jev

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the request-shape golden")

// scriptedTransport replays answers per question id, so a test can express the
// exact thing the model would have to say for a given outcome. Each call pops
// the next map, which is how a runoff is scripted separately from the groups.
type scriptedTransport struct {
	rounds   []map[string]answer
	requests []request
}

func (s *scriptedTransport) evaluate(_ context.Context, req request) (response, error) {
	s.requests = append(s.requests, req)
	round := map[string]answer{}
	if len(s.rounds) > 0 {
		round, s.rounds = s.rounds[0], s.rounds[1:]
	}
	return response{Answers: round}, nil
}

func signinState(t *testing.T, history []Step) (state, []candidate) {
	t.Helper()
	snap := parseFixture(t, "signin.snapshot")
	candidates := actionSpace(snap, []string{"password", "username"}, history)
	l := &loop{Options: Options{Task: "sign in to the demo shop"}, valueNames: []string{"password", "username"}, history: history}
	return l.state(snap, candidates), candidates
}

func TestActionSpacePrunesDedupsAndOffersTheValuesByName(t *testing.T) {
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

// A route already walked on this page is noise; a route that FAILED is not -
// the ref went stale or an overlay ate the click, and retrying is the fix.
func TestActionSpacePrunesDoneRoutesButKeepsFailedOnes(t *testing.T) {
	history := []Step{
		{URL: "http://127.0.0.1:8799/", Action: "button: Login"},
		{URL: "http://127.0.0.1:8799/", Action: "button: Help", Failed: true},
		{URL: "http://elsewhere.example/", Action: "link: Home"},
	}
	_, candidates := signinState(t, history)

	got := map[string]bool{}
	for _, c := range candidates {
		got[c.Label] = true
	}
	if got["button: Login"] {
		t.Error("an action that already worked on this page was offered again")
	}
	if !got["button: Help"] {
		t.Error("an action that failed must stay in the action space")
	}
	if !got["link: Home"] {
		t.Error("an action taken on a DIFFERENT page must not be pruned here")
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
	candidates := actionSpace(parseFixture(t, "consent_register.snapshot"), []string{"taxpayer_id"}, nil)
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

func TestDecideReportsNoneWhenEveryGroupDeclines(t *testing.T) {
	tr := &scriptedTransport{rounds: []map[string]answer{{"pick0": {Choice: noneKey}, "pick1": {Choice: ""}}}}
	dec, err := decide(context.Background(), tr, state{},
		[][]candidate{{{Key: "e1"}}, {{Key: "e2"}}})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if dec.Key != noneKey {
		t.Errorf("got %q, want %q", dec.Key, noneKey)
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

// The model picks WHICH field a value belongs in. What the value IS never leaves
// this process - it is looked up after the answer comes back.
func TestBuildRequestSendsNamesNotValues(t *testing.T) {
	st, candidates := signinState(t, nil)
	req := buildRequest(st, group(candidates))
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, secret := range []string{"hunter2", "{{cuttle:DEMO_PASS}}", "qa@example.com"} {
		if strings.Contains(string(body), secret) {
			t.Errorf("a prepared value reached the request body: %q", secret)
		}
	}
	if !strings.Contains(string(body), `"values":["password","username"]`) {
		t.Error("the request does not offer the prepared values by name")
	}
	// Page body text is the other half of this: the elements carry everything the
	// decision needs, and the prose around them is what must not be sent.
	if strings.Contains(string(body), "Some page text that must never be sent") {
		t.Error("page body text reached the request body")
	}
}

// The request shape is the contract with the API. Pinning it turns any change
// into a diff someone has to regenerate and read, the way the fingerprint golden
// does for the stealth args.
func TestRequestShapeGolden(t *testing.T) {
	st, candidates := signinState(t, []Step{
		{URL: "http://127.0.0.1:8799/", Action: "link: Cart (0)"},
		{URL: "http://127.0.0.1:8799/", Action: "button: Help", Failed: true},
	})
	got, err := json.MarshalIndent(buildRequest(st, group(candidates)), "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	checkGolden(t, "request.golden.json", append(got, '\n'))
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
