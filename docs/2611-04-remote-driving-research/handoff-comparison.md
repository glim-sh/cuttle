# Human handoff for a remote cuttle session: the options compared

Companion to [README.md](README.md) (same experiments, same anonymization, same caveats: one
laptop over loopback, numbers rank options rather than predict production, no physical phone).

A **handoff** is the moment an agent driving the browser hits something only a person can do -
a login, a captcha, 2FA, a passkey, a "verify it's you" page - and a human takes over, usually
from a phone, then hands control back. It has three parts, compared separately below:

1. **The viewer** - how the human sees and drives the browser.
2. **The link** - how the agent gives the human access, and what protects it.
3. **The flow** - how the agent waits, how secrets the human holds get in, and how control
   comes back.

What a good handoff must keep: the human's input must reach the browser as real OS input (the
same identity the site already trusts), the browser's claimed screen, keymap and timezone must
not change, and the link must not become a standing credential.

---

## 1. The viewer

### Requirements

| # | Requirement | Why |
|---|---|---|
| V1 | Input as real X input, not CDP `Input.*` | CDP-injected input carries Chromium's debugger marker and known tells (`screenX == clientX`, empty `code`) |
| V2 | Never resize the remote or emulate mobile | the persona's screen, window and pointer media are part of the fingerprint |
| V3 | Keep the persona keymap | rebinding keycodes changes `navigator.keyboard.getLayoutMap()` |
| V4 | Usable on a phone: tap, keyboard, paste, zoom | handoffs mostly happen away from a desk |
| V5 | Full desktop, not per tab | popups, native dialogs, file pickers and new tabs must stay visible |
| V6 | Fits the exposure path | here: a TCP-only HTTPS ingress (WebSocket yes, UDP no) |

### Measured candidates

| Viewer | V1 input | V2 screen | V3 keymap | V4 phone | V5 desktop | V6 TCP ingress | Latency (p50) | Idle cost |
|---|---|---|---|---|---|---|---|---|
| **KasmVNC 1.3.3** + cuttle's noVNC 1.5.0 page | yes (X) | yes | rebinds spare keycodes for missing characters | weak: no pinch-zoom on iOS, no on-screen keyboard or soft keys, bare canvas | yes | yes | key->frame 65-84 ms (52-68 ms with the lock-key fix), click 62-76 ms | 0 B/s, 0% CPU on a static page |
| **Selkies 2.2.1** (H.264 over WebSocket) | yes (XTEST) | yes with `--enable-resize=false --second-screen=false` | rebinds spare keycodes; restore with `SELKIES_RUN_AFTER_DISCONNECT` | best of the set: touch and trackpad modes, on-screen keyboard, soft modifier keys, clipboard on Safari | yes | yes | key->frame 74 ms, 90 ms through an HTTPS proxy | 7-15% CPU static with Turbo off |
| **neko 3.2.0** (WebRTC) | yes (XTEST) | yes | **resets the persona keymap on takeover**; non-US text arrives as `\0` | good, built-in control passing | yes | no: needs a media port (ICE-TCP works through a TCP L3 service) | key->frame 79-100 ms | 0 with no viewer |
| **MediaMTX WebRTC** + own input channel | yes (xdotool) | yes | depends on our input code | needs a client written | yes | no: needs a media port | click 152-168 ms | ~0.25 core at 30 fps |
| **WebTransport** (custom single stream) | yes (xdotool) | yes | depends on our input code | needs a client written; iOS Simulator works with one long-lived stream only | yes | no: needs UDP | capture->display 43-53 ms | high (JPEG); H.264 would be lower |
| **WebTransport via MoQ** (MediaMTX) | no input path | - | - | iOS: codec rejected, then stalls after ~108 s | yes | no | one-way ~64 ms | - |
| **CDP screencast + CDP input** (not run) | **no** (debugger-marked) | risky (products resize to the viewer) | yes | possible | **no**: per tab, no native dialogs | yes | 4-12 fps per Steel | - |

### Verdict

- **Selkies** is the best handoff viewer for this setup: same latency class as KasmVNC, fits the
  TCP-only ingress, and the only candidate with a ready phone client. It needs the three flags
  and the keymap restore hook.
- **KasmVNC** stays a solid fallback once its lock-key bug is fixed (#151); its gap is the phone
  client, not speed.
- **neko** has the smoothest motion but breaks the persona keymap; usable only with a patch.
- **WebRTC/WebTransport** options add a UDP path and a client to build, for gains (motion
  smoothness) that matter little on mostly static login and 2FA pages.
- **CDP screencast** fails V1 and V5; fine as a read-only "what is the agent doing" preview.
- On a phone, the dominant problem for every option is **scale** (a 1920 px desktop shown at
  ~390 px). A half-screen window during takeover, without changing the claimed screen, is the
  next thing worth testing.

## 2. The link

What the agent sends the human, and what stops a leaked copy from being useful.

| Option | How it works | Leak risk | Survives link previews (Telegram, iMessage) | Notes |
|---|---|---|---|---|
| **Static viewer URL, network-only protection** | a fixed URL reachable only on a private network (tailnet ACL to the owner's devices) | a leaked URL is harmless off the network, full access on it | yes | zero code; what this deployment runs today. Everyone the network admits can open it |
| **Bearer token in the URL** (`?token=`) | a long-lived token in the link | a leaked link is a standing credential | yes | Browserless, Steel (`debugUrl` documented as unauthenticated), Hyperbrowser (12 h live URL token) work roughly like this |
| **Signed expiring link** (HMAC over expiry) | stateless, expires on its own | replayable until expiry; no per-link revocation | yes | needs a key; no store |
| **One-time ticket + interstitial + session cookie** | the link opens a "tap to open" page; the tap (a POST) consumes the ticket and sets a short-lived HttpOnly cookie | a leaked link is dead after one use or its TTL; the viewer session is separate and closable | **yes** - the preview only fetches the interstitial, it does not consume the ticket | needs a small in-memory store; restart invalidates everything (intended). Chosen in [the remote-MCP plan](../plans/2611-04-remote-mcp-session.md) |
| **Identity from the network** (Tailscale identity headers, or `tsnet` WhoIs) | no token at all; the proxy or daemon checks who the caller is | none from the link itself | yes | headers are only trustworthy if nothing else can reach the backend; `tsnet` adds a node to run |
| **Provider-style one-time URL** (Anchor `one_time_url`) | invalidated after the first viewer connects and disconnects | low | **no** - a preview fetch can burn it | the interstitial variant above fixes this |

**Verdict:** a one-time ticket behind an interstitial, plus network-level restriction, is the
only option that is safe to paste into a chat app: leaked links die, previews cannot burn them,
and the viewer session can be closed when it expires.

## 3. The flow

| Step | Options seen | What works best here |
|---|---|---|
| **Tell the human** | agent sends a link in chat (this setup); a webhook carrying the live URL (Anchor `intervention.requested`); an in-product "Take over" prompt (Manus, ChatGPT agent); a Done / Couldn't-do-it card in the viewer (Browserless `liveComplete`); MCP URL-mode elicitation (needs a client UI; a headless agent has none) | the agent sends the link plus **one line of what to do and in which tab** |
| **Wait** | the agent polls snapshots; a blocking "wait until the page leaves this origin" (`cuttle open --until`); a webhook back | a blocking wait on a condition (`url:` / `gone:` / `title:` / a secret arriving), with progress notifications so client idle timers do not cut it |
| **Who drives meanwhile** | shared control (input races); exclusive control while the human acts (Skyvern, Manus, AgentCore pause the agent) | a driving lease: the agent's driving calls are refused while the human holds the viewer, reads still work |
| **Secrets the human holds** | the human logs in themselves in the viewer; the human types a code into the remote page; a password manager on the phone; a separate secret form | for logins, the human signs in in the viewer (cookies then persist). For a value the agent must type later, a **one-time secret form** whose value is **bound to the origin the human confirms** - the agent types it by name and cannot send it anywhere else |
| **Hand back** | the human says "done" in chat; the viewer's own Done button; the wait condition fires on its own | the wait condition (the page leaving the login origin) plus a "done" message as a fallback |

## 4. Recommended handoff for a remote cuttle session

1. Agent hits a wall and stops at the first one (no retries: every retry raises an account's
   risk score).
2. Agent requests a one-time viewer link and sends it with one line of instruction.
3. Human taps the link on the phone, taps "Open" on the interstitial, and solves it in
   **Selkies** (touch mode, on-screen keyboard, paste from the password manager).
4. Agent blocks on a wait condition (e.g. "the page is no longer on the login origin"), then
   re-reads the page and continues where it stopped.
5. After the session, the persona keymap is re-applied; the viewer session expires and its
   socket is closed.

Everything above except step 3's viewer swap and the ticket/wait/form pieces runs today; those
are specified in [the remote-MCP plan](../plans/2611-04-remote-mcp-session.md).
