---
type: Decision
title: Remote agents drive the session over an authenticated MCP endpoint, never raw CDP
description: An agent outside the container drives cuttle through an MCP aggregator in the daemon behind a bearer-authenticated listener - stateless toward agents, upstream state keyed by principal, one credential kind per route family - while CDP and the loopback API stay on loopback and the viewer on a Unix socket inside the container; an opt-in trusts network peers outside the pod with the viewer and raw CDP on the same URL.
tags: [serve, remote, mcp, security, drivers]
status: stable
generated: { by: claude-code/claude-opus-5-5, at: "2026-11-05T16:20:00+00:00" }
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
  - id: tsserve
    resource: https://github.com/tailscale/tailscale/blob/main/ipn/ipnlocal/serve.go
    title: Tailscale serve keeps the incoming Host
---

# Decision

`--remote-listen` opens a second listener in `cuttle serve`. Its only machine
surface is `/mcp`, an MCP server that aggregates the bundled Playwright MCP
(attached through the daemon's own CDP mux) and adds cuttle's tools; `/files`
serves download handles. Both take only `Authorization: Bearer`, hashed and
compared against a principals file. The human handoff takes one-time links on
`/t` and a viewer cookie on the viewer, which owns every other path (`/`, with
`/v/` as an alias); a bearer never opens the viewer and a cookie never opens
`/mcp`. With the listener on, the CDP listener binds loopback and the viewer
moves to a Unix socket behind the proxy.[^remote][^ops]

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
- **Driving rules apply around every call.** Driving tools take the lease, and
  so does any call that first has to open or select the principal's tab; a
  lease lost to a takeover is not taken back for a lease lifetime; the first
  action opens a tab of the principal's own and select or close of another's
  tab is refused; every text result is masked against held secrets and live
  password fields; screenshots are held for 30 s after a capture.[^mcp][^plan]
- **Hidden upstream tools.** `browser_resize` (breaks the persona screen),
  `browser_install`, and `browser_run_code_unsafe`, which runs arbitrary code in
  the Playwright process inside the pod - on the loopback side of every boundary
  this surface draws.[^mcp]
- **Viewer-agnostic proxy.** The viewer proxy forwards any path and any WebSocket upgrade
  to a configurable upstream (a Unix socket in the image, or a loopback port),
  and every request a viewer cookie carries runs under its session's context,
  so expiry and revocation end the sockets and long streams such as Selkies'
  focus stream alike (a trusted peer's have no session and end with the
  connection); it does not depend on which viewer serves behind it.[^remote]

# The browser is inside the boundary

The agent steers a tab, so a page it opens on 127.0.0.1 passes every check the
internal listener was built on (loopback peer, loopback Host, same-origin
Origin). In remote mode that listener refuses any request carrying
`Sec-Fetch-Site`, which Chrome sends to loopback addresses and the CLI, drivers
and aggregator never do, and any non-loopback Host, which is what a page reached
through a DNS name for 127.0.0.1 sends; the Playwright MCP child accepts only
a random per-start Host; and a managed `URLBlocklist` keeps pages off the
loopback literals. The viewer has no credential of its own, so it listens on a
Unix socket, which no page can reach; a viewer served on a loopback port instead
would be reachable through a DNS name for 127.0.0.1.[^remote][^mcp]

# One URL for the owner's devices

`--trust-network-peers` (chart `remote.trustNetworkPeers`, off by default) lets
the same public URL serve the viewer and raw CDP (`/json`, `/devtools`) to the
owner's own devices with no link or bearer, so one ingress fronts everything.
This does not reopen CDP to agents: an agent holds a bearer, and a bearer opens
neither.[^remote][^ops]

- **A trusted peer is a network fact, never a header.** The TCP peer must be
  neither loopback nor one of the pod's own interface addresses (re-read per
  decision, since one netlink dump is cheap and a cached list misses an address
  added later), and the Host must be the public host, which keeps a page rebound
  to the ingress's address from riding the owner's position; a Tailscale serve
  ingress forwards the Host unchanged.[^remote][^tsserve]
- **Why that is enough, and only then.** The NetworkPolicy admits nothing but
  the ingress proxy to the remote port and the ingress admits only the owner's
  devices, so a non-local peer came through both; the browser in the pod can only
  originate from loopback or its own address, so the agent's tab is refused. It
  fails open where another path reaches the port (a NodePort, a published
  container port, the pod's own node, which most CNIs admit past
  NetworkPolicy), where `ingressFrom` is broader than the proxy, or where the
  network rewrites the pod's own connection to its Service to another source
  (hairpin masquerade, a userland proxy); the chart refuses the value without
  `ingressFrom`.[^ops]
- **Pages stay out.** Network position is ambient, so a page another site
  serves the owner's browser could ride it: the viewer admits a cross-site
  request only as a top-level GET navigation (the cookie's SameSite=Lax line;
  never in a frame, and the viewer's responses carry `frame-ancestors 'self'`),
  CDP discovery refuses any request a page makes, and the `/json` query, which
  would choose the browser's launch proxy and identity, is dropped.[^remote]
- **The same CDP mux.** The remote listener mounts the internal listener's
  `/json` and `/devtools` handlers, so humanized input, substitution and the
  Origin rule (which refuses a page's socket, the viewer's own origin included)
  hold unchanged; discovery advertises `wss://<public host>` from the configured
  URL, never from forwarded headers. A raw CDP client takes no lease, like an
  in-container one that never asks.[^remote]

# Not decided here

Real isolation between principals (separate cookie jars) and sending secrets
out of the daemon are out of scope: principals share the person's cookies by
design of a personal browser.[^plan]

[^research]: Remote driving, egress and takeover-viewer research (sections 2 and 3)
[^plan]: Remote MCP session plan
[^remote]: Remote listener, principals, tickets, viewer proxy, file handles
[^mcp]: MCP aggregator and the Playwright MCP child
[^ops]: OPERATING.md, Remote agents over MCP
[^tsserve]: Tailscale serve keeps the incoming Host
