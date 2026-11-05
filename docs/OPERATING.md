# Operating cuttle

Install, remote backends, ports, pool mode and deployment. This is the material an
operator reads once per install - it is deliberately NOT in `cuttle skill`, which
every agent loads on every session and should carry only what changes how it drives
a page.

## Install

cuttle needs Docker (or OrbStack) for the local backend; the ssh/k8s backends need
only their own client (`ssh` / `kubectl`+`helm`). The CLI is a single static Go
binary named **`cuttle`**:

```bash
brew install tenequm/tap/cuttle            # homebrew cask (macOS/Linux)
go -C packages/cuttle install ./cmd/cuttle # from a clone of this repo (Go 1.27+)
```

The container image is `ghcr.io/glim-sh/cuttle` and `cuttle up` pulls it on first
run. Then, from any directory:

```bash
cuttle up      # start the container + takeover viewer (pulls the image if needed)
```

`up` is idempotent and profile-preserving: a stopped container is **restarted**
(logins persist), not recreated.

> **Apple Silicon:** the image is multi-arch, so on an arm64 Mac the local backend
> runs the native arm64 build (a macOS persona) - no emulation. Remote `ssh`/`k8s`
> backends run the amd64 build (a Windows persona).

**Keep the CLI and the image in step.** A release build pins its own version, so a
stale CLI drives a newer daemon while printing its own older guide. If `cuttle
skill` and observed behaviour disagree, check `cuttle status` for the image tag and
`cuttle --version` for the CLI.

## Contexts and backends

A **context** names where the browser runs, selected by `--context` >
`CUTTLE_CONTEXT` > the config `default_context` > built-in `local`. `--context`
and `--name` are global flags on `cuttle` itself, so every verb that reaches an
instance takes them - `cuttle pw` and `cuttle jev-browse` included:

- **local** - Docker on this host (the zero-config default).
- **ssh** - a container on a remote host, reached over `ssh -L`. Inherits
  `~/.ssh/config` (keys, jump hosts).
- **k8s** - a Deployment reached via `kubectl port-forward`. Inherits your kube config.
- **direct** - an already-running CDP endpoint, used as-is.

For the tunneled backends every CDP/VNC op still targets the stable local
`127.0.0.1:9222`/`:6080` - the backend owns the standing tunnel, established by
`up`, re-established by `status`, torn down by `down`. `cuttle context ls` lists
contexts and marks the active one; `cuttle context --help` covers both verbs.

```bash
cuttle context add box --backend ssh --host user@box.example --default
cuttle context add cluster --backend k8s --namespace browser --release cuttle
cuttle context add tailnet --backend direct --cdp-url http://cuttle.example:9222
```

Contexts live in `$XDG_CONFIG_HOME/cuttle/config.toml` (default
`~/.config/cuttle/config.toml`) and can be hand-edited - the only way to set
advanced k8s knobs (node_selector, tolerations, resources):

A k8s release cuttle did not install - one managed by helmfile, Argo or your own
`helm` - takes an attach-only context, `--external` on `context add` or
`external = true` in its stanza. cuttle never runs helm against it, so `helm` need
not be installed: `up` checks the release's Deployment exists and has a ready
replica (and names the namespace/release when it does not), then opens the
standing tunnel and prints the briefing; `status`, `pw`, `secret`, `downloads`,
`auth` and `open` work as on any k8s context. `down` only closes the tunnel, and
everything that would change the release - `up --recreate`, `--image`,
`--purge-profile`, `--ephemeral`, `--idle-timeout`, `--screen`, `--humanize`,
`--allow-context-creation`, `--block-third-party-cookies`, `down --purge`,
`purge-profile` - is refused. Such a context takes no `proxy`: the release sets
its own where it is managed.

```bash
cuttle context add shared --backend k8s --namespace browser --release cuttle --external
```

```toml
default_context = "box"

[context.box]        # ssh: docker on a remote amd64 host
backend = "ssh"
host    = "user@box.example"
name    = "scraper"  # optional: the container this context stands for (see below)
screen  = "1366x768" # optional: the screen the browser claims (see below)

[context.cluster]    # k8s: a Deployment via kubectl port-forward
backend   = "k8s"
namespace = "browser"
release   = "cuttle"
```

## One browser per container

A `cuttle up` container runs the daemon in **session mode** (the default): exactly
one Chrome, with one persisted identity, that every attaching agent shares and the
viewer shows. The daemon refuses a `?fingerprint=` seed on any connect or discovery
URL (HTTP 400 naming the mode), so a driver cannot fork a second browser with a
second cookie jar next to the one the human is looking at, and resource use is
bounded by construction. For many identities on one endpoint see pool mode below.

**Nobody can take it away from the others.** The browser is shared, so no single
party gets to end it. A driver's `Browser.close` (or `Browser.crash`) is answered
with success and detaches only that client; a driver that closes every tab is let
through one tab at a time, the daemon opening a replacement before the last one
goes, so Chrome always keeps a page and stays up. In the
viewer, the window has no close button, Alt+F4 is unbound and the titlebar menu
is gone. Closing the last tab by hand does exit Chrome - and the daemon relaunches
it on the spot, so what you get is a fresh blank tab with the same logins, never
an empty desktop. A crash is healed the same way (with backoff if it keeps
crashing). `cuttle status` reads the daemon's health rather than poking the
browser, so it never starts one as a side effect; if none is running at that
instant it says:

```
  note: no browser is running right now - `cuttle open` starts it (the profile is kept)
```

**The viewer fills the window.** The framebuffer is sized to the browser window,
which is itself sized to the screen the browser claims rather than to the display
- a browser claiming a 1440x900 screen while filling a 1920x1080 window is an
obvious lie. The entrypoint asks the daemon for that geometry (`cuttle
viewer-geometry`) before starting the X server; when the size is not knowable
ahead of the launch it falls back to 1920x1080. The display's width is rounded
up to a multiple of 8, which the viewer's encoder needs, and the window manager
never maximizes into those few extra pixels, so the window keeps the screen's
exact width and the viewer shows a strip of at most 7px on the right.

**The viewer is [Selkies](https://github.com/selkies-project/selkies).** It
streams the display as H.264 over one WebSocket and sends keyboard, mouse and
clipboard back as real X input, so every event a site sees is trusted. It works
from a desktop or a phone (touch and trackpad modes, an on-screen keyboard), and
the display stays the persona's size whatever the client is - a phone gets it
letterboxed, never resized. Copy and paste work in both directions; once you allow the viewer clipboard
access it also copies your local clipboard into the session whenever its tab
gains focus, so do not grant that with anything on the clipboard the session
should not see. Audio,
microphone, webcam, gamepads, printing, file transfers and sharing links are off.
On phones in direct-touch mode, tapping a cached editable field configures a
local keyboard in the tap gesture. Unknown fields wait up to one second for a
matching focus report; a blur closes it after a short grace period. The manual
keyboard button remains the fallback for browser chrome, cross-origin or transformed frames,
trackpad mode, unavailable focus reports, and rapid page changes. Passwords
use `autocomplete=off`: paste from your password manager; the viewer cannot
associate its origin with the remote site's credentials. OTP fields request
`one-time-code`. Physical iPhone keyboard visibility still needs device testing.

The same-origin `api/cuttle-focus` stream is served by a small build-time
server overlay, forwarding only to the daemon's loopback `/viewer/focus`.
Keep SSE buffering off at the ingress; no additional exposed port or CORS
allowlist is needed. A prefix proxy must forward this path with the rest of
the viewer. Sampling runs only while a viewer stream is open (only phone clients open
one), at most five reports per second per stream, and reads metadata rather
than field values.
If an attached automation client emulates focus on multiple tabs, prediction
is disabled rather than guessing which tab receives X input. Prediction covers
session mode; a pool-mode viewer keeps the manual keyboard.

Characters the persona's keyboard lacks (Cyrillic, accented letters) are typed
through spare keycodes, which a page can see in its keyboard layout map while a
person is connected; the container puts the persona layout back when the last
viewer leaves. Why Selkies and each setting:
[the decision](knowledge/decisions/selkies-is-the-takeover-viewer.md).

**The viewer needs a secure context.** It decodes with WebCodecs, which browsers
offer only on HTTPS or loopback. The plain-HTTP port works wherever it is
reached as `127.0.0.1` - `cuttle up`, the `ssh` and `k8s` tunnels, a
`port-forward`. Anywhere else, put it behind an HTTPS proxy (an ingress,
`tailscale serve`) and set the `direct` context's `vnc_url` to the `https://`
link. The proxy must forward WebSocket upgrades and keep the browser's `Host`
header, which the viewer compares against `Origin` before accepting its
socket. Under a path prefix, strip the prefix on every request, the socket
included, and serve the page with its trailing slash: the page opens its
socket at `api/websockets` beside itself.

**Which screen.** The browser may only claim a screen its persona ships with: the
amd64 image is a Windows desktop (1920x1080, 1366x768, 1440x900), the
arm64 image a MacBook Air (1440x900, 1470x956, 1710x1107 - its default scaled
resolution, and the GPU and cores follow the model); the window is that screen
minus the OS taskbar. A session browser
claims the largest by default - one human-facing window wants room. Pick another
with `cuttle up --screen 1366x768`, or durably per context with `screen = "..."`
in `config.toml` (the flag wins); anything off the table is refused with the list.
Changing it on an existing profile changes only the screen and window, not the
logins or the rest of the fingerprint. Pool mode keeps one screen per seed so a
fleet of identities does not all report the same monitor.

## The driver in the image

The image bundles playwright-cli 0.1.23, pinned by `PLAYWRIGHT_CLI_VERSION` in
`packages/browser/versions.env` and the Dockerfile ARG it feeds, so the driver is
version-locked to the image the way the browser is. `cuttle pw <args>` (long form
`cuttle playwright-cli`) hands the args to it verbatim inside the container -
stdin, stdout, stderr and the exit code pass straight back:

```bash
cuttle pw goto https://example.com            # any verb; it connects on its own
cuttle pw screenshot --filename=shot.png
cuttle downloads shot.png                     # pull the file to this host
cuttle pw detach                              # optional; the browser stays up
```

- **It execs into the container, so it works on every backend that has one**:
  `docker exec` for `local`, the same through the tunnel for `ssh`, `kubectl exec
  deploy/<release>` for `k8s`. The `direct` backend has no container to enter and
  says so - run a driver of your own against that CDP endpoint instead.
- **It can only attach.** The image sets `PLAYWRIGHT_MCP_CDP_ENDPOINT` to the
  daemon's in-container endpoint, which routes every browser acquisition through
  `connectOverCDP` - `open` included - and the wrapper refuses the `--endpoint` /
  `--extension` flags that could name another one. A driver that spawns its own
  browser - the failure that reads as a logged-out page - is not reachable from
  here.
- **No attach step.** When a verb finds no live session the wrapper runs
  `attach --cdp=<in-container endpoint>` for it and retries once, so the first
  command of a session is whatever you actually wanted to do.
- **State lives in the container and persists across invocations.** That attach
  spawns a session daemon beside the browser; every later `cuttle pw` is a thin
  client of it, which is why tabs, refs and page state survive between commands.
  `detach` and `close` stop that daemon only, leaving the browser running.
- **The session daemon does not survive a container restart.** The browser's
  profile is durable; the driver session deliberately is not - it cannot outlive
  the browser it attached to. Nothing to do about it: the first verb after
  `cuttle up` or `cuttle up --recreate` reconnects transparently.
- **If the browser dies mid-session, the next verb reconnects - to cuttle's own
  replacement.** Killing the browser takes the session daemon with it, so the
  next `cuttle pw` verb sees the same "is not open" the on-demand attach already
  answers: it re-attaches to the browser cuttle has brought back and runs what
  you asked for. The tabs and page state of the dead session are gone; the
  profile is not. Nothing drifts onto a browser of its own here - there is no
  playwright-managed browser in the image, and with the endpoint unset the driver
  fails loudly instead of launching one. The smoke harness kills the browser
  under a live session and asserts the driver lands back on cuttle's, UA and all.
  The replacement reuses the same profile dir, so `--filename` output written
  before the death is still there afterwards (`--ephemeral` is the exception: its
  profile is a scratch dir that goes with the browser it belonged to).
- **`--filename` outputs land in the downloads dir.** The exec workdir is the
  session's download directory, so a screenshot, PDF or saved snapshot comes back
  out with `cuttle downloads <name>` like a page download. The driver's own
  auto-named output goes to a `.playwright-cli/` dotdir the listing hides.
- **A snapshot lands on this host.** playwright-cli writes the page snapshot
  after `goto`, `click`, `press` and the like to a file in the container and
  prints only a link to it; `cuttle pw snapshot` is given a `--filename` in
  that same dir, so it links a file too instead of printing the whole tree
  (the first 40 lines follow the link). In the same exec, `cuttle pw` fetches
  the file through the daemon's loopback `GET /snapshot` route into
  `$XDG_STATE_HOME/cuttle/<instance>/snapshots/` (default `~/.local/state`; dir
  0700, files 0600, the newest 50 kept) and rewrites the link to that path.
  `<instance>` is the `--name` (the context name on k8s), so the default
  instance's copies sit under `cuttle/cuttle/` beside the other instances';
  discarding the profile (`down --purge`, `purge-profile`, `up
  --purge-profile`) removes that instance's dir with it. The
  daemon replaces every value its secret store holds for the session with its
  `{{cuttle:NAME}}` sentinel first, and the current value of every
  `<input type=password>` on the session's open pages (same-origin iframes
  included, read over CDP at fetch time) with `{{cuttle:password-field}}` -
  exact matches only (plain or as the snapshot quotes them), values under 4
  characters (6 if all digits) left alone. A page the daemon cannot read in
  time is served masked by held values alone. The verb's own stdout, and a
  `snapshot` with `--raw`, `--json` or its own `--filename`, are not masked. If
  the fetch fails the link is left pointing into the container, and a
  file-backed `snapshot` says so on stderr, naming `--raw snapshot`. The driver's
  console log (`.playwright-cli/console-<ts>.log`) is not copied: the printed
  line points at `cuttle pw console` instead.
- **Driver help is per verb.** A leading `cuttle pw --help` (or `-h`) prints
  cuttle's wrapper help; `cuttle pw <verb> --help` passes through and prints the
  driver's own help for that verb. playwright-cli 0.1.23 has no `help` or `docs`
  verb.
- **Never re-run `attach` or `open` mid-session.** Either one stops the running
  driver session and starts a new one, dropping every ref it minted (the
  browser, its tabs and logins stay); `open` with no URL also navigates the
  current tab to `about:blank`. The wrapper attaches on its own when there is no session, so
  neither is ever needed - `goto` is how to load a page.

## Autonomous browsing loop (jev-browse)

**Experimental.** Its flags, output and exit codes may still change.

`cuttle jev-browse` drives that same bundled driver on its own: it reads the
page's accessibility snapshot, asks a System-One decision model (TypeSafe's Jev)
which element to act on next, and has playwright-cli perform the pick - then
snapshots again. There is no LLM in the loop and no generated text: a decision is
one typed choice with a calibrated confidence, and a page where nothing makes
progress - or an answer naming an element the page never offered - ends the run
instead of being guessed at.

```bash
cuttle jev-browse --task 'download the latest invoice' \
  --url https://example.com/billing --max-steps 25
cuttle jev-browse --task 'sign in' --text user=qa@example.com \
  --text pass='{{cuttle:QA_PASS}}'          # only the NAMES are sent
cuttle jev-browse --task 'list the open tickets' \
  --extract 'one ticket, with its id and title' --json
cuttle jev-browse --task '...' --mock       # no key, no model call
```

Exit codes are the result: `0` the task is done, `1` an error, `3` blocked (a
person is needed), `4` the step budget ran out. The task can also be given as
the first argument (`cuttle jev-browse 'sign in'`), as sugar for `--task`.

- **`--url` is required on a fresh session.** Without it the run starts wherever
  the browser already is, and a blank tab is refused up front ("the session has
  no page") rather than spending a step on it.
- **The model judges the page; the loop acts.** A run ends as done only when
  the model rates the current page as finishing the task (0.8 or more), from
  the page's address, title, headings, first ~120 lines of text and every
  control - so a task framed as finding or reading something can end done on
  the page that holds the answer; read it with `--extract` or `cuttle pw
  snapshot`. Every control is offered, links to a section of the same page
  included; what was already tried is in the history the model sees, with
  whether it changed the page, and it is asked not to repeat it. A `none` - the
  model finding no useful action - ends the run with 3 at the confidence it
  gave; it never implies done.

- **Writes are refused on the chosen action, not pruned from the offer.** The
  loop runs on real signed-in accounts. A control whose own name carries an
  irreversible verb (pay, buy, purchase, checkout, place order, transfer,
  donate, delete, remove, send, post, publish, sign out, unsubscribe; for a
  link or tab only when its name leads with it) is refused outright; so is
  Enter when a button of the form or dialog holding the focus carries one.
  Typing into a box is never refused - a "Post code" field is named by its
  content, and the submit is where a write happens. Every other pick, Enter
  included, costs one more model call: "does taking this change something on
  the site rather than navigate, open, sort or filter?", refused at 0.2 or
  more. A refusal goes into the history as `refused` and the model picks again;
  the same pick on the same page is refused without asking, and that second
  refusal is printed and ends the run with 3, naming the write the task needs,
  for a person to take with `cuttle pw`. The word list errs toward refusing and
  is not a guarantee.
- **Options carry their section and state.** Each control is offered as
  `[<landmark>] <role>: <name>` (e.g. `[banner: Site] searchbox:
  Search`) with its filled/checked/expanded state, so a site-wide search box and
  a form's own box are different choices. While a dialog is open only its
  controls are offered.
- **One persistent driver client per run.** Verbs go to the playwright-cli
  session daemon through a single long-lived client inside the container instead
  of one exec each - same session, same humanized input as `cuttle pw`. Anything
  it does not handle (attach, open, a client that fails to start) falls back to
  an exec. The lease renew before each driving verb goes through it too, so a
  takeover still stops the run before its next action. `--json` reports
  per-step timings (`model_ms`, `driver_ms`, `api_calls`, `spawns`, `verbs`,
  `input_tokens`, `output_tokens`); the outcome adds the run totals,
  `elapsed_ms` and `model`.
- **The key is an environment variable, and only that.** `export
  CUTTLE_TYPESAFE_API_KEY=...` in the shell that runs `cuttle` - there is no flag
  and no config key for it, so it never reaches argv or `config.toml`. It is read
  on this host; the container never sees it. A value that must come from a vault
  goes in the same way as anything else here, e.g. `export
  CUTTLE_TYPESAFE_API_KEY=$(op read op://vault/typesafe/api-key)`.
- **An OpenRouter key works in the same variable.** OpenRouter resells the same
  model, and a key beginning `sk-or-` routes itself there - same variable, no
  flag and nothing to configure. The first-party API is the default for every
  other key. The OpenRouter model is pinned to an exact version rather than the
  `latest` alias, so an upgrade is a release of cuttle and never a silent change
  of judgement mid-run.
- **`--mock` needs no key.** It exercises the whole loop - snapshot parsing,
  element filtering, the driver calls, the exit paths - with a local decider that
  picks the first plausible element. Only the judgement is mocked: it clicks and
  types on the live page for real, so point it at a page you are willing to have
  it press the first button on.
- **It shares the driver session with `cuttle pw`.** Same session daemon, same
  tabs, same refs. When the run ends blocked (exit `3`: an escalation, a login
  wall, a native dialog) or runs out of steps (exit `4`), the browser is left
  exactly where it stopped, so the next step is a plain `cuttle pw snapshot` (with
  the same `--name`/`--context` as the run) and manual verbs from there - nothing
  to re-navigate, nothing to re-authenticate.
- **Humanized input stays on.** Every action the loop takes goes through the same
  humanization as any other driver action; there is no fast path for it, by
  design (see `docs/knowledge/decisions/humanize-over-speed.md`).
- **What is fast is the decision, not the action.** A choice comes back in a few
  hundred milliseconds where an LLM round trip is seconds, and that is where the
  saving is. The action is unchanged: a humanized click is about half a second,
  typing about an eighth of a second per character, plus whatever the page takes
  to load. Over a 20-step run the wall clock is dominated by the browser, not by
  the model, so expect a modest end-to-end speedup - the large factor is in
  tokens and per-decision latency, not in how fast the page gets driven.
- **Secrets work as everywhere else.** `--text` supplies what may be typed and
  only the NAMES are sent to the model - the value is looked up here afterwards
  and handed to the driver verbatim, so a `{{cuttle:NAME}}` sentinel from `cuttle
  secret set` passes through untouched and is substituted inside the CDP frame
  (see "Secrets the session types for you" below). Element labels and page text
  are data throughout: quoted into the log, offered as choices, never followed as
  instructions. A `--text` value is argv, visible in the host's `ps` while the run
  lasts; pass a secret as a `{{cuttle:NAME}}` sentinel to keep it out. One
  residual either way: a value typed into a box whose form submits with GET ends
  up in the page URL, and the URL is what the step log prints and what the next
  request names the page by - so a search box is not a place to put a credential.
- **`--extract` picks items, it does not answer.** On the page the run ended
  on, whatever the ending (done, blocked or out of steps), the model judges each
  line against the description and the matching lines are printed verbatim;
  headings, labels and descriptive prose are deliberately never picked.
  It works best for list-shaped answers (tickets, rows, results) and returns
  nothing useful for a question whose answer is a sentence. It needs the model,
  so it is refused with `--mock`.

## One driver at a time (the session lease)

Two drivers on one page interleave clicks and navigations into nonsense, so the
daemon hands out a driving lease per browser. `cuttle jev-browse` takes it for
the whole run, renews it every 40 seconds and before every action it takes, and
releases it on the way out, Ctrl-C included.

- **A second driver is refused, with who and how long.** Another `jev-browse`
  exits `1` without touching the page, and `cuttle pw` refuses verbs that drive
  it (click, fill, goto, eval, anything unrecognized); both name the holder and
  how long it has held the browser. Read verbs - `snapshot`, `console`,
  `tab-list`, `screenshot`, the cookie/storage/request listings - still run, so
  a person can watch a run without stopping it.
- **Takeover is explicit.** `cuttle jev-browse --takeover ...` or `cuttle pw
  --takeover <verb> ...` (the flag goes before the verb) frees the lease first.
  The evicted run notices before its next action and exits `1` with "session
  was taken over by <who>", leaving the page where it was. In front of a read
  verb it takes nothing over, since a read never needed the lease.
- **A holder that dies frees the browser by itself.** The lease lasts 120
  seconds without a renew, so a killed run blocks nobody for longer than that.
  Nothing is persisted: a daemon restart clears every lease, along with the
  browsers they guarded.
- **`cuttle pw` never takes the lease itself.** It only checks for a holder, so
  two agents both driving with `cuttle pw` are not serialized against each other;
  only a `jev-browse` run holds the browser.
- **Plain CDP clients are not gated.** The lease coordinates cuttle's own
  drivers; a client attached straight to the CDP endpoint does not ask. The
  HTTP surface is loopback-only (see "Secrets the session types for you" for
  what that enforces): `GET /lease` (status), `POST /lease?owner=` (acquire;
  `&token=` renews; `409` names the holder), `DELETE /lease?token=` (release) or
  `?force=true&owner=` (takeover).

## Reading what the daemon did

`cuttle logs` prints the container's log (`docker logs` / `kubectl logs`) - the X
server, the viewer, Chrome's own stderr, and the daemon's lines.

That log is discarded when the container is replaced, so a **session daemon with a
durable profile** - what `cuttle up` runs by default - also writes its own lines
to `/data/logs/serve.log` inside the profile volume. That copy survives `cuttle up
--recreate` and image upgrades, which is what makes yesterday's incident still
readable today:

```bash
docker exec cuttle cat /data/logs/serve.log
```

It is capped at 20MB with one previous generation kept alongside it
(`serve.log.1`). Pool mode does not write it (a fleet server's stdout is already
collected by compose or k8s), and neither does `--ephemeral`, which mounts no
volume for it to survive in.

## The profile is durable

The session's full Chrome profile - cookies, localStorage, IndexedDB, service
workers - lives in a named Docker volume (`cuttle-<container>-profile`) or a k8s
PVC. It survives `cuttle up` restarts, `cuttle up --recreate`, and image upgrades,
with no flag. Reset it deliberately: `cuttle up --recreate --purge-profile`,
`cuttle purge-profile`, or `cuttle down --purge`. `--ephemeral` opts out for a
disposable session. A plain `cuttle down` never touches the volume.

**A stop saves what the profile has not.** Chrome writes cookies to its profile on
a ~30s timer and localStorage on ~5s, so a login made moments before a stop lives
only in the browser's memory. On the way down the daemon snapshots each browser's
cookies over CDP first and restores them at the next launch, which is why logging
in and immediately running `cuttle down` still leaves you logged in. That ordering
is why the daemon puts Chrome and the X server in their own process groups: the
container's init signals the whole group at once, and a browser that dies in the
same instant as the daemon has nothing left to snapshot. Every seed is captured
concurrently under one 8s budget, inside the stop grace that docker (`-t 15`) and
k8s (30s by default) allow.

**Creation-fixed settings.** `--image`, the persistence choice, the host ports,
`--idle-timeout`, `--humanize`, `--allow-context-creation` and `--block-third-party-cookies` are
baked into the container at creation. (`--idle-timeout` reaps per-seed browsers
in a pool; a session daemon ignores it with a warning, since reaping the one
browser would empty the viewer.) Passing them against an existing container warns
and is ignored; use `--recreate` to change them. (On k8s they re-apply on every
`helm upgrade`.)

**Third-party cookies are allowed, like stock Chrome.** The upstream
ungoogled-chromium patch series compiles in a block-by-default
(`extra/inox-patchset/0006-modify-default-prefs.patch` re-registers
`kCookieControlsMode` as `kBlockThirdParty`), which is right for a privacy browser
and wrong for one whose job is to look like Chrome and keep logins: blocking
breaks embedded SSO, silent token refresh in an iframe, and some payment and
captcha challenges, all of which load but never finish. The daemon writes the pref
into every seed's profile on each launch, putting it back on stock behavior.
`cuttle up --block-third-party-cookies` (or `CUTTLE_BLOCK_THIRD_PARTY_COOKIES=1`,
or `blockThirdPartyCookies: true` in the chart) restores blocking for a
privacy-hardened profile.

## Secrets the session types for you

`cuttle secret` is the **host** half of daemon-owned secrets. You hand the running
session a value under a name; a driver then fills the sentinel `{{cuttle:NAME}}`
and the daemon substitutes the real value inside the CDP frame. The value never
enters a driver command line, an agent's context, or a log.

```bash
op read op://vault/github/password | cuttle secret set GH_PASS --stdin
cuttle secret set GH_TOTP --exec 'op item get GitHub --otp'   # registers a resolver
cuttle secret refresh GH_TOTP                                 # re-runs it, fresh TTL
cuttle secret prompt SMS_CODE                                 # ask the human, echo off
cuttle secret ls                                              # names and shape, never values
cuttle secret rm GH_PASS                                      # value AND resolver
```

- **A value only ever travels on stdin or in a request body.** Never in argv,
  which is world-readable in `/proc` and lands in shell history. `set` takes
  `--stdin` or `--exec`, never a positional value, and no verb prints a stored
  value back - `ls` reports source, state, length and origin only. The one verb
  that will take a value in argv is `jev-browse --text name=value`, which is
  exactly why a secret there belongs in a `{{cuttle:NAME}}` sentinel instead.
- **Resolution happens here, not in the container.** The daemon has no vault, no
  keychain and no biometrics, and there is no daemon-to-host callback, so
  `--exec` runs the command on this host at `set` time. Its stderr is discarded
  on purpose: vault error text routinely quotes item names and partial values.
- **`--exec` writes a recipe to your config file**, at
  `~/.config/cuttle/config.toml` (`$XDG_CONFIG_HOME`), as a plain table of name to
  command. The **command** is stored, never the value:

  ```toml
  [secret]
  GH_TOTP = "op item get GitHub --otp"
  ```

  That file is what makes `refresh` work after `cuttle down && cuttle up` - the
  daemon's memory is gone, the recipe is not. Treat it like any other dotfile
  holding a vault query: it is not a credential, but it names one.
- **Values live in daemon memory, per seed, under a TTL** - 15 minutes by default,
  `--ttl` to change it, clamped at 12 hours. Never on disk, never in the profile
  volume, never in a snapshot. A container stop, `cuttle down`, or a browser
  restart drops every value.
- **Expiry keeps the name.** A fired TTL clears the value but leaves the
  registration, so the substitution error can say "run `cuttle secret refresh
  NAME`" instead of "unknown name". This is why a TOTP works at all: resolve it
  at `set` time and it is dead in 30 seconds, so `refresh` immediately before the
  fill is the intended shape.
- **Every verb acts on ONE session.** With more than one running, name it:
  `cuttle --name staging secret ls` (its ports are discovered). Without `--name`
  (or `--context`) you are targeting the default session, which on a busy host is
  not necessarily the one being driven.
- **The routes are loopback-only, judged by the connection.** The daemon
  answers the secret, capture, grab, auth status, downloads, snapshot, window
  raise, lease and per-seed state routes only when the TCP peer is a loopback
  address (`127.0.0.0/8`, `::1`, IPv4-mapped included), and then only with a
  loopback `Host` and no cross-origin `Origin` - the DNS-rebinding defense. A
  peer elsewhere is refused with `403` whatever headers it sends, so another
  container or pod that can reach the port cannot read the store. That is why
  the CLI reaches these routes from inside the container: on `local` and `ssh`
  with curl over the same `docker exec` as `cuttle pw` (a `-p 127.0.0.1:PORT`
  mapping delivers the host's connection from the docker bridge gateway, not
  loopback), and on `k8s` over the port-forward, which connects to localhost
  inside the pod. A `direct` context gets these verbs only when the daemon sees
  this host as loopback - a native `cuttle serve` or a `kubectl port-forward`;
  through a docker port mapping, an `ssh -L` to one, or another host they answer
  `403`, so use the `local` or `ssh` backend there. The CDP endpoint itself
  (`/json/*`, the WebSocket) is not loopback-only: anything that can reach the
  port can still drive the browser, so keep it published narrowly - see
  "Running on a server".

## Getting bytes out without a screenshot

Reading a credential back is the leak this pair exists for: a snapshot taken to
debug a failed login, with the value still in the field.

```bash
cuttle secret capture API_KEY --selector '#new-token'                  # into session memory
cuttle secret capture API_KEY --selector '#new-token' --to file:key.txt
cuttle secret capture API_KEY --selector '#new-token' --to exec:'gh secret set API_KEY'
cuttle secret capture API_KEY --from-clipboard                         # after a "copy" button
cuttle grab https://app.example.com/export.csv out.csv                 # authenticated fetch
cuttle downloads --latest --wait 30s                                   # pull without naming it
```

- **`capture --to memory` is the default** and the cheapest: the value stays in
  the daemon under a TTL, ready to be filled as `{{cuttle:NAME}}`, so the
  common generate-on-A, type-into-B flow never lets it out of the container.
- **`file:` writes 0600 and atomically**, and **refuses a path inside a git
  working tree** (`--force` overrides). `os.WriteFile`'s mode applies only on
  create, so writing over an existing scratch file would otherwise leave a
  credential world-readable. `exec:` puts the value on the command's **stdin**,
  never in its arguments.
- **`grab` is cookie auth only.** It fetches from inside the browser with that
  origin's cookies and no `Authorization` header, so a token-auth API is out of
  reach this way. With a destination it writes 0600 and prints the path instead
  of the body.
- **`cuttle auth status`** reports, per domain, how many cookies the profile
  holds and when the first expires - never names or values. Cookies are not proof
  of a valid session, so treat a hit as "probably still signed in, navigate and
  look". It is the cheap check that stops a session re-running a login it did not
  need, and every avoided login is a credential handling event avoided.
- **`cuttle open --wait` / `--until`** hold the terminal while a human finishes
  something in the viewer - a captcha, an SSO redirect, a device approval -
  instead of the caller polling. `--until` takes `url:<glob>`, `gone:<glob>`,
  `title:<substring>` or `js:<expression>`.

## Picking ports

The browser verbs take `--cdp-port` and `--vnc-port`. Use them when the defaults
are taken:

```bash
cuttle up --cdp-port 9444 --vnc-port 6099
```

- **Ports are pinned only when `up` creates the container.** Every later verb -
  `up` itself (a restart, an idempotent `up`, `--recreate`), `status`, `open`,
  `downloads`, `secret`, `auth` and `grab` - discovers the instance's own ports,
  running or stopped, so afterwards you target it with just `--name` (and
  `--context`). `status` and every verb that needs the live session refuse a
  stopped instance and name the `up` that resumes it; none falls back to the
  default ports, which may be another instance's. `pw` and `jev-browse` exec
  inside the container and use no host port at all.
- **Port-shadow gotcha:** `docker run` errors on a docker-vs-docker clash, but
  **not** when a *native* process already owns the host port. `cuttle up` then
  prints a mapping that is silently dead - your client hits the other process.
  Verify with `lsof -nP -iTCP:<port> -sTCP:LISTEN` (want OrbStack/Docker), or check
  `curl http://127.0.0.1:<port>/json/version` names the engine you expect.
- **Moving an instance's ports takes `--recreate`.** A restarted container keeps
  the ports it was *created* with, so `--cdp-port`/`--vnc-port` on a plain `up` of
  an existing one warn and are ignored; `up --recreate --cdp-port N --vnc-port M`
  rebuilds it on the new ones (the persistent profile survives).

**`--name` is the other axis.** It runs a **separate** docker (local/ssh) instance -
its own container, profile volume and tunnel - so unrelated persistent sessions can
sit side by side. Give each its own ports and pass the same `--name` to every verb.
Two logged-in sessions you want to keep at once (two accounts on one site, say) are
two `--name` containers. For many disposable *identities* driven by code, run pool
mode (below) instead of multiple containers.

**Selecting an instance, once.** `--context` and `--name` are flags on `cuttle`
itself, not on a particular verb, so everything that reaches an instance honors
them - `cuttle pw` and `cuttle jev-browse` as much as `up`/`status`/`secret`. Each
resolves flag > env var > config > built-in default:

| | flag | env | config | default |
|---|---|---|---|---|
| context | `--context` | `CUTTLE_CONTEXT` | `default_context` | `local` |
| container | `--name` | `CUTTLE_NAME` | the context's `name` | `cuttle` |

```bash
cuttle --name scraper up --cdp-port 9444 --vnc-port 6099
cuttle --name scraper pw snapshot        # before the verb: see below
export CUTTLE_NAME=scraper               # or select it for the whole shell
cuttle status
```

`cuttle pw` passes every argument to the bundled driver verbatim, so cuttle's own
`--context`/`--name` have to come **first**, ahead of the driver's verb and any
driver flag (`cuttle --name scraper pw snapshot`); from the first driver arg on,
everything is the driver's. Every other verb takes them in any position. A
context that stands for one instance can carry the name instead (`name =
"scraper"` in its stanza), and then plain `cuttle --context box pw snapshot`
reaches it.

## Pool mode

For many isolated identities behind one endpoint - no CLI, no viewer - run the
container directly with `--mode=pool` and select a seed per connection:

```bash
docker run --rm -p 9222:9222 ghcr.io/glim-sh/cuttle:latest \
  cuttle serve --headless=false --mode=pool --idle-timeout=600
```

```
http://127.0.0.1:9222?fingerprint=12345
http://127.0.0.1:9222?fingerprint=12345&timezone=America/New_York&locale=en-US
```

Each distinct `fingerprint` seed gets its own isolated Chrome with a stable,
coherent identity; point one CDP client per seed at the seed-parameterized URL.
Pool mode **requires** the seed: an unseeded connect or `/json/version` is a 400,
so a probing client can never spawn a direct-egress default browser by accident.
`--fingerprint=<seed>` on the server names a seed for unseeded connections if you
want one. Nothing in pool mode bounds how many seeds run at once except
`--idle-timeout` (set it) and your client's own seed ring, so size the container's
memory for the number of seeds you actually cycle. The mode is `CUTTLE_MODE` as an
env var and, like `--idle-timeout`, is fixed for the life of the container.

**Proxy per seed:** pass an authenticated proxy on the connect URL - cuttle strips
the inline credentials and answers the proxy `407` over CDP, so fork binaries that
reject inline creds still work. WebRTC shows what Chrome shows without a camera
grant - a `.local` host candidate and a srflx - with the srflx at the seed's exit
IP (the proxy's under `geoip` or `webrtc-ip=auto`, the host's own egress for a
direct seed), and sends no UDP packet from the real interface. Real-time media
connects only through a TURN server over TCP or TLS, which Chrome dials through
the proxy; a page's own STUN server can tell the srflx was never asked for. A
proxied seed whose exit IP does not resolve pins a WebRTC handling policy
instead, so ICE gathers nothing and WebRTC's own DNS lookups (a TURN hostname)
fail rather than reach the local resolver. A connection's own `?proxy=` keeps
the server's default timezone (`CUTTLE_FINGERPRINT_TIMEZONE`, else UTC) unless it
also passes `geoip=true` or `timezone` and `locale` - set them together so the
identity is coherent. `CUTTLE_PROXY` sets
a server-level default for every seed, and its geo follows it without `geoip`.

## Running on a server

The amd64 image runs native on any Linux server:

```bash
docker run -d --restart unless-stopped --name cuttle \
  -p 127.0.0.1:9222:9222 --shm-size=2g \
  -v cuttle-profile:/data -e CUTTLE_KEEP_PROFILE=1 \
  ghcr.io/glim-sh/cuttle:latest
```

Bind CDP to `127.0.0.1` and reach it over an SSH tunnel
(`ssh -L 9222:localhost:9222 user@server`) - the tunnel is the auth boundary, CDP
has none of its own. `--shm-size=2g` avoids Chrome crashes under load. Add
`-p 127.0.0.1:6080:6080 -e CUTTLE_VNC=1` for the viewer. The `ssh` backend
automates exactly this from the CLI.

The tunnel reaches CDP, not the daemon's loopback-only routes (secrets,
downloads, snapshots, per-seed state, the lease): docker delivers a mapped
port's connection from the bridge gateway, which those routes refuse. The
`ssh` backend calls them over `docker exec` instead; a client of your own, in
pool mode too, calls them from inside the container's network namespace - an
exec (`docker exec cuttle curl -s 'http://127.0.0.1:9222/downloads?fingerprint=<seed>'`)
or a sidecar sharing the pod's network.

**Keep the volume AND `CUTTLE_KEEP_PROFILE=1` together.** The volume alone is not
a durable identity: without the env the daemon treats the profile as disposable,
so every browser launch gets a new fingerprint seed and the saved logins are
re-injected onto it - one account on a shifting device, which is what account
anti-fraud correlates. The startup log states which one you have (`session
profile is durable` / `disposable`), and `GET /` reports `durable_profile`.
`cuttle up` sets both; a hand-written `docker run` must too.

**A proxy for a signed-in account:**

- Set it with `-e CUTTLE_PROXY=http://user:pass@host:port`. Timezone, locale and
  the WebRTC address follow the proxy's exit IP on their own; pin them with
  `CUTTLE_FINGERPRINT_TIMEZONE` / `CUTTLE_FINGERPRINT_LOCALE` only when the exit
  does not geolocate where it really is.
- Keep the exit sticky: one account, one exit region. A login that moves between
  regions reads as a takeover.
- Never route the browser through a TLS-intercepting proxy (an egress gateway
  that injects credentials by terminating HTTPS with its own CA). The site then
  sees the gateway's TLS handshake, often HTTP/1.1 only, under a Chrome user
  agent - a mismatch no fingerprint setting can repair. Give Chrome a plain
  `CONNECT` tunnel to the proxy; check `https://tls.peet.ws/api/all` from the
  browser if unsure.

**The viewer is loopback-only and unauthenticated.** It serves plain HTTP; the
`-p 127.0.0.1:PORT` mapping is the security boundary. Whoever reaches the port
drives the browser, and can also take screenshots, record the session and
disconnect other viewers through its API. Never bind it publicly; reach it from
elsewhere through a tunnel or an authenticated HTTPS proxy (see "The viewer
needs a secure context" above).

**Probes and metrics** live on the CDP port and never launch a browser:

- `GET /healthz` - liveness: 200 while the daemon serves HTTP and its pool lock
  can be taken; 503 `wedged` if the lock is stuck. Use it for `livenessProbe` and
  `docker --health-cmd`. Do not probe `/json/version`: that endpoint launches a
  browser on demand, and in pool mode refuses an unseeded request outright.
- `GET /readyz` - readiness: 200 when attaches will work; 503 with a `reason`
  while the daemon drains at shutdown, when a headed browser's X display is
  gone, when the data dir is not writable, or when the session browser has
  failed to launch 3 times in a row.
- `GET /metrics` - Prometheus text: `cuttle_browsers_active`,
  `cuttle_cdp_connections_active`, `cuttle_cdp_attaches_total`,
  `cuttle_browser_launches_total{result}`, `cuttle_browser_launch_seconds`,
  `cuttle_browser_exits_total{cause}`, `cuttle_state_captures_total{result}`,
  `cuttle_state_injects_total{result}`, `cuttle_state_inject_seconds`, plus the
  Go runtime and process collectors.
- `GET /` - the CLI's briefing JSON (live browsers and their connections); it
  predates the probes and stays for `cuttle status`.

The helm chart wires both probes by default (`probes.*` in values.yaml).

## Remote agents over MCP

An agent in another container or on another host drives the session over one
authenticated HTTPS endpoint instead of raw CDP. `--remote-listen` opens a second
listener in the daemon; CDP, the loopback API and the viewer stay inside the
container, and the agent never holds a long-lived cuttle credential when an
egress gateway injects its bearer. Session mode only. Nothing changes for a
daemon started without it.

| Flag | Env | Meaning |
|---|---|---|
| `--remote-listen` | `CUTTLE_REMOTE_LISTEN` | listen address, e.g. `:8080`; off when empty |
| `--public-url` | `CUTTLE_PUBLIC_URL` | the HTTPS origin people and agents reach that port at; links are built from it and browser `Origin`s are checked against it, never against forwarded headers |
| `--principals-file` | `CUTTLE_PRINCIPALS_FILE` | one `<name> sha256:<hex>` line per caller |
| `--viewer-upstream` | `CUTTLE_VIEWER_UPSTREAM` | the loopback viewer the `/v/` proxy forwards to (default `http://127.0.0.1:6081`) |

With it on, the CDP listener binds `127.0.0.1` whatever `CUTTLE_HOST` says, the
image's entrypoint moves the viewer to `127.0.0.1:6081`, and the daemon runs the
bundled `playwright mcp` on `127.0.0.1:8931`, attached through its own CDP mux,
so humanized input and `{{cuttle:NAME}}` substitution apply to every remote
action. The child is restarted with backoff like Chrome.

**Principals.** A principal is a caller (an agent group). Mint a token outside
cuttle and keep only its hash in the pod:

```bash
TOKEN=$(openssl rand -base64 32 | tr '+/' '-_' | tr -d '=')
printf 'hq sha256:%s\n' "$(printf %s "$TOKEN" | sha256sum | cut -d' ' -f1)" >> principals
```

The file is re-read on `SIGHUP` and whenever its size or mtime changes (a
projected Secret update), and a missing or invalid file refuses every bearer
until it is valid again. Revoke a caller by deleting its line: its outstanding
links, viewer sessions (sockets included), file handles and browser session end
with the reload.

**Routes.** Each takes exactly one kind of credential:

| Route | Credential | What |
|---|---|---|
| `POST /mcp` | `Authorization: Bearer` | the MCP endpoint, stateless (no session id) |
| `GET /files/{h}` | bearer, same principal | a download handle from `cuttle_download`, valid 1 h |
| `GET`/`POST /t/{ticket}` | the one-time link | the handoff page; GET never spends it, POST does |
| `/v/...` | viewer cookie | the viewer, proxied (any path, any WebSocket) |
| `GET /healthz`, `/readyz` | none | the probes |

A bearer never opens the viewer and a viewer cookie never opens `/mcp`. Nothing
else is served on the remote port.

**What an agent gets.** Every Playwright MCP tool except `browser_resize` (it
would break the persona's screen), `browser_install` and
`browser_run_code_unsafe` (code in the Playwright process, inside the pod), plus:

| Tool | Does |
|---|---|
| `cuttle_status` | signed-in domains (shape only), stored secrets (names, source, bound origin), the lease holder, every tab and who opened it |
| `cuttle_viewer_link` | a one-time viewer link, 1-30 min (default 15) |
| `cuttle_wait` | waits for `url:<glob>` (`*` is any run, all else literal), `gone:<glob>`, `title:<text>` or `secret:<NAME>` across all tabs, up to 15 min, with progress every 15 s |
| `cuttle_secret_capture` | stores a value from the agent's tab (selector or clipboard) bound to that tab's origin |
| `cuttle_request_secret` | a one-time form link that asks the person for a credential |
| `cuttle_download` | size, type, sha256 and a `/files/{h}` url for a download; text under 40 KB inline |

The rules around them:

- **Own tabs.** A principal's first browser action opens a tab of its own, so no
  agent acts on the person's tab, and `browser_tabs` select or close is refused
  on a tab it did not open (popups of its tabs count as its own). All principals
  share the person's cookies - it is one personal browser.
- **The lease.** A driving call (any tool not marked read-only, and a capture)
  takes the session lease as `mcp:<principal>`; while `cuttle pw` or another
  principal holds it the call is refused with the holder's name and age, and
  read-only calls still pass. Free a stuck lease with `cuttle pw --takeover`.
- **Masking.** Every text result has each stored secret and each live password
  field's value replaced by its sentinel before it leaves the daemon. Images
  cannot be masked, so `browser_take_screenshot` is refused for 30 s on a tab a
  value was just captured from. `cuttle_download` never serves Playwright's own
  snapshot and screenshot files, which hold page text unmasked.
- **Recovery.** When the Playwright session behind a principal is lost the agent
  is told its refs and tab selection are gone; a call that lands while cuttle is
  relaunching Chrome is retried once. An upstream session idle for 30 minutes is
  closed and reopened on the next call.

**The browser is inside the boundary.** An agent steers a tab, and a page on
`127.0.0.1` would pass every loopback check the internal listener makes. So in
remote mode the internal listener refuses any request a browser makes to it
(one carrying `Sec-Fetch-Site`, or any Host that is not loopback, which is what
a DNS name for 127.0.0.1 sends), the Playwright MCP child accepts only a random Host
name the daemon generates per start, and the entrypoint installs a managed
`URLBlocklist` policy for `localhost`, `127.0.0.1`, `[::1]` and `0.0.0.0`. The
viewer on `127.0.0.1:6081` has no authentication of its own: a page reached
through a DNS name that resolves to 127.0.0.1 can still load it, which shows the
desktop and accepts input. Treat that as open until the viewer gets its own
credential.

**The handoff.** `cuttle_viewer_link` answers a `https://<public-url>/t/<ticket>`
link for the agent to send. Opening it shows who asked and why with one button,
so a chat app's link preview cannot spend it; the button POST redeems it once,
sets a `__Host-` session cookie for the rest of the link's lifetime and lands on
`/v/`. At expiry the daemon closes the viewer's socket. `cuttle_request_secret`
links to a form showing the principal, the name, the origin the value will be
bound to (editable) and the reason; the value goes to the session's secret store
(`source=human-form`) and is typed only into that origin. Links and viewer
sessions live in memory: a restart ends all of them.

**On Kubernetes**, set the chart's `remote.*` values:

```yaml
remote:
  enabled: true
  publicURL: https://cuttle.example.ts.net
  principalsSecret: cuttle-principals   # key `principals`
  ingressFrom:
    - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: ingress}}
```

The Service then exposes only the remote port, the NetworkPolicy admits
`ingressFrom` to it and nothing else, and both probes move to it. Put a TLS
ingress in front; a gateway that injects the bearer should do so on `/mcp` and
`/files/` only, never on `/t/` or `/v/`, which a person opens in a browser.
`metrics.podAnnotations` is ignored here: `/metrics` stays on the loopback CDP
port.

## Engine swap

The image bakes our stealth-Chromium build at `/opt/browser/chrome` and selects it
with `CUTTLE_BROWSER_BINARY`. Point that variable at another Chromium-family
binary present in the container to swap engines.

## Triage

- **"is running but CDP is not answering" / "started but CDP never came up."**
  Usually a stale container from a previous `cuttle up` that failed because the host
  port was taken. Current `cuttle up` auto-removes such zombies; on an older build
  run `cuttle up --recreate`. `cuttle status` prints a log tail with the real cause.
- **Graceful down matters.** `cuttle down` does `docker stop -t 15` so Chrome exits
  clean, which avoids crash-restore junk tabs. Never `docker rm -f` a running
  cuttle - the SIGKILL makes Chrome record a crash.
- **A crash on a `service_worker` target is a client bug, not detection.** Older
  `playwright-core` asserts on a service_worker target with no `browserContextId`.
  `cuttle serve` patches the shape so clients do not trip; with your own Playwright,
  pass `serviceWorkers: "block"` to `newContext`.
- **Chrome's container log noise is not a stealth failure.** `vkCreateInstance:
  Found no drivers`, `Automatic fallback to software WebGL`, dbus connect failures,
  `Failed to adjust OOM score` and `GPU stall due to ReadPixels` are expected on a
  headless host. Never add `--enable-unsafe-swiftshader`: it exposes the raw
  software renderer and makes the fingerprint worse.

### Phone keyboard smoke check

Install Playwright and its WebKit browser outside the image, then run against
a disposable viewer container (the script opens and closes its own test tab):

```sh
CUTTLE_PLAYWRIGHT_MODULE=/absolute/path/to/playwright/index.mjs \
CUTTLE_VIEWER_CDP_URL=http://127.0.0.1:9222 \
CUTTLE_VIEWER_URL=http://127.0.0.1:6080/ \
node packages/cuttle/test/smoke/phone-keyboard.mjs
```

On a physical iPhone, repeat field taps, ordinary text taps, password-manager
paste, SMS suggestions, composition, and remote blur. Check that the video
stays visible above the keyboard. DOM focus in WebKit emulation does not prove
that iOS displays the software keyboard.
