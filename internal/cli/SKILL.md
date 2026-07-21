---
name: cuttle
description: Run and drive cuttle - a local stealth-Chromium browser farm with persistent logins, anti-detect fingerprints, and a human-handoff viewer for captchas and Cloudflare. Use whenever the user says to use the browser, or asks to automate, scrape, test, or sign into a website, or names agent-browser, browser-use (bu, bu-cli), or playwright-cli. `cuttle up` prints the live briefing with installed drivers, exact CDP attach commands, and each driver's own docs command. Attach to cuttle's warm session - never launch a fresh browser or new profile.
metadata:
  version: "0.6.0" # x-release-please-version
  image: "ghcr.io/glim-sh/cuttle"
allowed-tools: Bash(cuttle:*) Bash(just:*) Bash(docker:*) Bash(curl:*) Bash(agent-browser:*) Bash(browser-use:*) Bash(playwright-cli:*)
---

# cuttle: local stealth-browser CDP farm

[cuttle](https://github.com/glim-sh/cuttle) runs a patched CDP multiplexer
(`cuttle serve`) that spawns one stealth Chrome per fingerprint seed - each with
its own coherent identity (fingerprint, proxy, geoip, locale, timezone) - behind
a single CDP endpoint. The engine is a free stealth-Chromium fork (clark MIT,
default; clearcote BSD-3, fallback); no proprietary binary. Point any CDP client
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
(logins persist), not recreated. Default ports: CDP 9222, VNC 6080.

### Contexts and backends

A **context** names where the browser runs, selected by `--context` >
`CUTTLE_CONTEXT` > the config `default_context` > built-in `local`:

- **local** - Docker on this host (the zero-config default).
- **ssh** - a container on a remote host, reached over `ssh -L`. Inherits
  `~/.ssh/config` (keys, jump hosts), so no cuttle-specific ssh setup.
- **k8s** - a Deployment reached via `kubectl port-forward`. Inherits your kube
  config.
- **direct** - an already-running CDP endpoint, used as-is.

For the tunneled backends every CDP/VNC op still targets a local
`127.0.0.1:<port>` - the backend owns the tunnel. `cuttle context ls` lists
contexts and marks the active one; `cuttle context --help` shows how to write an
ssh/k8s context. Contexts are hand-written in
`$XDG_CONFIG_HOME/cuttle/config.toml` (default `~/.config/cuttle/config.toml`):

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

**Local-canonical profiles.** Auth state (`--profile <name>`) lives on your
machine and is injected into the session over CDP at login, then checked back in
- so logins are portable across backends and survive a container being
discarded. Omitted profiles behave as ephemeral.

### Picking ports (important)

Every subcommand takes `--cdp-port` and `--vnc-port`. Use them when the defaults
are taken:

```bash
cuttle up --cdp-port 9444 --vnc-port 6099
```

- The CLI is **stateless** - pass the *same* ports (and `--name`) to
  `status`/`login`/`down` as you gave `up`, or they target the default 9222/6080.
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
- **Leave the user's tabs alone.** The session is warm and shared - it may hold a
  half-finished login or a page the user is watching in the viewer. Open your
  work in a new tab rather than navigating the current one away, and close only
  the tabs you opened.
- **One driver at a time.** Every client attached to a cuttle session shares one
  browser and one set of tabs; two agents navigating in parallel clobber each
  other. Serialize browser work, or give each worker its own identity with
  `?fingerprint=<seed>` (see [Multi-seed farm](#multi-seed-farm)).
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
  first one (agent-browser by default) unless the user names another
  (bu / bu-cli / browseruse = browser-use; playwright-cli). If the named driver
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
cuttle login https://accounts.google.com
# navigates there, opens the viewer, prints:  open the viewer to sign in: http://127.0.0.1:6080/
```

Open the viewer URL, sign in / solve the captcha there, and the CDP connection is
now logged in - VNC and CDP share one browser. This is why cuttle beats a fresh
headless browser for gated sites: the agent hits a wall, hands you the viewer
link, you sign in on the same session, nothing restarts.

## Lifecycle

```bash
cuttle status           # container + CDP state
cuttle down             # graceful stop (SIGTERM -> clean exit); KEEPS the profile
cuttle up               # restart the stopped container - logins still there
cuttle down --purge     # stop AND remove the container + discard the profile
cuttle up --recreate    # destroy any existing container, start fresh
```

- **Graceful down matters.** `cuttle down` does `docker stop -t 15` so Chrome
  exits clean; that avoids crash-restore junk tabs on next launch. Never
  `docker rm -f` a running cuttle - the SIGKILL makes Chrome record a crash.
- **Profile = state.** Logins live in the container filesystem and survive
  stop/start. `--purge` / `--recreate` are the only ways to discard them.
- `--keep-profile` (default on) is **fixed at container creation**; passing it (or
  `--no-keep-profile`) against an existing container warns and is ignored - use
  `--recreate` to change it.
- **`--image` and `--no-vnc` are creation-fixed too.** `--image` against an
  existing container warns and is ignored (it will not switch a running container
  to another image). `--no-vnc` is ignored *silently*: a container created with it
  has no viewer port, yet a later plain `up` still prints a viewer URL that nothing
  serves. `cuttle status` shows the container's real image and port bindings;
  `--recreate` is the only way to change either, and it discards the profile.
- **Do not reach for `--recreate` on a port error.** If `up` says "container
  restarted but CDP on :<port> never came up", suspect a port mismatch first: a
  restarted container keeps the ports it was *created* with, and `--recreate`
  would discard the logged-in profile. Run `cuttle status` - it prints the real
  port bindings and a log tail - then re-run `up` with those ports.

Also on every subcommand: `--name` (run several side by side), `--no-vnc`, and on
`up` an `--image` override. `cuttle skill` prints this guide to stdout, always
matching the installed CLI.

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

Both forks are baked in, selected by `CUTTLE_BROWSER_BINARY`:

```bash
docker run --rm -p 9222:9222 -e CUTTLE_BROWSER_BINARY=/opt/clearcote/chrome ghcr.io/glim-sh/cuttle:latest
```

- `/opt/clark/chrome` - clark, Chrome 148 (default)
- `/opt/clearcote/chrome` - clearcote, Chrome 149 (fallback)

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
5. **Human handoff is the VNC viewer.** When a login wall or captcha needs a human,
   run `cuttle login <url>` (or `cuttle connect` for a held session) and open the
   printed viewer URL - sign in there and the same CDP session is now authenticated.
   `cuttle view` is an alias of `cuttle connect`, not a separate window-raise.
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
