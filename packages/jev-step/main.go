// Command jev-step takes one browsing decision and performs it.
//
// An LLM plans a browse or test run once, writing the goal, the milestones and
// every value that may be typed into a plan file. From there each step is a
// single call to a fast decision model (TypeSafe's Jev): the page's interactive
// elements go in, a typed action comes out, and playwright-cli performs it. No
// text is generated, so the per-step cost is a rounding error next to asking
// the LLM again.
//
// The exit code is the result. See README.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// Exit codes. Anything a caller has to branch on is a code, not a parsed line
// of output: a loop driver in bash is the expected caller.
const (
	exitStepDone = 0
	exitError    = 1
	exitGoalDone = 2
	exitEscalate = 3
)

type options struct {
	goal      string
	plan      plan
	stepIndex int
	threshold float64
	loop      bool
	maxSteps  int
}

func main() {
	var opt options
	flag.StringVar(&opt.goal, "goal", "", "what the run is trying to achieve, in one sentence")
	flag.IntVar(&opt.stepIndex, "step", 0, "zero-based index of the plan step to work on")
	flag.Float64Var(&opt.threshold, "confidence-threshold", 0.7, "escalate when the decision's confidence is below this")
	flag.BoolVar(&opt.loop, "loop", false, "keep deciding and acting until the goal is done, an escalation, or --max-steps")
	flag.IntVar(&opt.maxSteps, "max-steps", 20, "in --loop mode, the most actions to take before escalating")
	planPath := flag.String("plan", "", "path to the plan file written by the planning LLM")
	mock := flag.Bool("mock", false, "decide locally instead of calling Jev: no API key, no judgement, for testing the loop")
	flag.Parse()

	os.Exit(run(opt, *planPath, *mock))
}

func run(opt options, planPath string, mock bool) int {
	if opt.goal == "" || planPath == "" {
		fmt.Fprintln(os.Stderr, "jev-step: --goal and --plan are both required")
		flag.Usage()
		return exitError
	}

	loaded, err := loadPlan(planPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "jev-step: %v\n", err)
		return exitError
	}
	opt.plan = loaded
	if opt.stepIndex < 0 || opt.stepIndex >= len(loaded.Steps) {
		fmt.Fprintf(os.Stderr, "jev-step: --step %d is outside the plan's %d steps\n", opt.stepIndex, len(loaded.Steps))
		return exitError
	}

	brain := decider(newMockDecider())
	if !mock {
		jev, jevErr := newJevDecider()
		if jevErr != nil {
			fmt.Fprintf(os.Stderr, "jev-step: %v\n", jevErr)
			return exitError
		}
		brain = jev
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	code, err := loopSteps(ctx, brain, opt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "jev-step: %v\n", err)
		return exitError
	}
	return code
}

func loopSteps(ctx context.Context, brain decider, opt options) (int, error) {
	// One invocation is one decision unless --loop says otherwise, so --max-steps
	// only bounds the loop it was written for.
	if !opt.loop {
		opt.maxSteps = 1
	}
	for range opt.maxSteps {
		snap, err := takeSnapshot(ctx)
		if err != nil {
			return exitError, err
		}
		if snap.Modal != "" {
			return escalate("the page is behind a dialog: %s", snap.Modal), nil
		}
		if len(snap.Elements) == 0 {
			return escalate("the page at %s offers nothing to act on", snap.URL), nil
		}

		dec, err := brain.decide(ctx, question{
			Goal:      opt.goal,
			Step:      opt.plan.Steps[opt.stepIndex],
			StepIndex: opt.stepIndex,
			StepCount: len(opt.plan.Steps),
			Snap:      snap,
		})
		if err != nil {
			return exitError, err
		}

		// The expected outcome is judged against the page as it stands, so a
		// finished step is recognized before anything else is done to it.
		if dec.StepDone {
			fmt.Printf("step %d/%d complete: %s\n", opt.stepIndex+1, len(opt.plan.Steps), opt.plan.Steps[opt.stepIndex].Expect)
			if opt.stepIndex+1 >= len(opt.plan.Steps) {
				return exitGoalDone, nil
			}
			opt.stepIndex++
			if !opt.loop {
				return exitStepDone, nil
			}
			continue
		}

		if dec.Confidence < opt.threshold {
			return escalate("%s: confidence %.2f is below the %.2f threshold", dec.Action, dec.Confidence, opt.threshold), nil
		}
		if dec.Action == actionDone {
			fmt.Printf("goal reached at %s\n", snap.URL)
			return exitGoalDone, nil
		}
		if dec.Action == actionEscalate {
			return escalate("the step needs something this tool cannot do"), nil
		}

		if err := execute(ctx, dec, opt.plan.Steps[opt.stepIndex]); err != nil {
			return exitError, err
		}
		logAction(dec, snap)

		if !opt.loop {
			return exitStepDone, nil
		}
	}
	return escalate("gave up after %d actions without finishing the goal", opt.maxSteps), nil
}

func logAction(dec decision, snap snapshot) {
	switch {
	case dec.Action == actionType:
		fmt.Printf("type %s into %s (confidence %.2f)\n", dec.Value, describe(snap, dec.Target), dec.Confidence)
	case dec.Target != "":
		fmt.Printf("%s %s (confidence %.2f)\n", dec.Action, describe(snap, dec.Target), dec.Confidence)
	default:
		fmt.Printf("%s (confidence %.2f)\n", dec.Action, dec.Confidence)
	}
}

func escalate(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "jev-step: escalating: "+format+"\n", args...)
	return exitEscalate
}
