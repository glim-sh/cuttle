package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// playwrightCLI is resolved from PATH rather than configured. Inside the cuttle
// image there is exactly one driver binary and one browser to drive; a flag
// pointing somewhere else would only ever name a browser this tool is not
// supposed to be driving.
const playwrightCLI = "playwright-cli"

var errDriver = errors.New("playwright-cli")

// driveTimeout bounds one driver command. playwright-cli does its own waiting
// for a page to settle, so this is the outer bound on a driver that never comes
// back at all, not a per-action budget.
const driveTimeout = 2 * time.Minute

// drive shells out to the driver and returns its combined output. The output is
// returned even on a non-zero exit because that is the only way the caller sees
// a `### Modal state` block: a pending native dialog makes `snapshot` an error,
// and the block naming the dialog rides along with it.
func drive(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, driveTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, playwrightCLI, args...)
	// WaitDelay makes the deadline and the interrupt real: CombinedOutput waits
	// for the output pipes to close, so a descendant still holding stdout would
	// otherwise keep this blocked long after the driver itself was killed.
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%w %s: %w", errDriver, args[0], err)
	}
	return string(out), nil
}

// takeSnapshot reads the current page. A driver error with a modal block is not
// a failure to report but a state to escalate on, so it comes back as a
// snapshot with Modal set rather than as an error.
func takeSnapshot(ctx context.Context) (snapshot, error) {
	out, err := drive(ctx, "snapshot")
	snap := parseSnapshot(out)
	if err != nil && snap.Modal == "" {
		return snapshot{}, err
	}
	return snap, nil
}

// execute performs the decided action against the page. The typed value is
// looked up from the plan step here, at the last possible moment, and passed to
// `fill` - the only verb cuttle's secret sentinels survive, because anything
// that types per character never lets `{{cuttle:NAME}}` reassemble.
func execute(ctx context.Context, dec decision, step planStep) error {
	switch dec.Action {
	case actionClick:
		_, err := drive(ctx, "click", dec.Target)
		return err
	case actionType:
		value, ok := step.Fill[dec.FillName]
		if !ok {
			return fmt.Errorf("%w: chose fill value %q, which the plan step does not define", errDriver, dec.FillName)
		}
		_, err := drive(ctx, "fill", dec.Target, value)
		return err
	case actionScroll:
		_, err := drive(ctx, "mousewheel", "0", "600")
		return err
	case actionBack:
		_, err := drive(ctx, "go-back")
		return err
	case actionDone, actionEscalate:
		return nil
	default:
		return fmt.Errorf("%w: unknown action %q", errDriver, string(dec.Action))
	}
}

// describe renders one element for the step log. Labels are authored by the
// page, so they are quoted into the log as data and never acted on as text.
func describe(snap snapshot, id string) string {
	for _, el := range snap.Elements {
		if el.ID == id {
			return fmt.Sprintf("%s %q", el.Role, el.Label)
		}
	}
	return strings.TrimSpace(id)
}
