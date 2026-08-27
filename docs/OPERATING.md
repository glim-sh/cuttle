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
brew install tenequm/tap/cuttle                        # homebrew cask (macOS/Linux)
go install github.com/glim-sh/cuttle/cmd/cuttle@latest # from source (needs Go 1.26+)
```

The container image is `ghcr.io/glim-sh/cuttle` and `cuttle up` pulls it on first
run. Then, from any directory:

```bash
cuttle up      # start the container + VNC viewer (pulls the image if needed)
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
`CUTTLE_CONTEXT` > the config `default_context` > built-in `local`:

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

```toml
default_context = "box"

[context.box]        # ssh: docker on a remote amd64 host
backend = "ssh"
host    = "user@box.example"
screen  = "1536x864" # optional: the screen the browser claims (see below)

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
ahead of the launch it falls back to 1920x1080.

**Which screen.** The browser may only claim a screen its persona ships with: the
amd64 image is a Windows desktop (1920x1080, 1536x864, 1366x768, 1440x900), the
arm64 image an Apple Silicon notebook (1440x900, 1470x956, 1512x982, 1710x1112,
1728x1117); the window is that screen minus the OS taskbar. A session browser
claims the largest by default - one human-facing window wants room. Pick another
with `cuttle up --screen 1536x864`, or durably per context with `screen = "..."`
in `config.toml` (the flag wins); anything off the table is refused with the list.
Changing it on an existing profile changes only the screen and window, not the
logins or the rest of the fingerprint. Pool mode keeps one screen per seed so a
fleet of identities does not all report the same monitor.

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

**Creation-fixed settings.** `--image`, the persistence choice, `--idle-timeout`,
`--humanize`, `--allow-context-creation` and `--block-third-party-cookies` are
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
  value back - `ls` reports source, state, length and origin only.
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
  `cuttle secret ls --name staging --cdp-port 9333`. Without those flags you are
  targeting the default session, which on a busy host is not necessarily the one
  being driven.
- **The routes are loopback-only**, behind the same Host and Origin guard as the
  rest of the daemon's HTTP surface. Anything that can reach the CDP port can
  reach them, so publishing that port to a network is publishing the secret
  store with it - see "Running on a server".

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

- **Ports are pinned only at `up`.** `status`/`open`/`downloads` auto-discover the
  running instance's published ports, so afterwards you target it with just
  `--name` (and `--context`). `down` needs no ports either.
- **Port-shadow gotcha:** `docker run` errors on a docker-vs-docker clash, but
  **not** when a *native* process already owns the host port. `cuttle up` then
  prints a mapping that is silently dead - your client hits the other process.
  Verify with `lsof -nP -iTCP:<port> -sTCP:LISTEN` (want OrbStack/Docker), or check
  `curl http://127.0.0.1:<port>/json/version` names the engine you expect.
- **Do not reach for `--recreate` on a port error.** If `up` says "container
  restarted but CDP on :<port> never came up", suspect a port mismatch first: a
  restarted container keeps the ports it was *created* with. Run `cuttle status` -
  it prints the real bindings and a log tail - then re-run `up` with those ports.

**`--name` is the other axis.** It runs a **separate** docker (local/ssh) instance -
its own container, profile volume and tunnel - so unrelated persistent sessions can
sit side by side. Give each its own ports and pass the same `--name` to every verb.
Two logged-in sessions you want to keep at once (two accounts on one site, say) are
two `--name` containers. For many disposable *identities* driven by code, run pool
mode (below) instead of multiple containers.

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
reject inline creds still work. A proxied seed also pins a WebRTC handling policy so
ICE cannot enumerate the host's real interfaces; pass your own
`--webrtc-ip-handling-policy` on the connect URL to override it. Set proxy,
`timezone` and `locale` together so the identity is coherent. `CUTTLE_PROXY` sets a
server-level default for every seed.

## Running on a server

The amd64 image runs native on any Linux server:

```bash
docker run -d --restart unless-stopped --name cuttle \
  -p 127.0.0.1:9222:9222 --shm-size=2g ghcr.io/glim-sh/cuttle:latest
```

Bind CDP to `127.0.0.1` and reach it over an SSH tunnel
(`ssh -L 9222:localhost:9222 user@server`) - the tunnel is the auth boundary, CDP
has none of its own. `--shm-size=2g` avoids Chrome crashes under load. Add
`-p 127.0.0.1:6080:6080 -e CUTTLE_VNC=1` for the viewer. The `ssh` backend
automates exactly this from the CLI.

**VNC is loopback-only and unauthenticated.** The viewer serves plain HTTP; the
`-p 127.0.0.1:PORT` mapping is the security boundary. Never bind it publicly.

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

## Engine swap

The image bakes our stealth-Chromium build at `/opt/browser/chrome` and selects it
with `CUTTLE_BROWSER_BINARY`. Point that variable at another Chromium-family
binary present in the container to swap engines.

## Triage

- **"Container running but CDP not answering" / "restarted but CDP never came up."**
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
