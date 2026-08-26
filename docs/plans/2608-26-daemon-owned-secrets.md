# Daemon-owned secrets: injection, capture, and masking

Plan for `feat/secrets`. Written 2026-08-26 against `main` at `762e66f` (v0.13.1).

Research backing this plan ran as three parallel agents on 2026-08-26: a CDP
protocol sweep (measured live against Chrome 152 on a throwaway profile), an
upstream-implementation sweep (playwright, agent-browser, CloakBrowser-Manager,
browser-use, Skyvern, 1Password), and a host-tooling sweep (`op`, `gh`, xclip/xsel,
KasmVNC, Go stdlib). Every claim below is either a file:line in this repo, a
file:line in a named upstream clone, or marked `[measured]` / `[unverified]`.

---

## 0. How to use this document

You are implementing this from scratch with no prior context. Read sections 1-7
before writing code; they contain findings that invalidate the obvious design.
Section 8 is the build order. Section 12 is the checklist.

**Three things will bite you if you skip ahead:**

1. `Input.imeSetComposition` is a live bypass of cuttle's existing humanizer, not
   just of the new secrets path (section 5.1). Closing it is a prerequisite, not
   an extra.
2. `Input.insertText` returns success even when nothing was inserted, and on a
   `disabled` target it inserts into *whatever was focused instead* (section 5.2).
   A secrets feature that trusts the CDP reply will type credentials into the
   wrong field and report success.
3. `internal/cli/SKILL.md` has a hard 16 KB budget with 2,799 bytes of headroom
   (section 10). The docs for this feature do not fit unless something is cut.

---

## 1. Why

cuttle owns no secret path. `rg -i 'secret|password|redact|otp' internal/ cmd/`
returns only proxy-credential parsing (`internal/fingerprint/proxy.go`,
`geoip.go`) and prose in `internal/cli/SKILL.md`. Everything else is delegated to
whichever driver the agent picked.

From the 14-day session audit (`docs/2608-18-improvements-issues-research/`,
issues A13/A17/A34, clusters B20/B21/B22, cross-session pattern H9.4):

- **8 sessions** hit one of the two secret failure shapes. Two ended in a human
  taking over the credential entry, one in a wrongly-aborted task, two in burned
  TOTP attempts.
- **One real credential rotation** was required (S14), caused not by injection
  but by `snapshot` printing a password field's value in cleartext (S26:
  `textbox "Password..." [ref=e28]: t2J6bXyCQumuF!cukv`).
- **Three sessions independently reinvented** the same
  `Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,'value').set` +
  `atob($(printenv X | base64))` hack, which also emits `isTrusted:false` and
  leaves the submit button disabled.
- S08 spent a whole task aborting to a human because `fill f7e182 HC_PASS`
  returned `len=7`: the driver had typed the literal string `HC_PASS`.

The four mechanisms that fail today:

| # | Mechanism | Failure |
|---|---|---|
| 1 | `PLAYWRIGHT_MCP_SECRETS_FILE` binds at session create | a time-bounded value cannot be refreshed without a new attach; S04 accumulated sessions `bt`, `bt2`, `bt3` in 90 seconds |
| 2 | unknown name substitution | **fails open** - types the literal name into a live password field, no error, no log (section 6.1) |
| 3 | reaches `browser_fill_form` / `browser_type` only | `run-code`'s vm sandbox has no `process`, so the efficient path forces the literal into argv |
| 4 | one driver of three | agent-browser and browser-use users have nothing |

And the reverse direction - getting a credential *out* of a page and into a store
- has no path at all. S05 hand-built a `createTreeWalker` scraper piped through
`jq` into a `chmod 600` file; S18 scraped a PAT into a shell variable piped to
`gh secret set` with correctness resting on remembering not to echo it.

**The AX framing.** The reason agents fumble this is not that any one mechanism
is hard. It is that the agent must know *which mechanism applies to which call
shape on which driver*, and every one of those conditions fails silently. The
fix in every strand below has the same shape: **one name, one failure mode,
loud.**

---

## 2. Scope: five strands

All five ship together. They share one per-seed table.

| # | Strand | Surface |
|---|---|---|
| A | Sentinel substitution at the CDP Input layer | `{{cuttle:NAME}}` typed by any driver in any call shape |
| B | Host-side just-in-time resolution | `cuttle secret set NAME --exec '...'` |
| C | Make typing rare | `cuttle auth status`, `cuttle handoff`, `cuttle secret prompt` |
| D | Close the exfiltration half | masking in cuttle-authored text, SKILL.md rules |
| E | Capture out of a page into a sink | `cuttle secret capture NAME --selector ... --to ...` |

**Out of scope, deliberately:** wire-side redaction of forwarded CDP frames (see
5.7 - it corrupts base64 payloads and misses encoded forms); a TOTP generator
(that is a credential manager's job, and it would put the shared secret in
cuttle's data dir - strand B's `--exec` covers it); any persistence of secret
values to disk (see 3.2).

---

## 3. Decisions locked

These were settled with the user before this plan was written. Do not relitigate
them; if you find a reason one is wrong, stop and raise it.

### 3.1 `--exec` resolution runs CLI-side, on the host

The daemon runs in a container. `op` runs on the host, with biometrics. So the
resolver cannot be daemon-side.

Flow: `cuttle secret set NAME --exec '...'` stores the *command* in host-side
config. At use time the CLI runs it, gets the value, and `PUT`s it to the daemon
over the existing loopback surface with a TTL. The container never shells out and
never learns the command.

This also keeps the 1Password approval prompt on the host where the human is.

### 3.2 Memory-only, with a TTL

No mirroring to `dataDir`. cuttle stores zero credentials today; writing them to a
Docker volume that survives `cuttle down` is a posture change the feature does not
need. Default TTL 15 minutes, settable per secret.

Contrast `stateStore` (`internal/serve/state.go:141-160`), which *does* persist -
the secret store deliberately mirrors its shape but not its `persist` method.

### 3.3 Per-seed

Keyed by seed, matching `/profile/{seed}/state`. A seed is an identity; its
credentials belong to it. In session mode (`cuttle up`, the common case) there is
exactly one seed, `__default__` (`internal/serve/pool.go:314-331`), so this costs
the user nothing.

### 3.4 `capture` defaults to `--to memory`

A capture with no sink lands in the same table under the TTL, so the
site-A-to-site-B flow (generate a token on A, type it into B) is two calls and the
value never leaves the daemon. Print a warning naming the TTL so a sink-less
capture cannot silently expire unnoticed.

### 3.5 `--to file:` refuses a path inside a git working tree

The corpus has driver scratch state committed twice (S02 swept ~3,100 lines of
console logs and page snapshots into a commit). A credential landing in a repo is
the same accident with a worse blast radius. Refuse with a message naming the
repo root and suggesting a path outside it. `--force` overrides.

---

## 4. Verified facts: cuttle's tree

Read these before designing anything. All line numbers are `main@762e66f`.

### 4.1 The humanizer is the right chokepoint, and it already works

`internal/serve/humanize.go`:

- `handleClientFrame` (`:403-428`) prefilters on `bytes.Contains(data, []byte("Input."))`
  then switches on exactly three methods (`:146-149`): `Input.dispatchMouseEvent`,
  `Input.dispatchKeyEvent`, `Input.insertText`.
- `handleInsertText` (`:613-687`) already rewrites one `insertText` into real
  keystrokes: `insertTextMaxRunes = 20` (`:166`) typed as keystrokes, remainder on
  one `insertText`, bounded by `insertTextBudget = 4500ms` (`:173`), and on
  abandonment answers a CDP error naming how many runes landed (`:672-680`).
- **This is rec C3 from the research doc, and it has already shipped.** The
  "38-to-80-character band times out the driver" bug described there is fixed. Do
  not re-fix it.
- `handleKey` (`:529-540`) **returns false** - it paces the keystroke but forwards
  the original unchanged.

### 4.2 The isolated-world machinery already exists and is correct

- `query(sid, expr)` (`:1007-1018`) evaluates in the session's isolated world,
  retrying once on a stale context.
- `createWorld` (`:1132-1166`) does `Page.getFrameTree` then
  `Page.createIsolatedWorld{worldName: "cuttle_probe"}` (`:153`).
- `invalidateWorld` (`:1073-1104`) drops the cache on `Page.frameNavigated`
  (main frame only), `Runtime.executionContextsCleared`, and a matching
  `Runtime.executionContextDestroyed`. **This triad is exactly right** - confirmed
  against Chromium source in section 5.5.
- `probeExpr` (`:1178+`) builds the expression with `fmt.Sprintf`. **Capture must
  not follow this pattern** - see 5.6.

### 4.3 The proxy already has interception patterns to copy

`internal/serve/wsproxy.go`:

- `preprocessClient` (`:199-246`) is the client-to-browser chokepoint. It already
  hosts `blockContextCreation` (`:205`), `blockBrowserTeardown` (`:213`), the
  keep-alive guard (`:227`), the humanizer (`:239`), and `rewriteFetchEnable`
  (`:243`).
- `cdpSessionOpts` (`:138-144`) is how per-seed config reaches a session:
  `user`, `pass`, `humanize`, `keepAlive`, `locale`, `allowContexts`. **Add the
  secret store here**, the same way `locale` arrives.
- `newHumanizer` (`:193`) is constructed per CDP connection. The secret store must
  therefore live on the pool/multiplexer, not on the humanizer.

### 4.4 cuttle already solves this exact problem once, for proxy credentials

`internal/fingerprint/proxy.go:57-70` strips proxy credentials from argv;
`internal/serve/pool.go:892-896` strips them from the status endpoint; the 407 is
answered over `Fetch.continueWithAuth` and never surfaced to the client
(`wsproxy.go:621-660`).

**This is the model.** The whole feature is that pattern generalized from one
hard-coded credential to a named table.

### 4.5 The HTTP surface and its guard

`internal/serve/http.go:59-76` routes exactly nine handlers.
`rejectUntrustedLoopback` (`:386-397`) requires a loopback `Host` (defeating DNS
rebinding) plus the Origin allow-list. Every new secret route must sit behind it,
exactly as `/downloads` and `/profile/{seed}/state` do.

`multiplexer` (`:49-57`) holds `pool`, `port`, `humanize`, `allowContexts`,
`draining`.

### 4.6 The CLI-to-daemon path

`runDownloads` (`internal/cli/commands.go:798-821`) is the template:
`resolveRunning` -> `reachStable` -> `endpointURLs(ep)` -> `getJSON(ctx, base+"/downloads", &payload)`.

`pullDownload` (`:847-881`) is the template for "move bytes without printing
them": streams to a 0600 file and prints only the path. Its doc comment states
the rule this feature generalizes.

Cobra commands are built in `internal/cli/commands.go`; the root sets
`SilenceUsage`/`SilenceErrors` (`root.go:51-52`), so a failing secret command will
not dump usage.

### 4.7 The daemon log is persisted, and funnels through one handler

`internal/serve/serve.go:38` - `var logger = slog.New(slog.NewTextHandler(os.Stderr, nil))`,
with `logInfo`/`logWarn`/`logError` shims at `:39-41`.

`internal/serve/logfile.go` tees this to `/data/logs/serve.log` on durable session
profiles, capped at 20 MB with one rotation. **A secret in a log line is a durable
leak, not a cosmetic one.** The single handler is the masking chokepoint.

### 4.8 Daemon-side CDP with no driver attached

Two existing idioms, both usable for `capture`:

- Raw websocket + `cdpRequest(ctx, conn, id, method, params)`
  (`internal/serve/keepalive.go:170`).
- chromedp via `internal/cdp` - `Extract`/`Inject` with a `connect(ctx, cdpBase, seed)`
  helper (`internal/cdp/cdp.go:145`, `:339`).

Prefer the raw path for capture: it needs one `Runtime.callFunctionOn` in an
isolated world, and chromedp's session management would create targets we do not
want.

### 4.9 cuttle's VNC viewer already implements BinaryClipboard correctly

`ops/docker/bin/vnc-viewer.html`:

- `sendClipboard` (`:43-54`) matches KasmVNC `SMsgReader.cxx:276-311`.
- `readBinaryClipboard` (`:63-94`) matches `SMsgWriter.cxx:80-99`, including the
  server-to-client-only `u32 id` field.
- It sends clipboard and keystrokes on the **same socket** (`:41-42`), so TCP
  ordering makes paste race-free. CloakBrowser-Manager uses two transports and has
  a real race.
- It reads via the `paste` event's `clipboardData` (`:181`), which needs no
  permission and no secure context - avoiding the failure that silently disables
  CloakBrowser's clipboard on plain `http://<LAN-IP>`.

**Strand E's clipboard source has a large head start here.** Do not rewrite it.

---

## 5. Verified facts: CDP

Measured against Chrome 152.0.7977.64 unless marked otherwise. `CDPROTO` =
`github.com/chromedp/cdproto@v0.0.0-20260714215040-dc233986426f` (the pinned
version).

### 5.1 `Input.imeSetComposition` is a live bypass - fix this first

A driver can put an arbitrary string into a field with **one**
`Input.imeSetComposition` and never commit it. The value is live in `.value`, the
form will submit it, and **no `insertText` or `dispatchKeyEvent` frame ever
crosses the wire.**

`[measured]` on an empty `<input>`:

```
imeSetComposition("ni",2,2)  -> compositionstart / compositionupdate /
                                beforeinput(insertCompositionText) / input
                                .value === "ni"     <-- readable, submittable
```

Params (`CDPROTO/input/input.go:222-228`): `text`, `selectionStart`,
`selectionEnd` required; `replacementStart`/`replacementEnd` optional (supplying
only one is `InvalidParams`).

Two consequences:

1. **This is an existing hole in the humanizer**, independent of secrets: a value
   placed this way is never humanized and never counted. It is the `fill()`
   zero-keystroke tell in a new costume.
2. `maxlength` is **not** enforced during composition (`typing_command.cc:422`
   skips `DispatchBeforeTextInsertedEvent` for `kTextCompositionUpdate`); the
   clamp only happens in `FinishComposingText`, which the CDP path never calls.
   `<input maxlength=3>` held `"abcdefgh"` `[measured]`.

Note: `Input.imeCommitComposition` **does not exist** - the doc comment referring
to it is stale, and calling it returns `-32601` `[measured]`. The commit path is
`Input.insertText`.

Playwright never emits `imeSetComposition`, so this costs nothing today - but the
interceptor must cover it or the sentinel is trivially bypassable.

### 5.2 `Input.insertText` lies about success, and can hit the wrong element

`InputHandler::InsertText` binds `sendSuccess` as the mojo reply closure, so
**it always reports success, even when nothing was inserted.**

Three silent-failure modes, all `[measured]`:

| target state | result |
|---|---|
| no focused element | total no-op, zero events, CDP still returns `{}` |
| `disabled` | full no-op - **the insert lands on whatever *was* focused** |
| `readonly` | value rejected, but `beforeinput` **and** `textInput` are dispatched carrying the full text - a page listening on `beforeinput` sees the secret |

The `disabled` case is the dangerous one for this feature: a secret aimed at a
disabled field goes somewhere else entirely, and the driver reports success.

**Therefore: a pre-flight target check is mandatory before substituting a
secret.** See 7.3.

Two more facts worth carrying:

- `maxlength` **is** respected on the `insertText` path (`typing_command.cc:422-430`
  -> `TextFieldInputType::HandleBeforeTextInsertedEvent`). `insertText("abcdef")`
  into `<input maxlength=3>` gives `"abc"` `[measured]`. So a silently truncated
  secret is reachable, which is what the post-type length check catches.
- **A secret typed through any path lands in the undo stack.**
  `CompositeEditCommand::AppliedEditing` registers an undo step;
  `execCommand('undo')` fires `input` with `inputType: "historyUndo"`
  `[measured]`. Note it in SKILL.md; there is no fix.

### 5.3 Playwright's `fill` sends one frame; `type` sends two per character

`locator.fill()` -> `dom.ts:610-634` -> injected `fill` returns `'needsinput'` ->
`keyboard.insertText` -> **one `Input.insertText` carrying the whole value.**
Confirmed in `PW/packages/playwright-core/src/server/chromium/crInput.ts:88-90`.

The injected side focuses and selects in JS with no CDP input events
(`injected/src/injectedScript.ts:898,902-928`) - no Ctrl+A, no triple-click.

`keyboard.type` / `pressSequentially` loops per character
(`server/input.ts:111-122`): a US-layout printable char is `press()` = `down()` +
`up()` = **two separate `Input.dispatchKeyEvent` messages**, each individually
acked. 20 characters is 40 CDP frames.

**Design consequence: the sentinel can only ride the `fill`/`insertText` path.**
On the key path it arrives as ~24 one-character frames and can never match. The
key path gets prefix detection and a hard error instead (7.2).

`Input.dispatchKeyEvent`'s `text` param is capped at **3 UTF-16 code units**
(`WebKeyboardEvent::kTextLengthCap == 4`, rejected when `size() >= 4`)
`[measured]`, which makes prefix detection cheap and unambiguous.

### 5.4 Reading a password field works from an isolated world

`HTMLInputElement::Value()` (`html_input_element.cc:1302-1316`) returns
`non_attribute_value_` with **no world check, no password-type check, no
user-gesture check, no `autocomplete` check**. Verified live: created an isolated
world, confirmed separateness, read `#pw.value` from both worlds - identical
`[measured]`. `autocomplete=off` has no effect `[measured]`.

**One real restriction, from Chrome's password manager.** A credential that
Chrome autofilled but the user has not yet interacted with is stored in
`suggested_value_` (`TextControlElement::suggested_value_`), which `Value()` never
reads: **the human sees it rendered, JS reads `""`.** It is released to the real
value on a user gesture (`PasswordValueGatekeeper::OnUserGesture`).

`Input.dispatchKeyEvent` (non-modifier keyDown) and any mouse press **do** grant
that activation; **`Input.insertText` and `Input.imeSetComposition` do not.**

So `capture --selector` on an autofilled-but-untouched password field returns
empty. Detect it and say so rather than reporting an empty capture.

### 5.5 Isolated worlds: cuttle's invalidation is correct, but ids are recycled

`Page.createIsolatedWorld` needs **neither `Runtime.enable` nor `Page.enable`**
for a settled frame - confirmed at Chromium source level. Keep it that way:
`Runtime.enable` is the one live protocol-level detection vector (the
prototype-chain Proxy variant in `console.*` argument preview is still unpatched
as of a March 2026 build).

It returns **only `executionContextId`**, not `uniqueId`
(`CDPROTO/page/page.go:286-289`).

**Numeric context ids are recycled.** On main-frame navigation Chrome emits
`Runtime.executionContextsCleared` *instead of* per-context destroyed events, and
the id counter restarts at 1. A stale cached id can therefore silently address a
*different* context. `uniqueContextId` avoids this but is only obtainable from
`Runtime.executionContextCreated`, which needs `Runtime.enable`.

cuttle's triad (`humanize.go:1081-1102`) is already the correct mitigation.
**Caveat: the two `Runtime.*` events only arrive if some client sent
`Runtime.enable`.** For a proxy relying on driver traffic, `Page.frameNavigated`
is the only one you can count on - which cuttle already handles.

**For capture, sidestep the whole problem: create the world fresh at point of
use.** Capture is a one-shot operation with no hot path to protect.

Also confirmed: a bfcached document **keeps a valid context id** after navigation,
which is exactly what cuttle's own comment at `humanize.go:1072-1076` says.
Evaluate-succeeds is not proof you are on the current document.

### 5.6 Use `callFunctionOn` with structured arguments, never a formatted expression

| | `Runtime.evaluate` | `Runtime.callFunctionOn` |
|---|---|---|
| numeric id param | `contextId` | `executionContextId` |
| body | `expression` string | `functionDeclaration` + `arguments: []CallArgument` |

**For anything touching a secret, use `callFunctionOn`.** The value goes in
`arguments` as a structured `CallArgument` instead of being concatenated into
script text, which eliminates escaping bugs and keeps the secret out of anything a
`sourceURL`-leak detector could surface.

`returnByValue: true` is required to get a value rather than an opaque `objectId`.
`awaitPromise: true` is required for `navigator.clipboard.readText()`.

Note `userGesture: true` is **real transient user activation**, not a flag - it
calls `LocalFrame::NotifyUserActivation` with
`UserActivationNotificationType::kDevTools`, granted on the frame, so it applies
document-wide including the main world.

### 5.7 Wire-side redaction is off the table - here is the exhaustive reason

A proxy must not byte-rewrite these, because they are base64:

**Command results:** `Network.getResponseBody.body`,
`Network.getRequestPostData.postData`, `Network.getResponseBodyForInterception.body`,
`Network.streamResourceContent.bufferedData`, `Fetch.getResponseBody.body`,
`IO.read.data`, `Page.captureScreenshot.data`, `Page.printToPDF.data`,
`Page.getResourceContent.content`, `Page.getAnnotatedPageContent.content`,
`Audits.getEncodedResponse.body`, `Debugger.getWasmBytecode.bytecode`,
`HeadlessExperimental.beginFrame.screenshotData`, `LayerTree` `PictureTile.picture`,
`CacheStorage` `CachedResponse.body`.

**Events:** `Page.screencastFrame.data`, `Page.compilationCacheProduced.data`,
`Network.dataReceived.data`, `WebSocketFrame.payloadData`,
`Network.directTCPSocketChunkSent/ChunkReceived.data`.

**Command inputs:** `Fetch.fulfillRequest.body` and `.binaryResponseHeaders`,
`Page.addCompilationCache.data`, `Tracing.start.perfettoConfig`,
`Browser.setDockTile.image`.

Padding is per-value, so any string-level rewrite pass over the transport
corrupts these. And it would still miss the JSON-escaped and percent-encoded forms
that are the *common* case for a secret in a log line. **Mask only text cuttle
authors itself** (section 7.6).

### 5.8 Clipboard: readable from an isolated world, with three real requirements

`navigator.clipboard.readText()` from an isolated world: **yes, definitively.**
`ClipboardPromise::ValidatePreconditions()` contains no `DOMWrapperWorld`, no
`ScriptState::World()`, no `IsMainWorld()`. Every world of a frame resolves to the
same `LocalDOMWindow`. `[unverified empirically - source-derived]`

The three requirements, none of them world-related:

1. **Secure context** (https or localhost).
2. **`Document.hasFocus()` must be true.** This is the one that bites - and
   **cuttle already fixes it**: `Emulation.setFocusEmulationEnabled` sets
   `is_emulating_focus_`, which `FocusController::IsActive()`/`IsFocused()` both
   honour. cuttle pins this per page already.
3. **`clipboardReadWrite` granted for the TOP-LEVEL origin.**

Transient user activation is **not** required for `readText()`.

**Use `Browser.setPermission`, never `Browser.grantPermissions`.**
`grantPermissions` **denies all 38 other permission types** as a side effect
(`permission_overrides.cc:203-215` loops every type and sets non-listed ones to
`DENIED`), which would silently break geolocation, notifications and the rest for
the whole browser context. `setPermission` sets one and leaves the rest alone.
`grantPermissions` is also **absent from the pinned cdproto** (deprecated, and
cdproto-gen drops deprecated commands), so it would need a raw send anyway.

Permission names, confirmed exactly (`CDPROTO/browser/types.go:88-89`):
**`clipboardReadWrite`** and **`clipboardSanitizedWrite`**. In a
`PermissionDescriptor` the spelling is the Permissions-API one: `"clipboard-read"`,
or `"clipboard-write"` with `allowWithoutSanitization: true`.

Scope is **per BrowserContext, not per target** - the override applies to every
page in the profile regardless of which session issued it. Omitting `origin`
grants for **all** origins; prefer naming the top-level origin.

**Headless changes which clipboard you read.** `--headless` installs
`HeadlessClipboard : public ui::ClipboardNonBacked` - in-memory, process-local, on
all platforms. cuttle runs **headed**, so this does not apply. This independently
confirms the host-tooling finding that the "Chrome under VNC never writes the X
selection" folklore is really about headless mode. **`docs/2608-18-improvements-issues-research/README.md:1653`
states the VNC version of that claim and is wrong; correct it.**

There is **no CDP-native clipboard read** - an exhaustive scan of
`browser_protocol.json` for `clipboard` yields five hits, all permission enums.
`Input.dispatchKeyEvent {commands:["selectAll","copy"]}` populates the real
clipboard with no permission needed, but is write-only.

### 5.9 `IO.resolveBlob` reads a page Blob without touching disk

`IO.resolveBlob {objectId}` -> `{uuid}` (`CDPROTO/io/io.go:98-101`), and
`StreamHandle` accepts `blob:<uuid>` (`CDPROTO/io/types.go:5-7`). That reads a
page-created Blob's bytes out through `IO.read` with no download and no file.

This is the clean implementation of `cuttle grab` for the case S01 hand-rolled
(a PDF that opens in a viewer tab; the agent synthesized a blob download to get at
it).

`IO.read` gotchas: **each chunk is its own base64 sequence** - decode each
independently and concatenate the decoded bytes, or you corrupt chunk boundaries.
And **always pass an explicit `size`** (32768); leaving it unset produces
truncated output on large documents.

### 5.10 Downloads

`Browser.setDownloadBehavior` is a **browser-level command with per-BrowserContext
scope** - send it on the browser session with no `sessionId`.

Enum (`CDPROTO/browser/types.go:396-399`): `deny`, `allow`, `allowAndName`,
**`default`** - `"default"` exists and means "use default Chrome behavior if
available, otherwise deny", i.e. it reverts the override without moving the
directory. That is what lets `eventsEnabled: true` be turned on without disturbing
cuttle's existing profile-preference download pin.

`Browser.downloadProgress.State` is `inProgress | completed | canceled` (no
`interrupted`). `filePath` is present on `completed` **but is experimental and not
guaranteed** - Playwright deliberately ignores it and derives the path itself.
Do the same.

`downloadWillBegin` can fire with a `frameId` belonging to no page (DevTools
window, extension page) - guard for it.

Side effect worth knowing for headed mode: `setDownloadBehavior` breaks the links
on `chrome://downloads` and in the download bubble. A human taking over in the
viewer sees a broken downloads UI.

---

## 6. Verified facts: upstream

### 6.1 What not to copy: Playwright's fail-open substitution

`PW/packages/playwright-core/src/tools/backend/context.ts:394-402`:

```ts
lookupSecret(secretName: string) {
  if (!this.config.secrets?.[secretName])
    return { value: secretName, ... , isSecret: false };
  ...
}
```

**Unknown name returns the name as the value.** No error, no warning, no log. Plus
a falsy bug: a secret whose value is the empty string takes the same branch, so
the *name* gets typed.

Playwright's own type docs concede the mechanism is not a security control
(`mcp/config.d.ts:149-154`): *"It is a convenience and not a security feature."*

**cuttle must fail closed.** An unmatched sentinel is a hard CDP error and nothing
is typed.

Two more Playwright findings that shape strand D:

- **The raw value enters `progress.log`** (`server/dom.ts:610-611`:
  `progress.log(\`  fill("${value}")\`)`) and escapes through two channels
  redaction never touches: `DEBUG=pw:api` stderr, and the **trace zip**
  (`rg -n redact packages/playwright-core/src/server/trace/` returns zero hits).
  A third near-miss: `backend/sessionLog.ts:47-58` writes `JSON.stringify(toolArgs)`
  to `session.md` with no redaction.
- **Redaction is a naive sequential `replaceAll`**, unsorted. Short or common
  values corrupt unrelated output - which is exactly the S14 incident in the
  corpus (`Console: <secret>MBCP_MC_<secret>MBCP_MC_5</secret></secret> errors`).

### 6.2 The single most valuable finding: Blink masks password values in the AX tree

`third_party/blink/renderer/modules/accessibility/ax_object.cc:3444-3449` and
`ax_node_object.cc:4856-4884`, gated on
`settings.json5:796-800` `accessibilityPasswordValuesEnabled`, **`initial: false`**.

**`Accessibility.getFullAXTree` over CDP never returns a cleartext password value
by default.** The value is replaced character-for-character with bullets.

Playwright leaks (issue #317, closed 2026-06-25 with no code change) because its
ARIA snapshot is built from **injected `element.value`**
(`injected/src/ariaSnapshot.ts:299-302`), computed in the page's utility world and
returned already-serialized. agent-browser does **not** leak because it builds
from the AX domain (`cli/src/native/snapshot.rs:948`).

**Two consequences:**

1. cuttle **cannot** redact Playwright's snapshot at the proxy. By the time a
   proxy sees it, the password is one string among thousands inside a
   `Runtime.callFunctionOn` return payload, with no structured field to null out.
   Do not attempt it. This is the honest limit of strand D.
2. cuttle **can** route around it: a SKILL.md rule saying that on a page with a
   filled credential field, `playwright-cli snapshot` leaks it and
   `agent-browser`'s does not. And cuttle's own read/capture paths should prefer
   the AX domain where a choice exists.

### 6.3 Nobody verifies a typed value landed, and browser-use's opt-out is instructive

| Project | Verification |
|---|---|
| Playwright | none. `browser_verify_value` exists but is a separate tool the model must choose, and on mismatch it puts the cleartext into the response |
| agent-browser | none. `handle_auth_login` returns `{"loggedIn": true}` unconditionally at `actions.rs:10918` |
| CloakBrowser-Manager | none - returns ok as soon as xclip's stdin closes |
| browser-use | **yes**, at `default_action_watchdog.py:2002-2020` - but explicitly `if not is_sensitive:` |

browser-use's opt-out takes the **concat-repair** with it (`:2022-2025`, also
gated `not is_sensitive`). That repair fixes "the field wasn't cleared so the new
text got appended to the old". Disabled for secrets, a password typed into a
field that failed to clear silently becomes `oldvaluenewpassword` and nothing
notices.

Everyone skips it for the same reason: **reading the value back is itself the
leak.** The way out nobody has built: verify a *derived* property. cuttle is
positioned to, because it can inject the check itself rather than trusting the
driver.

### 6.4 Patterns worth copying

| Pattern | Source | Why |
|---|---|---|
| No CLI verb ever prints a stored secret | agent-browser `auth.rs:374-388` - `show` returns selectors + username, `credentials_get` returns `hasPassword: true` | non-negotiable |
| Encoding expansion before matching: URL-encoded, JSON-escaped, HTML-escaped, base64, plus `MIN_SECRET_LENGTH = 4` guards | Skyvern `utils/secret_redaction.py:92-101` | the only implementation that handles the secret-in-a-URL-query case; fixes both Playwright's and browser-use's exact-byte gap |
| Longest-match-first, single-pass regex | browser-use `utils.py:76-89` | strictly better than sequential `replaceAll`, which lets a short secret chew through a longer one |
| Injection domain-scoped, redaction global | browser-use `service.py:452` vs `utils.py:59-73` | deliberately asymmetric and correct |
| Placeholder names injected into the agent's context | browser-use `message_manager/service.py:391-420` | **why the mechanism actually gets used.** Playwright's tool descriptions never mention secrets, so the model passes the real value |
| Confirmation keyed to the *capability* (`plugin:vault:credential.read`), not the keystroke | agent-browser `plugins.rs:144`, `actions.rs:2117-2123` | gate the retrieval |
| `stderr -> null` and do not propagate the resolver's error text | agent-browser `plugins.rs:209,291` | vault error text routinely quotes item names and partial values |
| Credential bound to its own login URL; no "fill into whatever page is open" affordance | agent-browser `AuthProfile.url` + `actions.rs:10692-10716` | removing the dangerous affordance beats adding a check to it |

**And what not to copy:** agent-browser's `security/page.mdx` carries three claims
its own code contradicts (credentials "do not pass through the daemon's IPC
channel" - they do for `auth save`; profiles "always encrypted" - reads accept
plaintext JSON at `auth.rs:231`; "pending confirmations auto-deny after 60
seconds" - no timeout exists). Treat every one of these projects' security docs as
unreliable against their source.

### 6.5 Host-side tooling

| Tool | stdin-safe invocation |
|---|---|
| `op` read | `op read --no-newline "op://vault/item/field"` - value on stdout, diagnostics on stderr prefixed `[ERROR]`, errors echo the *reference* never the value |
| `op` TOTP | `op item get <item> --otp` |
| `op` **write** | **`op item edit <id> 'field[text]=-'` stores a literal `-`.** The only argv-free path is a **JSON template on stdin**: `op item get <id> --format json \| jq '.fields += [...]' \| op item edit <id>` |
| `gh` | `gh secret set NAME` reads stdin when `--body` is absent |
| `aws` | `--secret-string file:///dev/stdin` |
| macOS `security` | **`add-generic-password -w` with a pipe exits 0 and stores nothing.** Use `security -i` |
| `pass` | `pass insert -m -f <name>` pipes stdin straight into gpg. Without `-e`/`-m` it reads twice |

**`op item get --format json` returns the plaintext value without `--reveal`.**
`--reveal` only gates human-readable output. Never log or error-wrap that output.
This is how a real credential leaked during this plan's own research.

`op` can **hang indefinitely** with no stdout, stderr or exit in headless
contexts. Always `exec.CommandContext` + `Cmd.WaitDelay`.

### 6.6 Clipboard tooling

Prefer **xsel** over xclip: its `--selectionTimeout <ms>` gives a self-expiring
clipboard, which is exactly right for a secret. trixie's xclip is 0.13-4 and has
no `-sensitive`/`-wait`. Incremental image cost is ~86 kB (xsel) vs ~101 kB
(xclip) - `x11-utils` already pulls the shared libs, so the usual "xclip drags in
libxmu6" argument does not apply here.

X11 has no clipboard buffer: the owner process must **stay resident** to answer
`SelectionRequest` events (ICCCM section 2.3.1). `xclip -loops 1` is broken under
VNC - Xvnc consumes 2 SelectionRequests on ownership change alone. The default
(unlimited) is correct.

**KasmVNC has a built-in keylogger and clipboard logger.** `DLP_Log`
(`common/rfb/ServerCore.cxx:185-188`), default `off`; at `verbose` it
percent-encodes and logs **full clipboard payloads and every keystroke**. cuttle
does not pass it. **Add a warning comment at the Xvnc invocation
(`ops/docker/bin/docker-entrypoint.sh:47`) so nobody enables it for debugging.**

Unrelated but found en route: **`-DisableBasicAuth` 401s all of `/api/*`** rather
than opening it (confirmed in the v1.3.3 tag and by a Kasm maintainer in
KasmVNC#268). cuttle passes it at `docker-entrypoint.sh:51`, so
`/api/get_screenshot` is unreachable in the shipped image. That kills rec C5 of
the research doc as written. **Out of scope here; file it separately.**

### 6.7 Go implementation

- **`[]byte` end to end, never `string`.** `clear(b)` at the boundary,
  `runtime.KeepAlive(b)` after it. Write `clear(b)` rather than a manual loop:
  both lower to `arrayClear` when optimizing, but the manual loop degrades to
  byte-at-a-time under `-N` or any `-race` build.
- **Do not use `memguard`.** A blank import zeroes the process's core-dump *hard*
  limit (irreversibly, issue #166); `NewEnclave` under `RLIMIT_MEMLOCK=0`
  deadlocks forever with no output (issue #173, unanswered 11+ months); it starts
  a permanent 500 ms ticker goroutine; +213 KiB stripped.
- **Do not use `runtime/secret`** (Go 1.26, `GOEXPERIMENT=runtimesecret`) - linux
  only, a silent no-op on darwin, outside the Go 1 compatibility promise, and it
  still has open flaky tests where the harness finds the secret in memory after
  `Do` returns.
- **Ceiling to be honest about:** wiping your own buffer stops mattering once the
  secret reaches stdlib crypto - `aes.NewCipher(key)` allocates reversible
  expanded round keys in ordinary GC heap. For a threat model of "keep it out of
  argv, transcripts and logs", `[]byte` + `clear` is the whole solution.
- **TTL cache:** mirror `pool.go`'s existing shape - `mu sync.Mutex` +
  `map[string]*entry` keyed by seed, armed with `time.AfterFunc`
  (`pool.go:141,178,256`), cancelled under the lock (`cancelIdleLocked`, `:238`).
  Use a plain `Mutex`, not `sync.Map`, which gives no way to zero a value
  atomically with its removal. **The `AfterFunc` closure must capture the key
  only, never the buffer** - capturing the buffer pins it for the full TTL and
  defeats the zeroing.
- **`exec`:** set `Stdout`/`Stderr` explicitly and **never use `cmd.Output()`** -
  `ExitError.Stderr` is populated only by `Output()` and lands in `err.Error()`.
  Never `CombinedOutput()`.
- **Git worktree detection:** no stdlib support; go-git costs +6.17 MB and 20
  modules for a boolean. Hand-roll ~25 lines. Walk up looking for `.git`;
  **handle the case where `.git` is a regular FILE** containing `gitdir: <path>`
  (this is `git worktree add` and submodules, and it is exactly the layout this
  branch is being developed in). The recorded path may be relative to the file's
  own directory. Honour `GIT_DIR`/`GIT_WORK_TREE` and `GIT_CEILING_DIRECTORIES`.
- **stdin:** `cmd.InOrStdin()` returns `io.Reader`, not `*os.File` - type-assert
  before `.Fd()`. `term.ReadPassword` fails with `ENOTTY` on a pipe, so branch on
  `term.IsTerminal` first. Do not use `os.ModeCharDevice` stat-detection; it
  misreports `/dev/null` as a terminal.

---

## 7. Architecture

### 7.1 The store

New file `internal/serve/secrets.go`.

```
type secretStore struct {
    mu sync.Mutex
    m  map[string]map[string]*secretEntry   // seed -> name -> entry
}

type secretEntry struct {
    val     []byte
    ttl     time.Duration
    timer   *time.Timer
    setAt   time.Time
    source  string   // "stdin" | "exec" | "capture" | "prompt" - never the command
}
```

Mirrors `stateStore` (`internal/serve/state.go:42-49`) minus `persist`. Lives on
`chromePool` beside `store *stateStore` (`pool.go:117`), reaches a CDP session
through `cdpSessionOpts` (`wsproxy.go:138-144`) the same way `locale` does.

Never expose values. `list` returns `{name, source, age, ttlRemaining, length}`.

### 7.2 Interception

In `handleClientFrame` (`humanize.go:403-428`), extend the switch to four methods.

**`Input.insertText`** - the primary path.
1. Scan `params.text` for `{{cuttle:NAME}}`.
2. No sentinel: unchanged behaviour.
3. Sentinel, unknown name: **hard CDP error**, nothing typed. Message names the
   secret name and `cuttle secret set`.
4. Sentinel, known: run the pre-flight check (7.3), substitute, type through the
   existing humanized path, run the post-type check (7.4).

A sentinel must be the **entire** text, not embedded. Partial substitution invites
a value being spliced into a larger string that then goes somewhere else.

**`Input.dispatchKeyEvent`** - detection only. `text` is capped at 3 UTF-16 units
(5.3), so a sentinel can never fit. If `text` matches a sentinel-opening prefix
(`{`, `{{`, `{{c`), answer a CDP error explaining that secrets ride `fill`, not
`type`. Otherwise unchanged.

**`Input.imeSetComposition`** - NEW. Two jobs:
1. Refuse a sentinel outright (composition is never committed through cuttle's
   humanizer, so a substituted value would be unhumanized and uncounted).
2. **Close the existing bypass** (5.1) by routing composition through the
   humanizer or, if that proves invasive, logging a loud warning naming the
   method. Decide during implementation; the warning alone is acceptable for v1
   as long as the sentinel is refused.

**`Runtime.evaluate` / `Runtime.callFunctionOn`** - if a sentinel appears in
script text, hard error: *"a cuttle secret can only be typed, not evaluated."*
That closes the obvious workaround where an agent tries to read the value into JS.

### 7.3 Pre-flight target check (mandatory)

Before any substitution, one `Runtime.callFunctionOn` in the session's isolated
world returning the **shape** of `document.activeElement`, never its value:

```
{ ok, tag, type, disabled, readonly, maxLength, isEditable, hasSuggestedValue }
```

Refuse and answer a CDP error naming the reason when:

- there is no focused element (`insertText` would be a silent no-op, 5.2)
- the element is `disabled` (**the secret would land on whatever else was
  focused** - the sharpest failure in the whole feature)
- the element is `readonly` (`beforeinput`/`textInput` would still carry the full
  value to any page listener, 5.2)
- `maxLength` is shorter than the secret (it would be silently truncated)

This check has no analogue in any upstream project and is the single highest-value
piece of the design.

### 7.4 Post-type verification, derived only

After the last keystroke, one isolated-world probe reading
`document.activeElement.value.length` and `selectionStart`. **Never the value,
never a prefix.**

On mismatch: repair once (select-all, retype as keystrokes), re-probe, and on a
second mismatch answer a CDP error naming the discrepancy - *"typed 24 runes,
field holds 18"*. Fail open if the probe cannot run.

Also suppress `typoProb` (`humanize.go:61`) for a secret value: `emitTypo`
corrects with a blind Backspace, which is wrong on a segmented auto-advancing
field (per-box OTP, grouped licence keys) where the wrong character advances focus
and the Backspace lands in the next box.

**On the "fill blackout"** (1Password's pattern - the agent stops reading the page
for the duration of the fill): assessed and **deferred**. It defends a narrow
window - a driver snapshotting *concurrently with* the fill - while the leak that
actually cost a rotation in the corpus was a snapshot taken *later*, to debug a
failed login, with the value sitting in the field. cuttle cannot redact that
snapshot at all (6.2). The routing rule in 7.7 is the higher-value move. Revisit
if a session ever shows a concurrent-snapshot leak.

### 7.5 Capture

`cuttle secret capture NAME --selector '#api-key' [--to ...]`.

Daemon side: dial the seed's CDP (4.8), create a **fresh** isolated world (5.5),
`Runtime.callFunctionOn` with the selector as a structured argument (5.6),
`returnByValue: true`, read `el.value ?? el.textContent`.

Return to the CLI over the loopback HTTP surface behind `rejectUntrustedLoopback`.
The CLI pipes it to the sink and prints only
`NAME  40 bytes  from #api-key  ttl 15m`.

**Sources**, in build order:

1. `--selector` - as above. Limits to state honestly in the error: no shadow-DOM
   piercing (`document.querySelector` cannot cross a shadow root; `DOM.getDocument
   {pierce:true}` + `DOM.resolveNode` is the escape hatch if it turns out to be
   needed), and a cross-origin iframe is a separate target. Detect the
   password-manager suggested-value case (5.4) and say so rather than returning
   empty.
2. `--from-download [--latest] [--wait]` - needs `Browser.setDownloadBehavior
   {behavior:"default", eventsEnabled:true}` at launch (5.10) so completion is an
   event rather than a poll for the absence of `.crdownload`. Plus `--latest` and
   glob on `cuttle downloads`, which today needs an exact filename
   (`commands.go:783-793`).
3. `cuttle grab <url>` - in-profile authenticated fetch writing bytes host-side.
   Use `IO.resolveBlob` + `IO.read` for the blob case (5.9).
4. `--from-clipboard` - `Browser.setPermission` (**not** `grantPermissions`, 5.8)
   for `clipboard-read` on the top-level origin, then
   `navigator.clipboard.readText()` in the isolated world with `awaitPromise:true`.
   Focus is already handled by cuttle's existing focus-emulation pin.

**Sinks**, all CLI-side:

- `--to memory` (default) - the table, under the TTL.
- `--to file:<path>` - 0600, refuses a path inside a git working tree (3.5).
- `--to exec:'<cmd>'` - value on the command's **stdin**, never argv (6.5).

One invariant, both directions: **stdin only.**

### 7.6 Masking

A `slog.Handler` wrapping the existing one (`serve.go:38`), plus the CDP error
builders (`humanize.go:1333`).

Match against every value the store holds, expanded to its **URL-encoded,
JSON-escaped, HTML-escaped and base64 forms** (Skyvern's approach, 6.4), sorted
longest-first, single-pass alternation (browser-use's, 6.4), with
`MIN_SECRET_LENGTH = 4` and a higher floor for all-numeric values to prevent the
over-redaction that shredded output in the corpus.

**Scope honestly.** This covers text cuttle authors: its own log lines, `cuttle
status`, CDP error messages. It does **not** and cannot cover a driver's snapshot
(6.2). Say so in SKILL.md rather than implying coverage.

Also mask credential-shaped URL query params in cuttle's own log lines - the S08
leak was `remix_userkey=25039df9...` in a routine retry log.

### 7.7 Docs and routing

The briefing (`internal/cli/briefing.go:25-80`) prints the secret **names** the
daemon holds for the seed, so the agent sees the exact token without being told.
Names only, never values, never sources.

SKILL.md rule 6 is rewritten (10.1). The load-bearing additions:

- The sentinel, and that it works in **every driver and every call shape**.
- On a page with a filled credential field, `playwright-cli snapshot` prints the
  value in cleartext and `agent-browser`'s does not, because one reads injected
  `element.value` and the other reads the AX tree which Blink masks (6.2). This is
  a routing rule cuttle cannot enforce in code.
- Capture before you look: on a one-time-display credential, `snapshot` and
  `screenshot` are the leak, not the diagnostic.
- A typed secret is recoverable from the undo stack (5.2).

---

## 8. Phases

Each phase is independently reviewable and leaves the tree green.

### Phase 0 - close the `imeSetComposition` bypass

Standalone, ships first, valuable without the rest. Add
`Input.imeSetComposition` to the humanizer's switch (`humanize.go:146-149,
403-428`). At minimum log a warning naming the method; ideally route it through
the humanizer. Add a test asserting the method is not silently forwarded.

`fix(serve): humanizer no longer ignores Input.imeSetComposition`

### Phase 1 - the store and the sentinel

- `internal/serve/secrets.go`: the store, TTL, zeroing (6.7).
- `cdpSessionOpts` plumbing (`wsproxy.go:138-144`).
- `handleInsertText` substitution, fail-closed (7.2).
- Key-path and `Runtime.*` prefix detection (7.2).
- Pre-flight target check (7.3).
- Post-type derived verification and typo suppression (7.4).
- `PUT /profile/{seed}/secrets/{name}`, `GET`, `DELETE` behind
  `rejectUntrustedLoopback`.
- `cuttle secret set|ls|rm`, stdin only.

### Phase 2 - host-side resolution

- `--exec` stored in host config, resolved CLI-side, `PUT` with TTL.
- `cuttle secret prompt NAME` for the human-hands-in-an-OTP leg.
- `exec` hygiene: `CommandContext` + `WaitDelay`, explicit `Stdout`/`Stderr`,
  never `Output()` (6.7). Do not propagate the resolver's stderr (6.4).

### Phase 3 - masking

- Wrapping `slog.Handler`; encoding expansion; length guards (7.6).
- Credential-shaped URL params in cuttle's own log lines.

### Phase 4 - capture

In the order given in 7.5. Sources 1 and 2 are the feature; 3 and 4 should each
justify themselves against a real session that needed them.

### Phase 5 - make typing rare

`cuttle auth status [origin]`, `cuttle handoff <url> --until <predicate>`.
`handoff` raises the seed's window with `xdotool` (already in the image at
`Dockerfile:203`) and blocks until the page leaves the sign-in origin. Keep it
strictly print-and-wait: the moment it clicks anything, cuttle has become the
driver.

### Phase 6 - docs

SKILL.md rewrite within budget (10.1), OPERATING.md operator half, briefing line.

---

## 9. Tests and tripwires

The project's culture is tripwires that turn a silent regression into a diff
someone must consciously review (`internal/fingerprint/testdata/golden.json`,
`internal/cli/skill_test.go`). Match it.

**The load-bearing test:** using the `recordingHumanizer` harness
(`humanize_keyboard_test.go:15-27`), assert that **no injected CDP frame's JSON
ever contains the secret's bytes** for a full sentinel type. This is the one test
that would have caught every failure mode in the corpus.

Also:

| Test | Asserts |
|---|---|
| unknown sentinel | CDP error, **zero** injected frames |
| empty-value secret | treated as unknown, not as a name to type (Playwright's falsy bug, 6.1) |
| sentinel on the key path | CDP error naming `fill` |
| sentinel in `Runtime.evaluate` | CDP error |
| `imeSetComposition` | not silently forwarded |
| pre-flight on a disabled target | refused, nothing typed |
| pre-flight on a readonly target | refused |
| maxLength shorter than the secret | refused |
| post-type length mismatch | one repair, then a CDP error naming the discrepancy |
| typo suppression | `typoProb` never fires on a secret value |
| TTL expiry | entry gone and buffer zeroed; the timer closure does not pin the value |
| masking | expanded encodings caught; a 3-char value does **not** trigger redaction |
| `--to file:` inside a git worktree | refused, including the `.git`-is-a-file layout |
| `secret ls` | never emits a value under any flag |

Run `just check` (fmt-check + lint + test). `.golangci.yml` enables `gosec`,
`err113`, `wrapcheck`, `revive` with `enable-all-rules` - expect to write
sentinel errors rather than `fmt.Errorf` with a dynamic string.

---

## 10. Docs

### 10.1 SKILL.md is the binding constraint

`internal/cli/SKILL.md` is 13,585 bytes against a **16,384 budget**
(`skill_test.go:13`) - **2,799 bytes of headroom**, and the budget's own comment
says raising it is a deliberate decision, not an accident: *"cut something
first."*

`TestSkillGuideKeepsLoadBearingRules` pins the literal string
`"Secrets never reach"`, so rule 6's heading must survive or the test must be
updated in the same commit.

Rule 6 today (`SKILL.md:94-107`) spends most of its budget explaining the
`PLAYWRIGHT_MCP_SECRETS_FILE` dance, which this feature replaces. **Delete that and
the space is roughly break-even.** The replacement must carry: the sentinel, that
it works in every driver and call shape, the snapshot routing rule (6.2), capture
before you look, and the undo-stack note.

Operator material - TTL semantics, `--exec` wiring, sink configuration - goes to
`docs/OPERATING.md`, which has no budget. That split is the existing convention
and the reason SKILL.md is small.

### 10.2 Corrections to the research doc

`docs/2608-18-improvements-issues-research/README.md:1653` states that Chrome
under KasmVNC never writes the X CLIPBOARD selection. **Refuted** - the real cause
is headless mode (5.8, and independently confirmed by a container test on
`ghcr.io/glim-sh/cuttle:0.13.1`). Correct it.

### 10.3 Unrelated findings to file separately

- `-DisableBasicAuth` 401s all of `/api/*` (6.6), so rec C5's KasmVNC screenshot
  path is unreachable in the shipped image.
- Add a warning comment at `docker-entrypoint.sh:47` that KasmVNC's `DLP_Log`
  must never be raised to `verbose` - it logs full clipboard payloads and every
  keystroke (6.6).

---

## 11. Risks and open questions

1. **Sentinel format collision.** `{{cuttle:NAME}}` in page content is harmless
   (substitution only happens on the type path), but confirm no driver
   pre-processes braces. Single-quoting in shell is safe; zsh does not
   brace-expand a token with no comma or range.
2. **The `imeSetComposition` fix may be invasive.** Routing composition through
   the humanizer means handling partial commits and replacement ranges. If it
   grows past a phase, ship the warning plus the sentinel refusal and file the
   humanization half.
3. **Clipboard read from an isolated world is source-derived, not executed**
   (5.8). Verify empirically in Phase 4 before building on it: create an isolated
   world on an https page, grant, and compare against the main-world result.
4. **`Browser.downloadProgress.filePath` is not guaranteed** (5.10). Derive the
   path instead, as Playwright does.
5. **Per-origin scoping is not in this plan.** browser-use is the only project
   that scopes injection per domain, and 1Password/Bitwarden both do eTLD+1
   matching with an explicit "only this exact host" mode. cuttle's sentinel is
   unscoped: a secret can be typed into any page. That is a real gap. It is
   deferred rather than dismissed because the pre-flight check plus a per-secret
   optional `--origin` is a small follow-up, and shipping unscoped-but-fail-closed
   beats shipping nothing. **Raise it with the user before Phase 1 lands.**
6. **The undo stack retains a typed secret** (5.2) with no fix available.
7. **KISS pressure.** `set`, `set --exec`, `ls`, `rm`, `prompt`, `capture` with
   four sources and three sinks is a lot of surface for a repo whose
   non-negotiables say *"a second implementation earns an abstraction; one does
   not."* Phases 1-3 are the feature. Every source beyond `--selector` and every
   sink beyond `file:`/`exec:` should have to name the session that needed it.

---

## 12. Execution checklist

- [ ] Read sections 4, 5 and 6 in full before writing code
- [ ] Phase 0: `imeSetComposition` - `fix(serve):`
- [ ] Phase 1: store + sentinel + pre-flight + verify - `feat(serve):`
- [ ] Phase 2: host-side `--exec` + `prompt` - `feat(cli):`
- [ ] Phase 3: masking - `feat(serve):`
- [ ] Phase 4: capture, sources in order - `feat(cli):`
- [ ] Phase 5: `auth status` + `handoff` - `feat(cli):`
- [ ] Phase 6: SKILL.md within budget, OPERATING.md, briefing - `docs:`
- [ ] Correct `README.md:1653` (clipboard/headless) - `docs:`
- [ ] `just check` green at every phase boundary
- [ ] Raise the per-origin-scoping question (11.5) before Phase 1 lands

**Commit types matter for releases.** Read `docs/RELEASING.md` before picking one:
`feat:`/`fix:`/`perf:` cut a release, `refactor:`/`chore:`/`docs:`/`test:` do
nothing. And per the repo's non-negotiables, never start a PR body line with
`word(` unless the `)` closes on the same line - release-please's parser throws
and the release is skipped silently with CI green.

---

## 13. References

**In-repo:** `internal/serve/humanize.go`, `wsproxy.go`, `http.go`, `state.go`,
`pool.go`, `downloads.go`, `serve.go`, `logfile.go`; `internal/cli/commands.go`,
`briefing.go`, `SKILL.md`, `skill_test.go`; `internal/cdp/cdp.go`;
`ops/docker/bin/vnc-viewer.html`, `docker-entrypoint.sh`;
`docs/2608-18-improvements-issues-research/README.md` (issues A13, A17, A34;
clusters B20-B22; recs C8, C14, C24, C25; pattern H9.4).

**Upstream clones:** `~/pjv/microsoft/playwright` (`1b44f5a`),
`~/pjv/vercel-labs/agent-browser` (`548b159`),
`~/pjv/cloakhq/cloakbrowser-manager` (`9dd49cf`), `~/pjv/browser-use/browser-harness`,
`~/pjv/kasmtech/kasmvnc` (`v1.3.3`).

**Protocol:** `CDPROTO` =
`~/go/pkg/mod/github.com/chromedp/cdproto@v0.0.0-20260714215040-dc233986426f`.

**External:** [1Password for Claude security model](https://support.1password.com/1password-claude-security)
(memory-only session keys, the fill blackout, what the agent is ever shown);
[playwright-cli#317](https://github.com/microsoft/playwright-cli/issues/317)
(password values in the ARIA snapshot, closed unfixed);
[KasmVNC#268](https://github.com/kasmtech/KasmVNC/issues/268) (`-DisableBasicAuth`
semantics); [memguard#166](https://github.com/awnumar/memguard/issues/166) and
[#173](https://github.com/awnumar/memguard/issues/173) (why not to use it).
