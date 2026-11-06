# Shared tabs per browser, and seeds that reuse pool mode

> Status: shipped. 0.17.11 (#170): shared tabs and masked driver output. 0.17.12 (#172):
> seeds. 0.17.13 (#173): a host Ctrl-C or disconnect stops the remote driver, so the CLI and
> image upgrade together. 0.17.14 (#176): screenshots of background tabs. The display stays
> shared (5.6 not taken). The rest is the working record as of 2026-11-05.

Plan for branch `docs/plan-shared-tabs-and-seeds`, written 2026-11-05 against `main` at
`c3c0750`.

- **Supersedes:** the first draft (6c2efb2), which rebuilt pool mode inside session mode.
  This revision follows two design reviews and a risk check (section 12).
- **Scope:** only what is needed for this to work and be safe. Everything else is in
  section 11.
- **Versions:** release-please derives them from commit types, so none are promised here.

Every code claim is a `path:line` read at `c3c0750`; paths under `packages/cuttle/` are
written from `internal/`. The other kinds of source:

- a linked upstream file;
- the bundled `playwright-core` 1.65.0-alpha-2026-10-30 (`coreBundle.js`). This is what the
  image's `@playwright/cli` 0.1.23 depends on: `packages/browser/versions.env:25`, and that
  package's `package.json`.

Anything not checked is marked `[unverified]`.

## 0. Summary

**Phase 1 - shared tabs and the CLI leak.**

- Inside one browser, every agent and the person share every tab.
- A new agent session starts in a fresh tab of its own.
- After a pause, an agent takes a new snapshot before acting, because someone may have
  closed or switched its tab.
- Everything `cuttle pw` and `cuttle jev-browse` print is masked, and still streams live.
- An image too old to mask withholds the output and says how to fix it.

**Phase 2 - seeds.**

- A principal line with `seed=<name>` gives that token its own browser in the same
  instance. That browser has its own Chrome, profile, cookies, fingerprint, lease,
  secrets and downloads.
- It starts on first use and stops when idle. Its cookies are kept.
- A line without `seed=` stays on today's browser, now called `main`, with its data in
  place.
- Trusted peers reach each seed's raw CDP at `/fingerprint/<seed>/`.
- The CLI gains `--seed`.
- All seeds share one display and the instance's proxy, timezone, locale and screen.

**Build DAG** (models in brackets; no estimates; section 6 has owners and gates):

```
 t=0, all in parallel                                             feeds
   A1  aggregator: delete ownership, fresh tab, retry, no-webmcp [astra high] -> P1R, E2b
   A2  cuttle tools: evalPage capture/pre-check/wait, hold, dl   [astra high] -> P1R, E2b
   B1  daemon: streaming /mask, shared pair helper, GET / fields [astra med]  -> P1R
   B2  CLI: mask-exec, every exec path, fail-closed hints        [astra med]  -> P1R, G
   W   docs: phase 1 docs now, phase 2 docs drafted from 5.x     [opus]       -> P1R, INT
   E1  principals seed=, capabilities carry seed, revoke         [astra high] -> E2b, INT
   E2a per-seed Playwright MCP child table (new file)            [astra high] -> E2b, INT
   P1  pool: allowlist, reaper + busy, supervision, fingerprints [astra high] -> INT
   P2  CDP routes per seed, rewrites, accounting key             [astra med]  -> INT
   P3  handover: keyboard follows X focus, no focus steal, raise [astra high] -> INT
 as soon as their inputs land
   E2b aggregator per seed          needs A1, A2, E1, E2a        [astra med]  -> INT
   G   CLI --seed                   needs B2                     [astra med]  -> INT
 rolling, from the first lane that lands
   R   one read-only review per landed lane, fixes back on its branch [astra med]
   P1R phase 1 PR = A1+A2+B1+B2+W: polish, merge, release, deploy, live check
       (off the critical path: nothing in phase 2 waits for it)
   INT integration branch [opus]: merges each phase 2 lane the moment it lands, wires
       serve.go; then a cross-lane polish IN PARALLEL WITH the image smoke test, fixes,
       re-smoke, merge after P1R, release, deploy, live check

 critical path: max(A1, A2, E1, E2a) -> E2b -> INT tail (polish || smoke -> release -> deploy)
```

**Decided** (owner, 2026-11-05 20:40): one display for all seeds (O1); the tabs cuttle
opens are not closed by it (O2).

## 1. Decisions (owner, 2026-11-05)

- **D1 - two separate instances.** One is owner-only; the other is shared with unattended
  agents. The instance is the trust boundary: every trusted network peer of an instance
  reaches every seed in it. There is no per-seed authorization.
- **D2 - seeds reuse pool mode's per-seed machinery.** Seed `main` is today's reserved
  seed: always on, self-relaunching, supervised, data in place.
- **D3 - principal lines bind seeds.** The format is `<name> sha256:<hex> [seed=<name>]`,
  defaulting to `main`. Session mode accepts only seeds that some principal is bound to.
- **D4 - shared tabs.** MCP tab ownership is deleted, and nothing parses a tab list.
- **D5 - no browser contexts,** and no several profiles in one Chrome.
- **D6 - one Playwright MCP child per running seed.**
- **D7 - non-main seeds start on first use** and are reaped when idle by pool mode's
  reaper. Session mode enables the reaper for non-main seeds only.
- **D8 - one display for all seeds,** with two handover fixes (decided, O1). A display per
  seed (5.6) is not built now.
- **D9 - no per-seed settings.** Every seed inherits the instance's settings.

## 2. Facts this relies on

**Pool and session mode**

- Session mode is the pool restricted to the reserved seed (`internal/serve/serve.go:79-85`).
- `seedKeyFor` refuses any seed in session mode (`internal/serve/pool.go:365-371`). Every
  loopback route resolves `?fingerprint=` through it (`internal/serve/secrets.go:387-394`).
- State already kept per seed:
  - processes, locks, refcounts and timers (`internal/serve/pool.go:146-153`);
  - the profile dir (`:815-839`);
  - leases (`internal/serve/lease.go:47-56`);
  - secrets, in memory only (`internal/serve/secrets.go:19-23`).
- The reaper (`internal/serve/pool.go:281-344`):
  - refuses while a CDP client is connected (`:309`);
  - captures supervised seeds, terminates Chrome and drops secrets (`:322-340`).
- Session mode ignores `--idle-timeout` (`internal/serve/serve.go:278-281`).
- Supervision and relaunch:
  - `supervised` is true for the reserved seed, and for PUT-seeded seeds under
    `--keep-profile` (`internal/serve/supervise.go:76-78`). It is read under `p.mu`
    (`:340-349`).
  - Only `main` self-relaunches (`internal/serve/pool.go:640-664`).
- The fork hashes `--fingerprint` as an opaque string
  (`packages/browser/patches/000-shared/cuttle_seed.cc:38-46`, `:115-122`).

**CDP routes**

- Per-seed WebSocket route: `GET /fingerprint/{seed}/devtools/{path...}`
  (`internal/serve/http.go:86`).
  - Its handler hands connection accounting the public name
    (`internal/serve/wsproxy.go:98`).
  - The default route normalizes on purpose (`:111-121`).
- `devtoolsFrontendUrl` hard-codes `/devtools/` (`internal/serve/http.go:376-379`).
- The remote listener:
  - its raw CDP is `/json`, `/json/` and `/devtools/` (`internal/serve/remote.go:399-404`);
  - those routes go through `trustedPeerOnly`, which refuses page requests and drops the
    query (`:494-518`).
- In remote mode every internal route refuses browser requests
  (`internal/serve/serve.go:352`).

**Aggregator**

- The MCP server is stateless (`internal/serve/mcp.go:286-290`), per the protocol decision
  (`docs/plans/2611-04-remote-mcp-session.md:378-379`).
- Per principal:
  - upstream state (`internal/serve/mcp.go:344-364`);
  - the lease token (`:713-737`);
  - `callMu` (`:213`).
- One child, started without `--no-webmcp` (`internal/serve/mcp.go:1286-1289`).
- Every non-hidden upstream tool reaches the agent and is re-listed on change
  (`internal/serve/mcp.go:421-433`, `:464-480`).

**Pinned Playwright bundle** (`coreBundle.js` lines)

- Tabs and sessions:
  - A new session lists the pages in order, and the first becomes current
    (`:67710-67716`, `:67804-67806`). That is usually the person's tab.
  - Closing the current tab makes a neighbour current (`:67717-67724`).
  - `selectTab` calls `bringToFront` (`:67572-67580`).
- Page tools:
  - A page's WebMCP tools become session tools (`:68313-68315`, `:68345-68351`).
  - `--no-webmcp` stops that (option `:76382`, config `:75259-75260`, check
    `:67366-67368`).
- Downloads and files:
  - Every session saves every download under the same name (`:67040-67042`,
    `:67111-67122`, `:67607-67609`).
  - Paths an agent passes must stay inside the output dir or the workspace
    (`:67472-67483`).
- New pages:
  - Playwright creates them with `Target.createTarget` and no `focus`
    ([crBrowser.ts](https://github.com/microsoft/playwright/blob/main/packages/playwright-core/src/server/chromium/crBrowser.ts)).
  - The protocol default then brings the window forward; `focus: false` does not
    ([Target.pdl:167-181](https://github.com/ChromeDevTools/devtools-protocol/blob/b0d96c6cdf39e5583e67a3762b2815a33154455b/pdl/domains/Target.pdl),
    experimental).

**CLI**

- Every driver exec runs through `execPlaywright` (`internal/cli/playwright.go:471-482`).
  `jev-browse` also starts a persistent client (`internal/cli/jevbrowse.go:423`).
- Nothing on those paths is masked: `replayHost` (`internal/cli/playwright.go:967-981`),
  `replay` (`:1051-1054`), and the raw stderr on retry (`:330`).
- Only the snapshot file is masked (`internal/serve/downloads.go:306-341`).
- Loopback routes admit any in-container process (`internal/serve/http.go:471-483`).
- A download counts as finished by its name alone (`internal/serve/downloads.go:106-109`).

**Display**

- One display stack: Xvfb `:99`, openbox and Selkies, all owned by the entrypoint
  (`ops/docker/bin/docker-entrypoint.sh:89-161`).
- Every Chrome inherits `DISPLAY` (`ops/docker/Dockerfile:452`).
- The window raise finds a window by its browser PID (`internal/serve/auth.go:209-263`).
- The phone keyboard's focus stream samples the request's seed
  (`internal/serve/viewerfocus.go:26-33`), and the viewer sends none
  (`ops/docker/selkies/apply.py:19-21`). With a second Chrome on the display, it would keep
  reading `main` while X input goes elsewhere.
- One pinned screen (`internal/serve/serve.go:282-289`) gives every seed the same window
  (`internal/fingerprint/args.go:391-396`, `:637-645`), so every window fits the display.

## 3. Interfaces between lanes

Lanes code against these signatures from t=0. Anything consumed across lanes is wired in
`internal/serve/serve.go` by the integration lane, so no parallel lane edits another's
file.

```go
// A2: internal/serve/mcpeval.go (new)
// evalPage runs fn in the session's current page through its own upstream call path
// (a.call); the caller holds s.callMu; the result never reaches the agent.
func (a *aggregator) evalPage(ctx context.Context, s *agentSession, fn string) (json.RawMessage, error)

// B1/B2: POST /mask?fingerprint=<seed> - the contract in 4.2.

// P1: pool.go (SetBoundSeeds, SetBusyCheck, seedKeyFor, afterLaunch);
// P3: auth.go (raiseSeed); P2: http.go (routes).
// afterLaunch, when set, runs after a non-main Chrome launched; INT wires P3's raise.
//   field on chromePool: afterLaunch func(seedKey string)
// SetBoundSeeds replaces the seed names principals bind ("main" excluded). Stored in
// an atomic.Pointer: supervised() reads it under p.mu.
func (p *chromePool) SetBoundSeeds(names map[string]bool)
// SetBusyCheck is OR-ed with the CDP refcount and a live lease when the reaper fires.
// Called with the seed's launch lock held and p.mu not held; must not launch.
func (p *chromePool) SetBusyCheck(busy func(seedKey string) bool)
// seedKeyFor (existing, internal/serve/pool.go:365) in session mode:
// "" or "main" -> "__default__", a bound name -> itself, anything else refused.
// raiseSeed raises the seed's window (the existing xdotool raise) and records it as
// the front seed. It launches nothing.
func (p *chromePool) raiseSeed(ctx context.Context, seedKey string) bool
// Routes on the internal listener and, for trusted peers, the remote one:
//   GET /fingerprint/{seed}/json/version[/]  /json/list[/]  /json[/]
//   GET /fingerprint/{seed}/devtools/{path...}   (existing, moved into cdpRoutes)
// The child endpoint and the CLI attach use http://127.0.0.1:<port>/fingerprint/<seed>;
// main keeps the root form.

// E1: internal/serve/remote.go
type remotePrincipal struct {
    name string
    seed string // a bound name or "main"
    hash [sha256.Size]byte
}
func principalOf(ctx context.Context) string     // unchanged
func principalSeedOf(ctx context.Context) string // the seed of the line that admitted the request
// ticket, viewSession and fileHandle each gain `seed string`; "" (old callers) means "main".
func (r *remote) mintFileSeed(principal, seed, dir, name string) (string, error)
func (r *remote) seedBusy(seedKey string) bool // an unexpired ticket or a live view session for it
func validBoundSeed(name string) bool          // ^[a-z][a-z0-9_]{0,31}$, minus "state" and "logs"
// Hooks the integration lane wires:
//   r.bindSeeds = pool.SetBoundSeeds
//   r.raiseSeed = pool.raiseSeed (called on redeem)

// E2a: internal/serve/mcpchild.go (new)
type seedChild struct {
    Endpoint string // http://127.0.0.1:<port>/mcp
    Host     string // its random --allowed-hosts name
}
func newChildTable(ctx context.Context, dataDir string, daemonPort int,
    allocPort func() (int, error)) *childTable
// childFor returns the seed's child, starting it on first use. Concurrent callers
// share one start. release drops one session; at zero the child is stopped and
// waited out before its entry goes.
func (t *childTable) childFor(ctx context.Context, seedKey, seedName string) (*seedChild, func(), error)
func (t *childTable) busy(seedKey string) bool
func (t *childTable) close()
```

## 4. Phase 1

### 4.1 Lanes A1 and A2 - shared tabs (gpt-6-astra high, in parallel)

Split for wall time, by file:

- **A1** owns `internal/serve/mcp.go`, `mcp_test.go` and `readPrincipals`: the deletion,
  the fresh tab, the relaunch retry, `--no-webmcp`, the instructions, `seed=main`.
- **A2** owns `internal/serve/mcptools.go`, `mcpsecrets.go` and a new `mcpeval.go` (+ its
  test file): `evalPage` and capture, the sentinel pre-check and `cuttle_wait` on it,
  `cuttle_viewer_link`, `tabOwners`/`errNoOwnTab`, the screenshot hold, the download
  settle check, the four tool descriptions. A2 calls today's upstream call path in
  `mcp.go` (`a.call`, `internal/serve/mcp.go:681`) and touches nothing else in it.
- The docs below go to lane W.

**Delete tab ownership:**

- In `internal/serve/mcp.go`:
  - the `agentSession` fields `owned`, `current`, `seen`, `batch` and `captured`
    (`:220-236`), their initialization (`:352`), and `tabMu` (`:256-258`);
  - everything from `adopt` through `ownedTab` (`:741-1163`);
  - in `forward`, the interception and `ensureOwnTab` (`:631-636`), and the own-tab half of
    the relaunch retry (`:664-675`);
  - `needsTab` (`:603-608`, `:617`), and the reset of `current` and `seen` (`:505`);
  - `browserOps.activate` (`:86`, `:181-194`).
- In `internal/serve/mcptools.go`: `tabOwners` and `tabOut.Owner` (`:145`, `:164-193`), and
  `errNoOwnTab` (`:44`).
- The six ownership tests (`internal/serve/mcp_test.go:463`, `:527`, `:570`, `:603`,
  `:623`, `:642`), and the own-tab assertion at `:438-441`.

**Keep:**

- The `browser_tabs` argument re-encoding (`internal/serve/mcp.go:1054-1064`). The lease
  decision reads `action` in Go, and a duplicate-key request must not pass as read-only.
- `list` as the only read-only `browser_tabs` action.

**Fresh tab:**

- When cuttle creates an upstream session (`internal/serve/mcp.go:447-496`), it sends
  `browser_tabs new` before forwarding the first call. Otherwise the session would start on
  the person's tab (section 2).
- This covers first use, the 30-minute idle close, and a restart.
- It takes the lease, as today's first call does (`internal/serve/mcp.go:603-621`).

**`browser_evaluate` instead of tab resolution.** One helper, `evalPage`, makes an internal
upstream call whose result never reaches the agent. Its caller holds `callMu`.

- **Capture:**
  - One evaluate returns the element's value and `location.origin` together. The value
    logic is `internal/serve/grab.go:408-415`, with the selector JSON-encoded.
  - The aggregator stores the value bound to that origin, and returns only the name,
    sentinel, origin and length.
  - Clipboard mode keeps the permission grant (`internal/serve/grab.go:497-504`), followed
    by one evaluate that returns the text and the origin.
  - It already holds `callMu` (`internal/serve/mcptools.go:425-426`).
- **Sentinel pre-check:**
  - It reads `location.origin`, and skips the check if the read fails.
  - The fill path still refuses a bound secret in a foreign frame
    (`internal/serve/secretfill.go:329-331`).
  - It runs under `passthrough`'s `callMu` (`internal/serve/mcp.go:597-598`).
- **`cuttle_wait`:**
  - It reads the URL and title at return, and at most once per progress message.
  - It takes `callMu` around each read (`internal/serve/mcptools.go:300-344`).
- **`cuttle_viewer_link`:** no upstream call and no tab activation
  (`internal/serve/mcptools.go:223-228` goes).

**Also in lanes A1 and A2** (A1: retry, page tools, instructions, principals; A2: hold, downloads; W: docs):

- **Screenshot hold:** after any capture, every screenshot is refused for
  `screenshotHold` (`internal/serve/mcp.go:44`). One timestamp, under a mutex.
- **Relaunch retry:** retry once on a closed-target error, only if the running instance
  changed, after `getOrLaunch`.
- **Page tools:** pass `--no-webmcp` to today's child (`internal/serve/mcp.go:1286-1289`).
- **Downloads:** `findDownload` (`internal/serve/mcptools.go:510-548`) skips a file modified
  in the last second, and reports "still downloading - retry".
  - Two sessions write a download at the same time, so once the file has settled, both
    writers are done, and the later `/files/{h}` fetch is safe.
- **Instructions** (`internal/serve/mcp.go:65-71`):
  - Tabs are shared: open your own and close what you opened.
  - After `cuttle_wait`, a handoff or any pause, take a `browser_snapshot` before acting.
  - Test: B closes A's tab, and A's next result shows the neighbour's URL.
- **Principals:** `readPrincipals` (`internal/serve/remote.go:145`) accepts a third field,
  `seed=main`, only. This ships in phase 1 (section 7).
- **Docs:**
  - `docs/OPERATING.md:909-965`;
  - both SKILL.md copies, kept byte-identical;
  - four tool descriptions (`internal/serve/mcptools.go:85`, `:92`, `:108`,
    `internal/serve/mcp.go:73`);
  - a "superseded" line in the 2611-04 plan.

### 4.2 Lanes B1 and B2 - the CLI masking leak (gpt-6-astra medium, in parallel)

Split for wall time: **B1** owns the daemon side (`internal/serve/downloads.go`, the `/mask`
line and the `GET /` fields in `http.go`); **B2** owns the CLI side (`internal/cli/*`, the
new `mask-exec` command) and codes against the `/mask` contract below, tested with a fake.

**`/mask` contract (fixed at t=0):** `POST /mask?fingerprint=<seed>`, loopback only,
chunked request body in, chunked masked body out, same bytes otherwise; 404 = an image
without masking (fail closed); any non-2xx = fail closed.

**Hard rule:** the daemon never returns secret values or mask pairs to any process.

**`POST /mask`:**

- A loopback route; the seed comes from `?fingerprint=`.
- It masks a chunked stream with the seed's held secrets and live password values, in
  their exact, YAML-quoted and JSON-escaped spellings.
- To catch a value split across writes, it holds back only a tail that could still start
  one.
- It shares one pair builder with `/snapshot`, factored out of
  `internal/serve/downloads.go:334-337`. The aggregator adopts it in E2b.
- It can confirm a guessed whole value, as every masked output can. It never confirms
  part of one.

**`cuttle mask-exec -- <argv>`** (hidden, runs in the container):

- It runs the driver in its own process group.
- It streams the driver's stdout and stderr through `/mask`.
- It keeps the driver's exit status.
- It kills the group when its own output closes, for example on a host Ctrl-C.

**Wiring:**

- `execPlaywright` (`internal/cli/playwright.go:471-472`) wraps every driver argv: verbs,
  attach, retries, help, the dialog hint and the jev runner.
- The jev persistent client is started the same way (`internal/cli/jevbrowse.go:423`).
- The retry stops passing stderr through raw (`internal/cli/playwright.go:330`).

**Fail closed:**

- If either piece is missing or fails, the stream is withheld and the exit status kept.
- The hint depends on the context:
  - attach-only contexts (`internal/config/config.go:83-86`): "the release's image
    predates this CLI - bump its chart pin";
  - otherwise: `<self> up --recreate`.
- `GET /` (`internal/serve/http.go:307-316`) gains `version` and `data_dir`.
- `cuttle up` warns when the daemon is older than the CLI.

## 5. Phase 2

### 5.1 Lanes P1, P2 and P3 - pool gates, routes, handover (in parallel)

Split for wall time, by file; all three stay on their branches until integration:

- **P1** (gpt-6-astra high) owns `internal/serve/pool.go`, `supervise.go` and the
  `serve.go` flag handling: the allowlist, the per-seed fingerprint, the reaper and busy
  check, supervision. It exposes `p.afterLaunch func(seedKey string)`, called after a
  non-main Chrome launches; the integration lane points it at P3's raise.
- **P2** (gpt-6-astra medium) owns the CDP routes in `http.go` (`cdpRoutes`, the prefix
  builder), `handleWSSeed`'s accounting key in `wsproxy.go`, and the one-line hunk in
  `remote.go` `handler`.
- **P3** (gpt-6-astra high) owns `viewerfocus.go`, `auth.go` (`raiseSeed`, the front
  seed) and the `Target.createTarget` rewrite in `wsproxy.go` (a different function from
  P2's).

**Allowlist:**

- `SetBoundSeeds` and `seedKeyFor`, as in section 3.
- A fingerprint per seed is persisted in `<dataDir>/<seed>.seed`, like `main`'s
  (`internal/serve/pool.go:873-889`). The name is not used as the fingerprint, because
  names recur across instances.

**Reaper:**

- In session mode `--idle-timeout` applies to non-main seeds, default 15 min.
- `idleReap` (`internal/serve/pool.go:299-344`) runs in this order:
  1. check the timer and connections under `p.mu`;
  2. without `p.mu`, check for a live lease and call the busy check, and re-arm if busy;
  3. re-check, then tear down.
- A pin that needs the browser goes through `getOrLaunch`, which takes the same seed lock.

**Supervision:**

- `supervised` (`internal/serve/supervise.go:76-78`) is also true for bound seeds.
- Their captures carry the supervised mark, so a seed that is later unbound keeps capture
  and re-inject.

**Routes:**

- Add the `/fingerprint/{seed}/json...` set and the moved devtools route to `cdpRoutes`
  (`internal/serve/http.go:92-104`).
- Add `/fingerprint/` to the remote gated list (`internal/serve/remote.go:402`, a one-line
  hunk in `handler`).
- One prefix builder covers the browser socket, the target sockets and
  `devtoolsFrontendUrl` (`internal/serve/http.go:333-341`, `:368-379`).
- `handleWSSeed` passes `seedKeyFor(name)` to accounting (`internal/serve/wsproxy.go:98`).

**One display, two fixes:**

- **(a) The keyboard follows X input.**
  - The focus stream (`internal/serve/viewerfocus.go:25-73`) samples only the instance
    whose browser PID owns the X-focused window. This is the inverse of the raise
    (`internal/serve/auth.go:236-263`).
  - The lookup is cached for at most 1 s, and falls back to `main`.
- **(b) Nothing steals focus.**
  - The CDP proxy adds `focus: false` to a driver's `Target.createTarget`, unless the
    driver set `focus` or `background` itself. This sits beside the existing interception
    (`internal/serve/wsproxy.go:874-904`).
  - After a non-main Chrome launches, the daemon raises the front seed (the one last given
    to `raiseSeed`), or `main` if that seed is not running.

### 5.2 Lane E1 - principals and capabilities (gpt-6-astra high)

- **Parsing:** `seed=` takes any `validBoundSeed` name. An unknown or reserved name fails
  the load, and a failed load refuses every bearer (`internal/serve/remote.go:165-172`).
- **Hooks:** after each load, E1 calls `r.bindSeeds`.
- **Capabilities carry their seed:** tickets, view sessions and file handles
  (`internal/serve/remote.go:255-276`).
- **Minting:** it checks the live (name, seed) pair under `r.mu`, where the name check is
  today (`internal/serve/remote.go:600-606`, `:876-881`). A call admitted under an old
  binding therefore cannot mint after a change.
- **Rebinding is a full revoke** of the principal's tickets, views, files, lease and
  upstream session.
  - The reload hands `revoke` (`internal/serve/remote.go:565-589`) the names whose pair is
    unchanged, so the aggregator's own sweep (`internal/serve/mcp.go:298-320`) ends the
    rebound session without an aggregator change.
  - The callback keeps its shape, so `internal/serve/serve.go:376` is untouched.
- **Secret form:** it stores into the ticket's seed (`internal/serve/remote.go:784`).
- **Redeem:** it launches the seed if needed and calls `r.raiseSeed`. The redirect stays
  `/` (`internal/serve/remote.go:751`).

### 5.3 Lane E2a - the per-seed child table (gpt-6-astra high)

E2a is a new file. It consumes nothing of lanes A1 or A2.

**Per child, the same arguments as today** (`internal/serve/mcp.go:1286-1289`), plus:

- `--no-webmcp`;
- port 8931 and the root endpoint for `main`;
- for other seeds, a port from the pool's allocator (`internal/serve/pool.go:1388-1407`),
  and `--cdp-endpoint http://127.0.0.1:<daemon port>/fingerprint/<seed>`. Playwright keeps
  the path
  ([chromium.ts](https://github.com/microsoft/playwright/blob/main/packages/playwright-core/src/server/chromium/chromium.ts));
- its own random allowed host;
- output and working dir `<dataDir>/<key>/Downloads`.

**Lifecycle:**

- One start per seed, under the table lock. Concurrent callers wait.
- A stopping child is waited out before a new one starts.
- The pruner runs once per seed dir under the daemon context, not per child
  (`internal/serve/mcp.go:1281`).

### 5.4 Lane E2b - aggregator wire-up (gpt-6-astra medium)

E2b needs A1, A2, E1 and E2a on their branches.

**`agentSession` gains `seed`,** set from `principalSeedOf` when the session is created
(`internal/serve/mcp.go:350-359`).

**Upstream sessions go per child:** each session gets its endpoint and host from
`childFor`, and its own `upstreamTransport` (`internal/serve/mcp.go:263-279`). The session
calls `release` when it closes.

**`s.seed` replaces `reservedSeed`:**

- in `cdpBrowser` (`internal/serve/mcp.go:101-107`, `:152-158`);
- in `internal/serve/mcp.go` at `:316`, `:520`, `:712` and `:1225`;
- in `internal/serve/mcptools.go` at `:156`, `:335`, `:350`, `:456`, `:473` and
  `:503-504`;
- in `internal/serve/mcpsecrets.go` at `:286`.

**Also:**

- Tickets and file handles carry `s.seed`, through `mintFileSeed`.
- The screenshot hold is kept per seed.
- The aggregator adopts B's pair helper.
- E2b deletes the single-child `runPlaywrightMCP` (`internal/serve/mcp.go:1276-1310`).

### 5.5 Lane G - CLI `--seed` (gpt-6-astra medium, after B)

**Selecting a seed:**

- The context gains a `seed` field (`internal/config/config.go:73-107`).
- `--seed` is accepted on `pw` (`internal/cli/playwright.go:189-222`), `secret`,
  `downloads`, `auth`, `open` and `viewer`.

**`main` is unchanged byte for byte:**

- session `cuttle` (`ops/docker/Dockerfile:460`);
- workdir `/data/__default__/Downloads` (`internal/cli/playwright.go:53`);
- the root attach (`:463-469`).

**Other seeds:**

- Session `cuttle-<seed>`, passed as `-s=` everywhere, including the attach probe
  (`internal/cli/playwright.go:465`).
- Attach to `--cdp=http://127.0.0.1:9222/fingerprint/<seed>`.
- Workdir `<data_dir>/<seed>/Downloads`.
- `?fingerprint=<seed>` on every loopback call:
  - lease acquire, renew, release and takeover (`internal/cli/lease.go:52`);
  - the inline gate (`:313`);
  - `/snapshot` and `/mask`;
  - `/window/raise` (`internal/cli/commands.go:1087`).
- `/snapshot` reads the seed's own dir (`internal/serve/downloads.go:321`).

### 5.6 Option: a display per seed (lane D, Opus)

**What lane D adds, per non-main seed:**

- A daemon-owned Xvfb and openbox, each in its own session
  (`ops/docker/bin/docker-entrypoint.sh:89-118`).
- An ordered shutdown: cookie capture first, then Chrome, then X. A display number is
  freed only after its processes exit.
- Selkies on demand, with a readiness page after redeem.
- A viewer proxy and a focus patch per seed.

**What it fixes:** a handoff shows only that seed. Keyboard focus and the clipboard stop
being shared.

**What it costs:**

- Two or three more processes per seed.
- About 0.5 core per watched seed while its screen moves
  (`docs/knowledge/decisions/selkies-is-the-takeover-viewer.md:78-80`).
- The newest and largest code in the plan.

It can run beside P, E1 and E2a and slip without blocking, because one display stays the
default.

## 6. Lanes

### 6.1 Who owns what

| Lane | Model | Starts | Owns | Provides | Acceptance |
|---|---|---|---|---|---|
| A1 | astra high | t=0 | `mcp.go`, `mcp_test.go`, `readPrincipals` | shared tabs, fresh tab, retry, `seed=main` | Tabs are shared. A new session opens its own tab first. A closed tab moves the session to its neighbour (test). Retry only on a new instance. No page tools. `seed=main` accepted, any other `seed=` refused. |
| A2 | astra high | t=0 | `mcptools.go`, `mcpsecrets.go`, new `mcpeval.go` + test | `evalPage`, capture, pre-check, wait, hold, settle | A capture never returns its value. Pre-check skips on a failed read. Wait reads at return and per progress only. A download still being written is skipped. |
| B1 | astra medium | t=0 | `downloads.go` (pairs, `/mask`), `/mask` line and `GET /` fields in `http.go` | `/mask`, `version`, `data_dir` | A value split across writes is masked. Pairs are never returned. 8 MiB line cap. |
| B2 | astra medium | t=0 | `internal/cli/*` except what G adds, new `mask-exec` | `mask-exec`, fail-closed wiring, version warning | Every exec path is masked (fake `/mask`). Old image: output withheld, the right hint, exit status kept. Ctrl-C stops the driver. |
| W | opus | t=0 | `docs/OPERATING.md`, both SKILL.md, the 2611-04 note; phase 2 docs as a draft file | docs | SKILL.md copies byte-identical. Phase 1 docs match A1/A2 as merged. |
| E1 | astra high | t=0 | `remote.go` except P2's hunk and A1's `readPrincipals` | principal record, seed fields, `seedBusy`, `mintFileSeed`, `validBoundSeed` | Reserved and unknown names fail the load. A rebind revokes everything. A call admitted before a reload cannot mint after it. Redeem raises the ticket's seed (through the hook). |
| E2a | astra high | t=0 | new `mcpchild.go` + test | `childFor`, `busy`, `close` | One start for concurrent first calls. The last release stops the child. A call during a stop waits. `--no-webmcp`. Output dir per seed. |
| P1 | astra high | t=0 | `pool.go`, `supervise.go`, `serve.go` flags | `SetBoundSeeds`, `SetBusyCheck`, `afterLaunch` | Allowlist and refusals. A busy or leased seed is not reaped. Concurrent first use and reap. A bound seed is captured on reap and on shutdown. |
| P2 | astra medium | t=0 | `http.go` CDP routes, `handleWSSeed` in `wsproxy.go`, the `remote.go` `handler` hunk | `/fingerprint/{seed}/` routes | `/fingerprint/main/` counts as `__default__`. Discovery then socket for a seed. Internal routes refuse browser requests. |
| P3 | astra high | t=0 | `viewerfocus.go`, `auth.go`, the `createTarget` rewrite in `wsproxy.go` | `raiseSeed`, front seed | The keyboard follows X focus. A new tab does not raise its window. |
| E2b | astra medium | A1, A2, E1, E2a landed | `mcp.go`, `mcptools.go`, `mcpsecrets.go` (per-seed changes only) | the per-seed aggregator | Two seeds get two children, two leases and two secret sets; no mask pair crosses seeds. |
| G | astra medium | B2 landed | `internal/config/config.go`, the `--seed` parts of `internal/cli/*`, `/snapshot`'s seed dir | `--seed` | Two contexts on one pod. Old and new CLIs share `main`. Lease calls go per seed. |
| R | astra medium | each landing | read-only | a review per lane | Findings fixed on that lane's branch by a fresh fix session before the lane counts as landed. |
| INT | opus | first phase 2 landing | `serve.go` wiring, conflict resolution, phase 2 docs from W's draft | phase 2 | The phase 2 gate. |

### 6.2 Sequencing rules

- **Branches.** Phase 1 lanes branch from `main` and merge into one phase 1 branch, which
  becomes the phase 1 PR. Phase 2 lanes branch from `main` too; E2b branches from the
  phase 1 branch once A1 and A2 are on it. INT starts from the phase 1 branch and merges
  each phase 2 lane as it lands, running `just check` after every merge; it merges `main`
  again after the phase 1 PR lands.
- **Wiring** (INT, in `serve.go`): the child table; `SetBoundSeeds`;
  `SetBusyCheck(func(k string) bool { return rem.seedBusy(k) || children.busy(k) })`;
  `r.raiseSeed` and `p.afterLaunch` to P3's raise; dropping the single child
  (`internal/serve/serve.go:377`).
- **Known shared files, different functions:** `http.go` (B1 vs P2), `wsproxy.go` (P2 vs
  P3), `remote.go` (E1 vs A1's `readPrincipals` vs P2's hunk). INT resolves.
- **Nothing waits for a release.** The phase 1 PR is polished, merged, released and
  deployed while phase 2 lanes run. Phase 2 merges only after it, but builds and tests
  independently.
- **The tail runs in parallel:** INT's cross-lane polish and the image smoke test start
  together on the integrated head; fixes from both land, then one re-smoke, then merge.

### 6.3 Execution

- Astra lanes run with full access in their own treehouse slot, commit and push their
  branch, never open a PR. Preflight: launch A1 first and confirm it can commit before
  starting the rest.
- If Astra rate-limits, medium lanes move to gpt-6.1-sol high.
- Each lane runs with `GOMAXPROCS=3`, so ten concurrent `just check` runs do not starve
  the host.

### 6.4 Gates

- **Phase 1:** two principals sharing tabs; a capture; `cuttle pw tab-list` on a `data:`
  URL holding a password value; the old-image hint.
- **Phase 2:**
  - two seeds on one display;
  - handover keyboard and no focus steal;
  - a quiet viewer outliving the idle timeouts;
  - a non-main seed's cookies surviving a reap and a pod restart;
  - a page unable to start a seed through discovery.

## 7. Rollout and rollback

**Order:** roll out the image first, then the principals, then the CLI. Roll back in
reverse.

- **The phase 1 image** accepts only `seed=main`; any other `seed=` refuses every bearer.
  So write non-main lines only after the phase 2 image is live, and remove them before a
  rollback.
- **A removed seed's files stay on disk,** never attached to `main`: its dir, `<seed>.seed`
  and `state/<seed>.json`. Delete them by hand.
- **In-memory state is lost on restart:** tickets, viewer sessions, file handles and
  secrets.
- **Old and new CLIs:**
  - An old CLI reaches `main` unchanged.
  - A new CLI with a seed against a phase 1 image is refused.

## 8. Security

| Risk | Answer |
|---|---|
| An agent acts on someone else's tab | Accepted (D4). Separation is a seed; the lease serializes drivers per seed. |
| A closed tab moves a session onto the person's tab | Accepted. Agents re-snapshot after a pause, and a test covers the move. |
| A new session drives the person's tab | It opens its own tab first. |
| Two clients share one token | They share one current tab and one lease token, so the lease does not separate them (`internal/serve/mcp.go:713-737`). `callMu` runs their calls one at a time. It stays per principal because input is paced on one CDP pump (`internal/serve/humanize.go:362-366`, `:934-945`). |
| A page adds tools to the agent | `--no-webmcp`. |
| A captured value reaches the agent | The aggregator consumes its own evaluate; the value is not persisted (live check). |
| A page sees cuttle's reads | Accepted. Evaluate runs in the page's main world, and only at a capture, a typed sentinel, and a wait's return and progress. |
| The CLI prints a secret | Everything passes through `/mask`; on failure output is withheld; mask pairs are never returned. |
| A token reaches another seed | MCP uses the bound seed only. Capabilities carry (name, seed), and a rebind revokes them all. |
| A page starts or drives another seed | Internal routes refuse browser requests; remote CDP is for trusted peers only. |
| A seed name aliases storage | `validBoundSeed`. |
| One display: windows, focus and clipboard are shared across seeds | Accepted under D1, with the two fixes in 5.1. A display per seed (5.6) removes it. |

## 9. Decided

- **O1 - display:** one display for all seeds (owner, 2026-11-05 20:40).
- **O2 - tab leak:** accepted; agents are told to close what they opened (owner,
  2026-11-05 20:40). Closing cuttle-opened tabs still on `about:blank` is in section 11.

## 10. Live checks

| Check | Lane |
|---|---|
| An evaluate result is not persisted without `filename`, and there is no session log | A2 |
| `--no-webmcp` leaves no page tools in the list | A1, E2a |
| The fork's Chrome honours `createTarget {focus: false}` | P3 |
| A Chrome window's PID is the browser PID | P3 |
| `mask-exec` stops the driver on a host Ctrl-C, through both docker and kubectl exec | B2 |
| Per-session request and event buffers grow until the 30-minute idle close (coreBundle.js `:67137-67150`, `:67349-67352`) | integration |
| Memory, `/dev/shm` and disk use with `main` plus two active seeds | integration |

## 11. Later

- **Per-client tabs within one token:**
  - An opt-in `X-Cuttle-Client` header read from `req.Extra.Header`, with capped keys per
    principal; the lease and `callMu` stay per principal.
  - Stateful MCP sessions are ruled out: they reverse the protocol decision, and their ids
    are not bound to principals.
- **Tab cleanup:** close cuttle-opened tabs still on `about:blank` at the idle close.
- **Covered handoff windows:** decide whether to intercept `Page.bringToFront` from seeds
  that are not in front.
- **Idle with a viewer open:** shipped with seeds (#172): while a viewer is open, a seed
  whose window is in front or holds the focus is busy and is not reaped.
- **Per-seed proxy, timezone or locale** as `key=value` fields on the principal line,
  treated as operator defaults for geo (`internal/serve/pool.go:478-484`).
- **Secrets of a reaped seed:** keep them? They are dropped today.
- **Clipboard capture on one display:** it reads what the person copied for any seed.
- **A window list** for switching seeds in the viewer.

## 12. Review findings and where they went

**First review:**

| Finding | Where |
|---|---|
| Names aliasing storage | `validBoundSeed` (3) |
| Cookies of non-main seeds | supervision (5.1) |
| Rebinding | full revoke (5.2) |
| Shutdown versus X | moot with one display; see 5.6 |
| Viewer pins | `seedBusy` (5.2), `busy` (5.3) |
| Current-tab helper | replaced by evaluate (4.1) |
| Locking | 4.1 |
| CDP rewrites | 5.1 |
| Proxy geo | moot under D9 |
| CLI sessions and lease threading | 5.5 |
| Start and stop races | 5.1, 5.3 |
| Rollback | 7 |
| Resources | 10 |
| Private names | removed |

**Second review:**

| Finding | Where |
|---|---|
| Busy pins | 5.1 to 5.3 |
| Rollback with `seed=` | 4.1, 7 |
| No `(current)` line | moot |
| A closed tab moves the session | 4.1 |
| Old-image hint | 4.2 |
| Child start race and relaunch retry | 5.3, 4.1 |
| Locking of the viewer link | 4.1 |
| Pages starting seeds | 2 |
| One CDP spelling | 5.1 |
| Versions | none promised |
| Supervision | 5.1 |
| Sizing | moot |

**Risk check:**

| Finding | Where |
|---|---|
| Per-session keying | left out (11) |
| `callMu` | 8 |
| A fresh session starting on the person's tab | 4.1 |
| `--no-webmcp` | 4.1, 5.3 |
| Duplicate download writes | 4.1 |
| Buffers | 10 |

## 13. Sources

- `docs/plans/2611-04-remote-mcp-session.md`
- The bundled `playwright-core` 1.65.0-alpha-2026-10-30, `lib/coreBundle.js`.
- Playwright upstream:
  [crBrowser.ts](https://github.com/microsoft/playwright/blob/main/packages/playwright-core/src/server/chromium/crBrowser.ts),
  [chromium.ts](https://github.com/microsoft/playwright/blob/main/packages/playwright-core/src/server/chromium/chromium.ts).
- [Target.pdl](https://github.com/ChromeDevTools/devtools-protocol/blob/b0d96c6cdf39e5583e67a3762b2815a33154455b/pdl/domains/Target.pdl)
- `docs/knowledge/decisions/selkies-is-the-takeover-viewer.md`
- The owner's live tests on the previous release (2026-11-05):
  - `cuttle pw tab-list` printed a password value inside a `data:` URL;
  - `select` failed with "there is no tab 1".
