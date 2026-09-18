package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubDriver puts a fake `playwright-cli` first on PATH: it answers `snapshot`
// with a captured fixture and appends every other invocation to a log. No
// production seam is needed for this - the real binary is found on PATH too.
func stubDriver(t *testing.T, fixture string) (logPath string) { //nolint:nonamedreturns // the name is the doc comment here
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = snapshot ]; then cat " + filepath.Join(dir, "snapshot.txt") + "; exit 0; fi\n" +
		"echo \"$@\" >> " + logPath + "\n"
	if err := os.WriteFile(filepath.Join(dir, "playwright-cli"), []byte(script), 0o700); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "snapshot.txt"), []byte(readFixture(t, fixture)), 0o600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// scriptedDecider replays decisions in order, so a test can express the exact
// sequence Jev would have to produce for a given exit code.
type scriptedDecider struct{ decisions []decision }

func (s *scriptedDecider) decide(context.Context, question) (decision, error) {
	dec := s.decisions[0]
	s.decisions = s.decisions[1:]
	return dec, nil
}

func twoStepPlan() plan {
	return plan{Steps: []planStep{
		{
			Description: "sign in with the demo account",
			Expect:      "the account menu is visible",
			Fill:        map[string]string{"password": "{{cuttle:DEMO_PASS}}"},
		},
		{Description: "open the orders page", Expect: "the orders table is visible"},
	}}
}

func TestLoopStepsExitCodes(t *testing.T) {
	cases := map[string]struct {
		decisions []decision
		loop      bool
		want      int
	}{
		"one action is one step": {
			decisions: []decision{{Action: actionClick, Target: "f2e11", Confidence: 0.9}},
			want:      exitStepDone,
		},
		"jev says the goal is reached": {
			decisions: []decision{{Action: actionDone, Confidence: 0.95}},
			want:      exitGoalDone,
		},
		"the last step's expected outcome is on the page": {
			decisions: []decision{
				{Action: actionClick, Target: "f2e11", Confidence: 0.9, StepDone: 0.95},
				{Action: actionClick, Target: "f2e11", Confidence: 0.9, StepDone: 0.95},
			},
			loop: true,
			want: exitGoalDone,
		},
		"a finished step tells a non-loop caller to advance": {
			decisions: []decision{{Action: actionClick, Target: "f2e11", Confidence: 0.9, StepDone: 0.95}},
			want:      exitStepComplete,
		},
		// The noul is the only confidence a step-completion answer carries, so a
		// near coin flip must not be able to skip a step or end the run.
		"an unconfident step-completion answer is not a finished step": {
			decisions: []decision{{Action: actionClick, Target: "f2e11", Confidence: 0.9, StepDone: 0.51}},
			want:      exitStepDone,
		},
		"a target the page never offered is refused": {
			decisions: []decision{{Action: actionClick, Target: "f9e99", Confidence: 0.99}},
			want:      exitEscalate,
		},
		"an empty target is refused": {
			decisions: []decision{{Action: actionType, Target: "", FillName: "password", Confidence: 0.99}},
			want:      exitEscalate,
		},
		"jev asks to escalate": {
			decisions: []decision{{Action: actionEscalate, Confidence: 0.99}},
			want:      exitEscalate,
		},
		"the decision is under the threshold": {
			decisions: []decision{{Action: actionClick, Target: "f2e11", Confidence: 0.69}},
			want:      exitEscalate,
		},
		"the loop runs out of steps": {
			decisions: []decision{
				{Action: actionClick, Target: "f2e11", Confidence: 0.9},
				{Action: actionClick, Target: "f2e12", Confidence: 0.9},
			},
			loop: true,
			want: exitEscalate,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			stubDriver(t, "signin.snapshot")
			code, err := loopSteps(context.Background(), &scriptedDecider{decisions: tc.decisions}, options{
				goal:      "sign in to the demo shop",
				plan:      twoStepPlan(),
				threshold: 0.7,
				loop:      tc.loop,
				maxSteps:  len(tc.decisions),
			})
			if err != nil {
				t.Fatalf("loop: %v", err)
			}
			if code != tc.want {
				t.Errorf("exit code: got %d, want %d", code, tc.want)
			}
		})
	}
}

// A dialog parks the renderer, so every read after it is a read of a stale page.
// Recognizing that is worth more than any action the loop could take next.
func TestLoopStepsEscalatesOnAModalState(t *testing.T) {
	stubDriver(t, "modal.snapshot")
	code, err := loopSteps(context.Background(), &scriptedDecider{}, options{
		goal: "sign in to the demo shop", plan: twoStepPlan(), threshold: 0.7, maxSteps: 1,
	})
	if err != nil {
		t.Fatalf("loop: %v", err)
	}
	if code != exitEscalate {
		t.Errorf("exit code: got %d, want %d", code, exitEscalate)
	}
}

// The value reaches the page exactly as the plan wrote it. cuttle substitutes
// its sentinels inside its own CDP frame, on the fill path, so anything this
// tool does to the string on the way past would break the substitution.
func TestLoopStepsPassesTheSentinelThrough(t *testing.T) {
	logPath := stubDriver(t, "signin.snapshot")
	if _, err := loopSteps(context.Background(), &scriptedDecider{decisions: []decision{
		{Action: actionType, Target: "f2e8", FillName: "password", Confidence: 0.9},
	}}, options{
		goal: "sign in to the demo shop", plan: twoStepPlan(), threshold: 0.7, maxSteps: 1,
	}); err != nil {
		t.Fatalf("loop: %v", err)
	}

	calls, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read driver log: %v", err)
	}
	if got, want := strings.TrimSpace(string(calls)), "fill f2e8 {{cuttle:DEMO_PASS}}"; got != want {
		t.Errorf("driver calls: got %q, want %q", got, want)
	}
}
