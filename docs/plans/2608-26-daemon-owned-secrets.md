# Daemon-owned secrets: injection, capture, and masking

Plan for `feat/secrets`. Written 2026-08-26 against `main` at `762e66f` (v0.13.1).
Revised the same day after a design review; see section 3 for what the review
changed and why.

Research backing this plan ran as three parallel agents on 2026-08-26: a CDP
protocol sweep (measured live against Chrome 152 on a throwaway profile), an
upstream-implementation sweep (playwright, agent-browser, CloakBrowser-Manager,
browser-use, Skyvern, 1Password), and a host-tooling sweep (`op`, `gh`, xsel,
KasmVNC, Go stdlib). Every claim below is either a file:line in this repo, a
file:line in a named upstream clone, or marked `[measured]` / `[unverified]`.

---

## 0. How to use this document

You are implementing this from scratch with no prior context. Read sections 1-7
before writing code; they contain findings that invalidate the obvious design.
Section 9 is the build order. Section 14 is the checklist.

**Four things will bite you if you skip ahead:**

1. `Input.imeSetComposition` is a live bypass of cuttle's existing humanizer, not
   just of the new secrets path (section 6.1). Closing it is a prerequisite.
2. `Input.insertText` returns success even when nothing was inserted, and on a
   `disabled` target it inserts into *whatever was focused instead* (section 6.2).
   Trusting the CDP reply means typing credentials into the wrong field and
   reporting success.
3. **cuttle already has a handoff verb.** `cuttle open` (`commands.go:716-775`)
   navigates, prints the briefing and launches the viewer, and SKILL.md documents
   it under "Human handoff: login walls and captchas". Do not add a `handoff`
   verb; add a wait flag to `open` (section 8.5).
4. The ownership boundary in section 4 is load-bearing. One verb in this plan
   deliberately crosses it, exactly once, with a stated reason.

---

## 1. Why

cuttle owns no secret path. `rg -i 'secret|password|redact|otp' internal/ cmd/`
returns only proxy-credential parsing (`internal/fingerprint/proxy.go`,
`geoip.go`) and prose in `internal/cli/SKILL.md`.

From the 14-day session audit (`docs/2608-18-improvements-issues-research/`,
issues A13/A17/A34, clusters B20-B22, cross-session pattern H9.4):

- **8 sessions** hit one of the two secret failure shapes. Two ended in a human
  taking over credential entry, one in a wrongly-aborted task, two in burned TOTP
  attempts.
- **One real credential rotation** was required (S14) - caused not by injection
  but by `snapshot` printing a password field in cleartext (S26:
  `textbox "Password..." [ref=e28]: t2J6bXyCQumuF!cukv`).
- **Three sessions independently reinvented** the same
  `Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,'value').set` +
  `atob($(printenv X | base64))` hack, which emits `isTrusted:false` and leaves
  the submit button disabled.
- S08 aborted a whole task to a human because `fill f7e182 HC_PASS` returned
  `len=7`: the driver had typed the literal string `HC_PASS`.

The four mechanisms that fail today:

| # | Mechanism | Failure |
|---|---|---|
| 1 | `PLAYWRIGHT_MCP_SECRETS_FILE` binds at session create | a time-bounded value cannot be refreshed without a new attach; S04 accumulated sessions `bt`, `bt2`, `bt3` in 90 seconds |
| 2 | unknown-name substitution | **fails open** - types the literal name into a live password field, no error, no log (7.1) |
| 3 | reaches `browser_fill_form` / `browser_type` only | `run-code`'s vm sandbox has no `process`, so the efficient path forces the literal into argv |
| 4 | one driver of three | agent-browser and browser-use users have nothing |

The reverse direction - getting a credential *out* of a page into a store - has no
path at all. S05 hand-built a `createTreeWalker` scraper piped through `jq` into a
`chmod 600` file; S18 scraped a PAT into a shell variable piped to `gh secret
set`, with correctness resting on remembering not to echo it.

**The framing.** Agents do not fumble this because any one mechanism is hard. They
fumble it because the agent must know *which mechanism applies to which call shape
on which driver*, and every one of those conditions fails silently. The fix in
every strand has the same shape: **one name, one failure mode, loud.**

---

## 2. Scope: five strands

| # | Strand | Surface |
|---|---|---|
| A | Sentinel substitution at the CDP Input layer | `{{cuttle:NAME}}` typed by any driver in any call shape |
| B | Host-side just-in-time resolution, configured once | `cuttle secret set NAME --exec '...'` |
| C | **Fewer credential-handling events** | `cuttle auth status`, `cuttle open --until`, the retrieval ladder |
| D | Close the exfiltration half | masking in cuttle-authored text, SKILL.md rules |
| E | Capture out of a page into a sink | `cuttle secret capture NAME --selector ... --to ...` |

Strand C was called "make typing rare", which described neither half. It is the
strand that reduces **how often a credential enters the flow at all** - see 8.5.

**Out of scope, deliberately:** wire-side redaction of forwarded CDP frames (6.7 -
it corrupts base64 payloads and misses encoded forms); a TOTP generator (strand
B's `--exec` covers it without putting a shared secret in cuttle's data dir); any
persistence of secret *values* to disk (3.2); the 1Password-style "fill blackout"
(7.4).

---

## 3. Decisions locked

Settled with the user before and during a design review. Do not relitigate; if you
find a reason one is wrong, stop and raise it.

### 3.1 `--exec` resolution runs CLI-side, on the host

The daemon runs in a container; `op` runs on the host, with biometrics. The
resolver cannot be daemon-side.

`cuttle secret set NAME --exec '...'` stores the *command*. At use time the CLI
runs it, gets the value, and `PUT`s it to the daemon over the existing loopback
surface with a TTL. The container never shells out and never learns the command.

### 3.2 Config persists globally; values are memory-only with a TTL

**Two different lifetimes, and collapsing them was the single biggest AX mistake in
the first draft.**

- The **`--exec` recipe** persists in `$XDG_CONFIG_HOME/cuttle/config.toml`
  (`internal/config/config.go:107`), the same file `cuttle context` already owns.
  Setup is once-ever, by a human. Global, not per-context: a credential is a
  credential, and per-context would mean re-registering the same 1Password
  reference for local and cluster.
- The **value** lives only in daemon memory, per seed, under a TTL (default 15
  minutes). Never mirrored to `dataDir`. cuttle stores zero credentials today;
  writing them to a volume that survives `cuttle down` is a posture change the
  feature does not need.

Contrast `stateStore` (`internal/serve/state.go:141-160`), which *does* persist -
the secret store mirrors its shape but not its `persist` method.

**Footgun to surface, not to design around:** a globally-registered secret is
offered to a seed in a shared cluster context exactly as it is locally. The
mitigation is the derived origin binding (3.5) plus per-seed TTL'd values, and the
briefing naming which context it is printing for.

### 3.3 Fail closed, and refuse a literal in a credential field

An unmatched sentinel is a hard CDP error; nothing is typed.

Stronger: **a `fill` of a literal into `<input type=password>` with no sentinel is
refused**, not warned. The pre-flight probe (7.3) already knows the element type,
so this is free, and it is the moment S08's whole lost task would have been saved.

Escape hatch follows the existing precedent: a daemon flag mirroring
`--allow-context-creation`. Per issue A45, **the error must lead with the
actionable token in the first 80 characters** - the `createBrowserContext`
rejection got truncated mid-sentence by every wrapping layer above it, six times,
before anyone found the flag.

### 3.4 `capture` defaults to `--to memory`

A capture with no sink lands in the table under the TTL, so the site-A-to-site-B
flow (generate a token on A, type it into B) is two calls and the value never
leaves the daemon. Print a warning naming the TTL so a sink-less capture cannot
silently expire unnoticed.

### 3.5 Origin binding is derived, never configured

Record the origin where a secret is first used successfully. On a later use at a
different origin, **warn (not block)**, naming both.

An `--origin` flag would be the `--context` story again: correct, and never set.
`--context` is the exact fix for a top-ranked problem, is never named in the
briefing, and was reached for **once in 14 days** (issue A6). Derive it; teach only
on the anomaly.

### 3.6 `--to file:` refuses a path inside a git working tree

The corpus has driver scratch state committed twice (S02 swept ~3,100 lines of
console logs and page snapshots into a commit). A credential landing in a repo is
the same accident with a worse blast radius. Refuse with a message naming the repo
root; `--force` overrides.

---

## 4. The ownership boundary

cuttle's model is *"cuttle is the browser, not the driver"* (`SKILL.md:17-19`).
Every verb in this plan was tested against one line:

> **cuttle owns the transport, the identity, the profile, the container boundary,
> and the human handoff. Drivers own resolving an element and acting on it.**

| Verb | Side | Verdict |
|---|---|---|
| the `{{cuttle:NAME}}` sentinel | a substitution in a CDP frame cuttle already rewrites | clean |
| `secret set` / `ls` / `rm` / `prompt` | configures the transport | clean |
| `auth status` | reports on the profile cuttle owns | clean |
| `open --until` | cuttle's own viewer, print-and-wait, never clicks | clean |
| `downloads --latest` / `--wait` | cuttle already owns the download dir | clean |
| `grab <url>` | bytes out of the authenticated context to the host - the container boundary, same as downloads | clean |
| **`secret capture --selector`** | **resolves a selector and reads a DOM node** | **crosses the line - the one exception** |
| `fill <sel> --secret NAME` | pure driver verb | **rejected** |

**Why the sentinel is not a driver verb.** cuttle already rewrites
`Input.insertText` into keystrokes, rewrites `Fetch.enable`, blocks
`Target.createBrowserContext`, and answers proxy 407s invisibly. The agent still
writes `playwright-cli fill e5 '{{cuttle:X}}'`; the driver still resolves the
element and performs the fill. A credential is part of the identity riding the
transport.

**Why `fill --secret` is rejected** despite being the obvious better-AX move: it
only works for the discrete-verb case, which is precisely the limitation that
makes Playwright's secrets file useless today - it cannot reach `run-code` or a
batched call. The sentinel rides inside whatever the driver is already doing.

**Why `--selector` is the one permitted exception.** The boundary-clean
alternative exists and works: `playwright-cli eval 'el => el.value' e5 | cuttle
secret set API_KEY --stdin`. A pipe never renders, so the value goes driver stdout
to cuttle stdin without touching the terminal or the transcript. **Document that
pattern.** But it is exactly what S18 did, and the plan's own note on it reads
*"correctness resting entirely on remembering not to echo it"* - agents check
things by running them alone first, and one exploratory run is the leak. The
justification is specific: the driver path's failure mode is a leak that cannot be
closed from the transport, because by the time the value is in driver stdout it
has already escaped. **This reason does not generalize and does not license a
second exception.**

---

## 5. Verified facts: cuttle's tree

All line numbers are `main@762e66f`.

### 5.1 The humanizer is the right chokepoint, and it already works

`internal/serve/humanize.go`:

- `handleClientFrame` (`:403-428`) prefilters on `bytes.Contains(data, []byte("Input."))`
  then switches on exactly three methods (`:146-149`).
- `handleInsertText` (`:613-687`) already rewrites one `insertText` into real
  keystrokes: `insertTextMaxRunes = 20` (`:166`), remainder on one `insertText`,
  bounded by `insertTextBudget = 4500ms` (`:173`), and on abandonment answers a CDP
  error naming how many runes landed (`:672-680`).
- **This is rec C3 from the research doc, and it has already shipped.** The
  "38-to-80-character band times out the driver" bug described there is fixed. Do
  not re-fix it.
- `handleKey` (`:529-540`) **returns false** - paces the keystroke, forwards the
  original unchanged.

### 5.2 The isolated-world machinery already exists and is correct

- `query(sid, expr)` (`:1007-1018`) evaluates in the session's isolated world,
  retrying once on a stale context.
- `createWorld` (`:1132-1166`) does `Page.getFrameTree` then
  `Page.createIsolatedWorld{worldName: "cuttle_probe"}` (`:153`).
- `invalidateWorld` (`:1073-1104`) drops the cache on main-frame
  `Page.frameNavigated`, `Runtime.executionContextsCleared`, and a matching
  `Runtime.executionContextDestroyed`. **This triad is exactly right** (6.5).
- `probeExpr` (`:1178+`) builds its expression with `fmt.Sprintf`. **Capture must
  not follow this pattern** - see 6.6.

### 5.3 The proxy already has interception patterns to copy

`internal/serve/wsproxy.go`:

- `preprocessClient` (`:199-246`) is the client-to-browser chokepoint, already
  hosting `blockContextCreation` (`:205`), `blockBrowserTeardown` (`:213`), the
  keep-alive guard (`:227`), the humanizer (`:239`), `rewriteFetchEnable` (`:243`).
- `cdpSessionOpts` (`:138-144`) is how per-seed config reaches a session. **Add
  the secret store here**, the way `locale` arrives.
- `newHumanizer` (`:193`) is per CDP connection, so the store must live on the
  pool, not the humanizer.

### 5.4 cuttle already solves this exact problem once, for proxy credentials

`internal/fingerprint/proxy.go:57-70` strips proxy credentials from argv;
`internal/serve/pool.go:892-896` strips them from the status endpoint; the 407 is
answered over `Fetch.continueWithAuth` and never surfaced (`wsproxy.go:621-660`).

**This is the model.** The feature is that pattern generalized from one hard-coded
credential to a named table.

### 5.5 cuttle already has a handoff verb

`cuttle open [url]` (`internal/cli/commands.go:716-775`, aliased from the
pre-overhaul `login`/`connect`) navigates the running session, prints the briefing,
and launches the viewer. `SKILL.md:164-179` documents it under "Human handoff:
login walls and captchas" with a "Recognize the wall early" rule.

Its doc comment states the gap plainly: *"It does not hold the terminal."* Three
things are missing, none of them a new verb:

1. **No wait condition** - the whole of S18's 36-hour stall and its hand-rolled
   90-iteration `document.title` poller.
2. **No per-seed window raise** - with two seeds on one X display,
   `openBrowser(viewer)` opens the shared framebuffer and the human gets whichever
   window openbox has on top (issue A25).
3. **No return signal** - nothing tells the agent the human acted, which is why
   S01's user had to say "check the current page pls" three times.

### 5.6 Other anchors

- **HTTP guard**: `rejectUntrustedLoopback` (`http.go:386-397`) requires a loopback
  `Host` plus the Origin allow-list. Every new secret route sits behind it.
- **CLI-to-daemon**: `runDownloads` (`commands.go:798-821`) is the template;
  `pullDownload` (`:847-881`) is the template for moving bytes without printing
  them.
- **Logging**: one `slog` handler (`serve.go:38`), teed to `/data/logs/serve.log`
  on durable profiles (`logfile.go`). **A secret in a log line is a durable leak.**
  The single handler is the masking chokepoint.
- **Daemon-side CDP with no driver attached**: raw websocket + `cdpRequest`
  (`keepalive.go:170`), or chromedp via `internal/cdp`. Prefer the raw path for
  capture - chromedp's session management would create targets we do not want.
- **The viewer already implements BinaryClipboard correctly**
  (`ops/docker/bin/vnc-viewer.html:43-94`), in both directions, on one socket so
  paste is race-free, reading via the `paste` event's `clipboardData` (`:181`)
  which needs no permission and no secure context. **Strand E's clipboard source
  has a large head start. Do not rewrite it.**

---

## 6. Verified facts: CDP

Measured against Chrome 152.0.7977.64 unless marked. `CDPROTO` =
`github.com/chromedp/cdproto@v0.0.0-20260714215040-dc233986426f` (the pinned
version).

### 6.1 `Input.imeSetComposition` is a live bypass - fix this first

A driver can put an arbitrary string into a field with **one** call and never
commit it. The value is live in `.value`, the form will submit it, and **no
`insertText` or `dispatchKeyEvent` frame ever crosses the wire.**

`[measured]` on an empty `<input>`: `imeSetComposition("ni",2,2)` produces
`compositionstart` / `compositionupdate` / `beforeinput(insertCompositionText)` /
`input`, and `.value === "ni"`.

Params (`CDPROTO/input/input.go:222-228`): `text`, `selectionStart`,
`selectionEnd` required; `replacementStart`/`replacementEnd` optional (supplying
only one is `InvalidParams`).

Two consequences: this is an **existing hole in the humanizer** independent of
secrets - a value placed this way is never humanized and never counted, the
`fill()` zero-keystroke tell in a new costume. And `maxlength` is **not** enforced
during composition; `<input maxlength=3>` held `"abcdefgh"` `[measured]`.

`Input.imeCommitComposition` **does not exist** - the doc comment referring to it
is stale, and calling it returns `-32601` `[measured]`. The commit path is
`Input.insertText`. Playwright never emits `imeSetComposition`, so closing this
costs nothing today.

### 6.2 `Input.insertText` lies about success, and can hit the wrong element

`InputHandler::InsertText` binds `sendSuccess` as the mojo reply closure, so **it
always reports success, even when nothing was inserted.**

| target state | result `[all measured]` |
|---|---|
| no focused element | total no-op, zero events, CDP still returns `{}` |
| `disabled` | full no-op - **the insert lands on whatever *was* focused** |
| `readonly` | value rejected, but `beforeinput` **and** `textInput` are dispatched carrying the full text to any page listener |

The `disabled` case is the sharpest: a secret aimed at a disabled field goes
somewhere else entirely and the driver reports success. **A pre-flight target check
is therefore mandatory** (7.3).

`maxlength` **is** respected on this path - `insertText("abcdef")` into
`<input maxlength=3>` gives `"abc"` `[measured]`, so silent truncation is
reachable, which the post-type length check catches.

**A secret typed through any path lands in the undo stack.** `execCommand('undo')`
fires `input` with `inputType: "historyUndo"` `[measured]`. Note it in SKILL.md;
there is no fix.

### 6.3 Playwright's `fill` sends one frame; `type` sends two per character

`locator.fill()` -> `dom.ts:610-634` -> injected `fill` returns `'needsinput'` ->
`keyboard.insertText` -> **one `Input.insertText` carrying the whole value**
(`crInput.ts:88-90`). The injected side focuses and selects in JS with no CDP input
events - no Ctrl+A, no triple-click.

`keyboard.type` loops per character (`server/input.ts:111-122`): a US-layout
printable is `down()` + `up()` = **two separate frames**, individually acked. 20
characters is 40 frames.

**So the sentinel can only ride the `fill`/`insertText` path.** On the key path it
arrives as ~24 one-character frames and can never match. `Input.dispatchKeyEvent`'s
`text` is capped at **3 UTF-16 code units** (`kTextLengthCap == 4`, rejected at
`>= 4`) `[measured]`, which makes prefix detection cheap and unambiguous.

### 6.4 Reading a password field works from an isolated world

`HTMLInputElement::Value()` (`html_input_element.cc:1302-1316`) returns
`non_attribute_value_` with **no world check, no password-type check, no
user-gesture check, no `autocomplete` check**. Verified live: isolated and main
world reads identical `[measured]`; `autocomplete=off` has no effect `[measured]`.

**One real restriction.** A credential Chrome autofilled but the user has not
interacted with lives in `suggested_value_`, which `Value()` never reads: **the
human sees it rendered, JS reads `""`.** It is released on a user gesture
(`PasswordValueGatekeeper::OnUserGesture`). `Input.dispatchKeyEvent` and mouse
press grant that activation; **`Input.insertText` and `imeSetComposition` do
not.** So `capture --selector` on an autofilled-but-untouched field returns empty -
detect it and say so rather than reporting an empty capture.

### 6.5 Isolated worlds: cuttle's invalidation is correct, ids are recycled

`Page.createIsolatedWorld` needs **neither `Runtime.enable` nor `Page.enable`** for
a settled frame. Keep it that way: `Runtime.enable` is the one live protocol-level
detection vector (the prototype-chain Proxy variant in console argument preview is
still unpatched as of a March 2026 build).

It returns **only `executionContextId`** (`CDPROTO/page/page.go:286-289`).

**Numeric context ids are recycled.** On main-frame navigation Chrome emits
`Runtime.executionContextsCleared` *instead of* per-context destroyed events and
the counter restarts at 1, so a stale id can silently address a different context.
`uniqueContextId` avoids it but is only obtainable via `Runtime.enable`.

cuttle's triad (`humanize.go:1081-1102`) is the correct mitigation. **Caveat: the
two `Runtime.*` events only arrive if some client sent `Runtime.enable`** - for a
proxy relying on driver traffic, `Page.frameNavigated` is the only dependable one,
which cuttle already handles. **For capture, sidestep it entirely: create the world
fresh at point of use.**

A bfcached document **keeps a valid context id** after navigation - exactly what
cuttle's own comment at `humanize.go:1072-1076` says. Evaluate-succeeds is not
proof you are on the current document.

### 6.6 Use `callFunctionOn` with structured arguments, never a formatted expression

`Runtime.evaluate` takes `contextId` + `expression`; `Runtime.callFunctionOn` takes
`executionContextId` + `functionDeclaration` + `arguments: []CallArgument`.

**For anything touching a secret, use `callFunctionOn`.** The value goes in
`arguments` instead of being concatenated into script text, which eliminates
escaping bugs and keeps it out of anything a `sourceURL`-leak detector could
surface. `returnByValue: true` for a value rather than an `objectId`;
`awaitPromise: true` for `navigator.clipboard.readText()`.

Note `userGesture: true` is **real transient user activation**, not a flag - it
calls `LocalFrame::NotifyUserActivation` with `kDevTools`, granted on the frame, so
it applies document-wide including the main world.

### 6.7 Wire-side redaction is off the table

A proxy must not byte-rewrite these, because they are base64:

**Results:** `Network.getResponseBody.body`, `Network.getRequestPostData.postData`,
`Network.getResponseBodyForInterception.body`,
`Network.streamResourceContent.bufferedData`, `Fetch.getResponseBody.body`,
`IO.read.data`, `Page.captureScreenshot.data`, `Page.printToPDF.data`,
`Page.getResourceContent.content`, `Page.getAnnotatedPageContent.content`,
`Audits.getEncodedResponse.body`, `Debugger.getWasmBytecode.bytecode`,
`HeadlessExperimental.beginFrame.screenshotData`, `PictureTile.picture`,
`CachedResponse.body`.

**Events:** `Page.screencastFrame.data`, `Page.compilationCacheProduced.data`,
`Network.dataReceived.data`, `WebSocketFrame.payloadData`,
`Network.directTCPSocketChunkSent/ChunkReceived.data`.

**Inputs:** `Fetch.fulfillRequest.body` and `.binaryResponseHeaders`,
`Page.addCompilationCache.data`, `Tracing.start.perfettoConfig`,
`Browser.setDockTile.image`.

Padding is per-value, so any string-level rewrite over the transport corrupts
these - and it would still miss the JSON-escaped and percent-encoded forms that are
the *common* case for a secret in a log line. **Mask only text cuttle authors**
(7.6).

### 6.8 Clipboard: readable from an isolated world, with three requirements

`navigator.clipboard.readText()` from an isolated world: **yes - `[measured]`.**
`ClipboardPromise::ValidatePreconditions()` contains no `DOMWrapperWorld`, no
`IsMainWorld()`; every world of a frame resolves to the same `LocalDOMWindow`.

Confirmed end to end on 2026-08-26 against the running container (cuttle 0.13.1,
Chrome 151.0.7922.137 - note the CDP sweep measured 152 on a throwaway profile, so
this is the pinned engine): on an `https://` page, `Browser.setPermission` for
`clipboard-read` and `clipboard-write` both returned ok, `document.hasFocus()` was
already `true` (cuttle's per-page focus pin, without which this fails), a
main-world `writeText` seeded a sentinel, and a `Runtime.evaluate` with the
`contextId` from `Page.createIsolatedWorld` **returned the same sentinel**. World
separation was proven in the same run: a `window.__mainOnly` set in the main world
read `undefined` from the isolated one. `typeof navigator.clipboard` is `object`
there. Probe kept at `scratchpad/clip_probe.py` - it opens its own tab and closes
it, per SKILL.md rule 11.

Requirements, none world-related: **secure context**; **`Document.hasFocus()`**
(already handled - `Emulation.setFocusEmulationEnabled` sets `is_emulating_focus_`,
which `FocusController::IsActive()`/`IsFocused()` both honour, and cuttle pins it
per page); **`clipboardReadWrite` for the TOP-LEVEL origin**. Transient user
activation is **not** required for `readText()`.

**Use `Browser.setPermission`, never `Browser.grantPermissions`.**
`grantPermissions` **denies all 38 other permission types** as a side effect
(`permission_overrides.cc:203-215`), silently breaking geolocation and
notifications for the whole context. It is also absent from the pinned cdproto
(deprecated). Names, confirmed (`CDPROTO/browser/types.go:88-89`):
**`clipboardReadWrite`**, **`clipboardSanitizedWrite`**; in a
`PermissionDescriptor` the spelling is `"clipboard-read"`. Scope is **per
BrowserContext**, and omitting `origin` grants for **all** origins.

**Headless changes which clipboard you read** - `--headless` installs
`HeadlessClipboard : ClipboardNonBacked`, in-memory and process-local, on all
platforms. cuttle runs **headed**, so this does not apply. This independently
confirms the host-tooling finding that the "Chrome under VNC never writes the X
selection" folklore is really about headless.
**`docs/2608-18-improvements-issues-research/README.md:1653` states the VNC
version of that claim and is wrong; correct it.**

There is **no CDP-native clipboard read** - an exhaustive scan of
`browser_protocol.json` yields five `clipboard` hits, all permission enums.

### 6.9 `IO.resolveBlob` reads a page Blob without touching disk

`IO.resolveBlob {objectId}` -> `{uuid}` (`CDPROTO/io/io.go:98-101`), and
`StreamHandle` accepts `blob:<uuid>`. That reads a page-created Blob's bytes out
through `IO.read` with no download and no file - the clean implementation of the
case S01 hand-rolled (a PDF that opens in a viewer tab).

`IO.read` gotchas: **each chunk is its own base64 sequence** - decode each
independently and concatenate decoded bytes. **Always pass an explicit `size`**
(32768); unset produces truncated output on large documents.

### 6.10 Downloads

`Browser.setDownloadBehavior` is browser-level with per-BrowserContext scope - send
it on the browser session with no `sessionId`.

Enum (`CDPROTO/browser/types.go:396-399`): `deny`, `allow`, `allowAndName`,
**`default`**. `"default"` means "use default Chrome behavior if available,
otherwise deny" - it reverts the override without moving the directory, which is
what lets `eventsEnabled: true` be turned on without disturbing cuttle's existing
profile-preference download pin.

`downloadProgress.State` is `inProgress | completed | canceled`. `filePath` is
present on `completed` but **experimental and not guaranteed** - Playwright
ignores it and derives the path itself. Do the same. `downloadWillBegin` can fire
with a `frameId` belonging to no page; guard for it.

Side effect for headed mode: `setDownloadBehavior` breaks the links on
`chrome://downloads` and in the download bubble.

---

## 7. Verified facts: upstream

### 7.1 What not to copy: Playwright's fail-open substitution

`PW/packages/playwright-core/src/tools/backend/context.ts:394-402`: an unknown
name **returns the name as the value**. No error, no warning, no log. Plus a falsy
bug - a secret whose value is the empty string takes the same branch, so the *name*
gets typed. Playwright's own docs concede it is not a security control
(`mcp/config.d.ts:149-154`).

Two more findings shaping strand D:

- **The raw value enters `progress.log`** (`server/dom.ts:610-611`) and escapes
  through two channels redaction never touches: `DEBUG=pw:api` stderr, and the
  **trace zip** (`rg -n redact .../server/trace/` returns zero hits). A third
  near-miss: `backend/sessionLog.ts:47-58` writes `JSON.stringify(toolArgs)` to
  `session.md` unredacted.
- **Redaction is a naive sequential `replaceAll`**, unsorted - which is exactly the
  S14 incident (`Console: <secret>MBCP_MC_<secret>MBCP_MC_5</secret></secret>`).

### 7.2 The single most valuable finding: Blink masks password values in the AX tree

`ax_object.cc:3444-3449` and `ax_node_object.cc:4856-4884`, gated on
`settings.json5:796-800` `accessibilityPasswordValuesEnabled`, **`initial: false`**.

**`Accessibility.getFullAXTree` never returns a cleartext password by default** -
the value is replaced character-for-character with bullets.

Playwright leaks (issue #317, closed 2026-06-25 with no code change) because its
snapshot reads **injected `element.value`** (`injected/src/ariaSnapshot.ts:299-302`)
in the page's utility world. agent-browser does not, because it reads the AX domain
(`cli/src/native/snapshot.rs:948`).

**Two consequences.** cuttle **cannot** redact Playwright's snapshot at the proxy -
by then the password is one string among thousands inside a
`Runtime.callFunctionOn` payload with no structured field to null out. Do not
attempt it; this is the honest limit of strand D. But cuttle **can** route around
it with a SKILL.md rule, and its own read paths should prefer the AX domain.

### 7.3 Nobody verifies a typed value landed

Playwright: none. agent-browser: none (`handle_auth_login` returns
`{"loggedIn": true}` unconditionally at `actions.rs:10918`). CloakBrowser-Manager:
none. browser-use: **yes** at `default_action_watchdog.py:2002-2020` - but
explicitly `if not is_sensitive:`, which takes the **concat-repair** with it
(`:2022-2025`). So a password typed into a field that failed to clear silently
becomes `oldvaluenewpassword` and nothing notices.

Everyone skips it for one reason: **reading the value back is itself the leak.**
The way out nobody has built: verify a *derived* property. cuttle can, because it
injects the check itself rather than trusting the driver.

### 7.4 Patterns worth copying, and one rejected

| Pattern | Source | Why |
|---|---|---|
| No CLI verb ever prints a stored secret | agent-browser `auth.rs:374-388` - `show` returns selectors + username, `credentials_get` returns `hasPassword: true` | non-negotiable |
| Encoding expansion before matching (URL, JSON, HTML, base64) + `MIN_SECRET_LENGTH = 4` | Skyvern `utils/secret_redaction.py:92-101` | the only implementation handling secret-in-a-URL-query; fixes Playwright's and browser-use's exact-byte gap |
| Longest-match-first single-pass regex | browser-use `utils.py:76-89` | a short secret can otherwise chew through a longer one |
| Injection domain-scoped, redaction global | browser-use `service.py:452` vs `utils.py:59-73` | deliberately asymmetric and correct |
| Placeholder names injected into the agent's context | browser-use `message_manager/service.py:391-420` | **why the mechanism gets used.** Playwright's tool descriptions never mention secrets, so the model passes the real value |
| Do not propagate the resolver's stderr | agent-browser `plugins.rs:209,291` | vault error text routinely quotes item names and partial values |
| Credential bound to its own login URL | agent-browser `AuthProfile.url` + `actions.rs:10692-10716` | removing the dangerous affordance beats adding a check to it |

**Rejected: the 1Password "fill blackout"** (agent stops reading the page for the
duration of the fill). Assessed on value, not on KISS. It defends a driver
snapshotting *concurrently with* a fill, while the leak that cost a rotation was a
snapshot taken *later*, to debug a failed login, with the value in the field -
which cuttle cannot redact at all (7.2). The routing rule in 8.7 is the higher
-value move. Revisit only if a session shows a concurrent-snapshot leak.

**And what not to trust:** agent-browser's `security/page.mdx` carries three claims
its own code contradicts. Treat every one of these projects' security docs as
unreliable against their source.

### 7.5 Host-side tooling

| Tool | stdin-safe invocation |
|---|---|
| `op` read | `op read --no-newline "op://vault/item/field"` - diagnostics on stderr prefixed `[ERROR]`, errors echo the *reference* never the value |
| `op` TOTP | `op item get <item> --otp` |
| `op` **write** | **`op item edit <id> 'field[text]=-'` stores a literal `-`.** The only argv-free path is a **JSON template on stdin**: `op item get <id> --format json \| jq '.fields += [...]' \| op item edit <id>` |
| `gh` | `gh secret set NAME` reads stdin when `--body` is absent |
| `aws` | `--secret-string file:///dev/stdin` |
| macOS `security` | **`add-generic-password -w` with a pipe exits 0 and stores nothing.** Use `security -i` |
| `pass` | `pass insert -m -f <name>` pipes stdin straight into gpg |

**`op item get --format json` returns the plaintext value without `--reveal`.**
`--reveal` only gates human-readable output. Never log or error-wrap that output -
this is how a real credential leaked during this plan's own research.

`op` can **hang indefinitely** with no output in headless contexts. Always
`exec.CommandContext` + `Cmd.WaitDelay`.

### 7.6 Clipboard and Go specifics

Prefer **xsel** over xclip: `--selectionTimeout <ms>` gives a self-expiring
clipboard, exactly right for a secret; trixie's xclip 0.13-4 has no `-sensitive`.
Incremental image cost ~86 kB vs ~101 kB (`x11-utils` already pulls the libs). X11
has no clipboard buffer - the owner must **stay resident** to answer
`SelectionRequest` events (ICCCM 2.3.1). `xclip -loops 1` is broken under VNC.

Go:

- **`[]byte` end to end, never `string`.** `clear(b)` at the boundary,
  `runtime.KeepAlive(b)` after. Write `clear(b)` not a manual loop: both lower to
  `arrayClear` when optimizing, but the loop degrades to byte-at-a-time under `-N`
  or any `-race` build.
- **No `memguard`** - a blank import zeroes the process's core-dump *hard* limit
  irreversibly (#166); `NewEnclave` under `RLIMIT_MEMLOCK=0` deadlocks forever with
  no output (#173, unanswered 11+ months); +213 KiB stripped.
- **No `runtime/secret`** (Go 1.26 experiment) - linux only, silent no-op on
  darwin, outside the Go 1 compatibility promise.
- **Honest ceiling:** wiping your own buffer stops mattering once the secret
  reaches stdlib crypto - `aes.NewCipher(key)` allocates reversible round keys in
  ordinary GC heap. For "keep it out of argv, transcripts and logs", `[]byte` +
  `clear` is the whole solution.
- **TTL cache:** mirror `pool.go`'s shape - `mu sync.Mutex` + `map[string]*entry`,
  `time.AfterFunc` (`pool.go:141,178,256`), cancelled under the lock
  (`cancelIdleLocked`, `:238`). Plain `Mutex`, not `sync.Map`, which gives no way
  to zero a value atomically with its removal. **The `AfterFunc` closure must
  capture the key only, never the buffer.**
- **`exec`:** set `Stdout`/`Stderr` explicitly; **never `cmd.Output()`** -
  `ExitError.Stderr` is populated only by `Output()` and lands in `err.Error()`.
- **Git worktree detection:** hand-roll ~25 lines; go-git costs +6.17 MB for a
  boolean. Walk up for `.git`, and **handle `.git` being a regular FILE** containing
  `gitdir: <path>` - that is `git worktree add`, which is the layout this branch is
  developed in. The recorded path may be relative to the file's own directory.
- **stdin:** `cmd.InOrStdin()` is an `io.Reader`, not `*os.File` - type-assert
  before `.Fd()`. `term.ReadPassword` fails `ENOTTY` on a pipe, so branch on
  `term.IsTerminal` first; do not use `os.ModeCharDevice`, it misreports
  `/dev/null`.

---

## 8. Architecture

### 8.1 The store

New `internal/serve/secrets.go`:

```
type secretStore struct {
    mu sync.Mutex
    m  map[string]map[string]*secretEntry   // seed -> name -> entry
}

type secretEntry struct {
    val    []byte
    timer  *time.Timer
    setAt  time.Time
    origin string    // derived on first successful use (3.5)
    source string    // "stdin" | "exec" | "capture" | "prompt" - never the command
}
```

Mirrors `stateStore` (`state.go:42-49`) minus `persist`. Lives on `chromePool`
beside `store *stateStore` (`pool.go:117`), reaches a session via `cdpSessionOpts`
(`wsproxy.go:138-144`). `list` returns `{name, source, age, ttlRemaining, length,
origin}` - **never a value, under any flag.**

### 8.2 Interception

Extend `handleClientFrame` (`humanize.go:403-428`) to four methods.

**`Input.insertText`** - the primary path.
1. Scan `params.text` for `{{cuttle:NAME}}`. The sentinel must be the **entire**
   text, never embedded.
2. No sentinel, and the pre-flight probe says the target is
   `<input type=password>`: **refuse** (3.3), leading with the flag name in the
   first 80 characters.
3. Sentinel, unknown name: **hard CDP error listing the names that do exist** and
   the `cuttle secret set` command.
4. Sentinel, known: pre-flight (8.3), substitute, type through the existing
   humanized path, post-type check (8.4), record/compare origin (3.5).

**`Input.dispatchKeyEvent`** - detection only. `text` caps at 3 UTF-16 units (6.3),
so a sentinel can never fit; on a sentinel-opening prefix, error naming `fill`.

**`Input.imeSetComposition`** - NEW. Refuse a sentinel outright, and close the
existing bypass (6.1) by routing composition through the humanizer or, if that
proves invasive, logging a loud warning naming the method. The warning alone is
acceptable for v1 provided the sentinel is refused.

**`Runtime.evaluate` / `callFunctionOn`** - a sentinel in script text is a hard
error: *"a cuttle secret can only be typed, not evaluated."*

### 8.3 Pre-flight target check (mandatory)

One `Runtime.callFunctionOn` in the isolated world returning the **shape** of
`document.activeElement`, never its value:

```
{ ok, tag, type, disabled, readonly, maxLength, isEditable, hasSuggestedValue }
```

Refuse, naming the reason, when: there is no focused element (silent no-op, 6.2);
the element is `disabled` (**the secret would land elsewhere**); it is `readonly`
(`beforeinput`/`textInput` would still carry the value to a page listener);
`maxLength` is shorter than the secret. This also drives the literal-in-password
refusal (3.3).

No upstream project has this. It is the highest-value piece of the design.

### 8.4 Post-type verification, derived only

After the last keystroke, one isolated-world probe reading
`document.activeElement.value.length` and `selectionStart`. **Never the value,
never a prefix.** On mismatch repair once (select-all, retype as keystrokes),
re-probe, and on a second mismatch answer a CDP error naming the discrepancy -
*"typed 24 runes, field holds 18"*. Fail open if the probe cannot run.

Suppress `typoProb` (`humanize.go:61`) for a secret: `emitTypo` corrects with a
blind Backspace, wrong on a segmented auto-advancing field where the wrong
character advances focus and the Backspace lands in the next box.

### 8.5 Strand C: fewer credential-handling events

**Not a new verb.** `cuttle open` is already the handoff verb (5.5). Add:

- **`cuttle open <url> --until <predicate> --timeout`** - blocks until the page
  leaves the sign-in origin (or the predicate holds), then returns. Strictly
  print-and-wait: **the moment it clicks anything, cuttle has become the driver**
  (section 4).
- **Per-seed window raise** - `xdotool search --pid <seed chrome pid> --onlyvisible
  windowactivate windowraise` (xdotool is already in the image, `Dockerfile:203`),
  plus the seed name on the briefing's viewer line.
- **`cuttle auth status [origin]`** - which origins have stored cookies, when state
  was captured, how old. Removes a fixed 3-5 call tax per session (A26: 11 sessions
  re-running the risky login flow because nothing surfaces per-origin auth state;
  the user's own workflow docs now open with "ASSUME LOGGED OUT").
- **`cuttle secret prompt NAME`** - a human hands in a code without it entering the
  transcript.

**The retrieval ladder replaces "2FA is a handoff trigger".** The real rule is *a
second factor you cannot retrieve*:

1. **TOTP with a registered resolver** - `cuttle secret set GH_TOTP --exec 'op item
   get GitHub --otp'`. Strand B already does this; the command runs at substitution
   time, so it yields a fresh code every use. Not an exception to the feature - it
   *is* the feature.
2. **A code retrievable out of band** - an MCP-reachable inbox, or an inbox already
   logged into cuttle where the agent opens a tab and reads it. Needs no verb, only
   a SKILL.md line telling the agent to check before escalating. Issue A34 lists
   the missing capability as *"a documented recipe for the mail-check leg"*, not a
   command.
3. **Push approval, passkey, hardware tap, captcha, or an unreachable inbox** -
   handoff.
4. **A human has the code and should not paste it into chat** - `secret prompt`.

Rungs 1 and 4 keep the code **out of the agent's context entirely**; rung 2 puts it
in. For a 6-digit code that expires in 30 seconds and is single-use, that is a
small and acceptable exposure - state it as a judgement, not an oversight.

### 8.6 Capture

`cuttle secret capture NAME --selector '#api-key' [--to ...]`. The one boundary
exception (section 4).

Daemon side: dial the seed's CDP (5.6), create a **fresh** isolated world (6.5),
`Runtime.callFunctionOn` with the selector as a structured argument (6.6),
`returnByValue: true`, read `el.value ?? el.textContent`. Return over the loopback
surface behind `rejectUntrustedLoopback`. The CLI pipes to the sink and prints only
`NAME  40 bytes  from #api-key  ttl 15m`.

**Sources:**

1. `--selector` - limits stated honestly in the error: no shadow-DOM piercing
   (`DOM.getDocument {pierce:true}` + `DOM.resolveNode` is the escape hatch if
   needed), a cross-origin iframe is a separate target, and the password-manager
   suggested-value case (6.4) reports empty - detect and say so.
2. `--from-download [--latest] [--wait]` - needs
   `Browser.setDownloadBehavior {behavior:"default", eventsEnabled:true}` at launch
   (6.10) so completion is an event, not a poll for the absence of `.crdownload`.
3. `--from-clipboard` - `Browser.setPermission` (**not** `grantPermissions`, 6.8)
   for `clipboard-read` on the top-level origin, then
   `navigator.clipboard.readText()` in the isolated world with `awaitPromise:true`.
   Focus is already handled by cuttle's focus-emulation pin.

**Sinks**, all CLI-side: `--to memory` (default, 3.4), `--to file:<path>` (0600,
refuses a git working tree, 3.6), `--to exec:'<cmd>'` (value on **stdin**, never
argv, 7.5).

One invariant, both directions: **stdin only.**

**Also document the boundary-clean alternative** (section 4):
`playwright-cli eval 'el => el.value' e5 | cuttle secret set NAME --stdin`.

### 8.7 Masking

A `slog.Handler` wrapping the existing one (`serve.go:38`), plus the CDP error
builders (`humanize.go:1333`).

Match every held value expanded to its **URL-encoded, JSON-escaped, HTML-escaped
and base64 forms** (Skyvern, 7.4), sorted longest-first, single-pass alternation
(browser-use, 7.4), with `MIN_SECRET_LENGTH = 4` and a higher floor for all-numeric
values to prevent the over-redaction that shredded output in the corpus. Also mask
credential-shaped URL query params in cuttle's own log lines - the S08 leak was
`remix_userkey=25039df9...` in a routine retry log.

**Scope honestly.** This covers text cuttle authors. It does **not** and cannot
cover a driver's snapshot (7.2). Say so rather than implying coverage.

### 8.8 Discovery

The briefing (`internal/cli/briefing.go:25-80`) prints the secret **names** the
daemon holds for the seed and which context they came from - names only, never
values, never sources. This is browser-use's insight (7.4): a substitution
mechanism the model is never told about does not get used.

---

## 9. Phases, and how they land

Each phase is independently reviewable and leaves the tree green.

| # | Phase | Type |
|---|---|---|
| 0 | Close the `imeSetComposition` bypass (6.1). Standalone, valuable without the rest | `fix(serve):` |
| 1 | Store, sentinel, pre-flight, derived verification, teaching errors, literal-in-password refusal, HTTP routes, `secret set\|ls\|rm` (stdin only) | `feat(serve):` |
| 2 | Strand C: `open --until`, per-seed window raise, `auth status`, `secret prompt` | `feat(cli):` |
| 3 | `--exec` with globally-persisted config (3.2), exec hygiene (7.6) | `feat(cli):` |
| 4 | Masking (8.7) | `feat(serve):` |
| 5 | `cuttle grab <url>` - authenticated fetch to the host, `IO.resolveBlob` for the blob case (6.9) | `feat(cli):` |
| 6 | Capture: `--selector`, then `downloads --latest/--wait` repair | `feat(cli):` |
| 7 | `--from-clipboard` (6.8 is verified; build on it) | `feat(cli):` |
| 9 | Container pass: correct README.md:1653, record the `-DisableBasicAuth` finding, add the `DLP_Log` comment (11.3) | `docs:` |

**Why this order.** Phase 2 is early because it is the cheapest build with the
largest behavioural change: `auth status` removes logins that should not happen at
all, which is worth more than making an unnecessary login safer. Phase 5 is its own
phase rather than a capture source because issue A9 makes authenticated extraction
the **dominant real use of cuttle** - 15 sessions, three of which sent zero input
events, with the identical `fetch`-refresh-token incantation appearing in six - and
it has no verb today.

**There is no docs phase.** An earlier draft had one, which was wrong:
`internal/cli/SKILL.md` is `//go:embed`'ed, so it is shipped behaviour. A trailing
docs phase would mean either documenting a verb that does not exist yet or
shipping a verb undocumented, and issue A7 is precisely about SKILL.md carrying
claims the daemon does not honour. **Each PR carries its own SKILL.md change**,
and the first one to touch SKILL.md also makes the four cuts from 11.1 that free
the budget.

### It ships as ONE PR

Fixed requirement. The phases above are **commits on `feat/secrets`, not separate
PRs.** Keep them as distinct commits and do not rebase them into one: the squash
throws them away on `main`, but the branch history is what a reviewer walks, and a
nine-file diff reviewed as one blob is the thing to avoid.

**PR title** - this becomes the squash subject on `main` regardless of the commit
titles inside (`PR_TITLE` mode, `docs/RELEASING.md`):

```
feat(serve): daemon-owned secret injection, capture and masking
```

**The `fix:` is not lost.** release-please parses `BEGIN_NESTED_COMMIT` /
`END_NESTED_COMMIT` blocks in the commit body as independent commits, with the
outer subject still its own entry (verified against release-please's own fixture
`test/fixtures/commit-messages/multiple-commits-with-separator.txt`, and
`src/commit.ts:381-389` `splitMessages`). So the PR body ends with:

```
BEGIN_NESTED_COMMIT
fix(serve): humanizer no longer ignores Input.imeSetComposition

A driver could place a whole value into a field with one
Input.imeSetComposition and never commit it: the value is live in .value,
the form submits it, and no insertText or dispatchKeyEvent frame ever
crosses the wire, so it was never humanized and never counted.
END_NESTED_COMMIT
```

Add one more block if the container pass (Phase 9) deserves its own line:

```
BEGIN_NESTED_COMMIT
docs: correct the KasmVNC clipboard claim and record the /api 401
END_NESTED_COMMIT
```

Text outside the blocks stays with the primary commit. Nested blocks change the
CHANGELOG, not the version arithmetic: pre-1.0 both `feat:` and `fix:` bump patch,
so this is still one bump.

**Two traps in the PR body, and they apply inside the nested blocks too.** A body
line that *starts* with `word(` whose `)` closes on a later line makes
release-please's parser throw, and the commit is dropped silently with CI green -
no error anywhere, the release just does not happen. Markdown is not a safe zone;
a fenced code block is parsed the same way. And the PR title, not any commit
title, is the subject. Read `docs/RELEASING.md` before opening it.

**Recovery if the release is skipped anyway:** edit the merged PR body, append a
`BEGIN_COMMIT_OVERRIDE` block with the subject you want, and re-run the `release`
job. release-please re-reads the merged body live and parses only that block.

---

## 10. Tests and tripwires

The project's culture is tripwires that turn a silent regression into a diff
someone must consciously review (`internal/fingerprint/testdata/golden.json`,
`internal/cli/skill_test.go`). Match it.

**The load-bearing test:** using the `recordingHumanizer` harness
(`humanize_keyboard_test.go:15-27`), assert that **no injected CDP frame's JSON
ever contains the secret's bytes** for a full sentinel type. This is the one test
that would have caught every failure mode in the corpus.

| Test | Asserts |
|---|---|
| unknown sentinel | CDP error, **zero** injected frames, error lists existing names |
| empty-value secret | treated as unknown, not as a name to type (Playwright's falsy bug, 7.1) |
| literal into `type=password` | refused; flag name inside the first 80 chars |
| sentinel on the key path | CDP error naming `fill` |
| sentinel in `Runtime.evaluate` | CDP error |
| `imeSetComposition` | not silently forwarded |
| pre-flight: disabled / readonly / short maxLength | refused, nothing typed |
| post-type length mismatch | one repair, then a CDP error naming the discrepancy |
| typo suppression | `typoProb` never fires on a secret value |
| origin mismatch | warns, does not block; the origin is recorded on first success |
| TTL expiry | entry gone, buffer zeroed, timer closure does not pin the value |
| masking | expanded encodings caught; a 3-char value does **not** trigger redaction |
| `--to file:` inside a git worktree | refused, including the `.git`-is-a-file layout |
| `secret ls` | never emits a value under any flag |

Run `just check` (fmt-check + lint + test). `.golangci.yml` enables `gosec`,
`err113`, `wrapcheck`, `revive` with `enable-all-rules` - expect sentinel errors
rather than `fmt.Errorf` with a dynamic string.

---

## 11. Docs

### 11.1 SKILL.md scope rule

**SKILL.md carries only the quirks, tips and rules for driving cuttle as a local
session-mode install.** Not operator material, not pool/server mode, and **not
anything `--help` already prints**. Everything else goes to `docs/OPERATING.md`,
which SKILL.md already links as the operator half.

That rule frees budget rather than consuming it. Current state: 13,585 bytes
against a **16,384 budget** (`skill_test.go:13`), whose comment says raising it is a
deliberate decision - *"cut something first."* Cuts this branch makes:

| Cut | Why |
|---|---|
| `cuttle downloads` three-example block (`:186-190`) | `--help` material |
| Lifecycle command block (`:198-203`) | `--help` material; keep the one non-obvious sentence, that logins survive `down`/`up` |
| Gotcha 5's `?fingerprint=` paragraph (`:225-229`) | pool mode leaking into the session-mode guide; move to OPERATING.md |
| Rule 6's `PLAYWRIGHT_MCP_SECRETS_FILE` paragraph (`:99-104`) | the feature replaces it |

Roughly 2.5 KB freed on top of 2,799 bytes of headroom.

`TestSkillGuideKeepsLoadBearingRules` pins the literal string
`"Secrets never reach"`, so rule 6's heading must survive or the test updates in
the same commit.

### 11.2 What the secrets material in SKILL.md must be

Only what changes how an agent drives a page. Verb syntax lives in
`cuttle secret --help`.

1. The sentinel exists and works in **every driver and every call shape**.
2. **`playwright-cli snapshot` prints a filled password in cleartext;
   `agent-browser`'s does not** - injected `element.value` versus the AX tree Blink
   masks (7.2). cuttle cannot enforce this in code, so it is a routing rule.
3. **Capture before you look.** On a one-time-display credential, `snapshot` and
   `screenshot` are the leak, not the diagnostic.
4. A typed secret is recoverable from the undo stack (6.2).
5. The retrieval ladder (8.5) - the handoff trigger is *a factor you cannot
   retrieve*, not "2FA".

### 11.3 Corrections and the container hardening pass

All three are **in scope for this branch** (Phase 9), not separate filings. They
were found while researching this feature and they all touch the same surface it
touches.

**1. Correct the research doc.**
`docs/2608-18-improvements-issues-research/README.md:1653` states that Chrome
under KasmVNC never writes the X CLIPBOARD selection. **Refuted** - the cause is
headless mode (6.8), confirmed three ways: Chromium source, a container test on
`ghcr.io/glim-sh/cuttle:0.13.1`, and the isolated-world probe in 6.8.

**2. `-DisableBasicAuth` does the opposite of its name - `[measured]`.**
It makes every `/api/*` request return a hardcoded 401 rather than opening the
endpoint (confirmed in the KasmVNC v1.3.3 tag, by a Kasm maintainer in
KasmVNC#268, and measured on the running container 2026-08-26):

```
GET /api/get_screenshot?width=800&height=600  ->  HTTP 401, body "401 Unauthorized"
GET /                                          ->  HTTP 200   (static viewer, unaffected)
```

cuttle passes the flag at `ops/docker/bin/docker-entrypoint.sh:51`, so
`/api/get_screenshot` is **unreachable in the shipped image today**. That kills
rec C5 of the research doc as written. Reaching it needs an owner-bit user
(`kasmvncpasswd -u <name> -w -o <file>`) and dropping the flag - which is a real
decision, because the flag is also what keeps the API shut on an
`-interface 0.0.0.0` listener. **Do not silently drop it.** For this branch,
document the finding and leave the flag; capture's clipboard source (8.6) does not
use the KasmVNC API.

**3. Warning comment at the Xvnc invocation** (`docker-entrypoint.sh:47`):
KasmVNC's `DLP_Log` must never be raised to `verbose` - it percent-encodes and
logs **full clipboard payloads and every keystroke** in both directions
(`common/rfb/ServerCore.cxx:185-188`, `VNCSConnectionST.cxx:398-437`). cuttle does
not pass it, so it is off; the comment exists so nobody turns it on while
debugging exactly this feature. `DLP_ClipSendMax` / `DLP_ClipAcceptMax` /
`DLP_ClipDelay` are the useful knobs on that channel and all default to unlimited.

---

## 12. Risks and open questions

1. **Sentinel format collision.** `{{cuttle:NAME}}` in page content is harmless
   (substitution happens only on the type path), but confirm no driver
   pre-processes braces. Shell single-quoting is safe; zsh does not brace-expand a
   token with no comma or range.
2. **The `imeSetComposition` fix may be invasive.** Routing composition through the
   humanizer means handling partial commits and replacement ranges. If it grows
   past a phase, ship the warning plus the sentinel refusal and file the
   humanization half.
3. **`Browser.downloadProgress.filePath` is not guaranteed** (6.10). Derive the
   path, as Playwright does.
4. **The undo stack retains a typed secret** (6.2) with no fix available.
5. **The literal-in-password refusal is a behaviour change for existing users.**
   Anyone typing a throwaway literal into a password field starts getting an error.
   The flag covers it, but the release note must say so plainly.

---

## 13. What the design review changed

Recorded so the reasoning is not re-derived.

| Was | Now | Why |
|---|---|---|
| KISS-constrained; sources 3-4 and strand C deferred | AX-first, but **"more verbs" is not better AX** | A6: `--context` is the exact fix for a top-ranked problem and was used **once in 14 days**. The budget buys correctness-by-construction and teaching, not surface |
| `--exec` config lifetime collapsed into the value's | config global and durable, value memory-only + TTL | setup once-ever instead of once-per-session - the largest single AX gain |
| warn on a literal in a password field | **refuse**, with a documented flag | it is the moment S08's lost task would have been saved |
| new `cuttle handoff` verb | `cuttle open --until` | **cuttle already has a handoff verb.** Adding a near-duplicate is the exact trap A6 describes |
| "2FA is a handoff trigger" | the retrieval ladder (8.5) | rung 1 is already built by strand B's `--exec`; rung 2 needs a doc line, not a verb |
| `grab` as capture source 3 | its own phase | A9 makes authenticated extraction the dominant real use of cuttle, with no verb |
| per-origin scoping deferred as a gap | derived on first use, warn on mismatch | an `--origin` flag would be `--context` again |
| SKILL.md budget "a hard question with no clean answer" | resolved by a scope rule | session-mode quirks only; no `--help` repetition; operator material to OPERATING.md. The rule **frees** ~2.5 KB |
| `--selector` bundled in unexamined | named as the one deliberate boundary exception | section 4 |
| strand C "make typing rare" | "fewer credential-handling events" | the old name described neither half |
| clipboard read `[unverified]`, gating Phase 7 | `[measured]` on the running container | the requirement that usually bites - `document.hasFocus()` - is already satisfied by cuttle's own focus pin |
| `-DisableBasicAuth` and `DLP_Log` filed separately | in scope as Phase 9 | both were found researching this feature and touch the surface it touches |
| a trailing docs phase | each PR carries its own SKILL.md change | SKILL.md is `//go:embed`'ed shipped behaviour; a trailing phase means shipping a verb undocumented or documenting one that does not exist - issue A7 exactly |
| implicitly one PR | briefly five, then **one PR** by requirement (9.1) | one PR is a fixed constraint. The objection that Phase 0's `fix:` would vanish under a `feat:` title is answered by `BEGIN_NESTED_COMMIT`, which release-please parses as an independent commit while the outer subject keeps its own entry - so one PR still yields both changelog lines. Reviewability is handled by keeping the phases as distinct commits on the branch |

---

## 14. Execution checklist

**One PR** off `feat/secrets` (9.1). Phases are commits on the branch; keep them
distinct and do not rebase them together. `just check` green at every commit.

- [ ] Read sections 4, 5, 6 and 7 in full before writing code
- [ ] Commit: Phase 0 - `fix(serve):` `imeSetComposition`
- [ ] Commit: Phase 1 - store, sentinel, pre-flight, derived verify, refusals,
      HTTP routes, `secret set|ls|rm`
- [ ] Commit: Phase 3 - `--exec` with globally-persisted config
- [ ] Commit: Phase 4 - masking
- [ ] Commit: Phase 2 - `open --until`, window raise, `auth status`,
      `secret prompt`
- [ ] Commit: Phase 5 - `cuttle grab`
- [ ] Commit: Phase 6 - capture `--selector`, downloads `--latest/--wait`
- [ ] Commit: Phase 7 - `--from-clipboard`
- [ ] Commit: Phase 9 - container pass (README.md:1653, `-DisableBasicAuth`
      finding, `DLP_Log` comment)
- [ ] Each commit carries its own SKILL.md change - there is no docs phase (9).
      The first commit to touch SKILL.md also makes the four budget cuts (11.1)
- [ ] PR title: `feat(serve): daemon-owned secret injection, capture and masking`
- [ ] PR body ends with the `BEGIN_NESTED_COMMIT` block for the `fix:` (9.1)
- [ ] Check every PR body line: none may *start* with `word(` unless the `)`
      closes on the same line, code fences included
- [ ] Release note for the literal-in-password refusal (12.5) - it is a behaviour
      change for anyone typing a throwaway literal into a password field

**Already settled, do not re-verify:** the isolated-world clipboard read (6.8) and
the `-DisableBasicAuth` 401 (11.3) were both measured against the running
container on 2026-08-26.

---

## 15. References

**In-repo:** `internal/serve/humanize.go`, `wsproxy.go`, `http.go`, `state.go`,
`pool.go`, `downloads.go`, `serve.go`, `logfile.go`; `internal/cli/commands.go`,
`briefing.go`, `SKILL.md`, `skill_test.go`; `internal/config/config.go`;
`internal/cdp/cdp.go`; `ops/docker/bin/vnc-viewer.html`, `docker-entrypoint.sh`;
`docs/2608-18-improvements-issues-research/README.md` (issues A6, A9, A13, A17,
A25, A26, A34, A44, A45; clusters B20-B22; recs C8, C14, C24, C25; pattern H9.4).

**Upstream clones:** `~/pjv/microsoft/playwright` (`1b44f5a`),
`~/pjv/vercel-labs/agent-browser` (`548b159`),
`~/pjv/cloakhq/cloakbrowser-manager` (`9dd49cf`), `~/pjv/browser-use/browser-harness`,
`~/pjv/kasmtech/kasmvnc` (`v1.3.3`).

**Protocol:** `CDPROTO` =
`~/go/pkg/mod/github.com/chromedp/cdproto@v0.0.0-20260714215040-dc233986426f`.

**External:** [1Password for Claude security model](https://support.1password.com/1password-claude-security);
[playwright-cli#317](https://github.com/microsoft/playwright-cli/issues/317)
(password values in the ARIA snapshot, closed unfixed);
[KasmVNC#268](https://github.com/kasmtech/KasmVNC/issues/268)
(`-DisableBasicAuth` semantics);
[memguard#166](https://github.com/awnumar/memguard/issues/166) and
[#173](https://github.com/awnumar/memguard/issues/173).
