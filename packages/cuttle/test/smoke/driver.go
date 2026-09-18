package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

var (
	errNoBrowserID        = errors.New("/json/version named no browser")
	errBrowserNotReplaced = errors.New("cuttle still serves the browser that was killed")
)

const (
	nameDriver       = "driver-passthrough"
	nameDrift        = "driver-attachment-drift"
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

	// The container `cuttle pw` execs into by default, and the one CI starts.
	driftContainer = "cuttle"
	// The only Chrome-family binary in the image; killing it is the closest
	// stand-in for the browser dying under the driver mid-session.
	driftBrowserBinary = "/opt/browser/chrome"
	// How long cuttle gets to bring a replacement browser up after the kill.
	driftRelaunchTimeout = 90 * time.Second
	// Read back through the driver and matched against cuttle's own
	// /json/version: a driver that had drifted onto a browser it launched itself
	// would report a different UA and a true webdriver.
	driftEvalJS      = "() => navigator.userAgent + '|webdriver=' + navigator.webdriver"
	driftWebdriverOK = "|webdriver=false"
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
func driverChecks(ctx context.Context, cuttleURL string) []checkResult {
	fmt.Println("\n== bundled driver passthrough (cuttle pw) ==")
	bin := os.Getenv("CUTTLE_BIN")
	if bin == "" {
		fmt.Println("  skipped: set CUTTLE_BIN=<path to a built cuttle> to exercise it")
		return nil
	}
	results := []checkResult{driverPassthrough(ctx, bin)}
	if drift := driverNoDrift(ctx, bin, cuttleURL); drift != nil {
		results = append(results, *drift)
	}
	return results
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

// driverNoDrift is the attachment-drift tripwire. The failure it guards against
// is not hypothetical: a playwright driver whose CDP endpoint goes away can fall
// back to launching a browser of its own and keep "working" - against a
// stealth-less, logged-out Chrome nobody asked for. This image is built so that
// cannot happen (every session goes through connectOverCDP, no
// playwright-managed browser was ever downloaded, and cuttle's own binary is the
// only Chrome in the image), and this check is what keeps that true: it kills
// cuttle's browser under a live driver session, waits for cuttle to bring a
// replacement up, and proves the next verb lands on THAT browser.
//
// It needs `docker exec` into the container `cuttle pw` targets, which the
// CUTTLE_BIN gate already implies for a local run but not for a remote cuttle -
// so a kill that cannot run is a printed skip, not a failure.
func driverNoDrift(ctx context.Context, bin, cuttleURL string) *checkResult {
	d := driverCLI{bin: bin, ctx: ctx}
	defer func() {
		if _, err := d.run("detach"); err != nil {
			fmt.Printf("  note: %v\n", err)
		}
	}()

	before, err := browserIdentity(ctx, cuttleURL)
	if err != nil {
		fmt.Printf("  skipped attachment-drift check: reading cuttle's identity: %v\n", err)
		return nil
	}

	// The passthrough check detaches on its way out, and a kill with no session
	// alive would only re-test the cold attach - so bring one up first, the drop
	// has to happen under it.
	if _, err = d.run("eval", driftEvalJS); err != nil {
		return &checkResult{nameDrift, statusFail, "opening the session to kill the browser under: " + err.Error()}
	}

	kill := exec.CommandContext(ctx, "docker", "exec", driftContainer, "pkill", "-f", driftBrowserBinary)
	if out, killErr := kill.CombinedOutput(); killErr != nil {
		fmt.Printf("  skipped attachment-drift check: killing the browser in %q: %v: %s\n",
			driftContainer, killErr, truncate(oneLine(string(out)), 200))
		return nil
	}

	after, err := waitForNewBrowser(ctx, cuttleURL, before.ws)
	if err != nil {
		return &checkResult{nameDrift, statusFail, fmt.Sprintf("cuttle did not bring a browser back: %v", err)}
	}

	// A loud failure here would not be drift, but in this image the wrapper is
	// meant to re-attach and carry on (the dead session reports the same "is not
	// open, please run" the on-demand attach already answers), so anything else
	// is a regression worth reading - and the driver's own wording comes back in
	// the detail.
	out, err := d.run("eval", driftEvalJS)
	if err != nil {
		return &checkResult{nameDrift, statusFail, "verb after the browser died: " + err.Error()}
	}
	if !strings.Contains(out, after.ua) {
		return &checkResult{nameDrift, statusFail, fmt.Sprintf(
			"driver is NOT on cuttle's browser - it reads %s, cuttle serves UA %q",
			truncate(oneLine(out), 200), truncate(after.ua, 80),
		)}
	}
	if !strings.Contains(out, driftWebdriverOK) {
		return &checkResult{nameDrift, statusFail, "navigator.webdriver is not false: " + truncate(oneLine(out), 200)}
	}
	return &checkResult{nameDrift, statusPass, fmt.Sprintf(
		"browser killed mid-session, cuttle relaunched it (%s -> %s), driver re-attached to THAT browser (UA matches, webdriver=false)",
		truncate(before.ws, 12), truncate(after.ws, 12),
	)}
}

// browserIdentity reads the unseeded /json/version - the same endpoint the
// bundled driver attaches to from inside the container.
func browserIdentity(ctx context.Context, cuttleURL string) (*browserID, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, cuttleURL+"/json/version", nil)
	if err != nil {
		return nil, fmt.Errorf("building /json/version request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reaching cuttle: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("reading /json/version: %w", err)
	}
	var v struct {
		UserAgent            string `json:"User-Agent"`
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, fmt.Errorf("decoding /json/version: %w", err)
	}
	if v.WebSocketDebuggerURL == "" || v.UserAgent == "" {
		return nil, fmt.Errorf("%w: %s", errNoBrowserID, truncate(oneLine(string(body)), 200))
	}
	return &browserID{ua: v.UserAgent, ws: browserUUID(v.WebSocketDebuggerURL)}, nil
}

type browserID struct{ ua, ws string }

// waitForNewBrowser polls until cuttle answers with a browser that is not the
// one named by prevWS: a relaunch mints a fresh /devtools/browser/<uuid>, so a
// changed uuid is what proves the kill landed and the replacement is up.
func waitForNewBrowser(ctx context.Context, cuttleURL, prevWS string) (*browserID, error) {
	deadline := time.Now().Add(driftRelaunchTimeout)
	var last error
	for time.Now().Before(deadline) {
		id, err := browserIdentity(ctx, cuttleURL)
		switch {
		case err != nil:
			last = err
		case id.ws == prevWS:
			last = fmt.Errorf("%w: still %s", errBrowserNotReplaced, truncate(prevWS, 12))
		default:
			return id, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err() //nolint:wrapcheck // caller prints it verbatim
		case <-time.After(time.Second):
		}
	}
	return nil, last
}

// browserUUID trims ws://host/devtools/browser/<uuid> down to the uuid, so the
// published-port half of the URL cannot make two readings look different.
func browserUUID(wsURL string) string {
	if i := strings.LastIndex(wsURL, "/"); i >= 0 {
		return wsURL[i+1:]
	}
	return wsURL
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
