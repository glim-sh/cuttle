# Daemon-owned secrets: injection, capture, and masking

Plan for `feat/secrets`. Written 2026-08-26 against `main` at `762e66f` (v0.13.1).
Revised the same day after two design reviews: a scope/AX review, then a
verification review (three agents against the code and upstream) that caught five
would-be blockers before any code was written. Section 13 records what both
reviews changed and why; sections 3, 8, 9 and 10 carry the resulting decisions.

Research backing this plan ran as three parallel agents on 2026-08-26: a CDP
protocol sweep (measured live against Chrome 152 on a throwaway profile), an
upstream-implementation sweep (playwright, agent-browser, CloakBrowser-Manager,
browser-use, Skyvern, 1Password), and a host-tooling sweep (`op`, `gh`, xsel,
KasmVNC, Go stdlib). Every claim below is either a file:line in this repo, a
file:line in a named upstream clone, or marked `[measured]` / `[unverified]`.

---

## 0. How to use this document

**Where this lives.** Branch `feat/secrets`, worktree
`/Users/tenequm/Projects/cuttle/.claude/worktrees/feat-secrets`. This file does
**not** exist on `main` - if you are in the main checkout you will not find it.
`git worktree list` to confirm, and do all work in the worktree.

You are implementing this from scratch with no prior context. Read sections 3-7
before writing code; they contain findings that invalidate the obvious design.
Section 9 is the build order. Section 14 is the checklist.

**Seven things will bite you if you skip ahead.** The first four were found by
reviewing the code, the last three by a verification review that killed the
obvious implementation of each:

1. `Input.imeSetComposition` is a live bypass of cuttle's existing humanizer, not
   just of the new secrets path (section 6.1). Closing it is a prerequisite.
2. `Input.insertText` returns success even when nothing was inserted, and on a
   `disabled` target it inserts into *whatever was focused instead* (section 6.2).
   Trusting the CDP reply means typing credentials into the wrong field and
   reporting success.
3. **cuttle already has a handoff verb.** `cuttle open` (`commands.go:712-774`)
   navigates, prints the briefing and launches the viewer, and SKILL.md documents
   it under "Human handoff: login walls and captchas". Do not add a `handoff`
   verb; add a wait flag to `open` (section 8.5).
4. The ownership boundary in section 4 is load-bearing. One verb in this plan
   deliberately crosses it, exactly once, with a stated reason.
5. **Never refuse a sentinel on raw frame bytes.** Playwright's own `fill` sends
   the value as a `Runtime.callFunctionOn` *argument* before the `Input.insertText`
   - a `bytes.Contains`-then-refuse breaks the primary flow on frame one. Scan
   script text only (section 8.2).
6. **`--exec` cannot resolve at substitution time.** There is no daemon-to-host
   callback; the daemon is in a container and the CLI is not resident. Resolution
   is at `set`/`refresh` time, and TOTP needs `refresh` in front of it
   (section 3.1).
7. **The interception must run outside the `--humanize` gate** (`wsproxy.go:239`),
   or `--humanize=false` types the sentinel literally into a live password field -
   the exact fail-open this plan exists to kill (section 3.3).

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

### 3.1 `--exec` resolution runs CLI-side, on the host - and only at set/refresh time

The daemon runs in a container for **every** backend, including local: `cuttle
serve` is the image entrypoint (`serve.go:148-150`, `Hidden: true`), launched by a
detached `docker run ... cuttle serve` (`internal/backend/local.go:378`), reached
over loopback HTTP; k8s/ssh are the same image behind a tunnel. There is no
host-native `serve`. So `op` and biometrics are on the host and the resolver
cannot be daemon-side. The sharper reason than "in a container" is the trust
boundary: the container has no host keychain, no biometrics, and under k8s/ssh is
not even on the same machine.

`cuttle secret set NAME --exec '...'` stores the *command* in `config.toml`
(3.2). **Resolution is one-shot, at `set`/`refresh` time - never at substitution
time.** This is a hard constraint, not a choice: substitution happens inside
`humanizer.handleInsertText` on the daemon's reader goroutine, in the container,
driven by a driver frame. **Nothing on the host is listening at that moment** -
the CLI is not resident (`runOpen`/`runDownloads` reach the daemon, do their work,
and exit; only the tunnel persists, not a process). There is no daemon->host
callback channel, and adding a resident host agent is a much larger design than
this feature. So:

- `cuttle secret set NAME --exec '...'` runs the command **once now**, stores the
  value in the daemon's TTL'd memory, and stores the *recipe* in `config.toml`.
- `cuttle secret refresh NAME` re-reads the recipe from `config.toml`, re-runs the
  command, and re-`PUT`s a fresh value under a fresh TTL. This is the verb an
  agent reaches for after a TTL expiry or before a use that needs a live value.
- **The unknown-sentinel CDP error names `cuttle secret refresh NAME`** whenever
  the name exists in config but has no live value (expired or never resolved),
  distinct from the "no such name" error. This is the one moment an agent must be
  told the value is stale, not absent.

The container never shells out and never learns the command.

**TOTP is therefore a refresh-immediately-before-use pattern, not a
resolve-at-type pattern.** `cuttle secret set GH_TOTP --exec 'op item get GitHub
--otp'` followed, right before the code is needed, by `cuttle secret refresh
GH_TOTP` then the sentinel type. A code resolved at `set` time and typed 30s later
is dead; the ladder's rung 1 (8.5) is honest only with `refresh` in front of it.
The default TTL is short enough (15 min) that a stale-value error is the common,
expected path for a TOTP, and the error tells the agent exactly what to run.

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

**Forward-compat break - acknowledge it, do not discover it.** `LoadFrom`
(`config.go:114-131`) calls `dec.DisallowUnknownFields()`, so a `config.toml`
carrying a new `[secret]`/`[secrets]` table makes **any older cuttle binary
hard-fail on load** - a total CLI error, not a skipped key - the exact breakage
the tolerated dead `Profiles` field (`config.go:46-48`) was added to prevent. And
an older binary's `Save` (which marshals a `Config` value) silently drops the
table. This bites a user who registers a secret with a new cuttle then falls back
to an older one, or runs two versions against a shared `$XDG_CONFIG_HOME`. The new
table is a **one-way version floor.** Handle it explicitly: model the table in the
`Config` struct in the same commit that first writes it, and add a release-note
line stating that a config with a registered `--exec` recipe will not load on a
pre-this-release cuttle. (Relaxing `LoadFrom` to tolerate-and-warn on unknown
top-level tables would only help *future* additions, not this one, so it is not
worth doing here.)

**Footgun to surface, not to design around:** a globally-registered secret is
offered to a seed in a shared cluster context exactly as it is locally. The
mitigation is the derived origin binding (3.5) plus per-seed TTL'd values. (Note
the briefing lists secret *names* only and cannot attribute a context - config is
global by decision, so context is not a property of an entry; see 8.8.)

### 3.3 Fail closed, and refuse a literal in a credential field

An unmatched sentinel is a hard CDP error; nothing is typed. **A sentinel that is
not the entire `params.text` - `{{cuttle:` appearing anywhere embedded, e.g.
`fill "Bearer {{cuttle:TOKEN}}"` - is also a hard CDP error** naming the
whole-string rule. Without that, an embedded sentinel matches nothing, falls
through, and the literal `Bearer {{cuttle:TOKEN}}` gets typed into the live field:
Playwright's fail-open bug (7.1) reintroduced.

**Scope of the literal refusal - stated narrowly and honestly.** The refusal
covers exactly one path and one field class: **a `fill` (`Input.insertText`) of a
literal into `<input type=password>` OR a field with `autocomplete="one-time-code"`
/ `inputmode=numeric`, with no sentinel**. The pre-flight probe (8.3) already
knows the element shape, so this is free, and it is the moment S08's whole lost
task would have been saved. Adding the OTP-field predicates is what makes the
headline `GH_TOTP` example (a TOTP field is `type=text`, never `type=password`)
actually covered.

**What the refusal does NOT cover, by decision** (do not imply otherwise in
SKILL.md or errors): a literal typed via `Input.dispatchKeyEvent` per-character
(`keyboard.type`), via `Input.imeSetComposition`, via a `Runtime.evaluate`
`.value`-setter, via `DOM.setAttributeValue`, via clipboard paste, or via
`Page.handleJavaScriptDialog`. Refusing across all of those means value-inspecting
every keystroke and every script frame - a different, far larger feature. The
refusal is a **high-value tripwire on the one path drivers overwhelmingly use for
credentials, not an airtight control.** Section 11.2 must state the residual paths
as a known gap so an agent is not told it is protected where it is not.

**The mechanism must sit outside the `--humanize` gate.** Interception today is
`if h.enabled && h.handleClientFrame(data)` (`wsproxy.go:239`), and
`--humanize=false` is a documented supported mode (`SKILL.md:91`). Left as-is,
turning humanize off types `{{cuttle:GH_PASS}}` literally into the password field -
the fail-open this whole plan condemns. **Either the secret path runs regardless of
`h.enabled`, or a daemon holding any registered secret refuses to start with
`--humanize=false`.** Pick the first (a `--humanize=false` session should still be
able to use secrets); a test with `enabled: false` asserting substitution still
happens is mandatory (section 10).

**Escape hatch - per connection, not a container restart.** The obvious mirror of
`--allow-context-creation` is wrong here: that is a `cuttle up` flag baked at
container start (`commands.go:380`, `warnBakedFlags:423`), so acting on the refusal
mid-login would require `cuttle down && cuttle up --allow-...`, dropping every
attached session and the half-finished login. Instead the override is
**per-connection and non-persistent**: a `cuttle secret allow-literal --once`
(next literal `fill` on this seed is permitted) or an equivalent CDP-visible
opt-in the driver sends. Per issue A45, **the error must lead with the actionable
token in the first 80 characters** - and that token must be the `--once` override
or the sentinel, never a destructive restart.

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

**Where the origin comes from.** Nothing in `internal/serve` tracks the current
page URL today (confirmed: `chromeInstance` carries seed/port/proxy/userDataDir,
no URL; `invalidateWorld` decodes `Page.frameNavigated` but reads only
`params.frame.parentId` and discards `frame.url`). Do **not** try to piggyback on
`frameNavigated`: it is gated on `h.enabled` and only arrives if the driver
enabled the `Page` domain, so it is not a sound authority for a security check.
Instead, **the mandatory pre-flight probe (8.3) returns `location.origin` as one
more field**, read in the isolated world at the exact moment the binding matters,
immune to both gates and to which domains the driver enabled. Caching a hint in
`invalidateWorld` is fine but must never be the authority.

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
answered over `Fetch.continueWithAuth` and never surfaced (`handleProxyAuth`, `wsproxy.go:621-657`).

**This is the model.** The feature is that pattern generalized from one hard-coded
credential to a named table.

### 5.5 cuttle already has a handoff verb

`cuttle open [url]` (`internal/cli/commands.go` - `newOpenCmd` :712-737, `runOpen`
:739-774, aliased at :720 from the pre-overhaul `login`/`connect`) navigates the
running session, prints the briefing, and launches the viewer. `SKILL.md:164-179`
documents it under "Human handoff: login walls and captchas" with a "Recognize the
wall early" rule.

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
- **Logging**: one `slog` handler (`serve.go:37`), teed to `/data/logs/serve.log`
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
`>= 4`) `[measured]` - which is also why **prefix detection on this path is not
worth building**: the largest observable fragment in one frame is a single `{`, so
detecting the sentinel would need a rolling per-session keystroke buffer and
erroring on a lone `{` would false-positive on every JSON or template string an
agent types. See 8.2 for the decision to leave the key path undetected.

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
cuttle's own comment at `humanize.go:1068-1072` says. Evaluate-succeeds is not
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
Confirmed empirically and by 1Password's own doc example (the secret-reference page
runs `op item get GitHub --format json` with no `--reveal` and shows the concealed
`password` value in cleartext); the CLI docs describe `--reveal` only as "Don't
conceal sensitive fields" and are silent on the format interaction, so treat this
as observed behaviour, not a documented guarantee. Never log or error-wrap that
output - this is how a real credential leaked during this plan's own research.

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
  irreversibly, and it inherits to `exec.Command` children (#166, open);
  `NewEnclave` deadlocks forever with no output when lockable memory is
  insufficient - `Panic` calls `Purge`, which re-locks the held mutex (#173, open,
  no maintainer reply, its PR still a draft); +213 KiB stripped.
- **No `runtime/secret`** (Go 1.26 experiment) - linux only, silent no-op on
  darwin, outside the Go 1 compatibility promise.
- **Honest ceiling:** wiping your own buffer stops mattering once the secret
  reaches stdlib crypto - `aes.NewCipher(key)` allocates reversible round keys in
  ordinary GC heap. For "keep it out of argv, transcripts and logs", `[]byte` +
  `clear` is the whole solution.
- **TTL cache:** mirror `pool.go`'s shape - `mu sync.Mutex` + `map[string]*entry`,
  `time.AfterFunc` (`pool.go:256` - the *only* existing `AfterFunc` in the package;
  `:141` and `:178` are the `idleTimers` field and its map init, not timers),
  cancelled under the lock (`cancelIdleLocked`, `:238`). Plain `Mutex`, not
  `sync.Map`, which gives no way to zero a value atomically with its removal.
  **The `AfterFunc` closure must capture the key only, never the buffer.**
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

**Plumbing gap to close:** `cdpSessionOpts` (`wsproxy.go:138-144`) carries no seed
key, but the store is keyed by seed (`state.go`-style `seed -> name -> entry`).
`serveWS` has the seed; thread it into `cdpSessionOpts` alongside the store, or the
humanizer cannot look up the right seed's secrets. State this - "add the store the
way `locale` arrives" (5.3) is not sufficient because `locale` is not seed-keyed.

**TTL vs. an in-flight type - copy before the first keystroke.** A secret type
takes up to `secretTypeBudget` of paced keystrokes; if the TTL `time.AfterFunc`
fires during that window, `clear(b)` zeroes the **same backing array** the typing
loop is ranging over - the tail types as NULs or the post-type check reports a
phantom mismatch. **The interception copies the value out under the store mutex
before the first keystroke and `clear`s its own copy in a `defer`.** The store's
own `clear` on TTL expiry then only ever races a copy nobody is reading. The
`AfterFunc` closure captures the key only, never the buffer (7.6).

`secretEntry.source` gains no new field for `refresh`; a `refresh` (3.1) re-runs
the stored recipe and overwrites `val`/resets the timer in place under the mutex.

### 8.2 Interception

**Prefilter.** `handleClientFrame` returns early unless
`bytes.Contains(data, []byte("Input."))` (`humanize.go:406`), and that prefilter is
load-bearing for perf: `preprocessClient` runs on every client->browser frame and
the prefilter exists to skip `decodeCDP` on the steady-state stream. **Do not widen
it to `"Runtime."`** - Playwright emits a `Runtime.callFunctionOn` for nearly every
action, so that would force a full JSON decode on almost every driver command.
Instead widen it to the sentinel bytes:

```go
if !bytes.Contains(data, []byte("Input.")) &&
   !bytes.Contains(data, []byte(sentinelPrefix /* "{{cuttle:" */)) {
    return false
}
```

The `Input.` arm is unchanged (it still needs *all* `Input.insertText` frames for
the pre-flight password refusal); the sentinel arm catches Runtime frames and any
future method cheaply. This whole path must also run **outside the `h.enabled`
gate** (3.3) - move the secret dispatch ahead of the `if h.enabled` check in
`preprocessClient`, or make `handleClientFrame`'s secret branch independent of it.

**`Input.insertText`** - the primary path.
1. Scan `params.text` for `{{cuttle:`. If present but not the whole string
   (embedded): **hard CDP error** naming the whole-string rule (3.3). If the whole
   string is exactly `{{cuttle:NAME}}`: it is a sentinel, go to step 3.
2. No sentinel, and the pre-flight probe says the target is a credential field
   (`type=password`, or `autocomplete="one-time-code"` / `inputmode=numeric`):
   **refuse** (3.3), leading with the `--once` override token in the first 80
   characters. If `allow-literal --once` is armed for this seed, consume it and
   forward.
3. Sentinel, unknown name: **hard CDP error listing the names that do exist** and
   the `cuttle secret set` command. Sentinel, name known-but-stale (in config, no
   live value): **distinct error naming `cuttle secret refresh NAME`** (3.1).
4. Sentinel, known and live: pre-flight (8.3), copy the value out under the store
   mutex (8.1), substitute, type through the existing humanized path, post-type
   check (8.4), record/compare origin (3.5).

   **Post-substitution `hasTypeable` decision.** After substitution the *value*
   may be all-CJK/emoji, for which `handleInsertText` currently `return false`s and
   forwards the **original** frame - which after substitution would type the raw
   sentinel or, worse, re-inject the value unhumanized. For a substituted secret,
   never fall back to forwarding the original: inject the value on one
   `Input.insertText` under the humanizer's own id and swallow the reply, exactly
   as the untypeable-tail path already does. State this so the implementer does not
   inherit the `return false`.

**`Input.dispatchKeyEvent`** - detection only, and it is a **documented gap, not a
guarantee.** `text` caps at 3 UTF-16 units (6.3), so a full sentinel can never fit
in one frame, and `keyboard.type` sends one character per frame - so detecting
`{{cuttle:` on this path would require a rolling per-session keystroke buffer the
humanizer does not have, and erroring on a lone `{` (the largest observable prefix)
false-positives on every JSON/template/code string an agent types. **Decision: do
not attempt key-path sentinel detection.** A sentinel typed character-by-character
simply will not match and will be typed literally - the same as any other literal
on the key path (3.3's uncovered set). Document it; do not ship an unimplementable
check. (If a future version wants it, it must specify the buffer's size, reset
rules, and per-session scoping - not in this plan.)

**`Input.imeSetComposition`** - NEW. Refuse a sentinel outright (whole-string or
embedded), and close the existing bypass (6.1) by routing composition through the
humanizer or, if that proves invasive, logging a loud warning naming the method.
The warning alone is acceptable for v1 provided the sentinel is refused.

**`Runtime.evaluate` / `Runtime.callFunctionOn`** - **scan only the script text**
(`expression` for `evaluate`, `functionDeclaration` for `callFunctionOn`). A
sentinel there is a hard error: *"a cuttle secret can only be typed, not
evaluated."* **`callFunctionOn`'s `arguments[].value` is passed through untouched**
- this is not optional: Playwright's own `fill` sends the fill value as a bare
`{ value }` argument in a `Runtime.callFunctionOn` frame *before* the
`Input.insertText` (`crExecutionContext.ts:59-72`, verified), so a naive
`bytes.Contains(frame, "{{cuttle:")`-then-refuse would hard-error the primary
supported flow on frame one. The sentinel arm's prefilter (above) will surface
these frames, so the handler **must** decode and check the script field
specifically, never the raw bytes. A test asserting a Playwright-shaped
`callFunctionOn` carrying a sentinel *argument* is forwarded (not refused) is
mandatory (section 10).

### 8.3 Pre-flight target check (mandatory)

One `Runtime.callFunctionOn` in the isolated world returning the **shape** of
`document.activeElement` plus the page origin, never any value:

```
{ ok, tag, type, disabled, readonly, maxLength, isEditable,
  hasSuggestedValue, autocomplete, inputmode, origin, nodeToken }
```

- `origin` is `location.origin`, feeding the derived origin binding (3.5).
- `nodeToken` is a fresh per-probe stamp written onto the element (a
  `WeakMap`-backed id, or a data-attribute cleared after) so 8.4 can prove the
  post-type element is *identically* the pre-flight element and not a
  focus-advanced sibling.
- If `document.activeElement` is a same-origin `<iframe>`, walk into its
  `contentDocument.activeElement` and report *that* shape (a login form in a
  same-origin iframe otherwise reports `tag: IFRAME`, dodging the whole check). A
  cross-document hop that throws is a **fail-open**, not a refusal.

Refuse, naming the reason, when: there is no focused element (silent no-op, 6.2);
the element is `disabled` (**the secret would land elsewhere**); it is `readonly`
(`beforeinput`/`textInput` would still carry the value to a page listener);
`maxLength` is shorter than the secret. This also drives the literal-in-credential
refusal (3.3).

No upstream project has this. It is the highest-value piece of the design.

**Timeout budget - the pre-flight must not blow the driver's action timeout.**
`insertTextBudget = 4500ms` (`humanize.go:173`) exists precisely to stay under the
driver's ~5s action timeout; a probe on top of it can push a single secret type
past 5s and get the whole thing retried into the field twice - the exact
double-type failure the budget was tuned to prevent (`humanize.go:166-179`). So:
**the total wall-clock for one secret type - world setup + pre-flight probe + type
+ post-type probe - must stay under 5s.** Concretely: world setup is `worldTimeout`
(500ms) and the probe is bounded well under `queryTimeout` (2s), which leaves the
type itself needing a **reduced budget for secrets** (set a `secretTypeBudget`
around 2500ms, or cap `insertTextMaxRunes` lower for secrets) so the sum stays
under 5s. There is **no repair retype** (8.4), which is what makes this fit.

**Probe-unavailable policy is explicit, for BOTH probes.** The isolated world can
be genuinely unavailable (`humanize.go:1120` already logs this downgrade as a real
case). When the pre-flight probe cannot run: **forward the frame unchanged and log
a warning** (fail-open) - refusing would break every `fill` on such a page, and
this path runs on every `Input.insertText`, not just secret ones. When a *sentinel*
is present and the pre-flight cannot run, that is the one case where fail-open is
unacceptable (the value would be typed unverified into an unknown field): **refuse
the sentinel** with an error saying the target could not be inspected. So:
non-sentinel + no probe -> forward; sentinel + no probe -> refuse.

### 8.4 Post-type verification, derived only - verify and report, never repair

After the last keystroke, one isolated-world probe reading
`document.activeElement.value.length`, `selectionStart`, and the `nodeToken` stamped
in 8.3. **Never the value, never a prefix.**

**Do not retype on mismatch.** The originally-planned "repair once (select-all,
retype)" is actively dangerous and is removed:

- On a **segmented OTP input** (the very case `typoProb` is suppressed for below)
  focus auto-advances per character, so `value.length` is 1 for a 6-digit code -
  a guaranteed false mismatch, and a select-all+retype lands in the last box.
- On **auto-submit on the final character** the page navigates, `activeElement`
  becomes `body`, and the retype fires a live credential into the post-submit page.

So the post-type step is **verify-and-report only**. Conditions, checked in order:
1. `nodeToken` does not match the pre-flight element, OR a navigation occurred
   since pre-flight (frame id / origin changed): the field moved under us. Do
   **not** retype; return success but note in the log that verification could not be
   confirmed because focus left the field (this is the OTP auto-advance / auto-submit
   case, and it is normal - not an error the agent must act on).
2. `nodeToken` matches and `value.length` != expected: a genuine short/truncated
   type. Answer a CDP error naming the discrepancy - *"typed 24 runes, field holds
   18"* - with no retype.
3. Match: success.

Fail open (success) if the post-type probe itself cannot run.

Suppress `typoProb` (`humanize.go:61`) for a secret: `emitTypo` corrects with a
blind Backspace, wrong on a segmented auto-advancing field where the wrong
character advances focus and the Backspace lands in the next box.

### 8.5 Strand C: fewer credential-handling events

**Not a new verb.** `cuttle open` is already the handoff verb (5.5). Add:

- **`cuttle open <url> --until <predicate> --timeout <dur>`** - blocks until the
  predicate holds, then returns. Strictly print-and-wait: **the moment it clicks
  anything, cuttle has become the driver** (section 4). Fully specified so an
  implementer does not invent the grammar:
  - **Predicate grammar - a small closed set, not open JS eval:** `url:<glob>`
    (matches `location.href`), `title:<substr>`, `gone:<glob>` (blocks *until* the
    URL no longer matches - the "left the sign-in origin" case), and `js:<expr>`
    as the escape hatch. Default when `--until` is omitted: `gone:` the launch
    URL's origin.
  - **Evaluation:** poll the predicate in the **isolated world** via the existing
    `query` path (never a main-world eval - that is the detection vector 5.2/6.5
    exist to avoid). `js:<expr>` is coerced to boolean.
  - **Poll interval 500ms; `--timeout` default 5m.**
  - **Exit contract:** exit 0 and print `signed in: <final url>` when the
    predicate holds; exit non-zero and print `timed out after <dur>; still at
    <url>` on timeout. This is the return signal S01's user had to ask for three
    times (5.5 gap 3).
- **Per-seed window raise** - capture the window id first, then act on it; the
  chained one-liner is broken (xdotool eats `windowactivate` as the search
  *pattern*, and commands default to `%1` not the whole stack - xdotool#221):

  ```sh
  wid=$(xdotool search --sync --onlyvisible --pid "$seed_chrome_pid" | head -1)
  xdotool windowactivate --sync "$wid" windowraise "$wid"
  ```

  xdotool is already in the image (`Dockerfile:203`). Put the seed name on the
  briefing's viewer line.
- **`cuttle auth status [origin]`** - which origins have stored cookies and how old
  the session cookie is. Removes a fixed 3-5 call tax per session (A26: 11 sessions
  re-running the risky login flow because nothing surfaces per-origin auth state;
  the user's own workflow docs now open with "ASSUME LOGGED OUT"). **Data source,
  specified - because the obvious one does not exist in session mode:**
  `rejectStateInSession` (`http.go:84-93`) closes the per-seed state API in session
  mode, and a session-mode daemon holds **zero** `stateStore` snapshots, so "when
  state was captured" has no answer there. Instead `auth status` **live-reads
  browser-global cookies** via the raw daemon-side CDP path - `getAllCookies`
  (`internal/cdp/cdp.go:61`, `Storage.getCookies` across contexts) - behind a new
  seedless loopback route (shaped like `/downloads`), guarded by
  `rejectUntrustedLoopback`. **State the honest limit in the output:** a cookie for
  an origin is not proof of a valid session (it can be expired server-side); the
  verb reports "has cookies for X, oldest expiry Y", never "logged in".
- **`cuttle secret prompt NAME`** - a human hands in a code without it entering the
  transcript.

**The retrieval ladder replaces "2FA is a handoff trigger".** The real rule is *a
second factor you cannot retrieve*:

1. **TOTP with a registered resolver** - `cuttle secret set GH_TOTP --exec 'op item
   get GitHub --otp'`, then `cuttle secret refresh GH_TOTP` immediately before the
   code is needed, then type the sentinel. The command runs on the host at
   set/refresh time (3.1) - **not** at substitution time, which is unbuildable
   (there is no daemon->host callback) - so the freshness comes from `refresh`, not
   from the type. Not an exception to the feature: it *is* the feature, with
   `refresh` in front of it.
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
surface behind `rejectUntrustedLoopback`.

**Which target - a seed can have several tabs.** "Dial the seed's CDP" is not a
page; with multiple tabs open there is no implicit "the page". Resolve the target
explicitly: default to the **active** page target (the one the human/viewer sees),
accept `--target <targetId>` to override, and error - not guess - when the active
target is ambiguous (e.g. only `chrome://` tabs). The same target-selection rule
applies to `--from-clipboard` (whose `Browser.setPermission` needs a concrete
top-level origin) and to `grab` (8.6b).

**Sources:**

1. `--selector` - limits stated honestly in the error: no shadow-DOM piercing
   (`DOM.getDocument {pierce:true}` + `DOM.resolveNode` is the escape hatch if
   needed), a cross-origin iframe is a separate target, and the password-manager
   suggested-value case (6.4) reports empty - detect and say so.
2. `--from-download [--latest] [--wait]` - needs
   `Browser.setDownloadBehavior {behavior:"default", eventsEnabled:true}` **at
   launch** (6.10, a `pool.go` launch-time change - see Phase 6 scope in section 9)
   so completion is an event, not a poll for the absence of `.crdownload`. The
   events land on the **browser-level** session, so the `--wait`/`--latest`
   consumer is the daemon, not a per-page session.
3. `--from-clipboard` - `Browser.setPermission` (**not** `grantPermissions`, 6.8)
   for `clipboard-read` on the resolved target's top-level origin, then
   `navigator.clipboard.readText()` in the isolated world with `awaitPromise:true`.
   Focus is already handled by cuttle's focus-emulation pin.

**Sinks:** `--to memory` (default, 3.4) is handled **entirely daemon-side** - the
route writes the store and returns only `{name, length}`, so the value never
leaves the daemon (this is what 3.4 promises; do not round-trip it to the CLI and
back). Only `--to file:<path>` (0600, refuses a git working tree, 3.6) and
`--to exec:'<cmd>'` (value on **stdin**, never argv, 7.5) return bytes to the CLI.
For those, the CLI pipes to the sink and prints only
`NAME  40 bytes  from #api-key  ttl 15m`.

One invariant for the bytes-returning sinks: **stdin only.**

**Also document the boundary-clean alternative** (section 4):
`playwright-cli eval 'el => el.value' e5 | cuttle secret set NAME --stdin`.

### 8.6b Capture the mechanism for `grab <url>` - and its CORS limit

`cuttle grab <url>` (Phase 5) fetches bytes from an authenticated context to the
host. The plan must name the mechanism, because the three candidates behave
completely differently and the obvious one fails on the dominant case:

- **Page-context `fetch(url, {credentials:'include'})` in an isolated world** is
  the tempting one and is **wrong for cross-origin**: an isolated world created via
  `Page.createIsolatedWorld` gets **no CORS exemption** (it is not an extension
  content script with host permissions), so `grab https://api.example.com/...`
  while the page is `app.example.com` returns a network error or an opaque,
  unreadable response - precisely the authenticated cross-service extraction A9
  says is the dominant real use. Use this path **only** when `url` is same-origin
  with the resolved target.
- **`Page.navigate` in a fresh scratch tab + `Network.getResponseBody`** works
  cross-origin, sends SameSite=Lax cookies from the browser-global jar, and is the
  default for a cross-origin `grab`. Caveats to state: it needs `Network.enable` on
  that tab's session, it carries no `Authorization` header (cookie-auth only), and
  a non-renderable content type becomes a download - route that case through the
  download path (`--from-download`) or `IO.resolveBlob` (6.9) for a page Blob.

**Decision:** `grab` picks by origin - same-origin as the resolved target ->
isolated-world `fetch`; cross-origin -> scratch-tab navigate + `getResponseBody`.
**State the CORS limit in the verb's own error text**, not just here, so an agent
that hits an opaque response is told why. Target resolution follows 8.6 (default
active target, `--target` override).

### 8.7 Masking

A `slog.Handler` wrapping the existing one (`serve.go:37`), plus the CDP error
builders (`humanize.go:1333`). Note the single `slog` handler tees to
`/data/logs/serve.log` only for durable session profiles, not pool mode
(`logfile.go:18-20`) - the wrap belongs at the handler, so it covers both.

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
daemon holds for the seed - names only, never values, never sources. This is
browser-use's insight (7.4): a substitution mechanism the model is never told
about does not get used.

Two corrections to the earlier wording: (1) **not** "which context they came
from" - config is deliberately global, not per-context (3.2), so context is not a
property of an entry; drop it. (2) `briefing.go` renders from a plain struct with
no daemon fetch, so this needs a new field on the briefing data plus a fetch from
the seedless secret-list route - name that as part of Phase 1's wiring, not a
free change.

---

## 9. Phases, and how they land

Each phase is independently reviewable and leaves the tree green.

Order is the **build order** - it matches the section 14 checklist exactly. The
numbering is sequential 0-8 (an earlier draft skipped 8; there is no gap now).

| # | Phase | Scope | Type |
|---|---|---|---|
| 0 | Close the `imeSetComposition` bypass (6.1). Standalone, valuable without the rest | serve | `fix(serve):` |
| 1 | Store (8.1, copy-before-type, seed-key plumbing), sentinel (8.2, prefilter widen, outside `--humanize` gate, callFunctionOn arg exemption, embedded-sentinel error), pre-flight incl. `location.origin` (8.3), derived verify (8.4, no repair), teaching errors, literal-in-credential refusal + `allow-literal --once` (3.3), seedless HTTP routes, briefing field (8.8), `secret set\|ls\|rm` (stdin only) | serve | `feat(serve):` |
| 2 | `--exec` at set-time, globally-persisted config + struct-modelled table (3.1, 3.2), `secret refresh`, exec hygiene (7.6) | cli | `feat(cli):` |
| 3 | Masking (8.7) | serve | `feat(serve):` |
| 4 | Strand C: `open --until` (grammar in 8.5), per-seed window raise, `auth status` incl. its seedless live-cookie route (8.5), `secret prompt` | cli+serve | `feat(cli):` |
| 5 | `cuttle grab <url>` - origin-split fetch (8.6b), `IO.resolveBlob` for the blob case (6.9) | cli+serve | `feat(cli):` |
| 6 | Capture `--selector`; **`setDownloadBehavior{default,eventsEnabled}` at launch in `pool.go` (serve-side)** + `downloads --latest/--wait` | cli+serve | `feat(cli):` |
| 7 | `--from-clipboard` (6.8 is verified; build on it) | cli+serve | `feat(cli):` |
| 8 | Container pass: correct README.md:1653, record the `-DisableBasicAuth` finding, add the `DLP_Log` comment (11.3) | docs/image | `fix(skill):` or drop the changelog line - see below |

**Why this order.** Phase 2 (`--exec`) comes **before** Phase 4 (strand C)
deliberately: Phase 4's SKILL.md ships the retrieval ladder (8.5), whose rung 1 is
`--exec`/`refresh` - documenting a flag before it exists is exactly the A7 trap the
"no docs phase" rule exists to avoid. This corrects an earlier draft that put
strand C second on a "cheapest build, largest behavioural change" argument; that
argument stands for its *value*, but the `--exec`-before-ladder dependency fixes
its *position*. Phase 5 (`grab`) is its own phase rather than a capture source
because issue A9 makes authenticated extraction the **dominant real use of cuttle**
- 15 sessions, three of which sent zero input events, the identical
`fetch`-refresh-token incantation in six - and it has no verb today.

**There is no separate docs phase.** `internal/cli/SKILL.md` is `//go:embed`'ed, so
it is shipped behaviour. A trailing docs phase would mean either documenting a verb
that does not exist yet or shipping one undocumented, and issue A7 is precisely
about SKILL.md carrying claims the daemon does not honour. **Each commit carries its
own SKILL.md change**, and the first one to touch SKILL.md also makes the four cuts
from 11.1 that free the budget. Note per `docs/RELEASING.md:62-65` a SKILL.md change
is typed `feat(skill):`/`fix(skill):`, **never `docs:`** - so a commit whose only
change is SKILL.md text still uses the skill scope.

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

**Do NOT add a `docs:` nested block for the container pass (Phase 8).** This repo's
`.github/release-please-config.json` sets no `changelog-sections`, so
release-please's defaults apply and **`docs` is a hidden type** - a `docs:` nested
block renders **nothing** in the CHANGELOG (verified: `DEFAULT_CHANGELOG_SECTIONS`
hides `docs`/`chore`/`style`/`refactor`/`test`/`build`/`ci`; visible are
`feat`/`fix`/`perf`/`revert`), and `docs/RELEASING.md:20,50-57` says the same and
forbids un-hiding types to work around it. If Phase 8 must appear in the changelog,
type its commit `fix(skill):` (it touches SKILL.md/image behaviour anyway) and give
it a `fix`-typed nested block; otherwise let it ride silently with the primary
commit. Nested blocks change the CHANGELOG, not the version arithmetic: pre-1.0
both `feat:` and `fix:` bump patch, so this is still one bump.

Text outside the blocks stays with the primary commit (verified: `splitMessages`
concatenates post-`END_NESTED_COMMIT` text back onto `messages[0]`).

**Two traps in the PR body, and they apply inside the nested blocks too.** A body
line whose **first token is immediately followed by `(`** throws release-please's
parser unless the parens close as a *simple single-level scope on that same line*.
Both an unclosed `word(` (closing on a later line) **and** a same-line nested
`TABLE(FN('x'))` throw (release-please#2564, open; upstream parser fix
PR #2790 closed unmerged) - the earlier "closes on a later line" wording was too
narrow. `word(x)` on one line parses fine; the same text mid-line is fine. On a
throw the commit is dropped and the run exits 0 - not literally silent (a
`commit could not be parsed:` line appears at debug level in the action log) but
invisible in CI's normal output, so the release just does not happen. Markdown is
not a safe zone; a fenced code block is parsed the same way. And the PR title, not
any commit title, is the subject. Read `docs/RELEASING.md` before opening it.

**Recovery if the release is skipped anyway:** edit the merged PR body, append a
`BEGIN_COMMIT_OVERRIDE` block with the subject you want, and re-run the `release`
job. release-please re-reads the merged body live and parses only that block.

---

## 10. Tests and tripwires

The project's culture is tripwires that turn a silent regression into a diff
someone must consciously review (`internal/fingerprint/testdata/golden.json`,
`internal/cli/skill_test.go`). Match it.

**The load-bearing test - worded to be satisfiable.** The earlier wording ("no
injected CDP frame's JSON ever contains the secret's bytes") is **false by
construction**: the secret must physically reach Chrome, so the browser-bound
`dispatchKeyEvent`/`insertText` frames necessarily carry its characters. The
correct, buildable invariant, for a full sentinel type:

- **No frame the driver sees carries the value.** The only client-bound frame is
  one synthesized `ok`/`error` response under the driver's own id (injected-frame
  replies are swallowed - `wsproxy.go:325-328`/`swallowInjected`). Assert nothing
  written to `clientSend` contains the value or the sentinel.
- **No CDP error message and no log line carries the value** (only lengths/counts).
- **No injected frame carries the un-substituted sentinel** `{{cuttle:NAME}}` -
  proving substitution happened before dispatch, not after.

**Harness prerequisite (Phase 1 work, name it):** `recordingHumanizer`
(`humanize_keyboard_test.go:14-28`) has `clientSend` and `waiters` **nil**, so it
cannot observe the client-bound side and the pre-flight probe would nil-map-panic
on `h.waiters[id] = ch` / time out with no responder. Extend it with a recording
`clientSend` and a scripted `waiters` responder before Phase 1 depends on it.

| Test | Asserts |
|---|---|
| unknown sentinel | CDP error, **zero** injected frames, error lists existing names |
| known-but-stale sentinel (expired) | CDP error naming `cuttle secret refresh NAME`, zero injected frames |
| empty-value secret | treated as unknown, not as a name to type (Playwright's falsy bug, 7.1) |
| embedded sentinel (`"Bearer {{cuttle:X}}"`) | hard CDP error naming the whole-string rule; nothing typed |
| literal into `type=password` / OTP field | refused; `--once` override token inside first 80 chars |
| `allow-literal --once` armed | next literal `fill` forwarded, then re-armed off |
| `--humanize=false` + registered secret | substitution STILL happens (path outside the gate) |
| Playwright-shaped `callFunctionOn` with sentinel **argument** | forwarded, NOT refused |
| sentinel in `Runtime.evaluate`/`callFunctionOn` script text | CDP error |
| `imeSetComposition` sentinel | refused; bypass not silently forwarded |
| pre-flight: disabled / readonly / short maxLength | refused, nothing typed |
| pre-flight unavailable + sentinel | refused; + non-sentinel | forwarded (fail-open) |
| post-type length mismatch, same element | CDP error naming the discrepancy, **no retype** |
| post-type focus left field (OTP auto-advance / auto-submit) | success, no retype, logged |
| post-substitution all-CJK value | injected as one `insertText`, original never forwarded |
| typo suppression | `typoProb` never fires on a secret value |
| origin mismatch | warns, does not block; origin recorded on first success; probe returns `location.origin` |
| TTL expiry mid-type | in-flight copy completes; store buffer zeroed; timer closure does not pin the value |
| masking | expanded encodings caught; a 3-char value does **not** trigger redaction |
| `--to file:` inside a git worktree | refused, including the `.git`-is-a-file layout |
| `--to memory` | value never returned to the CLI; route returns `{name,length}` only |
| `secret ls` | never emits a value under any flag |
| config with `[secret]` table on older binary | `LoadFrom` hard-fails (documented floor, 3.2) |

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
| Rule 6's `PLAYWRIGHT_MCP_SECRETS_FILE` paragraph (`:99-103`) | the feature replaces it |

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
   retrieve*, not "2FA". Rung 1 (TOTP) needs `cuttle secret refresh` immediately
   before use (3.1); a set-time TOTP is stale.
6. **What the sentinel does NOT cover (the honest gap).** The literal-in-credential
   refusal (3.3) is a tripwire on the `fill` path only. A literal typed via
   `keyboard.type` (per-character keys), via a `Runtime.evaluate` `.value`-setter,
   via `imeSetComposition`, or pasted, is **not** refused - so the rule for an agent
   is still "use the sentinel", not "cuttle will stop me if I don't". Say this
   plainly; do not imply blanket protection.

### 11.3 Corrections and the container hardening pass

All three are **in scope for this branch** (Phase 8), not separate filings. They
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
5. **The literal-in-credential refusal is a behaviour change for existing users.**
   Anyone typing a throwaway literal into a password or OTP field starts getting an
   error. The `allow-literal --once` override covers it, but the release note must
   say so plainly. It also covers only the `fill` path (3.3) - not a full control.
6. **Timing budget.** Pre-flight + type + post-type must stay under the driver's
   ~5s action timeout (8.3); the no-repair decision (8.4) and a reduced
   `secretTypeBudget` are what buy the headroom. If a future change re-adds a repair
   or a second probe, re-audit the total against 5s.
7. **Config version floor.** A `config.toml` with the new `[secret]` table will not
   load on a pre-this-release cuttle (`DisallowUnknownFields`, 3.2). Accepted and
   release-noted; not silently introduced.
8. **Same-origin iframe logins** need the pre-flight to walk `contentDocument`
   (8.3); a login widget in a same-origin iframe otherwise reports `tag: IFRAME`
   and dodges the refusal. Cross-origin iframes are separate targets and are fine.

---

## 13. What the two design reviews changed

Recorded so the reasoning is not re-derived. The first review is the top block;
the **second review (2026-08-26, three verification agents against code and
upstream)** is the block below it - it fixed five would-be blockers and a set of
factual errors before any code was written.

**Second review - blockers and corrections:**

| Was | Now | Why |
|---|---|---|
| `--exec` resolves at substitution time ("fresh code every use") | resolves at `set`/`refresh` time; new `secret refresh` verb; stale-value error names it (3.1) | **unbuildable as written**: substitution is on the daemon's in-container goroutine and there is no daemon->host callback; a set-time TOTP is dead in 30s |
| refuse a sentinel in any `Runtime.callFunctionOn` frame | scan only `expression`/`functionDeclaration`; pass `arguments[].value` through (8.2) | Playwright's own `fill` sends the value as a `callFunctionOn` argument *before* `insertText` - a raw-bytes refusal breaks the primary flow on frame one |
| interception lives inside the `if h.enabled` humanize gate | runs outside it; `--humanize=false` + secret still substitutes (3.3, 8.2) | otherwise `--humanize=false` (a documented mode) types the sentinel literally - the exact fail-open the plan condemns |
| pre-flight + type + post-type + repair retype | budget capped under 5s; **no repair retype** (8.3, 8.4) | the stack was ~16s vs a 5s driver timeout -> double-type; and the repair retypes into OTP-advanced/post-submit fields |
| load-bearing test: "no injected frame contains the secret's bytes" | "no client-bound frame / error / log carries it; no injected frame carries the un-substituted sentinel" + harness extension (10) | the old wording is false by construction (the secret must reach Chrome) and the named harness `waiters`/`clientSend` are nil |
| `Runtime.*` detection via `handleClientFrame` | prefilter widened to the sentinel bytes, not `"Runtime."` (8.2) | the `"Input."` prefilter drops Runtime frames today; widening to `"Runtime."` forces a JSON decode on nearly every driver command |
| literal refusal "into `type=password`" (implied broad) | narrowed + OTP fields added, residual paths documented as a gap (3.3, 11.2) | TOTP/API-key fields are not `type=password`; `keyboard.type`/`evaluate`/paste dodge it - claiming broad protection is dishonest |
| embedded sentinel unspecified | `{{cuttle:` not the whole string is a hard error (3.3, 8.2) | otherwise `"Bearer {{cuttle:X}}"` types the literal - Playwright's fail-open reintroduced |
| `auth status` data source assumed | specified: live browser-global cookie read via raw CDP behind a seedless route; honest "has cookies" wording (8.5) | session mode holds zero `stateStore` snapshots (`rejectStateInSession`) |
| `grab`/`open --until`/target selection undefined | `grab` origin-split with CORS limit (8.6b); `--until` closed grammar (8.5); explicit target resolution (8.6) | each would otherwise be invented by the implementer and documented as the invention |
| TTL timer could zero an in-flight buffer | copy value out under the mutex before the first keystroke (8.1) | `clear(b)` on TTL expiry races the typing loop's backing array |
| phase table order != checklist; missing Phase 8; download-launch change unowned | table = checklist = build order; sequential 0-8; launch change in Phase 6 scope (9, 14) | `--exec` (P2) must precede the ladder's SKILL.md (P4) or A7 recurs |
| optional `docs:` nested-commit block for the container pass | dropped - `docs` is hidden in this repo's changelog; use `fix(skill):` or no line (9) | verified: no `changelog-sections` override, so release-please hides `docs` |
| config forward-compat unmentioned | acknowledged one-way version floor + release note (3.2) | `DisallowUnknownFields` hard-fails an older binary on the new table |
| `word(` trap = "closes on a later line" | wider: any first-token-then-`(` not closing as a simple same-line scope (9) | release-please#2564: same-line nested parens `TABLE(FN('x'))` also throw |
| xdotool chained one-liner | capture the wid first, then act on it (8.5) | the chained form eats `windowactivate` as the search pattern (xdotool#221) |

**First review:**

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
| `-DisableBasicAuth` and `DLP_Log` filed separately | in scope as Phase 8 | both were found researching this feature and touch the surface it touches |
| a trailing docs phase | each PR carries its own SKILL.md change | SKILL.md is `//go:embed`'ed shipped behaviour; a trailing phase means shipping a verb undocumented or documenting one that does not exist - issue A7 exactly |
| implicitly one PR | briefly five, then **one PR** by requirement (9) | one PR is a fixed constraint. The objection that Phase 0's `fix:` would vanish under a `feat:` title is answered by `BEGIN_NESTED_COMMIT`, which release-please parses as an independent commit while the outer subject keeps its own entry - so one PR still yields both changelog lines. Reviewability is handled by keeping the phases as distinct commits on the branch |

---

## 14. Execution checklist

**One PR** off `feat/secrets` (9). Phases are commits on the branch; keep them
distinct and do not rebase them together. `just check` green at every commit.

Commit order below **is** the build order and matches the section 9 table.

- [ ] Read sections 3, 4, 5, 6 and 7 in full before writing code
- [ ] Commit: Phase 0 - `fix(serve):` close the `imeSetComposition` bypass
- [ ] Commit: Phase 1 - `feat(serve):` store (copy-before-type, seed-key
      plumbing), sentinel (prefilter widen to `{{cuttle:`, run outside the
      `--humanize` gate, `callFunctionOn` arg exemption, embedded-sentinel error),
      pre-flight incl. `location.origin`, verify-only (no repair), refusals +
      `allow-literal --once`, seedless HTTP routes, briefing field,
      `secret set|ls|rm`, plus the `recordingHumanizer` harness extension (10)
- [ ] Commit: Phase 2 - `feat(cli):` `--exec` at set-time + `secret refresh`,
      config table modelled in the `Config` struct, exec hygiene
- [ ] Commit: Phase 3 - `feat(serve):` masking
- [ ] Commit: Phase 4 - `feat(cli):` `open --until` (grammar 8.5), window raise
      (capture wid first), `auth status` + its live-cookie route, `secret prompt`
- [ ] Commit: Phase 5 - `feat(cli):` `cuttle grab` (origin-split fetch, 8.6b)
- [ ] Commit: Phase 6 - `feat(cli):` capture `--selector` + `setDownloadBehavior`
      at launch (`pool.go`, serve-side) + downloads `--latest/--wait`
- [ ] Commit: Phase 7 - `feat(cli):` `--from-clipboard`
- [ ] Commit: Phase 8 - `fix(skill):` container pass (README.md:1653,
      `-DisableBasicAuth` finding, `DLP_Log` comment)
- [ ] Each commit carries its own SKILL.md change - no separate docs phase (9); a
      SKILL.md-only change is `feat(skill):`/`fix(skill):`, never `docs:`. The
      first commit to touch SKILL.md also makes the four budget cuts (11.1)
- [ ] PR title: `feat(serve): daemon-owned secret injection, capture and masking`
- [ ] PR body ends with the `fix:`-typed `BEGIN_NESTED_COMMIT` block for Phase 0
      (9). Do NOT add a `docs:` nested block - `docs` is a hidden changelog type
      in this repo (9)
- [ ] Check every PR body line: none may have a first token immediately followed
      by `(` unless the parens close as a simple single-level scope on that same
      line (both unclosed AND same-line nested parens throw), code fences included
- [ ] Release note (a) literal-in-credential refusal (12.5) - behaviour change for
      anyone typing a throwaway literal into a password/OTP field; (b) config with
      a registered `--exec` recipe will not load on a pre-this-release cuttle (3.2)

**Already settled, do not re-verify:** the isolated-world clipboard read (6.8) and
the `-DisableBasicAuth` 401 (11.3) were both measured against the running
container on 2026-08-26. release-please nested-commit parsing, the `docs`-hidden
default, the `word(` parser trap's real grammar, and the xsel/xdotool/op/CDP/Go
facts in sections 6-7 were verified against upstream source on 2026-08-26.

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
