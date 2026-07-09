---
name: cuttle
description: Run and drive cuttle - a local stealth-Chromium CDP farm - for browser automation that needs persistent logins, anti-detect fingerprints, or passing Cloudflare. Covers `cuttle up/login/status/down`, the VNC login-handoff flow, and connecting agent-browser/browser-use/Playwright over CDP. Use instead of a plain headless browser whenever the task needs a coherent stealth identity or a session that survives restarts.
metadata:
  version: "0.1.0"
  image: "ghcr.io/glim-sh/cuttle"
allowed-tools: Bash(cuttle:*) Bash(uv:*) Bash(just:*) Bash(docker:*) Bash(curl:*) Bash(agent-browser:*) Bash(browser-use:*)
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
# cuttle ready  (container 'cuttle', image cuttle:local)
#   CDP     http://127.0.0.1:9222    # agent-browser --cdp 9222
#   viewer  http://127.0.0.1:6080/
```

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

## Connect a CDP client

cuttle serves standard CDP on `http://127.0.0.1:<cdp-port>`:

```bash
agent-browser --cdp 9222 open https://example.com          # invoke the agent-browser skill first
BU_CDP_URL=http://127.0.0.1:9222 browser-use <<'PY' ... PY  # invoke the browser-use skill first
playwright-cli attach --cdp=http://127.0.0.1:9222
```

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
`--image` (default `cuttle:local`). `cuttle skill` prints this guide to stdout.

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
