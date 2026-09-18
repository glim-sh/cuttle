package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const (
	nameDriver       = "driver-passthrough"
	driverCmdTimeout = 90 * time.Second

	// Percent-encoded so the URL survives the exec hop with no quoting at all -
	// no spaces, quotes or shell metacharacters. It decodes to:
	// <button onclick="this.textContent='cuttle-pw-clicked'">cuttle-pw-button</button>
	driverPageURL = "data:text/html,%3Cbutton%20onclick=%22this.textContent=%27cuttle-pw-clicked%27%22%3Ecuttle-pw-button%3C/button%3E"

	// Matched as the aria snapshot prints a node, quotes included: every
	// response also echoes the page URL, which carries the same bare words.
	driverButtonNode  = `button "cuttle-pw-button"`
	driverClickedNode = `button "cuttle-pw-clicked"`

	driverScreenshotName = "smoke-driver.png"
)

// Attached through cuttle's mux the ref carries a frame prefix (f1e2), not the
// bare e2 a directly-launched browser hands out - accept both.
var driverRefRE = regexp.MustCompile(`\[ref=([a-z0-9]+)\]`)

// driverChecks exercises `cuttle pw`, the passthrough to the playwright-cli
// bundled in the image: navigate, snapshot, click by ref, and read the mutation
// back. There is no attach step on purpose - the first verb runs against a cold
// container, so a pass proves the wrapper's on-demand attach. It is the only
// check here that goes through cuttle's own CLI rather than raw CDP, so it
// needs a built binary AND a reachable container - hence the CUTTLE_BIN gate,
// which keeps a plain `go run ./test/smoke` against a remote cuttle working.
//
// The screenshot step proves `--filename` resolves against the exec workdir,
// but only that the driver reported writing it: the file lands in
// /data/__default__/Downloads, which the downloads API cannot serve in pool
// mode (it keys every request by a seed, and the reserved __default__ seed is
// not addressable there). CI asserts the file on disk instead.
func driverChecks(ctx context.Context) []checkResult {
	fmt.Println("\n== bundled driver passthrough (cuttle pw) ==")
	bin := os.Getenv("CUTTLE_BIN")
	if bin == "" {
		fmt.Println("  skipped: set CUTTLE_BIN=<path to a built cuttle> to exercise it")
		return nil
	}
	return []checkResult{driverPassthrough(ctx, bin)}
}

func driverPassthrough(ctx context.Context, bin string) checkResult {
	d := driverCLI{bin: bin, ctx: ctx}

	// detach stops the driver daemon and leaves the browser running. Its
	// failure is noise, not a verdict on the passthrough.
	defer func() {
		if _, err := d.run("detach"); err != nil {
			fmt.Printf("  note: %v\n", err)
		}
	}()

	// No attach: goto is the first verb, so the wrapper has to attach for it.
	if _, err := d.run("goto", driverPageURL); err != nil {
		return driverFail(err.Error())
	}

	before, err := d.run("snapshot")
	if err != nil {
		return driverFail(err.Error())
	}
	ref := driverRef(before, driverButtonNode)
	if ref == "" {
		return driverFail("snapshot named no ref for the test button: " + truncate(oneLine(before), 200))
	}

	if _, err = d.run("click", ref); err != nil {
		return driverFail(err.Error())
	}
	after, err := d.run("snapshot")
	if err != nil {
		return driverFail(err.Error())
	}
	if !strings.Contains(after, driverClickedNode) {
		return driverFail(fmt.Sprintf("click %s left the button unchanged: %s", ref, truncate(oneLine(after), 200)))
	}

	shot, err := d.run("screenshot", "--filename="+driverScreenshotName)
	if err != nil {
		// The first screenshot of a cold container can blow the driver's 5s
		// action timeout on font warmup; one retry separates that from a real
		// failure.
		fmt.Printf("  note: first screenshot attempt failed (%v), retrying\n", err)
		if shot, err = d.run("screenshot", "--filename="+driverScreenshotName); err != nil {
			return driverFail(err.Error())
		}
	}
	if !strings.Contains(shot, driverScreenshotName) {
		return driverFail("screenshot did not report writing " + driverScreenshotName + ": " + truncate(oneLine(shot), 200))
	}

	return checkResult{nameDriver, statusPass, fmt.Sprintf(
		"auto-attach, goto, snapshot, click %s, mutation read back, %s written", ref, driverScreenshotName,
	)}
}

// driverCLI runs `cuttle pw <args>` and returns its combined output; a non-zero
// exit becomes an error carrying that output, since the driver explains itself
// on stderr.
type driverCLI struct {
	bin string
	ctx context.Context
}

func (d driverCLI) run(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(d.ctx, driverCmdTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, d.bin, append([]string{"pw"}, args...)...) //nolint:gosec // harness-operator-supplied binary path by design
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("cuttle pw %s: %w: %s",
			strings.Join(args, " "), err, truncate(oneLine(string(out)), 300))
	}
	return string(out), nil
}

// driverRef pulls the element ref off the snapshot line holding the node.
func driverRef(snapshot, node string) string {
	for line := range strings.SplitSeq(snapshot, "\n") {
		if !strings.Contains(line, node) {
			continue
		}
		if m := driverRefRE.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	}
	return ""
}

func driverFail(detail string) checkResult {
	return checkResult{nameDriver, statusFail, detail}
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
