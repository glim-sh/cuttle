package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	jevEndpoint = "https://api.typesafe.ai/v1/systemone"
	jevModel    = "jev-latest"
	typeChoice  = "choice"
	typeNoul    = "noul"

	// statusOverloaded is TypeSafe's "529 Overloaded", which has no stdlib
	// constant and is the second of the two statuses their API reference says to
	// retry after a short delay.
	statusOverloaded = 529

	// errBodyLimit bounds how much of a failed response is quoted back. The body
	// of a 401 or a 422 names the offending field, and that is the whole reason
	// to read it; a runaway error page is not.
	errBodyLimit = 2 << 10
)

var (
	errNoAPIKey = errors.New("JEV_API_KEY is not set (use --mock to run the loop without a key)")
	errJevAPI   = errors.New("jev api")
)

type action string

const (
	actionClick    action = "click"
	actionType     action = "type"
	actionScroll   action = "scroll"
	actionBack     action = "back"
	actionDone     action = "done"
	actionEscalate action = "escalate"
)

// needsTarget reports whether the action is meaningless without an element to
// aim at, which is also what decides whether the target answer's confidence
// counts towards the threshold.
func (a action) needsTarget() bool { return a == actionClick || a == actionType }

// decision is one step's worth of judgement.
type decision struct {
	Action action
	Target string
	// FillName names a value in the plan step's Fill map. The value behind it is
	// looked up locally, at the last moment, and never leaves this process.
	FillName   string
	Confidence float64
	// StepDone is the probability that the step's expected outcome is already on
	// the page. A noul answer carries no separate confidence - the probability IS
	// the confidence - so the loop holds this to --confidence-threshold itself.
	StepDone float64
}

// question is everything the decider gets to see. It is assembled from the
// plan and from the interactive elements of the snapshot, and nothing else.
type question struct {
	Goal      string
	Step      planStep
	StepIndex int
	StepCount int
	Snap      snapshot
}

// decider is the seam between the loop and the model. It exists because Jev
// access is waitlisted: mockDecider is the second implementation, and it is
// what makes the loop runnable end to end without a key.
type decider interface {
	decide(ctx context.Context, q question) (decision, error)
}

// ---------------------------------------------------------------- jev backend

type jevDecider struct {
	apiKey string
	client *http.Client
}

func newJevDecider() (*jevDecider, error) {
	key := os.Getenv("JEV_API_KEY")
	if key == "" {
		return nil, errNoAPIKey
	}
	return &jevDecider{apiKey: key, client: &http.Client{Timeout: 30 * time.Second}}, nil
}

type jevQuestion struct {
	Type         string         `json:"type"`
	Instructions string         `json:"instructions"`
	Criteria     map[string]any `json:"criteria,omitempty"`
}

type jevRequest struct {
	State     any                    `json:"state"`
	Model     string                 `json:"model"`
	Questions map[string]jevQuestion `json:"questions"`
}

type jevAnswer struct {
	Choice     string  `json:"choice"`
	Noul       float64 `json:"noul"`
	Confidence float64 `json:"confidence"`
}

type jevResponse struct {
	Answers map[string]jevAnswer `json:"answers"`
}

// buildRequest turns one question into the single API call that answers all of
// it. Jev evaluates every question in parallel against the same state, so the
// speculative ones (which element, which fill value, is the step finished) cost
// latency we would pay anyway rather than extra round trips.
func buildRequest(q question) jevRequest {
	fills := q.Step.fillNames()

	actionCriteria := map[string]any{
		"click":    "Click the element named in the target answer to make progress on the current step",
		"scroll":   "Nothing on this page advances the step yet; scroll down to load or reveal more of it",
		"back":     "This page is a dead end for the goal; return to the previous page",
		"done":     "The overall goal is already achieved and no further action is needed",
		"escalate": "The step needs something this tool cannot do: a captcha, a credential the plan does not carry, or a page state the plan did not anticipate",
	}
	if len(fills) > 0 {
		actionCriteria["type"] = "Type one of the plan's prepared values into the input element named in the target answer"
	}

	questions := map[string]jevQuestion{
		"action": {
			Type:         typeChoice,
			Instructions: "What is the single next action that makes the most progress on the current plan step?",
			Criteria:     actionCriteria,
		},
		"target": {
			Type:         typeChoice,
			Instructions: "Which interactive element should the action act on?",
			Criteria:     elementCriteria(q.Snap.Elements),
		},
	}
	if len(fills) > 0 {
		values := make(map[string]any, len(fills))
		for _, name := range fills {
			values[name] = nil // the names are chosen by the plan author and describe themselves
		}
		questions["value"] = jevQuestion{
			Type:         typeChoice,
			Instructions: "If the action is to type, which of the plan's prepared values belongs in the target element?",
			Criteria:     values,
		}
	}
	if q.Step.Expect != "" {
		questions["step_complete"] = jevQuestion{
			Type:         typeNoul,
			Instructions: "The page already shows this expected outcome: " + q.Step.Expect,
		}
	}

	return jevRequest{State: buildState(q), Model: jevModel, Questions: questions}
}

func buildState(q question) map[string]any {
	return map[string]any{
		"goal": q.Goal,
		"plan_step": map[string]any{
			"number":           q.StepIndex + 1,
			"of":               q.StepCount,
			"description":      q.Step.Description,
			"expected_outcome": q.Step.Expect,
		},
		"page": map[string]any{
			"url":   q.Snap.URL,
			"title": q.Snap.Title,
		},
		"elements": q.Snap.Elements,
	}
}

func elementCriteria(elements []element) map[string]any {
	criteria := make(map[string]any, len(elements))
	for _, el := range elements {
		if el.Label == "" {
			criteria[el.ID] = "an unlabelled " + el.Role
			continue
		}
		criteria[el.ID] = fmt.Sprintf("the %s labelled %q", el.Role, el.Label)
	}
	return criteria
}

func (d *jevDecider) decide(ctx context.Context, q question) (decision, error) {
	resp, err := d.post(ctx, buildRequest(q))
	if err != nil {
		return decision{}, err
	}
	return decisionFrom(resp), nil
}

// decisionFrom collapses the parallel answers into one decision. The reported
// confidence is the WEAKEST of the answers the action actually depends on: a
// certain "click" aimed at an element Jev was torn about is exactly the case
// the threshold exists to catch.
func decisionFrom(resp jevResponse) decision {
	act := action(resp.Answers["action"].Choice)
	dec := decision{
		Action:     act,
		Confidence: resp.Answers["action"].Confidence,
		StepDone:   resp.Answers["step_complete"].Noul,
	}
	if act.needsTarget() {
		target := resp.Answers["target"]
		dec.Target = target.Choice
		dec.Confidence = math.Min(dec.Confidence, target.Confidence)
	}
	if act == actionType {
		value := resp.Answers["value"]
		dec.FillName = value.Choice
		dec.Confidence = math.Min(dec.Confidence, value.Confidence)
	}
	return dec
}

// post sends the request, retrying the two statuses TypeSafe documents as
// "retry after a short delay". Everything else is reported as-is: a 401 or a
// 422 will not fix itself, and a loop that keeps retrying one hides the cause.
func (d *jevDecider) post(ctx context.Context, payload jevRequest) (jevResponse, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return jevResponse{}, fmt.Errorf("encode jev request: %w", err)
	}

	var lastStatus int
	var lastBody string
	for attempt := range 3 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return jevResponse{}, fmt.Errorf("jev request: %w", ctx.Err())
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}

		status, decoded, failure, err := d.attempt(ctx, body)
		if err != nil {
			return jevResponse{}, err
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
		return jevResponse{}, fmt.Errorf("%w: http %d: %s", errJevAPI, lastStatus, lastBody)
	}
	return jevResponse{}, fmt.Errorf("%w: http %d", errJevAPI, lastStatus)
}

// attempt makes one call. A failed one comes back as its status plus the body
// TypeSafe explains itself in, because that body is the only thing that says
// WHICH field a 422 rejected. Both paths drain what they do not consume, so the
// connection goes back to the pool for the next step instead of being dropped.
func (d *jevDecider) attempt(ctx context.Context, body []byte) (int, jevResponse, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, jevEndpoint, bytes.NewReader(body))
	if err != nil {
		return 0, jevResponse{}, "", fmt.Errorf("build jev request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+d.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := d.client.Do(req)
	if err != nil {
		return 0, jevResponse{}, "", fmt.Errorf("call jev: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, errBodyLimit))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		failure, _ := io.ReadAll(io.LimitReader(resp.Body, errBodyLimit))
		return resp.StatusCode, jevResponse{}, strings.TrimSpace(string(failure)), nil
	}
	var decoded jevResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return 0, jevResponse{}, "", fmt.Errorf("decode jev response: %w", err)
	}
	return resp.StatusCode, decoded, "", nil
}

// --------------------------------------------------------------- mock backend

// mockDecider fills the step's prepared values into the first inputs it has not
// touched yet, then clicks the first untouched button or link. It is not a
// simulation of Jev's judgement - it exists so the snapshot parsing, the element
// filtering, the playwright-cli shell-out and the exit codes can be exercised
// end to end while Jev access is waitlisted. Remembering what it already acted
// on is what lets `--loop` run a whole form instead of retyping one field.
type mockDecider struct{ acted map[string]bool }

var typableRoles = map[string]bool{"textbox": true, "searchbox": true, "combobox": true}

func newMockDecider() *mockDecider { return &mockDecider{acted: map[string]bool{}} }

func (m *mockDecider) decide(_ context.Context, q question) (decision, error) {
	fills := q.Step.fillNames()
	filled := 0
	for _, el := range q.Snap.Elements {
		if !typableRoles[el.Role] {
			continue
		}
		if m.acted[el.ID] {
			filled++
			continue
		}
		if filled >= len(fills) {
			break
		}
		m.acted[el.ID] = true
		return decision{Action: actionType, Target: el.ID, FillName: fills[filled], Confidence: 1}, nil
	}
	for _, role := range []string{"button", "link"} {
		for _, el := range q.Snap.Elements {
			if el.Role == role && !m.acted[el.ID] {
				m.acted[el.ID] = true
				return decision{Action: actionClick, Target: el.ID, Confidence: 1}, nil
			}
		}
	}
	return decision{Action: actionEscalate, Confidence: 1}, nil
}
