// Package jev drives cuttle's browser from TypeSafe's System-One model: no text
// is generated anywhere in the loop, so the per-step cost is a rounding error
// next to asking an LLM which button to press next.
//
// The model only ever CHOOSES - which of the page's elements to act on, whether
// the task is finished, whether it is blocked. Everything else is this package's
// code: the page is read with the bundled playwright-cli driver, the values
// typed into it come from the caller and never reach the API, and the action is
// performed by the same driver a person would use to take the session over.
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	endpoint = "https://api.typesafe.ai/v1/systemone"
	model    = "jev-latest"

	// OpenRouter resells the same System-One model, and the request body it takes
	// is the one above byte for byte - only the URL, the auth and the model name
	// differ. Which of the two a key belongs to is not a question worth a flag or
	// a second env var: an OpenRouter key says so itself in its first six
	// characters, so the key routes itself.
	openRouterEndpoint  = "https://openrouter.ai/api/alpha/decisions"
	openRouterKeyPrefix = "sk-or-"
	// openRouterModel is pinned to an exact version on purpose. The `~typesafe/
	// jev-latest` alias moves, and a model that changes under a fixed prompt
	// changes every decision this loop makes without a line of the diff to show
	// for it.
	openRouterModel = "typesafe/jev-1.13"

	typeChoice = "choice"
	typeNoul   = "noul"

	// questionDone and questionBlocked name the two page-level nouls. The keys are
	// ours to choose and the answers come back under them.
	questionDone    = "done"
	questionBlocked = "blocked"

	// statusOverloaded is TypeSafe's "529 Overloaded", which has no stdlib
	// constant and is the second of the two statuses their API reference says to
	// retry after a short delay.
	statusOverloaded = 529

	// errBodyLimit bounds how much of a failed response is quoted back. The body
	// of a 401 or a 422 names the offending field, and that is the whole reason
	// to read it; a runaway error page is not.
	errBodyLimit = 2 << 10

	requestTimeout = 30 * time.Second
	maxAttempts    = 3
)

// retryUnit is the base of the doubling backoff, and a var only so a test can
// exercise the retry without waiting out a real one.
var retryUnit = time.Second

// APIKeyEnv is the ONLY place the key is read from, and it is exported so the
// command's help can name it. No flag: a key on a command line lands in the
// shell history and in every `ps` on the host. Its VALUE is never logged.
const APIKeyEnv = "CUTTLE_TYPESAFE_API_KEY" //nolint:gosec // the name of an env var, not a credential

var (
	errNoAPIKey = errors.New(APIKeyEnv + " is not set (use --mock to run the loop without a key)")
	errAPI      = errors.New("typesafe api")
)

// question is one typed question. The three primitives share type+instructions;
// a noul's criteria is {true,false} prose and a choice's is option -> rubric.
type question struct {
	Type         string `json:"type"`
	Instructions any    `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

type noulCriteria struct {
	True  string `json:"true"`
	False string `json:"false"`
}

// request is one System-One call: every question is evaluated against the same
// state, in parallel, so bundling the done check, the blocked check and the
// element pick costs one round trip rather than three.
type request struct {
	// State is typed per call rather than once: a browsing step judges the page
	// and its history, an extract call judges a batch of lines, and the questions
	// address the fields of whichever one they were sent with by name.
	State     any                 `json:"state"`
	Model     string              `json:"model"`
	Questions map[string]question `json:"questions"`
}

// answer is the union of the three answer shapes. A noul answer carries NO
// confidence field - its probability IS its confidence - which is why the loop
// holds a noul to the same threshold every other answer clears.
type answer struct {
	Choice string  `json:"choice"`
	Noul   float64 `json:"noul"`
	// Confidence is what both APIs actually send on a choice. Probabilities is the
	// full distribution over the options, which only OpenRouter returns - and
	// which its documented answer shape carries INSTEAD of a confidence, so it is
	// read as the fallback when no confidence came back.
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
}

type response struct {
	Answers map[string]answer `json:"answers"`
}

// fill supplies the confidence an answer did not carry, from the winning
// option's own probability. The two are not the same number when both are sent
// - a pick at p=0.99 came back with confidence 0.98 - so the sent one always
// wins and this only ever covers its absence.
func (r response) fill() response {
	for id, a := range r.Answers {
		if a.Confidence == 0 && a.Choice != "" {
			a.Confidence = a.Probabilities[a.Choice]
			r.Answers[id] = a
		}
	}
	return r
}

// transport is the seam between the loop and TypeSafe. It exists for --mock,
// which is the second implementation and the one the tests run: the loop, the
// snapshot parsing, the driver shell-out and the exit codes are all exercised
// end to end without a key.
type transport interface {
	evaluate(ctx context.Context, req request) (response, error)
}

// ------------------------------------------------------------- http transport

type httpTransport struct {
	apiKey   string
	endpoint string
	model    string
	client   *http.Client
}

func newHTTPTransport() (*httpTransport, error) {
	key := os.Getenv(APIKeyEnv)
	if key == "" {
		return nil, errNoAPIKey
	}
	t := &httpTransport{
		apiKey:   key,
		endpoint: endpoint,
		model:    model,
		client:   &http.Client{Timeout: requestTimeout},
	}
	if strings.HasPrefix(key, openRouterKeyPrefix) {
		t.endpoint, t.model = openRouterEndpoint, openRouterModel
	}
	return t, nil
}

// body is the wire request. The model is stamped here rather than by the caller
// because it is the one field that belongs to the transport: the same question
// set is the same question set whichever of the two serves it.
func (t *httpTransport) body(req request) ([]byte, error) {
	req.Model = t.model
	out, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	return out, nil
}

// evaluate sends the request, retrying the two statuses TypeSafe documents as
// "retry after a short delay". Everything else is reported as-is: a 401 or a 422
// will not fix itself, and a loop that keeps retrying one hides the cause.
func (t *httpTransport) evaluate(ctx context.Context, req request) (response, error) {
	body, err := t.body(req)
	if err != nil {
		return response{}, err
	}

	var lastStatus int
	var lastBody string
	for attempt := range maxAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return response{}, fmt.Errorf("typesafe request: %w", ctx.Err())
			case <-time.After(time.Duration(1<<attempt) * retryUnit):
			}
		}
		status, decoded, failure, err := t.attempt(ctx, body)
		if err != nil {
			return response{}, err
		}
		if status == http.StatusOK {
			return decoded, nil
		}
		lastStatus, lastBody = status, failure
		if status != http.StatusTooManyRequests && status != statusOverloaded {
			break
		}
	}
	if lastBody != "" {
		return response{}, fmt.Errorf("%w: http %d: %s", errAPI, lastStatus, lastBody)
	}
	return response{}, fmt.Errorf("%w: http %d", errAPI, lastStatus)
}

// attempt makes one call. A failed one comes back as its status plus the body
// TypeSafe explains itself in, because that body is the only thing that says
// WHICH field a 422 rejected. Both paths drain what they do not consume, so the
// connection goes back to the pool for the next step instead of being dropped.
func (t *httpTransport) attempt(ctx context.Context, body []byte) (int, response, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, response{}, "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+t.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		return 0, response{}, "", fmt.Errorf("call typesafe: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, errBodyLimit))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		failure, _ := io.ReadAll(io.LimitReader(resp.Body, errBodyLimit))
		return resp.StatusCode, response{}, strings.TrimSpace(string(failure)), nil
	}
	var decoded response
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return 0, response{}, "", fmt.Errorf("decode response: %w", err)
	}
	return resp.StatusCode, decoded.fill(), "", nil
}

// ------------------------------------------------------------- mock transport

// mockTransport answers every question from the request itself: never done,
// never blocked, and the pick is the first option the request offered - a
// prepared value's field before a plain click, so a form is filled before it is
// submitted. It is not a simulation of the model's judgement. It exists so the
// whole loop runs without a key, and because the candidates are already pruned
// by history, "the first option" walks a page instead of pressing one button
// forever. When only `none` is left it answers `none`, which stops the loop.
type mockTransport struct{}

func (mockTransport) evaluate(_ context.Context, req request) (response, error) {
	st, _ := req.State.(state)
	answers := make(map[string]answer, len(req.Questions))
	for id, q := range req.Questions {
		if q.Type == typeNoul {
			answers[id] = answer{Noul: 0}
			continue
		}
		answers[id] = answer{Choice: mockPick(q, st), Confidence: 1}
	}
	return response{Answers: answers}, nil
}

// mockPick reads the options back off the choice question it is answering, so
// the mock never needs its own copy of the action space. History is consulted
// for the one case pruning does not cover: a field already filled on this page
// is still offered - a value can legitimately need retyping - and a mock that
// took it every time would fill one box until the step budget ran out.
func mockPick(q question, st state) string {
	options, _ := q.Criteria.(map[string]string)
	best, bestRank := noneKey, 0
	for key, label := range options {
		rank := mockRank(key)
		if rank == 0 || tried(st.History, st.Page.URL, label) {
			continue
		}
		// The key breaks ties, because a map has no order and a mock that picked a
		// different option on each run would make every test that uses it flaky.
		if best == noneKey || rank < bestRank || (rank == bestRank && key < best) {
			best, bestRank = key, rank
		}
	}
	return best
}

// mockRank orders the action space for the mock: prepared values go into their
// fields before anything is clicked, and within a kind the lowest ref number
// wins. 0 means "never pick this" - `back` and `enter` would let the mock walk
// in circles, and `none` is what is left when nothing else ranks. Judgement is
// what the real transport is for; this only has to be deterministic and to keep
// moving forward.
func mockRank(key string) int {
	switch key {
	case noneKey, backKey, enterKey:
		return 0
	}
	rank := refNumber(refOf(key)) + 1
	if !strings.HasPrefix(key, typeKeyPrefix) {
		rank += 1 << 20
	}
	return rank
}

var trailingDigitsRE = regexp.MustCompile(`(\d+)$`)

// refNumber is the element number a ref ends in, which on most pages runs in
// document order - near enough to "the topmost control" for a mock.
func refNumber(ref string) int {
	m := trailingDigitsRE.FindStringSubmatch(ref)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}
