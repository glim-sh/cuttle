---
type: Finding
title: How the bundled Playwright MCP behaves as an upstream
description: The Playwright MCP bundled with playwright-cli 0.1.20-0.1.23 (playwright-core 1.64 alpha) is session-bound and speaks 2025-11-25 only, accepts only Host localhost:<port>, kills a session whose GET stream misses its 3 s ping, shares one browser context and tab list across sessions, links action snapshots to files, and - if the client advertises roots - asks for them over the GET stream, stalling the session's first tool call for about a minute when that stream is never opened.
tags: [serve, remote, mcp, playwright, drivers]
status: stable
stale_after: "2027-05-04T00:00:00+00:00"
generated: { by: claude-code/claude-opus-5-5, at: "2026-11-04T23:25:00+00:00" }
sources:
  - id: research
    resource: /docs/2611-04-remote-driving-research/README.md
    title: Remote driving research, section 3 (Playwright MCP's session model)
  - id: probe
    resource: "probe on the 0.17.2 image, 2026-11-04: `node .../playwright/cli.js mcp --host 127.0.0.1 --port 8931 --cdp-endpoint http://127.0.0.1:9222 --shared-browser-context`, driven with curl (initialize at 2025-11-25, tools/list, browser_tabs, browser_navigate, browser_snapshot, browser_close)"
    title: Upstream probe with curl
  - id: smoke
    resource: /packages/cuttle/internal/serve/remote_smoke_test.go
    title: "TestRemoteSmoke: first browser call took 65 s with the Go SDK's default roots capability and 81 ms without, on the same image"
  - id: mcp
    resource: /packages/cuttle/internal/serve/mcp.go
    title: The aggregator's upstream client
---

# Finding

- **Sessions and protocol.** Session ids are generated server-side; a request
  without `initialize` gets 400, `MCP-Protocol-Version: 2026-07-28` gets 400, and
  only `Host: localhost:<port>` is accepted without `--allowed-hosts`.[^research]
- **The GET stream is a liability.** An open standalone GET stream is pinged
  every 3 s and the session is killed after 5 s without a reply.[^research]
- **Roots stall the first call.** The Go SDK client advertises the roots
  capability by default; Playwright MCP then requests `roots/list` when it first
  builds its browser context, over the GET stream. A client that never opens
  that stream waits about a minute on its first tool call (65 s measured, 81 ms
  with an empty capability set). The aggregator sends no client capabilities.[^smoke][^mcp]
- **Shared tabs.** Every session shares Chrome's default context: one tab list
  (the person's tabs included), each new session starting on tab 0, any session
  able to select or close any tab. Refs and the current tab are per session.
  `browser_close` answers "The browser context is shared between clients and
  cannot be closed" under `--shared-browser-context`.[^research][^probe]
- **Tab list format.** `browser_tabs` prints `- N: (current) [title](url)`;
  Playwright's URL for a new-tab page (`chrome://new-tab-page-third-party/`)
  differs from Chrome's target URL (`chrome://newtab/`), so the two meet only on
  ordinary URLs.[^probe]
- **Snapshots.** Action tools (`browser_navigate`, clicks) link their snapshot as
  a file under `--output-dir`; only `browser_snapshot` returns it inline.[^probe]
- **Relaunch.** When cuttle relaunches a killed Chrome the first call within
  ~15 ms fails with "Target page, context or browser has been closed"; calls
  after 370-500 ms succeed on the same session, its tabs and refs gone.[^research]

[^research]: Remote driving research, section 3 (Playwright MCP's session model)
[^probe]: Upstream probe with curl
[^smoke]: TestRemoteSmoke: first browser call took 65 s with the Go SDK's default roots capability and 81 ms without, on the same image
[^mcp]: The aggregator's upstream client
