package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func loginQuestion(t *testing.T) question {
	t.Helper()
	return question{
		Goal: "sign in and open the account page",
		Step: planStep{
			Description: "sign in with the test account",
			Expect:      "the account menu is visible",
			Fill: map[string]string{
				"username": "demo-user",
				"password": "{{cuttle:DEMO_PASS}}",
			},
		},
		StepIndex: 0,
		StepCount: 2,
		Snap:      parseSnapshot(readFixture(t, "signin.snapshot")),
	}
}

// The one guarantee the whole design rests on: a fill value, secret or not,
// never reaches TypeSafe. Only its NAME does, as a Choice option.
func TestBuildRequestSendsNamesNotValues(t *testing.T) {
	body, err := json.Marshal(buildRequest(loginQuestion(t)))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	payload := string(body)

	for _, secret := range []string{"demo-user", "{{cuttle:DEMO_PASS}}", "cuttle:DEMO_PASS"} {
		if strings.Contains(payload, secret) {
			t.Errorf("the request carries a fill value: %q", secret)
		}
	}
	for _, name := range []string{"username", "password"} {
		if !strings.Contains(payload, name) {
			t.Errorf("the request is missing the fill name %q", name)
		}
	}
	if strings.Contains(payload, "must never be sent") {
		t.Error("the request carries page body text")
	}
}

func TestBuildRequestQuestions(t *testing.T) {
	withFills := buildRequest(loginQuestion(t))
	if _, ok := withFills.Questions["value"]; !ok {
		t.Error("a step with fill values must ask which one to type")
	}
	if _, ok := withFills.Questions["action"].Criteria["type"]; !ok {
		t.Error("a step with fill values must offer the type action")
	}
	if _, ok := withFills.Questions["step_complete"]; !ok {
		t.Error("a step with an expected outcome must ask whether it is already met")
	}
	if got, want := len(withFills.Questions["target"].Criteria), 8; got != want {
		t.Errorf("target options: got %d, want %d", got, want)
	}

	bare := loginQuestion(t)
	bare.Step = planStep{Description: "read the dashboard"}
	stripped := buildRequest(bare)
	if _, ok := stripped.Questions["value"]; ok {
		t.Error("a step with nothing to type must not ask which value to type")
	}
	if _, ok := stripped.Questions["action"].Criteria["type"]; ok {
		t.Error("a step with nothing to type must not offer the type action")
	}
	if _, ok := stripped.Questions["step_complete"]; ok {
		t.Error("a step with no expected outcome has nothing to check it against")
	}
}

// Shaped exactly like a real response body, so a drift in the documented field
// names shows up here rather than as a silent zero-confidence escalation.
const typeResponse = `{
  "model": "jev-latest",
  "answers": {
    "action": {"type": "choice", "choice": "type", "probabilities": {"type": 0.9, "click": 0.1}, "confidence": 0.88},
    "target": {"type": "choice", "choice": "f2e8", "probabilities": {"f2e8": 0.7, "f2e7": 0.3}, "confidence": 0.41},
    "value": {"type": "choice", "choice": "password", "probabilities": {"password": 1.0}, "confidence": 1.0},
    "step_complete": {"type": "noul", "noul": 0.02}
  },
  "usage": {"input_tokens": 412, "output_tokens": 64}
}`

func TestDecisionFrom(t *testing.T) {
	var resp jevResponse
	if err := json.Unmarshal([]byte(typeResponse), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	dec := decisionFrom(resp)

	if dec.Action != actionType || dec.Target != "f2e8" || dec.Value != "password" {
		t.Errorf("got %+v", dec)
	}
	if dec.Confidence != 0.41 {
		t.Errorf("confidence: got %v, want the weakest answer's 0.41", dec.Confidence)
	}
	if dec.StepDone {
		t.Error("a 0.02 noul must not read as a finished step")
	}
}

func TestDecisionFromIgnoresSpeculativeAnswers(t *testing.T) {
	var resp jevResponse
	if err := json.Unmarshal([]byte(typeResponse), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Same body, but Jev picked an action that needs no element: the low target
	// confidence is now irrelevant and must not drag the decision under a
	// threshold.
	resp.Answers["action"] = jevAnswer{Choice: "done", Confidence: 0.95}
	dec := decisionFrom(resp)

	if dec.Action != actionDone || dec.Target != "" || dec.Value != "" {
		t.Errorf("got %+v", dec)
	}
	if dec.Confidence != 0.95 {
		t.Errorf("confidence: got %v, want 0.95", dec.Confidence)
	}
}

func TestMockDeciderWalksAFormThenClicks(t *testing.T) {
	q := loginQuestion(t)
	mock := newMockDecider()

	got := make([]string, 0, 4)
	for range 4 {
		dec, err := mock.decide(context.Background(), q)
		if err != nil {
			t.Fatalf("decide: %v", err)
		}
		got = append(got, string(dec.Action)+" "+dec.Target+" "+dec.Value)
	}

	// The mock pairs fill names to inputs in sorted order and has no idea which
	// field is which - hence "password" landing in the Username box. That is the
	// point of the backend: it exercises the loop, it does not judge the page.
	want := []string{
		"type f2e7 password",
		"type f2e8 username",
		"click f2e11 ", // Login, the first button once both values are in
		"click f2e12 ", // Help
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("decision %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestMockDeciderEscalatesOnADeadPage(t *testing.T) {
	dec, err := newMockDecider().decide(context.Background(), question{Step: planStep{Description: "do something"}})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if dec.Action != actionEscalate {
		t.Errorf("got %q, want escalate", dec.Action)
	}
}
