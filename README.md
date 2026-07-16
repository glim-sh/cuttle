# cuttle

`cuttle` - a stealth-Chromium CDP farm and a universal
browser-lifecycle CLI. It runs a patched Chrome DevTools Protocol (CDP)
multiplexer that spawns one stealth Chrome per fingerprint seed - each with its
own coherent identity (fingerprint, proxy, geoip, locale, timezone) behind a
single CDP endpoint - and manages that browser wherever you want it: locally in
Docker, in a Kubernetes cluster, over SSH, or against a pre-exposed URL.

The Chrome engine is a free, redistributable stealth-Chromium fork baked into
the image - [clark](https://github.com/clark-labs-inc/clark-browser) (MIT) by
default, [clearcote](https://github.com/clearcotelabs/clearcote-browser) (BSD-3)
as a fallback. No proprietary binary. Maintained by [glim.sh](https://glim.sh).

## Install / build

```bash
brew install tenequm/tap/cuttle                        # homebrew cask (macOS/Linux)
go install github.com/glim-sh/cuttle/cmd/cuttle@latest # from source (needs Go 1.26+)
just build                 # -> ./cuttle (native)
just build-release         # CGO_ENABLED=0, -trimpath -ldflags='-s -w'
```

The container image is `ghcr.io/glim-sh/cuttle`. The CLI shells out to Docker,
`kubectl`, `helm`, and `ssh` as the active context requires - it inherits your
existing kube context, ssh config, and routing with no cuttle-specific setup.

## Quickstart (local Docker)

```bash
cuttle up                                   # start the container + VNC viewer
cuttle login https://accounts.google.com    # sign in once via the viewer
cuttle status                               # browser + CDP state
cuttle down                                 # graceful stop, keeps the profile
```

`cuttle up` is idempotent and profile-preserving; `--image cuttle:local` drives a
local build. Point any CDP client at the printed endpoint and select a seed:

```
http://127.0.0.1:9222?fingerprint=12345
http://127.0.0.1:9222?fingerprint=12345&timezone=America/New_York&locale=en-US
```

## Contexts and backends

A **context** names where the browser runs. It is selected by
`--context` > `CUTTLE_CONTEXT` > `default_context` in the config file >
built-in `local`. Config lives at `$XDG_CONFIG_HOME/cuttle/config.toml`; list
contexts with `cuttle context ls`.

| Backend  | Where the browser runs | Reach |
|----------|------------------------|-------|
| `local`  | Docker on this machine | direct `127.0.0.1`, no tunnel |
| `k8s`    | a Deployment (`helm upgrade --install ops/helm/cuttle`) | `kubectl port-forward` -> ephemeral local port |
| `ssh`    | Docker on a remote host | `ssh -L` -> ephemeral local port |
| `direct` | a pre-exposed CDP/VNC URL | the config URL, used as-is |

Every CDP/VNC operation runs against `127.0.0.1:<port>`, so the transport (docker
/ port-forward / ssh tunnel / direct) is invisible to the rest of the CLI.

```toml
default_context = "cluster"

[context.local]
backend = "local"

[context.cluster]
backend = "k8s"
namespace = "browser"
release = "cuttle"
node_selector = { "glim.sh/browser" = "true" }
proxy = "http://user:pass@proxy.example:8080"   # applied at browser startup

[context.box]
backend = "ssh"
host = "user@box.example"

[context.edge]
backend = "direct"
cdp_url = "http://cuttle.example:9222"
vnc_url = "http://cuttle.example:6080"
```

The context `proxy` is a server-level default applied to every seed at startup;
geoip (timezone/locale/exit-IP) follows it automatically. A connection can still
override it per-request with `?proxy=`.

- `cuttle status` / `cuttle login` open an ephemeral forward for the command.
- `cuttle connect` holds the forward open in the foreground and prints the driver
  briefing (Ctrl-C to end) - use it for an interactive or agent session.

## Profiles (local-canonical auth state)

A named **profile** is a cuttle seed whose auth state lives on your machine at
`$XDG_DATA_HOME/cuttle/profiles/<name>/storage_state.json` (Playwright
storageState shape: cookies + per-origin localStorage). `--profile <name>` threads
through `login`, `connect`, and `mcp`, and appends `?fingerprint=<name>` to every
attach URL.

```toml
[profile.linkedin]
storage = "local"     # default: checkout/checkin over CDP, nothing persists remotely
[profile.bot]
storage = "remote"    # durable on the browser host (autonomous / always-on)
```

Local-canonical flow: at session start the profile's stored state is injected
into a freshly spawned remote seed over CDP; a periodic checkpoint and the final
check-in extract the updated state back to your machine. A single-writer lock
prevents a profile from being attached in two places at once.

Honest caveat: state resides locally *at rest*, but during an active session the
live cookies are necessarily on the remote browser (it must hold them to act as
you). `storage = "remote"` skips checkout/checkin entirely for always-on use where
your machine is not present to inject state.

## MCP (agent drivers)

```bash
cuttle mcp                       # default driver (browser-use), current context
cuttle mcp --profile linkedin    # point the driver at the linkedin profile seed
```

`cuttle mcp [driver]` installs the CDP driver if absent and writes its MCP client
config (JSON) pointed at the active context's CDP endpoint, with the profile seed
appended as `?fingerprint=<name>` (e.g. `BU_CDP_URL=http://127.0.0.1:9222?fingerprint=linkedin`).
The context proxy is already applied server-side.

## `cuttle serve`

`cuttle serve` is the in-container daemon (the image entrypoint): the CDP
multiplexer itself. It binds `0.0.0.0:9222` inside a container (detected for
docker/podman/k8s) and `127.0.0.1` on bare metal, spawns one Chrome per
`?fingerprint=` seed, answers authenticated-proxy `407`s over CDP, and rewrites
the `webSocketDebuggerUrl` host to the request's Host header so it stays correct
behind a port-forward or ssh tunnel. `CUTTLESERVE_PROXY` sets a default proxy;
`CUTTLESERVE_HOST` overrides the bind host.

## Development

```bash
just check      # fmt-check + lint (golangci-lint v2) + test (gotestsum -race)
just build      # ./cuttle
just vuln       # govulncheck
```

Business logic lives in `internal/`; `cmd/cuttle` is a thin entrypoint. The
fingerprint arg-builder, proxy normalization, and geoip resolution are
parity-tested byte-for-byte against a committed golden
(`internal/fingerprint/testdata/golden.json`, regenerated with `just
parity-golden`). The Dockerfile is Python-free: a static Go binary plus the
verbatim clark/clearcote engine and KasmVNC/noVNC stages.

## Licensing

MIT ([LICENSE](LICENSE)). The image redistributes the clark (MIT) and clearcote
(BSD-3) stealth-Chromium binaries; the fingerprint/serve code is a Go port of
CloakHQ's `cloakbrowser`/`cloakserve` (MIT). No proprietary CloakBrowser binary
is used or redistributed. Full notices in [THIRD-PARTY.md](THIRD-PARTY.md).
