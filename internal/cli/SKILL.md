---
name: cuttle
description: Run and drive cuttle - a local stealth-Chromium browser farm with persistent logins, anti-detect fingerprints, and a human-handoff viewer for captchas and Cloudflare. Use whenever the user says to use the browser, or asks to automate, scrape, test, or sign into a website, or names agent-browser, browser-use (bu, bu-cli), or playwright-cli. `cuttle up` prints the live briefing with installed drivers, exact CDP attach commands, and each driver's own docs command. Attach to cuttle's warm session - never launch a fresh browser or new profile.
metadata:
  version: "0.10.0" # x-release-please-version
  image: "ghcr.io/glim-sh/cuttle"
allowed-tools: Bash(cuttle:*) Bash(just:*) Bash(docker:*) Bash(curl:*) Bash(agent-browser:*) Bash(browser-use:*) Bash(playwright-cli:*)
---

# cuttle: local stealth-browser CDP farm

[cuttle](https://github.com/glim-sh/cuttle) runs a patched CDP multiplexer
(`cuttle serve`) that spawns one stealth Chrome per fingerprint seed - each with
its own coherent identity (fingerprint, proxy, geoip, locale, timezone) - behind
a single CDP endpoint. The engine is a free stealth-Chromium fork (clark, MIT);
no proprietary binary. Point any CDP client
at it - agent-browser, browser-use, Playwright, `chromium.connectOverCDP`.

The `cuttle` CLI does not automate pages itself - cuttle is the farm, not the
scraper. It runs the browser in a Docker container (headed Chrome on Xvfb,
watched over VNC), either locally or on a remote host, and drives it over CDP.
Two ways to use it:

- **Daily driver / login handoff** - one persistent browser you watch (via the
  VNC viewer) and log into, driven over CDP. Use the CLI: `cuttle up`. Start below.
- **Multi-seed farm** - many isolated identities behind one endpoint, no viewer.
  Run the container and pick a seed with `?fingerprint=`. See [Multi-seed farm](#multi-seed-farm).

> **Apple Silicon:** the container image is linux/amd64 only, so on an arm64 Mac
> the local backend runs under emulation (slow, memory-hungry). For native speed,
> run the browser on a remote amd64 host with the `ssh` or `k8s` backend - see
> [Contexts and backends](#contexts-and-backends).

## Setup

cuttle needs Docker (or OrbStack) for the local backend; the ssh/k8s backends need
only their own client (`ssh` / `kubectl`+`helm`). The CLI is a single static Go
binary named **`cuttle`**:

```bash
brew install tenequm/tap/cuttle                        # homebrew cask (macOS/Linux)
go install github.com/glim-sh/cuttle/cmd/cuttle@latest # from source (needs Go 1.26+)
```

The container image is `ghcr.io/glim-sh/cuttle` and `cuttle up` pulls it on first
run. If you only want the raw farm without the CLI, run the image directly (see
[Multi-seed farm](#multi-seed-farm)).

Then, from any directory:

```bash
cuttle up      # start the container + VNC viewer (pulls the image if needed)
```

`up` (and `status`) print **the briefing**: CDP + viewer URLs, cuttle's
version, which driver CLIs are installed - with the exact attach command and
the self-doc command for each - plus routing rules and install advisories for
missing drivers. The briefing is the live source of truth; follow it over any
cached knowledge, including this guide.

`up` is idempotent and profile-preserving: a stopped container is **restarted**
(logins persist), not recreated. The endpoints are stable on every backend
cuttle manages (`direct` uses its configured URLs as-is) -
CDP `http://127.0.0.1:9222` and viewer `http://127.0.0.1:6080/`. For the ssh/k8s
backends `up` establishes a standing tunnel on those ports that outlives the
command (a detached forward tracked under `$XDG_STATE_HOME/cuttle/`); `status`
health-checks and re-establishes it, and `down` tears it down. So the URLs the
briefing prints are the same across invocations and safe to hand to a driver.

`cuttle up --idle-timeout <seconds>` closes an idle per-seed browser after that
many seconds with no CDP client attached (`0` = off, the default); the browser
respawns on the next connection. Like `--image` and the persistence choice
(`--ephemeral`), it is fixed at container creation - against an existing container
it warns and is ignored (`--recreate` to change it).

### Contexts and backends

A **context** names where the browser runs, selected by `--context` >
`CUTTLE_CONTEXT` > the config `default_context` > built-in `local`:

- **local** - Docker on this host (the zero-config default).
- **ssh** - a container on a remote host, reached over `ssh -L`. Inherits
  `~/.ssh/config` (keys, jump hosts), so no cuttle-specific ssh setup.
- **k8s** - a Deployment reached via `kubectl port-forward`. Inherits your kube
  config.
- **direct** - an already-running CDP endpoint, used as-is.

For the tunneled backends every CDP/VNC op still targets the stable local
`127.0.0.1:9222`/`:6080` - the backend owns the standing tunnel. `cuttle context
ls` lists contexts and marks the active one; `cuttle context add` writes a
context without hand-editing; `cuttle context --help` covers both. Create the
common cases with flags:

```bash
cuttle context add box --backend ssh --host user@box.example --default
cuttle context add cluster --backend k8s --namespace browser --release cuttle
cuttle context add tailnet --backend direct --cdp-url http://cuttle.example:9222
```

Contexts live in `$XDG_CONFIG_HOME/cuttle/config.toml` (default
`~/.config/cuttle/config.toml`) and can also be hand-edited - the only way to set
advanced k8s knobs (node_selector, tolerations, resources):

```toml
default_context = "box"

[context.box]        # ssh: docker on a remote amd64 host
backend = "ssh"
host    = "user@box.example"

[context.cluster]    # k8s: a Deployment via kubectl port-forward
backend   = "k8s"
namespace = "browser"
release   = "cuttle"
```

**Persistent default profile (default).** The bare default session (plain `up`,
no seed) is durable **by default**: its full Chrome profile - cookies,
localStorage, IndexedDB, service workers - lives in a named Docker volume
(`cuttle-<container>-profile`, mounted at the container's data dir) on the docker
backends, or a durable PVC on k8s. So the default login **survives `cuttle up`
restarts, `cuttle up --recreate`, and image upgrades** with no named profile and
no `--profile` flag. To reset it deliberately: `cuttle up --recreate
--purge-profile`, the standalone `cuttle purge-profile`, or `cuttle down --purge`
(full teardown). `--ephemeral` (or the legacy `--keep-profile=false`) opts out for
a disposable session that is discarded on recreate. A plain `cuttle down` (stop)
never touches the volume.

**Local-canonical named profiles.** For *named* seeds, auth state (cookies +
per-origin localStorage) is also mirrored canonically on your machine. The daemon
supervises checkpoints - it snapshots a seed the moment the last client detaches,
on a slow backstop timer, and at clean shutdown - and re-injects the snapshot when
the seed relaunches. A plain `cuttle down` also pulls each running named seed's
state into the local store (`$XDG_DATA_HOME/cuttle/profiles/<seed>/`) as the safety
net (skipped on `--purge`, an explicit discard). So `--recreate`, `--purge`, and
box loss never strand named logins. Drive a named identity with `--profile <name>`
(on `open`) or `?fingerprint=<seed>`.

### Picking ports (important)

The browser verbs (`up`/`down`/`status`/`open`) take `--cdp-port` and
`--vnc-port`. Use them when the defaults are taken:

```bash
cuttle up --cdp-port 9444 --vnc-port 6099
```

- The CLI is **stateless** - pass the *same* ports to `status`/`open`/`down` as
  you gave `up`, or they target the default 9222/6080.
- **Port-shadow gotcha:** `docker run` errors on a docker-vs-docker port clash,
  but **not** when a *native* process (e.g. a local CDP shim on 9333) already owns
  the host port. `cuttle up` then prints a mapping that is silently dead - your
  client hits the other process, not cuttle. Verify with
  `lsof -nP -iTCP:<port> -sTCP:LISTEN` (want the owner to be OrbStack/Docker), or
  just check `curl http://127.0.0.1:<port>/json/version` reports the engine you
  expect. Pick a genuinely free port when unsure.

## Drive it (drivers + routing)

cuttle serves standard CDP on `http://127.0.0.1:<cdp-port>`. Drive it with a
driver CLI - the briefing lists the ones actually installed, the attach command
for each, and the command that prints that driver's own usage guide.

- **Attach, never spawn.** Connect to cuttle's running browser and its default
  context. Never launch your own Chromium and never create a new profile or
  context - logins live in this one session and persist across restarts. cuttle
  is a CDP endpoint, not a Chrome binary: never point a driver's
  `--executable-path` at it, and do not pass a local `--profile` next to `--cdp`
  (drivers reject that - the profile lives in cuttle's container).
- **Prove you are attached before believing a login wall.** A driver that fails
  to attach does not error - it quietly drives its *own* fresh browser, and the
  symptom is a logged-out page, which looks exactly like a real logged-out state.
  `agent-browser connect <port>` is the known trap: on macOS it can relaunch a
  local Chrome (`[agent-browser] relaunched browser`), so pass `--cdp` on every
  command instead. To confirm you are on cuttle, check that the driver sees the
  session's existing tabs (`playwright-cli attach` prints them) or that
  `curl http://127.0.0.1:<cdp-port>/json/version` names the same browser.
- **Leave the user's tabs alone, and don't tear down mid-work.** The session is
  warm and shared - it may hold a half-finished login or a page the user is
  watching in the viewer. Open your work in a new tab rather than navigating the
  current one away, and close only the tabs you opened. Do not close tabs or
  detach/end the session until it is absolutely necessary, and NEVER while work or
  analysis on a page is still ongoing (mid-extraction, a multi-step flow, a page
  the user is examining) - closing eagerly loses scroll/DOM state and interrupts
  the task. Leave it open and attached until the work is truly done.
- **One driver at a time.** Every client attached to a cuttle session shares one
  browser and one set of tabs; two agents navigating in parallel clobber each
  other. Serialize browser work, or give each worker its own identity with
  `?fingerprint=<seed>` (see [Multi-seed farm](#multi-seed-farm)).
- **Input is humanized by default.** Mouse moves, clicks, scrolls, and typing are
  rewritten into human-paced motion (curved trajectories, off-centre clicks,
  right-skewed keystroke timing) before they reach Chrome, so interactions defeat
  behavioral detection. It is transparent - your clicks and keystrokes still land,
  events stay `isTrusted`, and the net typed text is unchanged - but interactions
  take human-realistic time (a click roughly half a second, typing roughly a fifth
  of a second per character): that pacing is the feature, not a hang, so do not
  treat a slow click/type as a stuck driver. Reads and navigation are unaffected.
  It is a daemon setting fixed at container start, not a per-command toggle - turn
  it off with `cuttle up --humanize=false` (or `CUTTLE_HUMANIZE=0`) when a trusted
  flow needs raw speed.
- **Read the site's API, not its DOM.** In a logged-in session the page already
  carries the cookies and CSRF token, so an in-page `fetch()` of the site's own
  JSON API (via the driver's `eval`) returns clean, complete data. Obfuscated or
  lazily-hydrated class names make CSS-selector scraping report "element not
  found" even when the content is on screen, and rendered text can silently
  differ from what the site actually stored.
- **A logged-in session is the user's real account.** Reads (navigate, snapshot,
  extract) are fine. Anything that writes - posting, commenting, reacting,
  sending, purchasing, changing settings - needs the user's explicit go-ahead in
  the current turn. Draft the content and hand it over; do not submit it.
- **Routing.** The briefing lists installed drivers in priority order; use the
  first one (playwright-cli by default) unless the user names another
  (bu / bu-cli / browseruse = browser-use; agent-browser). If the named driver
  is not installed, use the first listed instead and tell the user you fell back.
- **Driver docs are fetched, not memorized.** Each driver self-documents at a
  version-true source; the briefing gives the command per driver. Prefer the
  full outputs (with templates/examples) over compact ones, and never rely on
  a cached copy of another tool's docs.
- **No driver installed?** Stop and ask the user before installing anything.
  Default offer: all three; minimal: just agent-browser. Drivers attach to
  cuttle's browser, so skip their own browser downloads.

Raw CDP libraries work too:

```javascript
const browser = await chromium.connectOverCDP("http://127.0.0.1:9222");
const page = browser.contexts()[0].pages()[0];
```

## Log into a site (VNC handoff)

Log in **once** by hand via the VNC viewer; the profile keeps you logged in across
restarts while a CDP client drives the same live session.

```bash
cuttle open https://accounts.google.com
# navigates there, prints the briefing, opens the viewer, and returns immediately
```

`cuttle open [url]` is the human-handoff verb: it optionally navigates the running
session to `url`, prints the briefing, opens the viewer in your browser (pass
`--no-open` to just print the URL), and **returns right away** - it does not hold
the terminal. The session lives in the daemon and its login persists on its own;
`--profile <name>` only selects which seed to drive. Sign in via the viewer at
your leisure; nothing needs to stay running. (`login` and `connect` are deprecated
aliases of `open`.)

Open the viewer URL, sign in / solve the captcha there, and the CDP connection is
now logged in - VNC and CDP share one browser. This is why cuttle beats a fresh
headless browser for gated sites: the agent hits a wall, hands you the viewer
link, you sign in on the same session, nothing restarts.

## Lifecycle

```bash
cuttle status                       # container + CDP state
cuttle down                         # graceful stop; KEEPS the profile (and its volume)
cuttle up                           # restart the stopped container - logins still there
cuttle up --recreate                # fresh container; the persistent profile RE-ATTACHES (logins kept)
cuttle up --recreate --purge-profile # fresh container AND reset the profile (logins discarded)
cuttle purge-profile                # reset the profile (remove its volume); `up` after for a fresh session
cuttle down --purge                 # stop AND remove the container + delete the profile volume (full teardown)
cuttle up --ephemeral               # disposable profile (no volume), discarded on recreate
```

- **Persistence is the default.** The default profile lives in a named volume
  (docker) or PVC (k8s), so `--recreate` and image upgrades keep your logins.
  Reset it only on purpose: `--purge-profile`, `cuttle purge-profile`, or
  `cuttle down --purge`. A plain `cuttle down` never removes the volume.
- **Graceful down matters.** `cuttle down` does `docker stop -t 15` so Chrome
  exits clean; that avoids crash-restore junk tabs on next launch. Never
  `docker rm -f` a running cuttle - the SIGKILL makes Chrome record a crash.
- **Named seeds are local-canonical too** (see
  [Persistent default profile](#contexts-and-backends) above for the full story):
  `down` pulls named logins into the local store as a safety net.
- `--ephemeral` opts out of the persistent profile for a disposable session (the
  legacy `--keep-profile=false` is a synonym; `--keep-profile` is now the default
  and a no-op). The persistence choice is **fixed at container creation** - passing
  `--ephemeral`/`--keep-profile` against an existing container warns and is ignored;
  use `--recreate` to change it.
- **`--image` is creation-fixed too.** `--image` against an existing container
  warns and is ignored (it will not switch a running container to another image).
  `cuttle status` shows the container's real image; `--recreate` is the only way
  to change it (the persistent profile survives the recreate).
- **Do not reach for `--recreate` on a port error.** If `up` says "container
  restarted but CDP on :<port> never came up", suspect a port mismatch first: a
  restarted container keeps the ports it was *created* with. Run `cuttle status` -
  it prints the real port bindings and a log tail - then re-run `up` with those
  ports. (`--recreate` keeps the profile, but changing ports still needs the real
  ones.)

The browser verbs (`up`/`down`/`status`/`open`) take `--context`, `--cdp-port`,
`--vnc-port`, and `--name`; `up` also
takes `--image`, `--recreate`, `--purge-profile`, `--ephemeral`, and
`--idle-timeout`. For many
isolated identities on one host, use per-seed `?fingerprint=` (see
[Multi-seed farm](#multi-seed-farm)), not multiple containers. `--name` is the
other axis: it runs a **separate** docker (local/ssh) instance - its own
container, profile volume, and tunnel - so you can keep unrelated persistent
sessions side by side (give each its own `--cdp-port`/`--vnc-port`); pass the
same `--name` to every verb that should target it. `cuttle skill`
prints this guide to stdout, always matching the installed CLI.

## Multi-seed farm

For many isolated identities behind one endpoint - no CLI, no VNC - run the
container directly and select a seed per connection:

```bash
docker run --rm -p 9222:9222 ghcr.io/glim-sh/cuttle:latest
```

```
http://127.0.0.1:9222?fingerprint=12345
http://127.0.0.1:9222?fingerprint=12345&timezone=America/New_York&locale=en-US
```

Each distinct `fingerprint` seed gets its own isolated Chrome with a stable,
coherent identity; point one CDP client per seed at the seed-parameterized URL.
**Proxy per seed:** pass an authenticated residential proxy on the connect URL -
cuttle strips the inline credentials and answers the proxy `407` over CDP, so
fork binaries that reject inline creds still work. Set proxy, `timezone`, and
`locale` together so the identity is coherent. A server-level default proxy for
every seed can be set with `CUTTLE_PROXY`.

## Engine swap

The image bakes clark (`/opt/clark/chrome`, Chrome 148, the default). The
clearcote fallback (Chrome 149) is **not** baked: its build stage in
`ops/docker/Dockerfile` is commented out. To use it, re-enable that stage,
rebuild the image, and select the engine with
`-e CUTTLE_BROWSER_BINARY=/opt/clearcote/chrome`.

## Gotchas

1. **Headed by default, on purpose.** The image runs `cuttle serve --headless=false`
   on a built-in Xvfb; headed Chrome clears escalated anti-bot challenges headless
   cannot. Do not force headless.
2. **VNC is loopback-only, no auth.** The viewer serves plain HTTP; the
   `-p 127.0.0.1:PORT` mapping is the security boundary. Never bind it publicly.
3. **The reported Chrome version is a coherent major-only string, by design.**
   clark's binary is Chromium 148 and reports `Chrome/148.0.0.0` over CDP/UA (the
   container presents a Windows persona) - the `.0.0.0` build suffix is the common,
   coherent fingerprint every real Chrome sends, not a wrong build. Do not treat
   the reported version as a defect.
4. **"Logged in" can be false - but so can "logged out".** A CDP context may still
   render a login form (geo often defaults to the egress country, which also drives
   page *language* on logged-out pages - do not "fix" the language before checking
   the signed-in page). The reverse trap is more common: a cookie read that returns
   zero cookies is usually the driver probing its *own* blank page, not the site's
   tab, while the session is perfectly alive. Verify auth by navigating the tab to
   the site and checking what renders - and if the viewer shows you logged in,
   trust the viewer.
5. **Human handoff is the VNC viewer.** Login wall or captcha -> `cuttle open
   <url>` and sign in at the printed viewer URL (see
   [Log into a site](#log-into-a-site-vnc-handoff)).
6. **Sessions can be IP-bound.** A cookie minted at your real location, replayed
   through a proxy in another geo, may force re-login + 2FA. Match the proxy geo
   to where the session was created.
7. **"Container running but CDP not answering" / "restarted but CDP never came
   up."** Usually a stale container from a *previous* `cuttle up` that failed
   because the host port was taken (the failed run leaves a half-created
   container with no live port binding). Current `cuttle up` auto-removes such
   zombies and starts fresh; if you hit it on an older build, run
   `cuttle up --recreate`. `cuttle status` prints a log tail with the real cause.
8. **One failed load is not a verdict on the browser.** Escalated challenges are
   dominated by exit-IP reputation, not by the fingerprint: the same seed and
   flags can clear in ~7s on a clean exit and take ~200s or fail on a flagged one.
   If a page walls you, retry on a *new* identity (a different `?fingerprint=`
   seed, and a different proxy exit if you use one) rather than hammering the same
   one - do not conclude cuttle is broken from a single attempt.
9. **Chrome's container log noise is not a stealth failure.** `vkCreateInstance:
   Found no drivers`, `Automatic fallback to software WebGL`, `Failed to connect to
   the bus` (dbus), `Failed to adjust OOM score`, and `GPU stall due to ReadPixels`
   are all expected on a headless host and do not mean the browser is broken or
   detectable - a probe of the running seed returns a coherent spoofed GPU
   (an ANGLE/Direct3D11 vendor-renderer pair), not SwiftShader. Do not "fix" them,
   and never add `--enable-unsafe-swiftshader`: it exposes the raw software renderer
   and makes the fingerprint worse.
10. **A crash on a `service_worker` target is a client bug, not detection.** Some
    Chrome builds report a `service_worker` target without a `browserContextId`, and
    older `playwright-core` asserts on it inside a CDP handler, killing the process
    mid-run on a page that was loading fine. `cuttle serve` patches the target shape
    so clients do not trip; if you drive cuttle with your own Playwright and still
    hit it, pass `serviceWorkers: "block"` to `newContext`. Not a challenge failure.

## Running on a server

The amd64 image runs native on any Linux server:

```bash
docker run -d --restart unless-stopped --name cuttle \
  -p 127.0.0.1:9222:9222 --shm-size=2g ghcr.io/glim-sh/cuttle:latest
```

Bind CDP to `127.0.0.1` and reach it over an SSH tunnel
(`ssh -L 9222:localhost:9222 user@server`) - the tunnel is the auth boundary, CDP
has none of its own. `--shm-size=2g` avoids Chrome crashes under load. Add
`-p 127.0.0.1:6080:6080 -e CUTTLE_VNC=1` for the viewer on the server too. The
`ssh` backend automates exactly this from the CLI.
