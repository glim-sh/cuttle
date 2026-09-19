//go:build live

// The one test that spends money. It is behind a build tag rather than a
// skip-if-unset so that `just check` can never reach it by accident: a suite
// that silently calls a paid API is a suite nobody can run in a loop.
//
//	CUTTLE_TYPESAFE_API_KEY=... go test -tags=live -run TestLive -v ./internal/jev/
package jev

import (
	"context"
	"testing"
	"time"
)

// TestLiveOneStep sends one real request per fixture, shaped the way a
// jev-browse step shapes one: both page-level nouls and the pick. It asserts
// the answers are well formed rather than what they say - the model's
// judgement is not this repo's to pin - except for the one thing that is ours:
// every key it may answer with came from the request. The logged picks are
// what a reader compares against the fixture's intended route.
func TestLiveOneStep(t *testing.T) {
	tr, err := newHTTPTransport()
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	t.Logf("endpoint %s model %s", tr.endpoint, tr.model)

	st, candidates := signinState(t, []Step{
		{URL: "http://127.0.0.1:8799/", Action: "[banner] link: Cart (0)", Changed: true},
		{URL: "http://127.0.0.1:8799/", Action: "button: Help", Failed: true},
	})
	t.Run("signin", func(t *testing.T) { liveStep(t, tr, st, candidates) })
	for fixture, task := range map[string]string{
		"article_toc.snapshot":   "reach the Geography section of the Lisbon article",
		"filters_panel.snapshot": "show only remote jobs",
		"jobs_landing.snapshot":  "search for jobs by the keyword",
	} {
		snap := parseFixture(t, fixture)
		l := &loop{Options: Options{Task: task}, valueNames: []string{"query"}}
		t.Run(fixture, func(t *testing.T) { liveStep(t, tr, l.state(snap), actionSpace(snap, l.valueNames)) })
	}
}

func liveStep(t *testing.T, tr *httpTransport, st state, candidates []candidate) {
	t.Helper()
	groups := group(candidates)
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	start := time.Now()
	resp, err := tr.evaluate(ctx, buildRequest(st, groups))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("live evaluate: %v", err)
	}
	t.Logf("answered in %s", elapsed.Round(time.Millisecond))

	for _, id := range []string{questionDone, questionBlocked} {
		a, ok := resp.Answers[id]
		if !ok {
			t.Fatalf("no answer came back for %q", id)
		}
		if a.Noul < 0 || a.Noul > 1 {
			t.Errorf("%s: noul %v is not a probability", id, a.Noul)
		}
		t.Logf("%s = %.4f", id, a.Noul)
	}

	pick, ok := resp.Answers[groupKey(0)]
	if !ok {
		t.Fatalf("no answer came back for %q", groupKey(0))
	}
	t.Logf("pick = %q at confidence %.4f over %d options", pick.Choice, pick.Confidence, len(pick.Probabilities))
	for key, p := range pick.Probabilities {
		if p > 0.001 {
			t.Logf("  %-24s %.4f", key, p)
		}
	}
	if pick.Confidence <= 0 || pick.Confidence > 1 {
		t.Errorf("confidence %v is not a probability - neither a sent one nor a derived one landed", pick.Confidence)
	}
	// The whole safety property of the pick: the loop acts on this key, so it has
	// to be one the page offered.
	if _, offered := find(groups[0], pick.Choice); !offered && pick.Choice != noneKey {
		t.Errorf("the answer %q is not an option this request offered", pick.Choice)
	}
}
