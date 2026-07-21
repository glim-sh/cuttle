# Native macOS backend - built, then removed (post-mortem)

Status: **removed.** Built 2026-07-17 (commit `97a5b99`, shipped in 0.5.3),
hardened in PR #12, then cut in the same PR before merge. This doc is the record:
why it existed, what it proved, the wall it hit, and why removing it was right. It
is kept so the measured stealth findings and the window dead-end are not
relitigated from scratch.

The removal returns cuttle to a single local backend (docker), plus the remote
backends (ssh, k8s, direct). On Apple Silicon the linux/amd64 image runs under
emulation; the intended answer for native speed is a remote amd64 host via ssh/k8s.

## What it was

On Apple Silicon, `cuttle up` defaulted to a **native** backend instead of the
Rosetta-emulated linux/amd64 Docker image: it ran clark's `darwin-arm64` stealth
Chromium as a detached, pidfile-supervised `cuttle serve` daemon - no Docker, no
Xvfb, no VNC. Local-only and macOS-only; the server/Docker path was untouched.

Motivation: ~3-4x less RAM and ~2.5-4.5x less CPU than the emulated path, and a
real desktop browser window instead of a VNC viewer.

Mechanism:
- provisioning: download + sha256-verify + extract the pinned `darwin-arm64`
  clark release into `~/.cache` (`CUTTLE_BROWSER_BINARY` overrides).
- supervisor: `Setsid`-detached daemon, tracked by a pidfile under
  `~/.local/share/cuttle/native/<name>/`; `--use-mock-keychain` to suppress the
  Keychain prompt; `--window-size=1280,800` (CloakBrowser #273).
- handoff: `cuttle view [seed]` resolved the seed's Chrome pid (`pgrep -P
  <servePID>` + `ps -o command=` matched on `--user-data-dir`) and raised it via
  System Events, falling back to `open -a`.

## Why the macOS persona (measured, not assumed)

Empirical check of clark `chromium-148.0.7778.96-stealth5` `darwin-arm64`,
native, headed, on M1 Max (2026-07-17):

- `--fingerprint-platform=windows`: UA-CH reported `architecture: x86` (clark
  spoofs it, not arm), no renderer crash on browserleaks webrtc/webgl/canvas - so
  CloakBrowser #444 (arm UA-CH leak) and #396 (windows-spoof crash) did **not**
  reproduce.
- BUT the WebGL unmasked renderer was `ANGLE (Apple, ANGLE Metal Renderer: Apple
  M1 Max)` under **both** personas. The real GPU is unspoofable on the mac build.
  That is incoherent with a Windows persona (which needs a spoofed D3D11 pair) and
  coherent only with macOS (a real Mac reports exactly this plus the frozen
  `Intel Mac OS X 10_15_7` Chrome UA).

Conclusion at the time: native arm64 forces a macOS persona, and a genuine Mac on
a Mac is the strongest possible fingerprint - so it was the right identity, not a
compromise.

### The UA / Client-Hints coherence fix (PR #12 headline)

Pinning the macOS persona surfaced a real leak, root-caused in clark's own source:
clark computes several identity values in **two independent code paths** - one for
the network/HTTP stack, one for the JS surface - that fall back to *different*
defaults when left unset:

- **User-Agent**: the JS patch rewrote `navigator.userAgent` to a clean Chrome UA,
  but the network-stack header fell back to the build's compiled-in
  `HeadlessChrome` token and leaked it on **every request**. Fixed by pinning
  `--user-agent`.
- **UA-CH full version**: the network path defaulted `Sec-CH-UA-Full-Version-List`
  to the true build version (`148.0.7778.96`) while the JS path hardcoded
  `148.0.0.0`. Fixed by pinning `--fingerprint-brand-version` (both paths to one
  value); `--fingerprint-platform-version` / `--fingerprint-brand` pinned for the
  same defense-in-depth.

This was wire-reproduced and captured in the golden as macOS `fork_parity_args`
cases. **All of it was removed with the backend** - no supported engine runs on
darwin (docker/ssh/k8s are all Linux/Windows-persona), so the mac persona had no
reachable consumer. The Windows persona output is unchanged and still golden-locked.
(The separate `getDefaultStealthArgs` darwin -> `--fingerprint-platform=macos`
branch predates the native backend - it is original oracle-parity code and was
left intact.)

## Why it was removed: the window never became visible

The native backend's whole UX premise was a **visible** desktop window for human
handoff (login walls, captchas, Cloudflare). It never worked, and the cause is
fundamental, not a bug we could patch.

The daemon must outlive the `cuttle up` CLI invocation, so it detaches. On CLI
exit it reparents to `launchd` and **loses the Aqua/GUI session** - and macOS only
renders windows for processes that belong to a GUI session. Every lever was tried,
from a real terminal (not an automation shell), and disproven:

- **`Setsid`** (detached session): window off-screen.
- **`Setpgid`** (stay in the CLI's process group): "cuttle ready", still no window.
- **LaunchServices `open --args`**: a regression, not a fix - Chrome never came up
  on CDP, the daemon backed off and died, and 7 Chrome processes orphaned. Reverted
  (this is why `pool.go` is net-zero in the PR).
- Control case: clark launched **directly from the terminal** rendered a normal,
  visible window (tab bar, address bar, page). So the binary is genuinely headful;
  only the detached-daemon boundary strands the window.

clark could not be inspected via computer-use either: it lives in `~/.cache`, so
LaunchServices refuses to expose it (even symlinked into `/Applications`) and the
compositor blacks it out of screenshots.

### The two candidate fixes, and the decision

1. **launchd LaunchAgent** - the macOS-proper fix (launchd starts the daemon, so
   it lives in the Aqua session and windows are visible). **Rejected** for its
   cost, which is paid always and by everyone, not just when a human is needed:
   - installs a persistent startup item into `~/Library/LaunchAgents/` (survives
     `brew uninstall` -> orphaned job; reads like malware to EDR/MDM);
   - resurrects the browser you just killed (`KeepAlive`) or silently runs one at
     login (`RunAtLoad`);
   - a second source of truth per instance (plist vs pidfile) that drifts, exactly
     the orphan class we had just fixed;
   - rewrites `up`/`down` around `launchctl` and loses the port-collision fast-fail
     (launchd swallows the failure into its own status);
   - freezes flags/env into a static plist;
   - re-triggers macOS permission grants from a faceless background job.
2. **CLI-launched handoff** - `cuttle login`/`view` run in the foreground terminal
   (a GUI session), so they launch the visible browser as a child of that command.
   The proven-visible path. Its cost is one bounded tradeoff: Chrome locks a
   profile to one process, so handoff must stop the daemon's browser for that seed,
   let the human act, then hand back (login walls clean; a captcha already mid-load
   must be re-triggered).

Rather than ship option 2 as a half-feature (or take on option 1's standing cost),
the decision was to **remove native entirely**. A Mac user's non-emulated option is
now a remote amd64 host via ssh/k8s, where VNC handoff already works and there is no
window-visibility problem at all.

## Also removed alongside it

- `cuttle mcp` (the driver MCP-config generator): redundant with the driver
  briefing table `cuttle up`/`status` already print. cuttle promotes driving via
  the driver CLIs directly (agent-browser / browser-use / playwright-cli), which
  the briefing points at with exact attach commands.
- `cuttle view` as a window-raise verb: it was folded into the session-holding
  verb (`connect` at the time of removal; the CLI UX overhaul later merged
  `login`/`connect` into `cuttle open`), since on the remaining backends
  "viewing" is just clicking the VNC URL the CLI already prints.

## What replaced the emulation warning

`cuttle up` on an arm64 host using the local backend now prints a one-time notice
that the image is amd64-only (runs emulated) and points at the ssh/k8s backends via
`cuttle context --help`. (At removal time that meant hand-writing a context in
`~/.config/cuttle/config.toml`; the CLI UX overhaul later added
`cuttle context add`.) No wizard - deferred.
