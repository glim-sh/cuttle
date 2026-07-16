# cuttle Go rewrite + remote backends + local-canonical profiles

Status: PLAN (approved to build). Author handoff doc - written so a fresh agent
with no prior conversation context can build the whole thing. Read this top to
bottom before writing code.

## 1. Goal

Rewrite `cuttle` from Python to Go as a single, minimal, elegant codebase that:

1. Owns the code outright - fold the in-container daemon (`bin/cuttleserve`) and
   the vendored `cloakbrowser` Python into first-class authored Go. Retire the
   `vendor/` + `scripts/sync.sh` upstream-vendoring relationship.
2. Turns cuttle into a universal browser-lifecycle CLI that manages the stealth
   browser in one of four places, selected by a named context: **local** (docker
   here), **k8s** (a Deployment in a cluster), **ssh** (docker on a remote host),
   **direct** (a pre-exposed CDP/VNC URL). Same `cuttle up/down/status/login/
   connect` verbs regardless of where the browser runs.
3. Adds **local-canonical profiles**: the user's browser profiles + auth state
   live on the user's machine (XDG data dir) and are injected into whatever
   remote browser per session, so sensitive data resides locally at rest. Named
   profiles map to cuttle "seeds".
4. Installs + configures the browser-use MCP (and other CDP drivers) pointed at
   the active context's browser, so `cuttle mcp` gives an agent a working,
   authenticated browser with no manual wiring.

The end state is what the user wants for themselves first (reliable
authenticated browser access without local-browser babysitting), built cleanly
enough to become a product later. This is NOT a public paid service build.

## 2. What we do NOT rewrite / own

The stealth is a **patched Chromium fork** shipped as a prebuilt linux-x64
binary, downloaded SHA-pinned in the Dockerfile and selected via
`CLOAKBROWSER_BINARY_PATH`. We keep consuming it unchanged:

- `clark-browser` (MIT): ungoogled-chromium 148 + `--fingerprint-*` patches
  (default) - `github.com/clark-labs-inc/clark-browser`
- `clearcote-browser` (BSD-3): Chromium 149 + fingerprint patches (fallback)

We never fork Chromium. The Go rewrite covers only the thin orchestration around
that binary (CLI, CDP multiplexer, arg-building, geoip, proxy). The one ongoing
cost of owning the wrapper: we now track clark/clearcote's `--fingerprint-*`
flag surface ourselves (flags, not C++).

## 3. Locked decisions

| Decision | Choice |
|---|---|
| Language | Go 1.26+ (full rewrite: CLI + daemon + cloakbrowser wrapper) |
| Keep Python? | Yes, temporarily, as a reference oracle in `packages/cuttle-py/` until Go reaches parity + passes STEALTH-VERIFICATION, then delete |
| Repo layout | Monorepo: `packages/cuttle-py/` (old) + `packages/cuttle-go/` (new); per-language configs live in each package |
| Helm chart location | `ops/helm/cuttle/` (repo root, shared) |
| Config file format | TOML via `github.com/pelletier/go-toml/v2` (DECIDED - hand-edited, needs comments; comment-preserving write-back via `unstable/edit`). |
| Generated MCP config | JSON via stdlib `encoding/json` (MCP convention, no dep) |
| k8s / helm / ssh access | Shell out to `kubectl` / `helm` / `ssh` (inherit the user's kube context, ssh config, and whatever routing they provide - e.g. tailnet - with zero cuttle-specific setup). No client-go, no helm SDK. |
| Remote reachability | Native `kubectl port-forward` and `ssh -L` tunnels that map the remote browser onto `127.0.0.1:<port>`, so all CDP/VNC code is transport-agnostic. `direct` backend uses a config URL as-is (escape hatch for any pre-exposed endpoint). |
| Profile storage default | local-canonical (storage_state checkout/checkin over CDP; remote profile dir ephemeral). Per-profile opt-in to remote-persistent for always-on/autonomous use. |
| Proxy | A server-level default set at `cuttle up` from context config, applied to the browser regardless of backend. (Today proxy is per-connection only; add a default.) geoip auto-follows the proxy. |
| idle-timeout | local only; remote browsers run without reaping. |
| CLI framework | `github.com/spf13/cobra` (help, completion, POSIX flags). Stdlib `flag` acceptable if we want fewer deps. |

## 4. Non-goals for v1

- No multi-tenant/public service, no billing, no OAuth-gated hosted VNC proxy.
  (Those belong to a later `glim_browser` product; this is single-owner tooling.)
- No Go rewrite of `test/harness.py` semantics beyond what parity needs.
- No client-go / helm-Go-SDK. Shelling out is the deliberate minimal choice.

## 5. Repo restructure (do this first, as a mechanical move)

Target layout:

```
cuttle/                          (repo root, glim-sh/cuttle)
  README.md  LICENSE  CHANGELOG.md
  .github/workflows/             (CI for both packages)
  docs/                          (shared docs incl. this plan)
  ops/
    helm/cuttle/                 (Helm chart - NEW)
  packages/
    cuttle-py/                   (the ENTIRE current repo tree, moved verbatim)
      cuttle/  vendor/  bin/  test/  Dockerfile  pyproject.toml  uv.lock
      flake.nix  Justfile  ...   (all Python-side config moves here)
    cuttle-go/                   (NEW authored Go)
      cmd/cuttle/main.go
      internal/...
      Dockerfile
      go.mod  go.sum
      .golangci.yml  lefthook.yml  Justfile
```

Rules:
- Move the existing Python tree wholesale into `packages/cuttle-py/` (git mv, one
  commit, no code changes) so it keeps building and stays the oracle.
- Each language's build/lint/format config lives inside its package dir. Repo
  root holds only shared things (LICENSE, top README, `.github/`, `docs/`,
  `ops/`).
- Go module path during coexistence: `module github.com/glim-sh/cuttle/packages/cuttle-go`.
  This makes `go install github.com/glim-sh/cuttle/packages/cuttle-go/cmd/cuttle@latest`
  correct but ugly; users install via brew/release binaries where the path is
  invisible, so it is low-impact. When `cuttle-py` is retired, promote `cuttle-go`
  to repo root and rename the module to `github.com/glim-sh/cuttle`.
  Optional: add a root `go.work` for local multi-module dev ergonomics (does not
  affect remote `go install`).

## 6. Toolchain (go-dev stack, pinned)

Inside `packages/cuttle-go/`:
- Go 1.26+, `go mod`, tool deps tracked with `go get -tool` (Go 1.24+).
- `golangci-lint` v2.11+ (config `.golangci.yml`, template from the go-dev skill).
- `gofumpt` v0.9+ (via golangci-lint formatters, extra-rules on).
- `gotestsum` v1.13+ for test output.
- `just` task runner (`Justfile` from the go-dev skill: fmt/lint/test/check/build).
- `lefthook` pre-commit (fmt -> lint -> mod-tidy) and pre-push (test -race).
- `govulncheck` in CI.

Use the exact `.golangci.yml`, `Justfile`, `lefthook.yml`, and GitHub Actions
templates from the `go-dev` skill. Keep `main.go` thin; business logic in
`internal/`.

## 7. Dependencies (verified current, 2026-07-16)

| Concern | Package | Notes |
|---|---|---|
| CDP driving (navigate, cookies, localStorage eval) | `github.com/chromedp/chromedp` + `github.com/chromedp/cdproto` | Mature, zero external deps, MIT. Connect to the already-running fork via `chromedp.RemoteAllocator` (do NOT let chromedp launch Chrome). Use `cdproto/network` (GetCookies/SetCookies), `cdproto/storage`, `cdproto/runtime` (Evaluate for localStorage) for storage_state. |
| Raw WebSocket (the multiplexer's client<->Chrome CDP proxy) | `github.com/coder/websocket` | Modern nhooyr successor, zero deps, idiomatic, context-first. Use for accepting client CDP WS and piping frames to the seed's Chrome WS. |
| GeoIP (tz/locale/exit-IP from proxy) | `github.com/oschwald/geoip2-golang/v2` | v2 API uses `netip.Addr`; `record.Location.TimeZone`, `record.Country.ISOCode`. We download+cache the GeoLite2-City.mmdb ourselves (see 8.3), the lib only reads it. Requires Go 1.25+. |
| TOML config | `github.com/pelletier/go-toml/v2` | TOML 1.1, strict mode (`DisallowUnknownFields`), and `unstable/edit` for comment-preserving write-back (future `cuttle context add/use`). |
| CLI | `github.com/spf13/cobra` | Subcommands, POSIX flags, shell completion, man pages. |
| SOCKS/HTTP proxy dialer (exit-IP echo call through the proxy) | `golang.org/x/net/proxy` | For resolving the proxy exit IP (WebRTC spoof + geoip), mirroring the Python `socksio` path. |
| Everything else | stdlib | `os/exec` (docker/kubectl/helm/ssh/Chrome), `net/http` (CDP `/json`, mmdb download, echo-IP), `encoding/json` (MCP config + CDP messages), `net/url`, `net/netip`. |

Reference docs to consult while implementing: pkg.go.dev for each of the above;
`chromedp/examples` repo for RemoteAllocator + raw CDP patterns.

## 8. Component specifications

### 8.1 Config (`internal/config`)

- Path: `$XDG_CONFIG_HOME/cuttle/config.toml` (default `~/.config/cuttle/`).
- Selection precedence for the active context: `--context` flag > `CUTTLE_CONTEXT`
  env > `default_context` in file > built-in `local`.
- Read with go-toml strict mode (typo protection). v1 is read-mostly; provide
  `cuttle context ls` (list + show active). Defer write-back commands
  (`context add/use`) to a later pass (they use `unstable/edit`).

Schema:

```toml
default_context = "cluster"

[context.local]
backend = "local"                # docker here; today's behavior

[context.cluster]
backend = "k8s"
namespace = "browser"
release = "cuttle"
# kube_context omitted -> inherit kubectl's current context (which already
# carries the user's routing, e.g. a tailnet; cuttle stays ignorant of it)
node_selector = { "glim.sh/browser" = "true" }
tolerations = [{ key = "browser", operator = "Exists", effect = "NoSchedule" }]
resources = { requests = { memory = "1Gi" }, limits = { memory = "4Gi" } }
proxy = "http://user:pass@proxy.example:8080"   # applied at browser startup

[context.box]
backend = "ssh"
host = "misha@box.example"        # inherits ~/.ssh/config, keys, jump hosts
proxy = "..."

[context.tailnet]
backend = "direct"
cdp_url = "http://cuttle.ts.net:9222"
vnc_url = "http://cuttle.ts.net:6080"

# Profiles (= cuttle seeds). Optional; omitted profiles behave as ephemeral.
[profile.default]
storage = "local"                 # local-canonical (default). "remote" = persist on the browser host.
[profile.linkedin]
storage = "local"
```

### 8.2 cloakbrowser port (`internal/fingerprint`) - STEALTH-CRITICAL

Port these from `packages/cuttle-py/vendor/cloakbrowser/`:
- `browser.py` -> arg building: `build_args`, proxy URL normalization
  (`_ensure_proxy_scheme`, `_assemble_proxy_url`, `_reconstruct_socks_url`,
  `_normalize_socks_string_url`), `_resolve_webrtc_args` (replace
  `--fingerprint-webrtc-ip=auto` with the resolved proxy exit IP).
- `geoip.py` -> `resolve_proxy_geo_with_ip` (tz/locale/exit-IP), the
  `COUNTRY_LOCALE_MAP`, and mmdb download+cache (see 8.3).
- `config.py` -> the small config/env resolution + `CLOAKBROWSER_BINARY_PATH`
  override (`download.py` collapses to "read the env path, error clearly").

MANDATORY parity requirement: the Go `build_args` output (the full Chrome argv,
including `--fingerprint-*`, `--proxy-server`, `--user-data-dir`, ordering) MUST
be byte-identical to the Python `build_args` for a matrix of inputs (seed x
proxy present/absent x timezone/locale set/unset x webrtc auto/none). Write a
table-driven Go test that asserts this against golden argv captured from the
Python oracle. A silent drift here = silent stealth loss (no error, just
detection). geoip resolution and proxy-URL normalization get the same
parity-test treatment.

### 8.3 GeoIP mmdb download

Mirror the Python: download `GeoLite2-City.mmdb` (~70 MB) on first use from the
P3TERX mirror (`https://github.com/P3TERX/GeoLite.mmdb/raw/download/GeoLite2-City.mmdb`),
cache under `$XDG_CACHE_HOME/cuttle/` (or the geoip cache dir), background
re-download after 30 days. Never block browser startup on the download; a
missing DB degrades to "no tz/locale" but still returns the exit IP (used for
WebRTC). Read via `geoip2-golang/v2`.

### 8.4 `cuttle serve` (the multiplexer, `internal/serve`) - the daemon

This is the in-container daemon, folded in as a `cuttle serve` subcommand (the
image entrypoint). Port `packages/cuttle-py/bin/cuttleserve`. It is a CDP
multiplexer: one Chrome process per fingerprint seed, all fronted on one port.

Behaviors to port faithfully (the subtle, battle-tested parts):
- HTTP endpoints `/json/version` and `/json` that return CDP target info.
  **Rewrite the `webSocketDebuggerUrl` host to the request's Host header** so it
  is correct behind `kubectl port-forward` / `ssh -L` (this fixes the port-forward
  wrinkle by construction - the Python fought a sibling loopback-bind bug).
- Connect-URL query parsing: `?fingerprint=<seed>&timezone=..&locale=..&proxy=..`.
  Seed validation regex `^[A-Za-z0-9_-]{1,128}$`; reserved seed `__default__`.
- Per-seed Chrome pool: spawn one Chrome per seed with `user_data_dir =
  {data_dir}/{seed}`, connection refcounting, and **idle reap only when
  idle-timeout > 0** (local only; remote leaves it 0 = never reap).
- WebSocket Origin allow-list (verify it accepts `kubectl port-forward` origins,
  which are typically `127.0.0.1`).
- Clean Chrome drain on SIGTERM (the CLI stops with a grace period > the internal
  drain so a clean exit is recorded and no junk-tab crash-restore happens).
- Bind host: `0.0.0.0` in a container (detected), `127.0.0.1` otherwise. Env
  override `CUTTLESERVE_HOST`. (The Python already handles the k8s/containerd
  detection; port it.)
- The three cuttle-specific patches on top of upstream cloakserve (see
  `packages/cuttle-py/docs/UPSTREAM.md`), port each deliberately:
  1. proxy-auth-over-CDP injection (answering the proxy auth challenge via CDP),
  2. service_worker `browserContextId` stamp,
  3. fork launch-parity flags.

NEW in the Go serve:
- `--proxy` / `CUTTLESERVE_PROXY`: a server-level DEFAULT proxy. A connection
  that omits `?proxy=` inherits it. This is what makes proxy a `cuttle up`/config
  concern applied uniformly regardless of backend. geoip keys off the effective
  proxy, so tz/locale follow it automatically.
- Ephemeral profile dir support (see 8.6): when a profile is local-canonical,
  the seed's `user_data_dir` is an ephemeral scratch (emptyDir/tmpfs); nothing
  sensitive persists on the remote.

### 8.5 CLI + `reach()` + backends (`internal/cli`, `internal/backend`)

Model the whole thing on one seam: a `Backend` obtains a reachable local CDP
endpoint. Every CDP/VNC-facing operation runs against `127.0.0.1:<port>`.

```
type Backend interface {
    State(ctx) (string, error)        // running / stopped / absent
    Start(ctx, opts) error
    Stop(ctx, purge bool) error
    Reach(ctx) (Endpoint, release func(), error)  // yields local cdp/vnc ports
}
```

Implementations:

| Backend | State | Start | Stop | Reach |
|---|---|---|---|---|
| local | `docker inspect` | `docker run` (port map, shm, keep-profile) | `docker stop`/`rm` | direct 127.0.0.1, no tunnel |
| k8s | `kubectl get pod` | `helm upgrade --install ops/helm/cuttle` | `helm upgrade --set replicaCount=0` (`--purge` = `helm uninstall` + delete PVC) | `kubectl port-forward svc/cuttle` -> ephemeral local port |
| ssh | `ssh host docker inspect` | `ssh host docker run` | `ssh host docker stop` | `ssh -L` (ControlMaster) -> ephemeral local port |
| direct | CDP probe of config URL | error (not cuttle-managed) | error | parse `cdp_url`/`vnc_url` from config, use as-is |

Port today's `cuttle/cli.py` docker logic verbatim into the `local` backend so
current behavior does not regress (default context = `local` with no config
file present).

Reachability rules:
- `status` / `login` open an EPHEMERAL forward for the command's duration
  (stateless, matches cuttle's "container is the state" ethos).
- NEW `cuttle connect`: holds the forward open in the foreground, prints local
  CDP/VNC URLs + the driver attach briefing, Ctrl-C to end. This is what an
  agent/interactive session uses to avoid re-forwarding per call.
- Auto-pick a free local port for forwards so a k8s attach never collides with a
  `local` container already on 9222.

Presence checks (mirror today's `_docker()`): verify `docker`/`kubectl`/`helm`/
`ssh` on PATH per backend, with clear errors. Thread `kube_context` onto every
kubectl/helm invocation when set.

Preserve the existing "driver briefing" output (agent-facing: prints installed
CDP drivers - agent-browser / browser-use / playwright-cli - with attach lines
and self-doc commands). Port the `_DRIVERS` table.

### 8.6 Profiles = seeds; local-canonical storage_state (`internal/profile`)

Named profile == cuttle seed. `--profile <name>` threads through `login`,
`connect`, `mcp`, and every attach line as `?fingerprint=<name>`. Profile data
lives locally at `$XDG_DATA_HOME/cuttle/profiles/<name>/storage_state.json`.

The unit is a SESSION (attach -> detach), NOT a request - a browser holds
cookies in memory across many requests; there is no per-request injection seam.

local-canonical flow (default):
1. Checkout (session start): read the local profile's storage_state (cookies +
   per-origin localStorage/IndexedDB - Playwright storageState shape, small
   JSON) and inject into the freshly spawned remote seed over CDP
   (`Network.SetCookies` + `Runtime.Evaluate` per origin for localStorage). The
   remote seed uses an ephemeral user_data_dir.
2. Session runs; nothing sensitive persists on the remote.
3. Checkin (session end + periodic checkpoint): extract the updated
   storage_state via CDP (`Network.GetCookies`, read localStorage per origin)
   and write it back locally. This is where snowballed cookies land.

Rules & honest caveats to implement:
- Single-writer lock per profile (local lockfile): a profile checked out to one
  remote session cannot be concurrently attached elsewhere (Chrome's own rule).
- Periodic checkpoint (every N minutes) during long sessions so a crash before
  checkin does not lose the cookie delta.
- storage_state covers cookie/localStorage/IndexedDB auth (enough for
  LinkedIn/X/GitHub). It does NOT capture full Chrome-profile fidelity
  (extensions, SW caches, device-bound state). Provide a full-dir sync escape
  hatch only if a site needs it (heavy; requires Chrome stopped for a consistent
  snapshot). Not v1-critical.
- "Resides locally" is true AT REST. During an active session the live cookies
  are necessarily on the remote (the browser must hold them to act as the user).
  This is inherent to remote browsing; document it, do not pretend otherwise.

remote-persistent opt-in (`storage = "remote"`): the seed's user_data_dir is a
durable path on the browser host; no checkout/checkin. Needed for autonomous /
always-on use where the user's machine is not present to inject state. For k8s
this means a PVC (see 8.8), which reintroduces RWO/Recreate/SingletonLock
handling ONLY for remote-persistent profiles.

### 8.7 `cuttle mcp` (`internal/mcp`)

- `cuttle mcp [driver] --profile <name>`: ensure the driver is installed
  (default browser-use: `uv tool install browser-use`) and write the MCP client
  config (JSON, stdlib) pointing at the active context's CDP endpoint with the
  profile seed, e.g. `BU_CDP_URL=http://127.0.0.1:9222?fingerprint=<name>` (plus
  the context proxy already applied server-side).
- Reuse the `_DRIVERS` table for install commands + attach templates.

### 8.8 Dockerfile (`packages/cuttle-go/Dockerfile`) - Python-free

Port `packages/cuttle-py/Dockerfile` but replace the Python runtime with a
static Go binary:
- Multi-stage: build the `cuttle` binary (`CGO_ENABLED=0 go build -trimpath
  -ldflags="-s -w"`), copy into a slim base.
- Keep the browser-engine stages verbatim (SHA-pinned clark/clearcote tarball
  downloads to `/opt/clark` and `/opt/clearcote`, `CLOAKBROWSER_BINARY_PATH`).
- Keep the headed stack (Xvfb/Xvnc, openbox, noVNC/websockify, the Windows font
  pack) and the entrypoint that starts Xvnc + `cuttle serve`.
- linux/amd64 only (clark/clearcote ship linux-x64). Result: no Python, no uv,
  no pip, no venv in the image.
- Note: `scripts/rename-fonts.py` is a build-time font step; either keep it as a
  build-only Python invocation or reimplement with a Go/CLI font tool. Low
  priority; keep the Python build step if simpler.

### 8.9 Helm chart (`ops/helm/cuttle/`)

Bundle-agnostic (the CLI shells `helm upgrade --install` against this path;
consider embedding via `embed.FS` so an installed binary carries the chart).

Templates:
- Deployment: the cuttle image running `cuttle serve` (keep-profile only for
  remote-persistent profiles), `replicas: 1`, `CUTTLESERVE_HOST=0.0.0.0`, the
  context proxy passed via `CUTTLESERVE_PROXY`, `nodeSelector`, `tolerations`,
  `resources`, and a HARD `nodeSelector: kubernetes.io/arch: amd64` (the image is
  amd64-only; never let it schedule on arm).
- Volume:
  - Default (local-canonical profiles): `emptyDir` for the profile scratch -
    nothing persists, no RWO/Recreate/SingletonLock headache.
  - remote-persistent profiles: a PVC (RWO) at the data dir, `strategy: Recreate`
    (RWO cannot mount into a new pod while the old holds it), stale Chrome
    `SingletonLock` cleared on boot, `helm.sh/resource-policy: keep`.
- Service: ClusterIP exposing CDP 9222 + VNC 6080 (reached via port-forward; no
  ingress, nothing public).
- NetworkPolicy: deny ingress to the pod except from nothing (port-forward does
  not traverse the Service network path). CDP has no auth; anyone who can reach
  it drives your logged-in browser - lock it down.
- values.yaml: `image.tag` pinned to the CLI version, `nodeSelector`,
  `tolerations`, `resources`, `dataDir`, `proxy`, `replicaCount`,
  `profileStorage` (local|remote).

## 9. Stealth fidelity: the safety net

The rewrite's only real risk is silently degrading stealth. Mitigations, all
mandatory before deleting `cuttle-py`:
1. Parity tests (8.2): Go `build_args` / geoip / proxy-normalization output ==
   Python oracle output, byte-for-byte, across an input matrix. Golden files
   captured from the Python.
2. STEALTH-VERIFICATION pass: bring up a Go-built seed and run the checklist in
   `packages/cuttle-py/docs/STEALTH-VERIFICATION.md` (navigator.webdriver=false,
   coherent Win32 + Chrome UA, real ANGLE/Direct3D11 WebGL renderer NOT
   SwiftShader/llvmpipe, WebRTC only the proxy exit IP). Do NOT add
   `--enable-unsafe-swiftshader` (it is a stealth regression, not a fix).
3. Live challenge clear: confirm a Go-driven seed clears an escalated anti-bot
   challenge headed-on-Xvnc, same as Python. Remember: cold-clear is dominated by
   the proxy exit-IP reputation, not the fingerprint - test on a clean exit.

## 10. Build sequence (phases, on branch `feat/go-rewrite`)

Keep `packages/cuttle-py/` working as the oracle until the very end.

1. Restructure: `git mv` current tree into `packages/cuttle-py/`; create
   `packages/cuttle-go/` scaffold with the go-dev toolchain; `ops/helm/`
   placeholder. One commit, Python still builds.
2. `internal/fingerprint`: port build_args + proxy-normalize + geoip + webrtc.
   Land the parity tests against the Python oracle FIRST - this is the
   stealth-critical core, lock it before anything depends on it.
3. `internal/serve`: the CDP multiplexer + default-proxy + ws-host rewrite +
   ephemeral-dir support + the three UPSTREAM patches. Smoke it against a real
   clark browser locally over CDP.
4. `internal/config` + `internal/backend` (local first, proving no-regression,
   then k8s, ssh, direct) + `internal/cli` (up/down/status/login/connect,
   driver briefing).
5. `internal/profile`: storage_state checkout/checkin over CDP + locking +
   checkpoints; wire local-canonical default; remote-persistent opt-in.
6. `internal/mcp`: install + JSON config write for the active context/profile.
7. Dockerfile (Python-free) + Helm chart (`ops/helm/cuttle/`, emptyDir default,
   PVC for remote-persistent) + docs (port README/SKILL/AGENTS/CHANGELOG,
   retire UPSTREAM.md + scripts/sync.sh).
8. Parity + STEALTH-VERIFICATION + live-challenge pass. Then remove
   `packages/cuttle-py/` and promote the Go module to repo root
   (`github.com/glim-sh/cuttle`).

## 11. Testing strategy

- Pure functions (fingerprint args, proxy normalize, geoip mapping, connect-URL
  assembly, config resolution, backend command construction): table-driven unit
  tests, `-race`. These carry the correctness weight and are fully testable
  without infra.
- Parity tests: golden argv/geoip captured from the Python oracle (a small
  script in cuttle-py dumps them for a fixed input matrix).
- Backend dispatch: mock the exec layer (inject a command runner) so
  local/k8s/ssh/direct routing + flag construction are tested without docker/
  kubectl/ssh present.
- Integration (manual, documented checklist, not CI): real local clark browser
  smoke; real k8s bring-up over port-forward; browser-use + agent-browser attach
  through a forward (verifies the ws-host rewrite); STEALTH-VERIFICATION.
- CI: go-dev GitHub Actions (lint + test matrix + govulncheck). Integration
  stays manual (needs a cluster + browser).

## 12. Open decisions for the owner (resolve before/at build start)

1. Config format: DECIDED - TOML (`go-toml/v2`).
2. CLI framework: cobra (rich UX, a dep) vs stdlib flag (minimal). Plan assumes
   cobra.
3. `rename-fonts.py`: keep the tiny Python build-step in the Go image, or
   reimplement in Go? (Plan: keep it; it is build-time only.)

## 13. Source map (Python -> Go)

| Python (packages/cuttle-py) | Go target (packages/cuttle-go) |
|---|---|
| `cuttle/cli.py` | `cmd/cuttle/main.go` + `internal/cli/*` + `internal/backend/local.go` |
| `cuttle/cdp.py` | `internal/cdp/*` (chromedp RemoteAllocator + coder/websocket) |
| `cuttle/view.py` (experimental CDP-screencast viewer) | `internal/view/*` (optional; port last or drop for v1) |
| `bin/cuttleserve` | `internal/serve/*` (the `cuttle serve` subcommand) |
| `vendor/cloakbrowser/browser.py` | `internal/fingerprint/args.go` + `proxy.go` |
| `vendor/cloakbrowser/geoip.py` | `internal/fingerprint/geoip.go` |
| `vendor/cloakbrowser/config.py` + `download.py` | `internal/fingerprint/binary.go` |
| `Dockerfile` | `packages/cuttle-go/Dockerfile` |
| `docs/UPSTREAM.md` + `scripts/sync.sh` | RETIRED (we own the code now) |
| `SKILL.md` / `AGENTS.md` / `README.md` | ported, updated for Go + backends |

Key existing behaviors to preserve are cited by file:line in
`packages/cuttle-py/` - read `bin/cuttleserve` (bind/detect, seed pool, drain),
`vendor/cloakbrowser/browser.py` (build_args, webrtc), and `cuttle/cli.py`
(docker lifecycle, driver briefing) before porting each.
```
