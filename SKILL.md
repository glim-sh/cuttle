---
name: cuttle
description: Run and drive cuttle - a local stealth-Chromium browser farm with persistent logins, anti-detect fingerprints, and a human-handoff viewer for captchas and Cloudflare. Use whenever the user says to use the browser, or asks to automate, scrape, test, or sign into a website, or names agent-browser, browser-use (bu, bu-cli), or playwright-cli. `cuttle up` prints the live briefing with installed drivers, exact CDP attach commands, and each driver's own docs command. Attach to cuttle's warm session - never launch a fresh browser or new profile.
metadata:
  version: "0.2.0"
  image: "ghcr.io/glim-sh/cuttle"
allowed-tools: Bash(cuttle:*) Bash(uv:*) Bash(just:*) Bash(docker:*) Bash(curl:*) Bash(agent-browser:*) Bash(browser-use:*) Bash(playwright-cli:*)
---

# cuttle: local stealth-browser CDP farm

[cuttle](https://github.com/glim-sh/cuttle) runs a patched CDP multiplexer
(`cuttleserve`) that spawns one stealth Chrome per fingerprint seed - each with
its own coherent identity (fingerprint, proxy, geoip, locale, timezone) - behind
a single CDP endpoint. The engine is a free stealth-Chromium fork (clark MIT,
default; clearcote BSD-3, fallback); no proprietary binary. Point any CDP client
at it - agent-browser, browser-use, Playwright, `chromium.connectOverCDP`.

The `cuttle` CLI wraps a Docker container; it does not automate pages itself -
cuttle is the farm, not the scraper. Two ways to use it:

- **Daily driver / login handoff** - one persistent browser you watch and log
  into via VNC, driven over CDP. Use the CLI: `cuttle up`. Start below.
- **Multi-seed farm** - many isolated identities behind one endpoint, no VNC.
  Run the container and pick a seed with `?fingerprint=`. See [Multi-seed farm](#multi-seed-farm).

## Setup

Requires Docker (or OrbStack). The CLI is published on PyPI as
**`cuttle-browser`**; the command it installs is **`cuttle`**.

```bash
uv tool install cuttle-browser        # persistent global install (recommended)
# one-off, no install (note --from: the package is cuttle-browser, the command is cuttle):
uvx --from cuttle-browser cuttle up
# or: pipx install cuttle-browser  /  pip install cuttle-browser
```

The CLI shells out to Docker and defaults to image `cuttle:local`. Use the
published image with `--image ghcr.io/glim-sh/cuttle:latest`, or build a local
one from the repo (`just build`). Then, from any directory:

```bash
cuttle up      # start the container with the VNC viewer on
```

`up` (and `status`) print **the briefing**: CDP + viewer URLs, cuttle's
version, which driver CLIs are installed - with the exact attach command and
the self-doc command for each - plus routing rules and install advisories for
missing drivers. The briefing is the live source of truth; follow it over any
cached knowledge, including this guide.

`up` is idempotent and profile-preserving: a stopped container is **restarted**
(logins persist), not recreated. Default ports: CDP 9222, VNC 6080.

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
  context - logins live in this one session and persist across restarts.
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

Log in **once** by hand; the profile keeps you logged in across restarts while a
CDP client drives the same live session.

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

Also on every subcommand: `--name` (run several side by side), `--no-vnc`,
`--image` (default `cuttle:local`). `cuttle skill` prints this guide to stdout,
always matching the installed CLI. The briefing prints the installed
cuttle-browser version and this guide's frontmatter carries the same number -
if they differ, the copy in your context is stale: rerun `cuttle skill`.

## Multi-seed farm

For many isolated identities behind one endpoint - no CLI, no VNC - run the
container directly and select a seed per connection:

```bash
docker run --rm -p 9222:9222 cuttle:local     # or ghcr.io/glim-sh/cuttle:latest
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
`locale` together so the identity is coherent.

## Engine swap

Both forks are baked in, selected by `CLOAKBROWSER_BINARY_PATH`:

```bash
docker run --rm -p 9222:9222 -e CLOAKBROWSER_BINARY_PATH=/opt/clearcote/chrome cuttle:local
```

- `/opt/clark/chrome` - clark, Chrome 148 (default)
- `/opt/clearcote/chrome` - clearcote, Chrome 149 (fallback)

## Gotchas

1. **Headed by default, on purpose.** The image runs `cuttleserve --headless=false`
   on a built-in Xvfb; headed Chrome clears escalated anti-bot challenges headless
   cannot. Do not force headless.
2. **VNC is loopback-only, no auth.** The viewer serves plain HTTP; the
   `-p 127.0.0.1:PORT` mapping is the security boundary. Never bind it publicly.
3. **The engine stealth-presents an older version.** clark's binary is Chromium
   148 but reports `Chrome/146.x` over CDP/UA by design - a coherent, common
   fingerprint, not a wrong build. Confirm the real binary with
   `docker exec <name> /opt/clark/chrome --version` if in doubt.
4. **"Logged in" can be false.** A CDP context may still render a login form (geo
   often defaults to US). Verify auth with an in-page render check, not a cookie
   read.
5. **`cuttle view` is experimental** (CDP-screencast viewer instead of VNC): page
   viewport only, no OAuth popups, frame delivery not yet reliable. Use the VNC
   viewer (`cuttle up` / `cuttle login`) for real login flows.
6. **Sessions can be IP-bound.** A cookie minted at your real location, replayed
   through a proxy in another geo, may force re-login + 2FA. Match the proxy geo
   to where the session was created.
7. **"Container running but CDP not answering" / "restarted but CDP never came
   up."** Usually a stale container from a *previous* `cuttle up` that failed
   because the host port was taken (the failed run leaves a half-created
   container with no live port binding). Current `cuttle up` auto-removes such
   zombies and starts fresh; if you hit it on an older build, run
   `cuttle up --recreate`. Check the real cause with `docker logs <name>`.

## Running on a server

The amd64 image runs native on any Linux server:

```bash
docker run -d --restart unless-stopped --name cuttle \
  -p 127.0.0.1:9222:9222 --shm-size=2g ghcr.io/glim-sh/cuttle:latest
```

Bind CDP to `127.0.0.1` and reach it over an SSH tunnel
(`ssh -L 9222:localhost:9222 user@server`) - the tunnel is the auth boundary, CDP
has none of its own. `--shm-size=2g` avoids Chrome crashes under load. Add
`-p 127.0.0.1:6080:6080 -e CUTTLE_VNC=1` for the viewer on the server too.

## See also

- `README.md` - product overview, quickstart, architecture.
- `AGENTS.md` - repo working rules (vendored code, build, non-negotiables).
- `test/harness.py` (`just smoke`) - neutral CDP smoke: per-seed isolation,
  stealth coherence, connection stability.
- `docs/STEALTH-VERIFICATION.md` - confirm a seed presents a coherent identity.
