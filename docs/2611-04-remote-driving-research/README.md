# cuttle: remote driving, egress and takeover-viewer experiments

Research artifact, 2026-11-04. Covers cuttle 0.17.1-0.17.2 (`ghcr.io/glim-sh/cuttle`), with
the follow-up fixes that landed the same day (#147, #150, #151).

The question behind all of it: how should agents in **other containers or hosts** drive one
signed-in cuttle session safely, how should its traffic leave from a **residential IP** when the
pod runs in a datacenter, and which **viewer** gives a human the best takeover, especially on a
phone.

**Everything here is anonymized.** Host names, tailnet names, IP addresses, account names and
agent-group identifiers are replaced with roles ("the home server", "the Kubernetes cluster",
"an agent container"). Upstream projects, versions, issues and PRs are public and cited exactly.

**Companion doc:** [handoff-comparison.md](handoff-comparison.md) compares the human-handoff
options (viewer, link, flow) on their own.

**How to read the numbers.** Every viewer measurement ran on one laptop (Apple Silicon, an
OrbStack arm64 VM with 5 vCPUs) over loopback, often while other experiments shared the VM.
They rank the candidates; they do not predict production latency. Each row says what was
measured. Nothing in section 4 was tested on a physical phone.

---

## Contents

1. [Setup that was tested](#1-setup-that-was-tested)
2. [Driving a remote cuttle: CDP clients and proxies](#2-driving-a-remote-cuttle-cdp-clients-and-proxies)
3. [Agents over MCP instead of raw CDP](#3-agents-over-mcp-instead-of-raw-cdp)
4. [Takeover viewers compared](#4-takeover-viewers-compared)
5. [Residential egress for a datacenter pod](#5-residential-egress-for-a-datacenter-pod)
6. [Networking gotchas](#6-networking-gotchas)
7. [Bugs found in cuttle along the way](#7-bugs-found-in-cuttle-along-the-way)
8. [How comparable products do live view and takeover](#8-how-comparable-products-do-live-view-and-takeover)
9. [Conclusions](#9-conclusions)

---

## 1. Setup that was tested

- One session-mode cuttle (Helm chart `ops/helm/cuttle`, durable profile on a PVC) in a
  Kubernetes cluster on a cloud provider.
- Exposed **tailnet-only** through the Tailscale Kubernetes operator's L7 HTTPS Ingress, as one
  host with paths: the viewer at `/`, CDP at `/json` and `/devtools`. cuttle rewrites the
  `webSocketDebuggerUrl` from `X-Forwarded-Host`/`X-Forwarded-Proto`, so discovery through the
  ingress returns a correct `wss://` URL.
- Agents run in containers on a separate home server and reach the browser over the tailnet.
  Their HTTPS egress goes through a credential-injecting MITM gateway
  ([OneCLI](https://github.com/onecli/onecli) 1.45.0).
- NetworkPolicies: ingress to the browser pod only from the ingress proxy pods; egress DNS plus
  the public internet only (private ranges, metadata and the CGNAT tailnet range excluded).

## 2. Driving a remote cuttle: CDP clients and proxies

### No common CDP client sends its WebSocket through an HTTP proxy

| Client | Discovery (`GET /json/version`) | WebSocket upgrade | Source |
|---|---|---|---|
| [agent-browser](https://github.com/vercel-labs/agent-browser) 0.41.0-0.41.2 | direct | direct; `tokio-tungstenite` with `rustls-tls-webpki-roots`, no proxy CONNECT step, custom CA not applied | [client.rs](https://github.com/vercel-labs/agent-browser/blob/v0.41.0/cli/src/native/cdp/client.rs) |
| Playwright 1.64.1 `connectOverCDP` | through `HTTPS_PROXY` | direct, on Node 22.23.4 and 26.13, with and without `NODE_USE_ENV_PROXY=1` | measured with a logging CONNECT proxy |

Consequence: a gateway that injects credentials into proxied HTTPS cannot authenticate raw CDP
for these clients. Put the CDP host in `NO_PROXY` and reach it directly, or do not expose CDP
at all (section 3).

### agent-browser rewrites the persona when attached; playwright-cli does not

agent-browser 0.41.2, attached to an external browser over CDP, applies its own emulation to the
pages it drives. With the client container at `TZ=Asia/Tokyo`, a page reported `Asia/Tokyo`,
`en-US`, screen `800x600`, viewport `1280x633`. At `TZ` unset it reported `UTC`. It also
auto-dismisses `alert` and `beforeunload` dialogs. No flag turns these off. Against cuttle that
replaces the persona's timezone, locale and screen and makes the page disagree with its exit IP.

`playwright-cli attach --cdp=<url>` (0.1.20 and 0.1.23) left everything intact under the same
client `TZ`: the page reported the persona's zone, `en-GB`, `1920x1080`. A plain Playwright
`connectOverCDP` + `browser.contexts()[0]` behaves the same.

**Rule:** drive a cuttle session with playwright-cli (cuttle's own driver) or plain Playwright,
never agent-browser.

## 3. Agents over MCP instead of raw CDP

### Plain HTTPS MCP passes a credential-injecting gateway end to end

- A throwaway MCP server (official [Go SDK](https://github.com/modelcontextprotocol/go-sdk)
  v1.8.0) behind HTTPS on the tailnet. Through the gateway, a client that sent **no**
  `Authorization` header arrived with the injected bearer on every POST.
- A 6 s `tools/call` streamed one progress notification per second with arrival times matching
  send times: the gateway does not buffer the SSE stream.
- Claude Code 2.1.318 routes a remote HTTP MCP server through `HTTPS_PROXY` (a logging CONNECT
  proxy saw the MCP host) and completed a streamed tool call.
- The Go SDK 403s a request that arrives on a loopback address with a non-loopback `Host` unless
  `DisableLocalhostProtection` is set - relevant whenever it sits behind a reverse proxy.

### Playwright MCP's session model (bundled in the 0.17.1 image, playwright-core 1.64 alpha)

- `playwright mcp` is in the image already: Streamable HTTP, `--cdp-endpoint`,
  `--shared-browser-context`, `--allowed-hosts`, `--output-dir`, `--snapshot-mode`, `--secrets`.
- **Session-bound, 2025-11-25 only.** Session ids are hard-coded
  (`sessionIdGenerator: () => crypto.randomUUID()`); a request without `initialize` gets
  `400 Server not initialized`; `MCP-Protocol-Version: 2026-07-28` gets 400 even on a valid
  session. Only `Host: localhost:<port>` is accepted without `--allowed-hosts`.
- An open GET stream is pinged every 3 s and the session is killed after 5 s without a reply.
- **Per session:** refs and current tab. **Shared by all sessions:** Chrome's one default
  context, so the same tab list (the human's tabs included); every new session starts on tab 0;
  any session can select or close any tab. Without `--shared-browser-context` it still uses the
  default context and never calls `Target.createBrowserContext` (only `--isolated` does, which
  cuttle refuses).
- When cuttle relaunches a killed Chrome, the first call within ~15 ms fails with
  "Target page, context or browser has been closed"; calls after 370-500 ms succeed on the same
  session. Its tabs and refs are gone.

The design that follows (an authenticated MCP aggregator inside the daemon, stateless toward
agents, upstream sessions keyed by caller) is in
[docs/plans/2611-04-remote-mcp-session.md](../plans/2611-04-remote-mcp-session.md).

## 4. Takeover viewers compared

Definitions: **key->frame** = key press in the viewer until the remote page's change is
visible; **click->frame** = the same for a click; **one-way** = remote page draw until
displayed. All input below arrived as real X input: every remote event `isTrusted: true`.

| Viewer | Transport | Latency (p50) | Frame rate | CPU / bandwidth | Verified catches |
|---|---|---|---|---|---|
| **KasmVNC 1.3.3** + cuttle's noVNC 1.5.0 page (shipped viewer), dedicated run at load 5-7 | one WebSocket | key->frame 65-84 ms (52-68 ms after the fix in section 7), click->frame 62-76 ms, one-way 48-65 ms | 51 fps on a 60 fps page | 0 B/s and 0% Xvnc on a static page; 32-55% Xvnc, 7-26 Mbit/s while animating | lock-key injection (section 7); binds spare keycodes for characters the persona lacks (212 -> 221 keys, `key "\u0000"`, `code` `F19`/`F24`) and the first use of a new character can drop; no pinch-zoom on iOS ([KasmVNC#329](https://github.com/kasmtech/KasmVNC/issues/329)) |
| **[Selkies](https://github.com/selkies-project/selkies) 2.2.1**, H.264, attached to cuttle's Xvnc `:99` | one WebSocket; also through an HTTPS L7 proxy | key->frame 74 ms (p90 133), 90 ms through the proxy | 20-34 fps on an animated box | ~0.5 core in full-motion 1080p software x264; 7-15% static with Turbo off; 1.1-1.4 Mbit/s | Turbo mode on by default costs ~33% CPU idle (`--video-streaming-mode=false`); needs `--enable-resize=false --second-screen=false` (then the server holds 1920x1080 against a 2532x1170 phone); rebinds spare keycodes and changes `navigator.keyboard.getLayoutMap()` (48 -> 49 entries) until the persona keymap is re-applied (`SELKIES_RUN_AFTER_DISCONNECT`); typed non-ASCII Latin exactly; clipboard both ways; settled text within ~3 s matches KasmVNC (38-40 dB PSNR between the two); +179 MB image |
| **[neko](https://github.com/m1k1o/neko) 3.2.0**, VP8 | WebRTC, one UDP+TCP mux port | key->frame 79-100 ms (720p), 94 ms (1080p in cuttle's image) | 25 fps | 0.2-0.5 core per viewer, 0 with none; 1-1.9 Mbit/s | **resets the persona XKB keymap on takeover** (`setxkbmap -layout us`); non-US characters and shifted ASCII sent without Shift arrive as `\0`; Ctrl+V does not paste from a macOS client (Cmd+V does); TCP-only mux works; Apache-2.0; input via XTEST |
| **[MediaMTX](https://github.com/bluenviron/mediamtx) v1.21.3 WebRTC** (WHEP) + a WebSocket input channel into xdotool | WebRTC, fixed ICE port | click->frame 152-168 ms (UDP), 182-269 ms (TCP fallback); one-way 95-135 ms | 30 / 60 fps, 0 dropped | ffmpeg ~0.25 core at 30 fps 1080p (0.5 at 60); 0.9 Mbit/s static | the ICE port must not be remapped; TCP fallback is automatic; disable its default MoQ listener |
| **WebTransport via MediaMTX Media-over-QUIC** | HTTP/3 over UDP | one-way ~64 ms (desktop Chrome 156) | 30 fps 1080p | as above, ~4 Mbit/s cap | video only, no input; no TCP fallback (failed after 4.1 s with UDP blocked); iOS Safari rejects its `avc3` codec, and with an `avc1` rewrite plays 30 fps then stalls for good after ~108 s |
| **WebTransport, custom** ([webtransport-go](https://github.com/quic-go/webtransport-go) v0.13.0, JPEG down, input up) | HTTP/3 over UDP | capture->display 43 ms (desktop Chrome), 53 ms (iOS Simulator) | 14-20 fps (CPU-bound JPEG) | 13-16 Mbit/s | see below |
| CDP `Page.startScreencast` + CDP input (not run) | WebSocket | - | 4-12 fps per [Steel](https://steel.dev/blog/webrtc) | - | debugger-marked input, no native dialogs or file pickers: preview only, never takeover |

### WebTransport in iOS Simulator Safari (Xcode 27.2, iOS 27.2)

- `WebTransport` and `VideoDecoder` exist; `datagrams.writable` does **not** (only
  `datagrams.createWritable()`); `VideoDecoder` supports `avc1`, vp8, vp9, hevc, not `av01` or
  `avc3`.
- **Certificates:** a publicly trusted certificate connects; a self-signed certificate with
  `serverCertificateHashes` also connects (ECDSA or RSA, valid 14 days or less; 15 days, a wrong
  hash or no hash fail with an empty error message). This contradicts the
  [MediaMTX docs](https://mediamtx.org/docs/publish/moq), which say iOS Safari lacks the option.
  Desktop Chrome rejected RSA.
- **One stream per frame stalls for good after 30-110 s** (~16 MiB delivered on loopback; the
  server ends up blocked in `Write`), consistent with
  [moq.dev's note](https://doc.moq.dev/concept/transport) that WebKit stalls long sessions.
  Desktop Chrome on the same server ran 100 s without a stall. **One long-lived stream ran
  10.5 minutes** at 19.8 fps average, 0 decode errors, ~40 ms p50.
- The Simulator uses the Mac's network stack and CPU; a physical iPhone was not tested, and the
  input test used synthetic events, not a real tap.

### What fits a TCP-only ingress

Tailscale serve, the operator's L7 Ingress and Tailscale Services carry TCP only
([tailscale#8868](https://github.com/tailscale/tailscale/issues/8868),
[tailscale#19559](https://github.com/tailscale/tailscale/issues/19559); the Services
configuration docs: "only the `tcp` protocol is supported"). WebSocket viewers (KasmVNC,
Selkies) fit unchanged. WebRTC can fall back to ICE-TCP through a TCP L3 Service. WebTransport
needs UDP, which only the operator's standalone L3 proxy (single pod, DNAT) is documented to
carry - not tested here.

## 5. Residential egress for a datacenter pod

Requirement: the browser's traffic should leave from a home IP, not the cloud provider's.

Options considered: a forward proxy on a home host reached through an egress Service; a
kernel-mode Tailscale sidecar using a home host as exit node (needs NET_ADMIN and a privileged
namespace); an App Connector (splits identity per domain); an SSH dynamic SOCKS tunnel; joining
the home host to the cluster as a node; a paid residential proxy.

**Chosen and verified:** a **userspace Tailscale sidecar** (native sidecar: an `initContainer`
with `restartPolicy: Always`, health-gated so Chrome starts only once the tailnet is up) with
`--exit-node=<home host> --socks5-server=127.0.0.1:1055`, and cuttle's chart
`proxy: socks5://127.0.0.1:1055`. No NET_ADMIN, no credentials on the proxy.

- Verified by bytes, not by IP alone: a 5 MB download through the SOCKS5 port moved ~5.5 MB
  through the exit node's counters for that peer (the laptop used for the first check sat
  behind the same home IP, so an IP match proved nothing).
- cuttle's "default proxy brings its geo" works through SOCKS5: Chrome launched with
  `--fingerprint-timezone`, `--fingerprint-locale` and `--fingerprint-webrtc-ip` matching the
  exit IP.
- The sidecar joins with a dedicated Tailscale OAuth client (scope `auth_keys`, one tag) used
  as `<secret>?ephemeral=true&preauthorized=true`: the secret never expires, so no 90-day key
  rotation. A Terraform-held OAuth client can only mint keys for its own tags or tags they own
  ([Tailscale OAuth docs](https://tailscale.com/docs/features/oauth-clients)).

## 6. Networking gotchas

- **Containers on a home server could not resolve the new tailnet name.** Docker containers
  inherit the LAN router's DNS; a router that runs Tailscale answers MagicDNS names only for
  peers it can reach, and the ACL kept the router away from the browser. The host itself
  resolved fine through `100.100.100.100`. Fix: an ICMP-only grant from the router's tag to the
  browser's tag (DNS visibility without TCP access), with an ACL test that keeps TCP 443/80
  denied.
- **A loopback-only route guard must check the peer, not the Host header** (section 7, #147).
- **A docker `-p 127.0.0.1:PORT` mapping delivers host connections from the bridge gateway, not
  loopback** (measured on Docker 29.8 / OrbStack), so a peer-address check breaks host-side
  clients that go through the mapping; cuttle's CLI now makes those calls inside the container.

## 7. Bugs found in cuttle along the way

| Bug | Impact | Fix |
|---|---|---|
| The loopback-only routes (secrets, lease, snapshot, state, downloads, auth, grab) trusted the `Host` header alone | any peer able to reach the CDP port could read the secret store with `Host: 127.0.0.1` | [#147](https://github.com/glim-sh/cuttle/pull/147): require a loopback peer (`netip`), local and ssh backends call through exec |
| The viewer page's Cmd-swallowing wrapper forwarded 3 of noVNC 1.5.0's 5 `onkeyevent` arguments | after each lock-state report every keystroke was wrapped in synthetic CapsLock + NumLock taps (30 presses -> 30 CapsLock + 30 NumLock + 15 Shift keydowns, all trusted): a behavioural fingerprint, plus 13-17 ms latency | [#151](https://github.com/glim-sh/cuttle/pull/151): forward every argument; a `viewer-lock-keys` smoke check |
| playwright-cli 0.1.23 appends ` (showing first N)` to `find`'s header | `cuttle pw find` printed raw driver output; with N >= 40 the "more matches" count was wrong | [#150](https://github.com/glim-sh/cuttle/pull/150) |
| Open: the first use of a character the persona keymap lacks can drop | a one-character loss on first use (KasmVNC binds a spare keycode and Chrome races the keymap reload) | not fixed |

## 8. How comparable products do live view and takeover

Public documentation as of 2026-11; transport details marked "unverified" where the docs do not
say.

| Product | Live view / takeover transport | Mobile |
|---|---|---|
| [Browserbase](https://docs.browserbase.com/platform/browser/observability/session-live-view) | per-tab debugger URLs (one per page; new tabs must be followed) | mobile viewport set at session creation; "Mobile keyboards aren't officially supported" - forward keys yourself via `page.keyboard` |
| [Browserless](https://docs.browserless.io/baas/monitor-sessions/hybrid-automation) | CDP screencast JPEG + CDP input | viewport follows the viewer's screen by default |
| [Cloudflare Browser Run](https://developers.cloudflare.com/browser-run/features/live-view/) | DevTools-frontend live view, `handoff` CDP commands (beta) | - |
| [Browser Use Cloud](https://docs.browser-use.com/cloud/browser/live-preview), [Notte](https://docs.notte.cc/features/sessions/live-view) | screencast in an iframe | - |
| [Steel](https://steel.dev/blog/webrtc) | moved from CDP screencast ("4-12fps, unstable") to WebRTC H.264 with OS-level capture | interactive URL |
| [Kernel](https://kernel.sh/docs/browsers/live-view) | neko WebRTC, noVNC fallback | full phone layer ([kernel-images#448](https://github.com/kernel/kernel-images/pull/448)) |
| [AWS Bedrock AgentCore](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/browser-dcv-integration.html) | Amazon DCV stream | taking control disables automation |
| [Skyvern](https://skyvern.com/docs/cloud/getting-started/monitor-a-run), [Anthropic computer-use demo](https://github.com/anthropics/anthropic-quickstarts/blob/main/computer-use-demo/README.md) | VNC (x11vnc + websockify / noVNC) | basic |
| OpenAI Atlas, Perplexity Comet (desktop), Opera Neon, Claude in Chrome | local browser, no streaming | n/a |
| ChatGPT agent cloud browser, Manus, Hyperbrowser, Anchor, Airtop | unverified | takeover from phone offered by some |

Pattern: API-first cloud browsers ship cheap per-tab CDP screencasts with CDP input; the
products that care about fidelity moved to OS-level capture (WebRTC, DCV). None of them treats
undetectable human input as a goal, which is where cuttle differs.

## 9. Conclusions

1. **Do not expose CDP to remote agents.** Expose an authenticated HTTPS MCP endpoint from the
   daemon and keep CDP on loopback; credential-injecting gateways work for HTTPS, not for CDP
   WebSockets.
2. **Never drive a cuttle session with agent-browser**; use playwright-cli or plain Playwright.
3. **Viewer:** on latency KasmVNC and Selkies are equivalent and both fit a TCP-only ingress;
   Selkies ships the better phone client (touch/trackpad modes, on-screen keyboard, Safari
   clipboard). Both need the persona keymap re-applied after a takeover. neko needs a keymap
   patch first. WebTransport is viable on iOS only as one long-lived stream and needs a UDP
   path; not ready for the default path.
4. **Residential egress** from a datacenter pod: a userspace Tailscale sidecar with a home exit
   node and cuttle's SOCKS5 default proxy is the simplest verified option.
5. **On a phone the dominant UX problem is scale** (1920 px shown at ~390 px), not codec
   quality; worth trying a half-screen window during takeover without changing the claimed
   screen.
