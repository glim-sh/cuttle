---
type: Decision
title: Remote agents drive the session over an authenticated MCP endpoint, never raw CDP
description: An agent outside the container drives cuttle through an MCP aggregator in the daemon behind a bearer-authenticated listener - stateless toward agents, upstream state keyed by principal, one credential kind per route family - while CDP, the loopback API and the viewer stay on loopback inside the container.
tags: [serve, remote, mcp, security, drivers]
status: stable
generated: { by: claude-code/claude-opus-5-5, at: "2026-11-05T00:30:00+00:00" }
sources:
  - id: research
    resource: /docs/2611-04-remote-driving-research/README.md
    title: Remote driving, egress and takeover-viewer research (sections 2 and 3)
  - id: plan
    resource: /docs/plans/2611-04-remote-mcp-session.md
    title: Remote MCP session plan
  - id: remote
    resource: /packages/cuttle/internal/serve/remote.go
    title: Remote listener, principals, tickets, viewer proxy, file handles
  - id: mcp
    resource: /packages/cuttle/internal/serve/mcp.go
    title: MCP aggregator and the Playwright MCP child
  - id: ops
    resource: /docs/OPERATING.md
    title: "OPERATING.md, Remote agents over MCP"
---

# Decision

`--remote-listen` opens a second listener in `cuttle serve`. Its only machine
surface is `/mcp`, an MCP server that aggregates the bundled Playwright MCP
(attached through the daemon's own CDP mux) and adds cuttle's tools; `/files`
serves download handles. Both take only `Authorization: Bearer`, hashed and
compared against a principals file. The human handoff takes one-time links on
`/t` and a viewer cookie on `/v`; a bearer never opens the viewer and a cookie
never opens `/mcp`. With the listener on, the CDP listener binds loopback and
the viewer moves to loopback behind the `/v/` proxy.[^remote][^ops]

# Why not expose CDP

Credential-injecting egress gateways work on HTTPS requests, and no common CDP
client sends its WebSocket through a proxy: agent-browser dials directly, and
Playwright sends discovery through `HTTPS_PROXY` but the upgrade directly. So a
gateway cannot authenticate raw CDP, while plain HTTPS MCP passed one end to end
with the bearer injected on every POST and progress streamed unbuffered.[^research]
CDP is also the wrong grain: it bypasses masking, the lease and tab ownership,
and agent-browser attached over CDP rewrites the persona's timezone, locale and
screen.[^research]

# Shape

- **Stateless toward agents, keyed by principal.** The bearer names the caller
  on every request, so no session id rides the wire; each principal gets one
  upstream Playwright MCP session, its own tabs and its own lease owner
  `mcp:<principal>`.[^mcp]
- **Driving rules apply around every call.** Driving tools take the lease;
  the first action opens a tab of the principal's own and select or close of
  another's tab is refused; every text result is masked against held secrets
  and live password fields; screenshots are held for 30 s after a capture.[^mcp][^plan]
- **Hidden upstream tools.** `browser_resize` (breaks the persona screen),
  `browser_install`, and `browser_run_code_unsafe`, which runs arbitrary code in
  the Playwright process inside the pod - on the loopback side of every boundary
  this surface draws.[^mcp]
- **Viewer-agnostic proxy.** `/v/` forwards any path and any WebSocket upgrade
  to a configurable loopback upstream, relaying upgrades by hand so expiry can
  close them; it does not depend on which viewer serves behind it.[^remote]

# The browser is inside the boundary

The agent steers a tab, so a page it opens on 127.0.0.1 passes every check the
internal listener was built on (loopback peer, loopback Host, same-origin
Origin). In remote mode that listener refuses any request carrying
`Sec-Fetch-Site`, which Chrome sends to loopback addresses and the CLI, drivers
and aggregator never do, and any non-loopback Host, which is what a page reached
through a DNS name for 127.0.0.1 sends; the Playwright MCP child accepts only a random per-start Host; and a managed
`URLBlocklist` keeps pages off the loopback literals. The viewer port has no
credential of its own and is still reachable through a DNS name for 127.0.0.1;
any replacement viewer needs one.[^remote][^mcp]

# Not decided here

Real isolation between principals (separate cookie jars) and sending secrets
out of the daemon are out of scope: principals share the person's cookies by
design of a personal browser.[^plan]

[^research]: Remote driving, egress and takeover-viewer research (sections 2 and 3)
[^plan]: Remote MCP session plan
[^remote]: Remote listener, principals, tickets, viewer proxy, file handles
[^mcp]: MCP aggregator and the Playwright MCP child
[^ops]: OPERATING.md, Remote agents over MCP
