package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A key shaped like OpenRouter's and worthless: routing reads the prefix and
// nothing else, so the rest of it only has to not look like a real credential.
const fakeOpenRouterKey = "sk-or-fake-test-key"

func transportFor(t *testing.T, key string) *httpTransport {
	t.Helper()
	t.Setenv(APIKeyEnv, key)
	tr, err := newHTTPTransport()
	if err != nil {
		t.Fatalf("newHTTPTransport: %v", err)
	}
	return tr
}

// One env var serves both APIs, and the key's own prefix says which. Getting
// this wrong sends a live credential to the wrong host, so it is pinned.
func TestTheKeyPrefixRoutesTheTransport(t *testing.T) {
	cases := map[string]struct{ key, endpoint, model string }{
		"first party": {"ts-live-whatever", endpoint, model},
		"openrouter":  {fakeOpenRouterKey, openRouterEndpoint, openRouterModel},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tr := transportFor(t, tc.key)
			if tr.endpoint != tc.endpoint {
				t.Errorf("endpoint: got %q, want %q", tr.endpoint, tc.endpoint)
			}
			if tr.model != tc.model {
				t.Errorf("model: got %q, want %q", tr.model, tc.model)
			}
		})
	}
}

// The model an OpenRouter call names must be an exact version. The `~typesafe/
// jev-latest` alias moves on its own, and every decision this loop makes would
// move with it.
func TestTheOpenRouterModelIsPinnedToAVersion(t *testing.T) {
	if strings.HasPrefix(openRouterModel, "~") || strings.HasSuffix(openRouterModel, "latest") {
		t.Errorf("%q is a moving alias, not a pinned version", openRouterModel)
	}
}

// The transport stamps its own model over whatever the caller built, so one
// request builder serves both APIs.
func TestTheTransportStampsItsOwnModel(t *testing.T) {
	body, err := transportFor(t, fakeOpenRouterKey).body(request{Model: model})
	if err != nil {
		t.Fatalf("body: %v", err)
	}
	var sent request
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if sent.Model != openRouterModel {
		t.Errorf("model on the wire: got %q, want %q", sent.Model, openRouterModel)
	}
}

// The OpenRouter body is the first-party body with one field changed. Pinning it
// separately is what proves that stays true.
func TestOpenRouterRequestShapeGolden(t *testing.T) {
	st, candidates := signinState(t, []Step{
		{URL: "http://127.0.0.1:8799/", Action: "link: Cart (0)"},
		{URL: "http://127.0.0.1:8799/", Action: "button: Help", Failed: true},
	})
	body, err := transportFor(t, fakeOpenRouterKey).body(buildRequest(st, group(candidates)))
	if err != nil {
		t.Fatalf("body: %v", err)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, body, "", "  "); err != nil {
		t.Fatalf("indent: %v", err)
	}
	pretty.WriteByte('\n')
	checkGolden(t, "request.openrouter.golden.json", pretty.Bytes())
}

type round struct {
	status int
	body   string
}

// serving stands in for the API. Each call takes the next response in the list,
// so a retry is scripted as the status that must be retried followed by the
// answer it eventually gets.
func serving(t *testing.T, rounds ...round) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		next := rounds[min(calls, len(rounds)-1)]
		calls++
		w.WriteHeader(next.status)
		_, _ = w.Write([]byte(next.body))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// The live body OpenRouter returned, verbatim. It carries fields the first-party
// API does not - the distribution, the resolved model, usage - and decoding must
// go on ignoring all of them.
const openRouterLiveBody = `{"model":"typesafe/jev-1.13-20260917","answers":{` +
	`"blocked":{"type":"noul","noul":0.11},"done":{"type":"noul","noul":0.08},` +
	`"pick0":{"type":"choice","choice":"type:f2e7:username","probabilities":{` +
	`"type:f2e9:password":0,"type:f2e8:password":0.01,"type:f2e7:username":0.99,"f2e10":0,` +
	`"back":0,"f2e11":0,"type:f2e9:username":0,"none":0,"f2e3":0,"f2e12":0,` +
	`"type:f2e7:password":0,"type:f2e8:username":0},"confidence":0.98}},` +
	`"usage":{"input_tokens":1474,"output_tokens":198,"cost":0.000061908},` +
	`"id":"gen-dec-1789745451-Hu0cuD7SHa4SLgv5tSRD","provider":"TypeSafe"}`

func evaluateAgainst(t *testing.T, srv *httptest.Server) (response, error) {
	t.Helper()
	tr := transportFor(t, fakeOpenRouterKey)
	tr.endpoint = srv.URL
	return tr.evaluate(context.Background(), request{})
}

func TestOpenRouterAnswersDecode(t *testing.T) {
	srv, _ := serving(t, round{http.StatusOK, openRouterLiveBody})
	resp, err := evaluateAgainst(t, srv)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if got := resp.Answers[questionDone].Noul; got != 0.08 {
		t.Errorf("done: got %v, want 0.08", got)
	}
	if got := resp.Answers[questionBlocked].Noul; got != 0.11 {
		t.Errorf("blocked: got %v, want 0.11", got)
	}
	pick := resp.Answers["pick0"]
	if pick.Choice != "type:f2e7:username" {
		t.Errorf("choice: got %q", pick.Choice)
	}
	// The sent confidence is the one that counts. It is NOT the winner's
	// probability - that pick came back at p=0.99 and confidence 0.98 - so a
	// derivation that overwrote it would report a number the API did not give.
	if pick.Confidence != 0.98 {
		t.Errorf("confidence: got %v, want the 0.98 the API sent, not the 0.99 probability", pick.Confidence)
	}
}

// OpenRouter's documented answer shape carries the distribution and no
// confidence at all, so the winner's own probability stands in for it.
func TestAConfidencelessChoiceFallsBackToItsProbability(t *testing.T) {
	srv, _ := serving(t, round{
		http.StatusOK,
		`{"answers":{"pick0":{"choice":"e2","probabilities":{"e1":0.3,"e2":0.7}}}}`,
	})
	resp, err := evaluateAgainst(t, srv)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if got := resp.Answers["pick0"].Confidence; got != 0.7 {
		t.Errorf("confidence: got %v, want the winner's probability 0.7", got)
	}
}

// A rate limit is the one class of failure that fixes itself, and the body of
// the eventual success is what the caller gets.
func TestARateLimitIsRetried(t *testing.T) {
	retryUnit = time.Millisecond
	t.Cleanup(func() { retryUnit = time.Second })

	srv, calls := serving(t,
		round{http.StatusTooManyRequests, `{"error":{"message":"rate limited","code":429}}`},
		round{http.StatusOK, `{"answers":{"done":{"noul":0.42}}}`})
	resp, err := evaluateAgainst(t, srv)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if *calls != 2 {
		t.Errorf("made %d calls, want the 429 plus one retry", *calls)
	}
	if got := resp.Answers[questionDone].Noul; got != 0.42 {
		t.Errorf("done: got %v, want the retry's answer 0.42", got)
	}
}

// A bad key and an empty balance are the two failures a retry cannot fix. Both
// must come back on the first call, carrying the body that says which it was -
// a loop that hid either behind three attempts would look like a slow network.
func TestAFailureThatCannotFixItselfIsReportedAtOnce(t *testing.T) {
	cases := map[string]round{
		"unauthorized":         {http.StatusUnauthorized, `{"error":{"message":"User not found.","code":401}}`},
		"insufficient credits": {http.StatusPaymentRequired, `{"error":{"message":"Insufficient credits","code":402}}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv, calls := serving(t, tc)
			_, err := evaluateAgainst(t, srv)
			if err == nil {
				t.Fatal("a failed call reported success")
			}
			if *calls != 1 {
				t.Errorf("made %d calls, want one - this status will not fix itself", *calls)
			}
			// The body is the only thing that says WHICH failure it was, so it has to
			// survive into the error.
			if !strings.Contains(err.Error(), tc.body) {
				t.Errorf("the error dropped the API's explanation: %v", err)
			}
		})
	}
}
