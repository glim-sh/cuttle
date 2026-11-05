# Remote MCP session: agents drive cuttle over one authenticated HTTPS endpoint

Plan for `feat/remote-mcp`. Written 2026-11-04 against `main` at `140328f` (v0.17.1).
Status: implemented in one PR (all of sections 4 and 5, with the deviations
listed in section 11). Depends on the loopback-guard fix
([#147](https://github.com/glim-sh/cuttle/pull/147): loopback routes now require a
loopback peer; docker-backend CLI calls moved to exec because a `-p 127.0.0.1` mapping
arrives from the bridge gateway). The bearer-authenticated remote listener here is
also the opt-in answer #147 names for the callers it deliberately breaks (`direct`
contexts and pool clients over a published port). Lands best after the Go 1.27 bump
([#146](https://github.com/glim-sh/cuttle/pull/146)), whose `testing/synctest` and
`httptest.NewTestServer` the tests below use.

Every claim is a `path:line` in this repo, a linked upstream source, `[measured]`
(run during this design, details in section 2) or `[unverified]`.

---

## 0. How to use this document

Section 3 is the architecture and section 4 the per-component spec - read them as the
contract. Section 7 is the build order as PR-sized slices; each slice is mergeable on
its own. Section 9 lists what is still open. Nothing here changes behaviour for a
deployment that does not set `--remote-listen`: local docker, ssh and the current k8s
backend keep working exactly as today.

## 1. Goal and requirements

A cuttle session-mode browser runs on a Kubernetes cluster. Agents in other containers
(nanoclaw on bl) drive it; the owner takes over from a phone for logins, captchas and
2FA. Requirements:

- **R1 - no credential in the agent.** The agent's HTTPS egress goes through the OneCLI
  gateway, which injects the bearer. The agent never holds a long-lived cuttle
  credential.
- **R2 - working handoff links.** The agent hands the owner a link (over Telegram) that
  opens the live viewer, short-lived and survives link-preview fetches.
- **R3 - protection inside the cluster.** Auth holds against any other pod or
  container, not only against the tailnet ACL or a NetworkPolicy.
- **R4 - the full cuttle feature set remotely.** Humanized input, `{{cuttle:NAME}}`
  sentinels, masking, the driving lease, downloads and secret capture all apply to
  remote agents.
- **R5 - stdlib-first Go, minimal dependencies.** One new module: the official MCP Go
  SDK.
- **R6 - 2FA stays human.** No TOTP seeds in the daemon (owner decision 2026-11-04).

## 2. What was established before writing this

- **No common CDP client proxies its WebSocket.** agent-browser v0.41.0 dials the CDP
  WebSocket directly with webpki roots only
  ([client.rs](https://github.com/vercel-labs/agent-browser/blob/v0.41.0/cli/src/native/cdp/client.rs),
  [tokio-tungstenite connect.rs](https://github.com/snapview/tokio-tungstenite/blob/v0.24.0/src/connect.rs)).
  Playwright 1.64.1 sends CDP discovery through `HTTPS_PROXY` but the WebSocket upgrade
  directly, on Node 22.23.4 and 26.13, with and without `NODE_USE_ENV_PROXY=1`
  `[measured]`. So OneCLI cannot inject a token into raw CDP, and exposing CDP is the
  wrong surface.
- **Plain HTTPS MCP works through OneCLI end to end** `[measured]`: a throwaway Go SDK
  v1.8.0 server behind `tailscale serve`; from bl, `onecli run --agent <test> -- curl`
  with no Authorization header reached the server with the injected bearer on every
  POST, and a 6 s `tools/call` streamed one progress event per second with arrival
  times matching send times (no buffering). Separately, Claude Code 2.1.318 routed a
  remote HTTP MCP server through `HTTPS_PROXY` (a logging CONNECT proxy saw the MCP
  host) and completed a streamed tool call.
- **OneCLI injects into WebSocket handshakes too** (v1.45.0
  [websocket.rs](https://github.com/onecli/onecli/blob/v1.45.0/apps/gateway/src/gateway/websocket.rs)),
  not needed by this design.
- **The image already ships Playwright MCP**: `playwright mcp` from the bundled
  playwright-core (`PLAYWRIGHT_CLI_VERSION=0.1.20`, `packages/browser/versions.env:25`)
  serves Streamable HTTP and takes `--host`, `--port`, `--cdp-endpoint`,
  `--cdp-header`, `--shared-browser-context`, `--output-dir`, `--snapshot-mode`,
  `--secrets`, `--allowed-hosts`, `--caps` `[measured: help output in 0.17.1]`.
- **Go MCP SDK v1.8.0** negotiates 2026-07-28 only with `StreamableHTTPOptions.Stateless
  = true`; stateful handlers negotiate down to 2025-11-25
  ([release notes](https://github.com/modelcontextprotocol/go-sdk/releases)). By
  default it 403s a request that arrives on a loopback address with a non-loopback
  Host; `DisableLocalhostProtection` turns that off `[measured]`.
- **Tailscale Serve (operator L7 ingress) sets `X-Forwarded-Proto/Host` and identity
  headers and strips client-supplied identity headers**
  ([serve.go](https://github.com/tailscale/tailscale/blob/main/ipn/ipnlocal/serve.go),
  upstream main `[unverified on the deployed operator]`). This design does not rely on
  identity headers.
- **Current loopback guard checks the Host header only** (`internal/serve/http.go:437`);
  fixed separately by the loopback-guard PR.
- **Secrets are bound to an origin only by a warning today**:
  `internal/serve/secretfill.go:421` logs when a name is typed on a second origin and
  types it anyway.

## 3. Architecture

```
 agent container (bl)                       cuttle pod (k8s)
 +----------------------+   HTTPS via      +---------------------------------------------+
 | Claude Code          |   OneCLI         | remote listener :8080 (CUTTLE_REMOTE_LISTEN)  |
 |  MCP client  --------+--(bearer is------+-> /mcp       bearer   -> MCP aggregator (Go)   |
 |  curl (files) -------+---injected)------+-> /files/{h} bearer   -> downloads dir         |
 +----------------------+                  |   /t/{tkt}   ticket   -> interstitial + redeem |
                                           |   /v/...     cookie   -> viewer proxy          |
 owner's phone (tailnet) ------------------+-> /healthz /readyz (open)                      |
                                           |                                               |
                                           | internal listener 127.0.0.1:9222 (unchanged)  |
                                           |   CDP mux, humanize, sentinels, loopback API  |
                                           |   ^             ^                             |
                                           |   |  playwright mcp 127.0.0.1:8931 (child)    |
                                           |   |  Xvnc / KasmVNC 127.0.0.1:6081            |
                                           +---------------------------------------------+
```

- **Two listeners.** The existing listener keeps every current route, bound to
  `127.0.0.1` in this mode (`CUTTLE_HOST=127.0.0.1`), so CDP is never reachable from
  outside the pod. `kubectl exec`-based `cuttle pw`, `secret`, `logs` keep working
  through it. The new remote listener serves only the routes in this document, and
  every one except the probes requires a credential.
- **The aggregator is the agent interface.** It is an MCP server to agents and an MCP
  client to the Playwright MCP child. Playwright tools pass through; cuttle adds its
  own tools and applies masking and the lease around every call.
- **Playwright MCP attaches through the daemon's CDP mux** (`--cdp-endpoint
  http://127.0.0.1:9222`), so humanized input and sentinel substitution apply to every
  Playwright action exactly as for `cuttle pw` (the reason recorded in
  `docs/knowledge/decisions/playwright-cli-is-the-driver-interface.md`).
- **The Service exposes only the remote port.** Chart change in section 5.

## 4. Components

### 4.1 Remote listener and principals

- Flags/env: `--remote-listen` (`CUTTLE_REMOTE_LISTEN`, e.g. `:8080`; off when empty),
  `--public-url` (`CUTTLE_PUBLIC_URL`, e.g. `https://cuttle.brown-hydra.ts.net`; the
  only origin used to build links and to check `Origin` - never derived from forwarded
  headers), `--principals-file` (`CUTTLE_PRINCIPALS_FILE`).
- Principals file (mounted from a k8s Secret): one line per caller,
  `<name> sha256:<hex of the token>`. Names are agent groups (`hq`, `household`).
  Tokens are 32 random bytes, base64url, minted outside cuttle; only hashes live in
  the pod. Reloaded on SIGHUP and on file change (projected Secret updates).
- Bearer middleware: `Authorization: Bearer <token>` only (no query tokens on the
  machine surface); hash, constant-time compare against each principal
  (`crypto/subtle`); the principal name rides the request context for logs, the
  lease owner and link attribution. Missing or wrong: `401` with
  `WWW-Authenticate: Bearer`. Never log the header.
- Routes accepting a bearer: `/mcp`, `/files/{h}`. Routes accepting a viewer cookie:
  `/v/...` only. A bearer never opens the viewer and a cookie never opens `/mcp`.
- `http.CrossOriginProtection` (Go 1.25+) wraps every cookie-authenticated
  state-changing route (`POST /t/{tkt}`). Viewer WebSocket upgrades (GET) get an
  explicit `Origin == public-url` check, because CrossOriginProtection passes GET.
- `Server.MaxHeaderValueCount` (Go 1.27) and conservative read/header timeouts on the
  remote `http.Server`.

### 4.2 MCP aggregator

- Built on `github.com/modelcontextprotocol/go-sdk` v1.8.0, **stateless toward agents
  (protocol 2026-07-28, `StreamableHTTPOptions.Stateless = true`, which still serves
  2025-era clients)**, with upstream state **keyed by the authenticated principal**:
  the bearer OneCLI injects names the caller on every request, so no session id rides
  the wire and the model carries nothing. `DisableLocalhostProtection: true` on the
  SDK handler (the remote listener's own checks are in front; see section 2).
- Upstream: the daemon starts `playwright mcp --host 127.0.0.1 --port 8931
  --cdp-endpoint http://127.0.0.1:9222 --shared-browser-context --output-dir
  <downloads dir>` as a supervised child when `--remote-listen` is set (restart with
  backoff, logged like Chrome's own restarts). Same pinned bundle as `cuttle pw`.
- Playwright MCP is session-bound and 2025-11-25 only `[measured, 0.17.1 image]`:
  session ids are hard-coded (`sessionIdGenerator: () => crypto.randomUUID()` in the
  bundled `coreBundle.js`), a request declaring `MCP-Protocol-Version: 2026-07-28`
  gets 400, an open GET stream gets a 3 s ping and dies after 5 s unanswered, and only
  `Host: localhost:<port>` is accepted without `--allowed-hosts`. Per MCP session it
  keeps its own refs and current tab, but every session shares Chrome's one default
  context: the same tab list for all (the owner's tabs included), every new session
  starts on tab 0, and any session can select or close any tab.
- So, per principal: one upstream session, `initialize` once (protocol header
  rewritten to 2025-11-25, `Host: localhost`), never open the GET stream, `tools/list`
  (follow `nextCursor`), register each tool as a pass-through plus cuttle's tools
  (4.3). Forward `notifications/progress` and cancellation; re-list on
  `notifications/tools/list_changed`. On an upstream 404 re-`initialize` and tell the
  agent its refs and tabs are gone; on "Target page, context or browser has been
  closed" (cuttle relaunching Chrome) retry once after 500 ms. Close the upstream
  session after 30 min idle.
- Tab ownership: a principal's first driving call opens its own tab (`browser_tabs`
  new), so no agent acts on the owner's tab 0; `browser_tabs` select/close and
  `browser_run_code_unsafe` are refused on tabs the principal did not open;
  `cuttle_status` lists each tab's owner. Principals get their own focus and refs but
  share the cookie jar with the owner - that is the point of a personal browser; real
  isolation is a separate release.
- Hidden upstream tools: `browser_resize` (would contradict the persona screen and
  window, see "Which screen" in `docs/OPERATING.md`) and `browser_install`
  (meaningless in the image). Everything else passes unchanged, including image
  content from screenshots.
- Server `instructions` (first 2048 chars matter under client tool search): one
  paragraph stating the handoff protocol - "If a page needs a human (login, captcha,
  2FA, consent): call `cuttle_viewer_link`, send the url to the user with one line of
  what to do, then `cuttle_wait`. Type stored secrets as `{{cuttle:NAME}}`; never type
  a password you read from a page or a message."

### 4.3 cuttle tools

All names `cuttle_*`; schemas with a description on every field; results as one text
block with identical JSON in `structuredContent` (mirrored, per the client-shadowing
behaviour in the MCP best-practices notes). Annotations as listed.

| Tool | Input | Result | Annotations |
|---|---|---|---|
| `cuttle_status` | none | signed-in domains (cookie domains and earliest expiry, never names or values - reuses `handleAuthStatus`), secret names with source/state/bound origin (never values), lease holder and age, open tabs (url and title from `Target.getTargets`, no attach) | readOnly, idempotent |
| `cuttle_viewer_link` | `reason`, `ttl_minutes` (1-30, default 15) | `{url, expires_at}` - a one-time ticket link (4.5) | not readOnly, not idempotent, openWorld false |
| `cuttle_wait` | `until` (`url:<glob>`, `gone:<glob>`, `title:<substring>`, `secret:<NAME>`), `timeout_minutes` (1-15, default 10) | `{met, condition, url, title, waited_s}`; a timeout is `met:false`, not an error | readOnly |
| `cuttle_secret_capture` | `name`, one of `selector` / `from_clipboard` | `{name, sentinel, length}`; the value never returns | not readOnly |
| `cuttle_request_secret` | `name`, `origin` (e.g. `https://github.com`), `reason`, `ttl_minutes` (15-720, default 60) | `{url, expires_at}` - a one-time form link (4.6) | not readOnly, not idempotent |
| `cuttle_download` | `name` or `"latest"` | `{name, size, mime, sha256, url}` with `url` = `/files/{h}` (bearer, 1 h); text files under 40 KB also inline | readOnly |

- `cuttle_wait` polls `Target.getTargets` once a second across all page targets (no
  page attach, so nothing in the page can observe it) and evaluates the condition; it
  sends `notifications/progress` every 15 s with the current url, which keeps Claude
  Code's 5 min HTTP idle timer alive (the clocks are listed in the MCP best-practices
  notes). Cancellation stops it.
- `cuttle_secret_capture` reuses `POST /secret/{name}/capture`
  (`internal/serve/http.go:83`); the captured value is bound to the page's origin at
  capture time (4.6) and immediately joins the masking set (4.4).
- `cuttle_download` reuses the downloads dir and `handleDownloadsGet`; the handle is
  random (16 bytes), maps to one file and one principal, and expires after 1 h. The
  agent fetches it with `curl -o` through OneCLI. Binaries are never inlined.

### 4.4 Masking

Before any upstream text content reaches the agent, the aggregator replaces (a) every
value in the session's secret store and (b) the current value of every `<input
type=password>` on the session's open pages (same-origin iframes included, read over
CDP at result time) with its `{{cuttle:NAME}}` / `{{cuttle:password-field}}` sentinel.
Reuse `internal/mask` and the reader behind `GET /snapshot`
(`internal/serve/masking.go`); same thresholds as today (exact matches, values under
4 characters - 6 if all digits - left alone). Image content cannot be masked; see
section 9.

### 4.5 Viewer: tickets, interstitial, proxy

- Xvnc moves to loopback: `docker-entrypoint.sh:81` gets `-interface 127.0.0.1
  -websocketPort 6081` when `CUTTLE_REMOTE_LISTEN` is set (the local docker backend
  keeps `0.0.0.0:6080`, because its boundary is the `127.0.0.1` port mapping). Keep
  `-DisableBasicAuth` exactly as it is (`docker-entrypoint.sh:73` explains why).
- Ticket: 32 random bytes, base64url, stored as a hash with `{kind, principal, reason,
  expires_at}`, single use, max 50 outstanding per principal. In memory: a restart
  invalidates every ticket and viewer session, which is the intended failure mode.
- `GET /t/{tkt}`: an interstitial page ("Open the cuttle viewer - requested by
  <principal>: <reason>", one button). It does not consume the ticket, so a
  Telegram/iMessage link-preview fetch is harmless. `Cache-Control: no-store`,
  `Referrer-Policy: no-referrer`.
- `POST /t/{tkt}` (CrossOriginProtection): atomically consume the ticket, mint a
  separate random viewer session, set `__Host-cuttle_view` (Secure, HttpOnly,
  SameSite=Lax, Path=/, Max-Age = remaining ticket lifetime, capped at 60 min), `303`
  to `/v/`.
- `/v/...`: cookie checked server-side on every request. Static assets through
  `httputil.ReverseProxy` (`Rewrite`/`SetURL` to `http://127.0.0.1:6081`, prefix
  stripped, cookie and Authorization removed upstream). The `websockify` upgrade is
  proxied by a small explicit hijack-and-copy handler instead, so the daemon can close
  it when the viewer session expires (ReverseProxy gives no handle on an upgraded
  connection). `vnc-viewer.html` already resolves `websockify` relative to its page
  (`ops/docker/bin/vnc-viewer.html:36`), so the `/v/` prefix needs no viewer change
  `[verify in slice 4]`.
- Expiry semantics: at session expiry the daemon closes the viewer socket; the page
  shows "session ended - ask the agent for a new link".

### 4.6 Secret request form and origin binding

- `cuttle_request_secret` mints a `kind=secret` ticket. `GET /t/{tkt}` shows: who asks
  (principal), the secret name, the bound origin (pre-filled from the request,
  editable), the reason, a TTL choice (15 min / 1 h / 12 h) and a password-type input.
  `POST` (CrossOriginProtection) stores the value in the daemon's existing secret
  store with `source=human-form` and the confirmed origin; the response page says
  "saved - you can close this". The value travels phone -> daemon in the POST body
  only, never to the agent; `cuttle_wait` with `secret:<NAME>` reports when it lands.
- **Origin binding becomes enforcement.** A secret entry gains `boundOrigin`. When set,
  `secretfill.go` refuses substitution into any frame whose origin differs (error to
  the driver naming the bound origin), instead of the warning at
  `internal/serve/secretfill.go:421`. Set by the form, by `cuttle_secret_capture`
  (origin of the page it was read from) and by a new optional `cuttle secret set
  --origin <origin>` on the CLI. CLI secrets without `--origin` keep today's
  first-use warning, so local behaviour is unchanged.
- Rationale: an agent reads untrusted pages, so a prompt-injected agent could request
  a credential and type its sentinel into an attacker's form. Binding confines a
  human-supplied value to the origin the human confirmed.

### 4.7 Lease

Playwright tools carry `readOnlyHint`. On the first non-read-only call a session
acquires the existing lease (`internal/serve/lease.go`) as owner `mcp:<principal>`
and renews it on each such call; it releases after 120 s without a driving call, and the existing 120 s
expiry covers a vanished client. Another holder: the call returns `isError` naming the
holder and its age - read-only calls always pass. `--takeover` has no MCP equivalent
in v1; the owner frees a stuck lease with `cuttle pw --takeover` via exec.

## 5. Deployment surface

- **Chart (`ops/helm/cuttle`)**: values `remote.enabled`, `remote.port` (8080),
  `remote.publicURL`, `remote.principalsSecret` (name of a Secret with key
  `principals`), `remote.ingressFrom` (pod/namespace selectors allowed into the remote
  port). When enabled: `CUTTLE_HOST=127.0.0.1`, the remote env, the Secret mount; the
  Service exposes only `remote`; the NetworkPolicy allows ingress to the remote port
  from `remote.ingressFrom` and nothing else; probes move to the remote port's
  `/healthz` `/readyz`.
- **Fleet (infra repo, not this repo)**: the release, a Tailscale operator Ingress to
  the remote port under its own tag, tailnet grants for the owner's devices and bl,
  and the principals Secret from sops. Tracked in the infra plan.
- **OneCLI**: one generic secret per principal, host
  `cuttle.brown-hydra.ts.net`, paths `/mcp*` and `/files/*`, header `Authorization`,
  value format `Bearer {value}`, granted to that group's OneCLI agent only. Not on
  `/t/*` or `/v/*`.
- **nanoclaw**: per group `mcp_servers` entry `{type: "http", url:
  "https://cuttle.brown-hydra.ts.net/mcp"}` with no headers, and a short container
  skill pointing at the server instructions.

## 6. Security model

| Threat | Mitigation |
|---|---|
| Another pod reaches the browser | CDP and every loopback route on `127.0.0.1` only; remote port needs a bearer or a cookie; NetworkPolicy as a second wall |
| Leaked agent bearer | Held only by OneCLI; per-principal tokens, revoked by removing one hash line |
| Leaked handoff link (chat history, preview bots) | Interstitial does not consume; POST consumes once; 30 min max; viewer session separate and closable |
| Prompt-injected agent asks for a credential | Form shows principal, name and origin; value bound to the confirmed origin and refused elsewhere |
| Agent sees a secret in a result | Masking of stored values and password fields on every text result |
| Cross-site requests against cookie routes | `http.CrossOriginProtection` on POST; explicit Origin check on the viewer upgrade; `__Host-` cookie |
| Two agents interleaving actions | Lease on non-read-only calls |
| Forwarded-header spoofing | Links and origin checks use `--public-url` only; forwarded headers never authorize anything |

## 7. Build order (PR slices)

1. **Remote listener + principals + bearer** (4.1), probes, `CUTTLE_HOST` loopback mode
   documented. Tests: principal table, 401 shapes, no-auth probe routes, reload.
2. **Aggregator pass-through** (4.2) with the Playwright MCP child, hide list,
   masking (4.4), lease (4.7) and tab ownership (4.2).
3. **Tools** `cuttle_status`, `cuttle_wait`, `cuttle_secret_capture`, `cuttle_download`
   + `/files/{h}` (4.3).
4. **Viewer** (4.5): Xvnc to loopback in remote mode, tickets, interstitial, cookie,
   proxy and closable websocket, `cuttle_viewer_link`.
5. **Secret form + origin binding** (4.6): `boundOrigin` enforcement, `--origin` on
   the CLI, `cuttle_request_secret`.
6. **Chart + docs** (5): values, Service, NetworkPolicy, probes; OPERATING.md section
   "Remote agents over MCP"; KB decision for "MCP aggregator over exposed CDP" and the
   findings from section 2.

Estimate: about 3 to 3.5 days for slices 1-6.

## 8. Tests

- Unit: table tests per route family for auth (bearer / cookie / none x right / wrong
  surface); ticket lifecycle with `testing/synctest` fake time (interstitial does not
  consume, POST consumes once, expiry, replay refused); origin binding (same origin,
  other origin, cross-origin iframe); masking of store values and password fields in
  pass-through results.
- Aggregator: a fake upstream MCP server (Go SDK) in-process via
  `httptest.NewTestServer`: pass-through of text and image content, progress and
  cancellation forwarding, `list_changed`, hidden tools, lease refusal on a
  non-read-only call while another session holds it.
- End to end (smoke, opt-in): the image with `--remote-listen`, the Go SDK client
  calling `browser_navigate`, `cuttle_status`, `cuttle_viewer_link`; a raw request
  from another container with `Host: 127.0.0.1` to the remote port without a bearer
  must 401, and CDP must be unreachable from outside the pod.

## 9. Open questions and deferred work

- **Shared upstream state** (answered 2026-11-04, see 4.2): refs and current tab are
  per session; tabs and the context are shared. Still `[unverified]`: whether one
  principal's partial snapshot breaks another's refs under `--shared-browser-context`,
  and how this interacts with a concurrent `cuttle pw` session.
- **Screenshots of captured values** (decided: v1): image content cannot be masked, so
  after `cuttle_secret_capture` the aggregator refuses `browser_take_screenshot` on
  that tab for 30 s and the tool description tells the agent to navigate away first.
- **Drivers other than Playwright**: agent-browser (0.41.x) overrides a CDP-attached
  page's timezone (from the client's `TZ`), locale, screen and viewport and
  auto-dismisses dialogs `[measured]`, so it must never drive a cuttle session; the
  aggregator's pass-through of Playwright MCP avoids it by construction.
- **Viewer transport and phone UX** (research 2026-11-04, a separate follow-up plan):
  keep KasmVNC over the WebSocket and rebuild the viewer client for touch, porting the
  touch layer of [kernel-images #448](https://github.com/kernel/kernel-images/pull/448)
  (local pinch-zoom, swipe scroll, long-press right-click, a hidden textarea for the
  iOS keyboard, extra-keys row, paste). cuttle-specific: mirror the focused field's
  type (read over CDP in an isolated world) into that textarea so iOS AutoFill and
  password managers work, and send text as X key events at human speed. Rejected for
  takeover: CDP screencast + CDP input (debugger-marked input events, no native
  dialogs). Later options: Selkies over WebSocket (H.264/WebCodecs), neko WebRTC
  (needs an L3 tailnet Service for UDP). Never let the viewer resize the remote or
  emulate mobile. Tickets, cookies and the proxy in 4.5 are independent of the
  transport and stay.
- **Protocol era** (decided): stateless 2026-07-28 toward agents, 2025-11-25 sessions
  only on the loopback hop to Playwright MCP, which cannot do better today.
- **Pool mode**: out of scope; session mode only.

## 10. Sources

- MCP spec 2026-07-28 and 2025-11-25 via the `mcp-best-practices` skill notes
  (`references/spec-2026-07-28.md`: stateful tools and handles, MRTR, timeouts,
  content vs structuredContent).
- [Go MCP SDK releases](https://github.com/modelcontextprotocol/go-sdk/releases) (v1.7.0
  2026-07-28 support, v1.8.0).
- Research report: auth for a remotely reachable cuttle session (astra, 2026-11-04),
  covering Browserless, Steel, Browserbase, Kernel, Hyperbrowser, Anchor, neko,
  Selkies and KasmVNC handoff mechanisms.
- [Go 1.27 release notes](https://go.dev/doc/go1.27) (`httptest.NewTestServer`,
  `synctest.Sleep`, `Server.MaxHeaderValueCount`).

## 11. As built (2026-11-04)

Implemented in `internal/serve/remote.go`, `mcp.go` and `mcptools.go`, the
entrypoint, the chart and `docs/OPERATING.md` ("Remote agents over MCP").
Where it differs from sections 4-5:

- **Viewer-agnostic proxy.** `/v/` forwards any path and any WebSocket upgrade
  to `--viewer-upstream` (default `http://127.0.0.1:6081`), every upgrade by
  hand-relay so expiry closes it - not only `websockify`, because the viewer
  is being replaced by a parallel change.
- **`browser_run_code_unsafe` is hidden**, not refused per tab: it runs code in
  the Playwright process inside the pod, past masking, binding and the lease.
- **Tab ownership matches Playwright's index to Chrome's target by URL**; a
  select or close passes only when every page at that URL is the principal's,
  so two same-URL tabs of different owners refuse both. Popups of an own tab
  are own tabs.
- **The lease is released by its own 120 s expiry**, which is the same instant
  an explicit release after 120 s idle would fire. `cuttle_viewer_link`,
  `cuttle_request_secret`, `cuttle_status`, `cuttle_wait` and `cuttle_download`
  do not take it; `cuttle_secret_capture` does.
- **The screenshot hold lasts the full 30 s**, navigation or not: a fragment or
  `history.replaceState` changes the URL with the value still on screen.
- **The browser is treated as inside the boundary** (not in section 6): a page
  on 127.0.0.1 passes the loopback checks, so the internal listener refuses
  requests carrying `Sec-Fetch-Site` or a non-loopback Host in remote mode, the Playwright MCP child
  accepts only a per-start random `--allowed-hosts` name, and the entrypoint
  installs a `URLBlocklist` policy for the loopback literals. Still open: the
  unauthenticated viewer port is reachable through a DNS name for 127.0.0.1.
- **`browser_tabs` arguments are re-encoded** from what was checked, because Go
  and JavaScript disagree on duplicate and differently-cased JSON keys.
- **Revoking a principal** ends its links, viewer sessions, file handles and
  upstream session at the reload.
- **The upstream client advertises no capabilities**: with the SDK's default
  roots capability Playwright MCP stalled the first tool call for about a minute
  (found by the opt-in `TestRemoteSmoke`).
- **`notifications/tools/list_changed`** is honoured when it arrives on a POST
  response; with the GET stream never opened, an upstream that only sends it on
  that stream is re-listed on the next re-initialize instead.
- **Sending secrets out** is dropped (owner decision), not deferred.
