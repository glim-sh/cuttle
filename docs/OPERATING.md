# Operating cuttle

Install, remote backends, ports, farm mode and deployment. This is the material an
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

[context.cluster]    # k8s: a Deployment via kubectl port-forward
backend   = "k8s"
namespace = "browser"
release   = "cuttle"
```

## Profiles and persistence

**The default profile is durable.** The bare default session (plain `up`, no seed)
keeps its full Chrome profile - cookies, localStorage, IndexedDB, service workers -
in a named Docker volume (`cuttle-<container>-profile`) or a k8s PVC. It survives
`cuttle up` restarts, `cuttle up --recreate`, and image upgrades, with no named
profile and no flag. Reset it deliberately: `cuttle up --recreate --purge-profile`,
`cuttle purge-profile`, or `cuttle down --purge`. `--ephemeral` opts out for a
disposable session. A plain `cuttle down` never touches the volume.

**Named profiles are local-canonical.** For *named* seeds, auth state (cookies +
per-origin localStorage) is mirrored on your machine. The daemon snapshots a seed
when the last client detaches, on a slow backstop timer, and at clean shutdown, and
re-injects it when the seed relaunches. `cuttle down` also pulls each running named
seed's state into `$XDG_DATA_HOME/cuttle/profiles/<seed>/` as a safety net (skipped
on `--purge`). So `--recreate`, `--purge` and box loss never strand a named login.

**Creation-fixed settings.** `--image`, the persistence choice, `--idle-timeout`,
`--humanize` and `--allow-context-creation` are baked into the container at
creation. Passing them against an existing container warns and is ignored; use
`--recreate` to change them. (On k8s they re-apply on every `helm upgrade`.)

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
For many isolated *identities*, use per-seed `?fingerprint=` instead (below), not
multiple containers.

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

## Engine swap

The image bakes our stealth-Chromium build (`/opt/browser/chrome`, Chrome 148, the
default). The clearcote fallback (Chrome 149) is **not** baked: its build stage in
`ops/docker/Dockerfile` is commented out. To use it, re-enable that stage, rebuild
the image, and select the engine with
`-e CUTTLE_BROWSER_BINARY=/opt/clearcote/chrome`.

## Triage

- **"Container running but CDP not answering" / "restarted but CDP never came up."**
  Usually a stale container from a previous `cuttle up` that failed because the host
  port was taken. Current `cuttle up` auto-removes such zombies; on an older build
  run `cuttle up --recreate`. `cuttle status` prints a log tail with the real cause.
- **Graceful down matters.** `cuttle down` does `docker stop -t 15` so Chrome exits
  clean, which avoids crash-restore junk tabs. Never `docker rm -f` a running
  cuttle - the SIGKILL makes Chrome record a crash.
- **Chrome's container log noise is not a stealth failure.** `vkCreateInstance:
  Found no drivers`, `Automatic fallback to software WebGL`, dbus connect failures,
  `Failed to adjust OOM score` and `GPU stall due to ReadPixels` are expected on a
  headless host. Never add `--enable-unsafe-swiftshader`: it exposes the raw
  software renderer and makes the fingerprint worse.
