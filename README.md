# cuttle

Your agent's stock browser fails on the real web. Sites show captchas,
Cloudflare walls and "verify you are human" pages. The browser forgets every
login when the process ends. You cannot see what the agent sees. cuttle is a
browser for your agent that fixes these three problems:

- **Not blocked.** The browser is a patched Chromium build that looks like a
  normal person's browser, with one consistent identity: fingerprint, proxy,
  IP location, locale and timezone.
- **Stays signed in.** Sign in once through the viewer. The login persists
  across agent sessions and across restarts.
- **You can step in.** A built-in viewer shows the live browser. When a site
  wants a human, you solve the captcha or 2FA yourself and the agent continues.

It works with playwright-cli, agent-browser and browser-use.

## Why not Claude in Chrome or ChatGPT?

They overlap a lot: all three use a real browser, keep your logins, pass bot
walls, and read pages fast. The difference is control. As of 2026-08:

| | cuttle | Claude in Chrome | ChatGPT |
|---|---|---|---|
| Banking, tax, government, work accounts | your call | discouraged by policy | gated, auto-pauses |
| Cookies, response bodies, replay from your code | yes | no | no |
| Identities at once | one per container (pool mode for many) | one profile, manual switch | one shared profile |
| Who can drive it | any CDP client | Claude only | ChatGPT only |
| Where it runs | your Docker, server or k8s | your desktop Chrome | OpenAI's cloud |
| Take over when a site pushes back | any browser or phone | at that desk | desktop web only |

The row that matters most is the second one. Reading the full cookie jar,
seeing request and response bodies, and calling any origin is what lets an
agent work out a site's own API and then replay it from your code. Neither
extension exposes that.

Not a reason to choose cuttle: persistent logins, bot walls, page-reading
speed, password-manager sign-in. All three do those.

**How it works.** cuttle runs one Chrome per container behind a single Chrome
DevTools Protocol (CDP) endpoint - every agent that attaches shares the same
tabs and logins, and the viewer shows exactly that browser. Any CDP client
attaches to it, so your existing driver and scripts do not change. The browser runs where you want
it: in Docker on your machine, in a Kubernetes cluster, over SSH, or at a URL
you already expose.

The Chromium build is our own, free and redistributable. It derives from the
[clark](https://github.com/clark-labs-inc/clark-browser) (MIT) patch series.
There is no proprietary binary. Maintained by [glim.sh](https://glim.sh).

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
cuttle up                                  # start the container + VNC viewer
cuttle open https://accounts.google.com    # sign in once via the viewer (Ctrl-C to end)
cuttle status                              # browser + CDP state
cuttle down                                # graceful stop; pulls named logins local
```

`cuttle up` is idempotent and profile-preserving; it also takes `--image` (e.g.
`cuttle:local` for a local build), `--recreate` (fresh container; the persistent
profile re-attaches), `--purge-profile` (reset the profile on recreate),
`--ephemeral` (disposable profile, no volume), `--idle-timeout <seconds>`
(reap the browser after idle; `0` = off), and `--name <name>` (run several
isolated docker instances on one host - each gets its own container, profile
volume, and ports). `cuttle skill` prints the full agent-facing guide. Point
any CDP client at the printed endpoint; there is nothing to select, the
container is the browser.

Need many disposable identities driven by code rather than one session a person
shares with agents? That is **pool mode**: run the image directly with
`cuttle serve --mode=pool` and pick a seed per connection with
`?fingerprint=<seed>` (plus `&proxy=`, `&timezone=`, `&locale=`). See
[docs/OPERATING.md](docs/OPERATING.md).

## Contexts and backends

A **context** names where the browser runs. It is selected by
`--context` > `CUTTLE_CONTEXT` > `default_context` in the config file >
built-in `local`. Config lives at `$XDG_CONFIG_HOME/cuttle/config.toml`; list
contexts with `cuttle context ls`, create one with `cuttle context add <name>
--backend ssh --host user@box.example` (hand-edit only for advanced k8s knobs).

| Backend  | Where the browser runs | Reach |
|----------|------------------------|-------|
| `local`  | Docker on this machine | direct `127.0.0.1`, no tunnel |
| `k8s`    | a Deployment (`helm upgrade --install ops/helm/cuttle`) | standing `kubectl port-forward` on stable local ports |
| `ssh`    | Docker on a remote host | standing `ssh -L` tunnel on stable local ports |
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

The context `proxy` is a server-level default applied to the browser at startup;
geoip (timezone/locale/exit-IP) follows it automatically. A connection can still
override it per-request with `?proxy=`.

- For ssh/k8s, `cuttle up` establishes a standing tunnel on the stable local
  ports (default 9222/6080) that outlives the command; `status` health-checks and
  re-establishes it, `down` tears it down.
- `cuttle open [url]` optionally navigates there, prints the driver briefing,
  opens the viewer, and holds the session until Ctrl-C - use it for logins and
  interactive or agent sessions (`login`/`connect` are deprecated aliases).

## The profile

The session's Chrome profile (cookies + localStorage + IndexedDB + service
workers) lives in a named Docker volume (`cuttle-<container>-profile`), or a PVC
on the k8s backend, so it survives `cuttle up --recreate` and image upgrades with
no flag. Reset it with `cuttle up --recreate --purge-profile`, `cuttle
purge-profile`, or `cuttle down --purge`; `cuttle up --ephemeral` opts out for a
disposable session. An older config's `[profile.*]` blocks are ignored.

## `cuttle serve`

`cuttle serve` is the in-container daemon (the image entrypoint): the CDP
multiplexer itself. It binds `0.0.0.0:9222` inside a container (detected for
docker/podman/k8s) and `127.0.0.1` on bare metal, answers authenticated-proxy
`407`s over CDP, and rewrites the `webSocketDebuggerUrl` host to the request's
Host header so it stays correct behind a port-forward or ssh tunnel. `--mode`
(`CUTTLE_MODE`) picks `session` (default: one browser, `?fingerprint=` refused)
or `pool` (one Chrome per `?fingerprint=` seed, seed required). `CUTTLE_PROXY`
sets a default proxy; `CUTTLE_HOST` overrides the bind host;
`CUTTLE_IDLE_TIMEOUT` (set by `cuttle up --idle-timeout`) reaps an idle browser.

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
self-built stealth-Chromium engine and the KasmVNC/noVNC stages.

## Licensing

MIT ([LICENSE](LICENSE)). The image ships our own stealth-Chromium build
(ungoogled-chromium + the clark (MIT) patch series, built by
`packages/browser`) plus the KasmVNC (GPL-2.0) / noVNC (MPL-2.0) viewer stack;
the fingerprint and serve code is authored Go. No proprietary or licensed
browser binary is used or redistributed. Full notices and attributions in
[docs/THIRD-PARTY.md](docs/THIRD-PARTY.md).
