---
type: Finding
title: playwright-cli attach and session model
description: How the bundled playwright-cli 0.1.23 decides to attach vs launch, where its session daemon keeps state, and the failure wordings the cuttle pw wrapper relies on.
tags: [playwright-cli, cuttle-pw, drivers, docker]
status: stable
stale_after: "2027-03-18T00:00:00+00:00"
generated: { by: claude-code/claude-opus-5-5, at: "2026-11-04T22:00:00+00:00" }
sources:
  - id: src
    resource: https://github.com/microsoft/playwright-cli
    title: microsoft/playwright-cli v0.1.23 (logic lives in its pinned playwright-core dependency)
  - id: core
    resource: "playwright-core 1.65.0-alpha-2026-10-30 as installed by @playwright/cli 0.1.23 in the image: lib/tools/cli-client/program.js (startSession, case open), session.js (Session.run), registry.js, output.js and help.json; lib/coreBundle.js (makeSocketPath)"
    title: playwright-core cli-client source at the pinned version
  - id: live
    resource: "live validation against local cuttle image builds, 2026-09-18 (0.1.20) and 2026-11-04 (0.1.23, arm64): smoke harness plus manual kill/restart and close experiments"
    title: Local validation
---

# Finding

Facts about `@playwright/cli` 0.1.23 that `cuttle pw`
(`packages/cuttle/internal/cli/playwright.go`) and the image ENV design
depend on. They were verified in the driver's source and against a running
container.[^src][^live] The exact-version pin (versions.env, the Dockerfile
ARG, and the Go const, drift-checked by test) is what makes relying on them
safe; re-verify on every pin bump.

- `--cdp` is a daemon-start option, not a per-verb flag. Config precedence
  at daemon start: defaults < `~/.playwright/cli.config.json` < project
  config < env `PLAYWRIGHT_MCP_CDP_ENDPOINT` < CLI flags. With a
  cdpEndpoint configured from any source, every browser acquisition -
  `open` included - goes through `connectOverCDP` and never launches. Only
  explicit `attach --endpoint=<ws>` or `attach --extension` flags override
  the env, which is why the wrapper rejects exactly those two.[^src]
- `open`/`attach` spawn a detached per-session daemon holding all browser
  state (page, tabs, element refs); every other invocation is a thin client
  over a unix socket. Session identity: `-s=`/`--session=` > env
  `PLAYWRIGHT_CLI_SESSION` > `default`. State lives under
  `${XDG_CACHE_HOME:-$HOME/.cache}/ms-playwright/daemon/<workspaceHash>/`;
  the hash is constant when no `.playwright` directory exists above the
  working directory, so separate `docker exec` invocations share one
  session. The daemon socket goes under `$XDG_RUNTIME_DIR/playwright/` when
  that variable is set, else under `$TMPDIR/pw-<userhash>/`; the image sets
  no `XDG_RUNTIME_DIR`, so every exec lands on the same socket.[^src][^core]
- `attach` is not idempotent: it kills and respawns the session daemon,
  discarding minted refs and the current-tab selection (the browser's tabs
  themselves survive, possibly reordered). Attach once, then drive verbs - the
  reason the wrapper auto-attaches only on evidence of a missing session
  rather than before every verb.[^src][^live] `open` goes through the same
  `startSession`, so it restarts the session the same way, and with no URL it
  navigates to `about:blank`.[^core]
- There is no `help` or `docs` verb (`playwright-cli help` answers `Unknown
  command: help`). Global help is `playwright-cli --help`, which `cuttle pw`
  intercepts for its own wrapper help; per-verb help, `<verb> --help`, passes
  through.[^core]
- A verb with no live session exits 1 in one of two wordings. With no
  session file it prints `The browser '<session>' is not open, please run
  open first` (output.js); a clean container stop leaves it this way. When a
  session file outlives its daemon - `docker kill`, an unclean restart, the
  daemon process killed - the socket connect fails and it throws an uncaught
  `Browser '<session>' is not open. Run` (session.js), stack trace and all, on
  every verb until something re-attaches. The wrapper matches both
  (`is not open, please run`, `is not open. Run`) to trigger its single
  auto-attach retry.[^core][^live] Attached sessions have no idle timeout;
  `detach` refuses sessions not created by attach; `close` on an attached
  browser disconnects only - cuttle's browser, tabs and logins survive
  both.[^src][^live]
- Element refs served through cuttle's CDP mux come back frame-prefixed
  (`f1e2`), not bare (`e2`) as against a directly-launched browser -
  anything parsing snapshots must accept both.[^live]
- The first screenshot on a cold container can exceed the driver's 5s
  action timeout while fonts warm up; the second attempt takes about 1.4s.
  The smoke harness retries once for this reason.[^live]
- No playwright-managed browser exists in the image
  (`PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1` at install, nothing runs
  `playwright-cli install`), so even a misconfigured launch path fails
  loudly instead of silently driving a non-stealth browser.[^src]

Related decision: [Humanized input is the value proposition](/decisions/humanize-over-speed.md).

[^src]: microsoft/playwright-cli v0.1.23 (logic lives in its pinned playwright-core dependency)
[^core]: playwright-core cli-client source at the pinned version
[^live]: Local validation
