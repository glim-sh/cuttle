# cuttle

Your agent's stock browser fails on the real web. Sites show captchas,
Cloudflare walls and "verify you are human" pages. The browser forgets every
login when the process ends. You cannot see what the agent sees. cuttle is a
browser for your agent that fixes these three problems:

- **Not blocked.** The browser is a patched Chromium build that looks like a
  normal person's browser. Each profile has one consistent identity:
  fingerprint, proxy, IP location, locale and timezone.
- **Stays signed in.** Sign in once through the viewer. The login persists
  across agent sessions and across restarts.
- **You can step in.** A built-in viewer shows the live browser. When a site
  wants a human, you solve the captcha or 2FA yourself and the agent continues.

It works with playwright-cli, agent-browser and browser-use.

## Why not Claude in Chrome?

The two overlap a lot. Both use a real Chrome, keep your logins, pass bot
walls, and read pages fast. cuttle does five things the extension does not:

1. **Work the extension's policy refuses.** Bank exports, tax portals,
   government and work accounts, sensitive data. Anthropic tells you not to
   use the extension for these. With cuttle you are the operator.
2. **Take the session out of the browser.** Read the full cookie jar,
   httpOnly included. See request and response bodies. Call any origin.
   Then replay the site's internal API from Python or curl. The extension
   can call an endpoint inside the tab; it cannot show you the response or
   hand the session to your code.
3. **Several identities at once, started by the agent.** `cuttle up --name
   work` gives a work persona or a second person its own browser and logins
   next to yours. The extension is one Chrome profile per session, switched
   by hand, carrying whatever that profile carries.
4. **Any agent, any machine.** Claude Code, Codex, browser-use or a cron job
   attach over CDP. It runs in Docker, on a server or on Kubernetes, with
   your laptop closed. The extension is Claude, in your desktop Chrome,
   while it is open.
5. **Take over from anywhere.** The viewer opens in any browser or on a
   phone. You solve the captcha or 2FA; the agent continues in the same
   tab. The extension's handoff is the desk the browser is on.

Not a reason to choose cuttle: persistent logins, bot walls, page-reading
speed, password-manager sign-in. Both do those.

**How it works.** cuttle runs one Chrome per profile behind a single Chrome
DevTools Protocol (CDP) endpoint. Any CDP client attaches to it, so your
existing driver and scripts do not change. The browser runs where you want
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
(reap an idle per-profile browser; `0` = off), and `--name <name>` (run several
isolated docker instances on one host - each gets its own container, profile
volume, and ports). `cuttle skill` prints the full
agent-facing guide. Point any CDP
client at the printed endpoint and select a profile (`?fingerprint=<name>`):

```
http://127.0.0.1:9222?fingerprint=12345
http://127.0.0.1:9222?fingerprint=12345&timezone=America/New_York&locale=en-US
```

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

The context `proxy` is a server-level default applied to every profile at startup;
geoip (timezone/locale/exit-IP) follows it automatically. A connection can still
override it per-request with `?proxy=`.

- For ssh/k8s, `cuttle up` establishes a standing tunnel on the stable local
  ports (default 9222/6080) that outlives the command; `status` health-checks and
  re-establishes it, `down` tears it down.
- `cuttle open [url]` optionally navigates there, prints the driver briefing,
  opens the viewer, and holds the session until Ctrl-C - use it for logins and
  interactive or agent sessions (`login`/`connect` are deprecated aliases).

## Profiles (local-canonical auth state)

A named **profile** is a browser identity whose auth state lives on your machine at
`$XDG_DATA_HOME/cuttle/profiles/<name>/storage_state.json` (Playwright
storageState shape: cookies + per-origin localStorage). `--profile <name>` on
`cuttle open` checks the state in for the session and back out on exit; any CDP
client selects the same identity by appending `?fingerprint=<name>`.

```toml
[profile.linkedin]
storage = "local"     # default: checkout/checkin over CDP, nothing persists remotely
[profile.bot]
storage = "remote"    # durable on the browser host (autonomous / always-on)
```

Local-canonical flow: at session start the profile's stored state is injected
into a freshly spawned remote browser over CDP; the daemon checkpoints it back (on
last-client detach, a slow backstop timer, and clean shutdown), and `cuttle down`
pulls every running named profile's state into the local store before stopping
(skipped on `--purge`, an explicit discard). So `--recreate`, `--purge`, and box
loss no longer strand named logins. A single-writer lock prevents a profile from
being attached in two places at once.

The **default (unnamed) session** is durable by default with full Chrome-profile
fidelity (cookies + localStorage + IndexedDB + service workers): its profile
lives in a named Docker volume (`cuttle-<container>-profile`), or a PVC on the
k8s backend, so it survives `cuttle up --recreate` and image upgrades with no
named profile. Reset it with `cuttle up --recreate --purge-profile`, `cuttle
purge-profile`, or `cuttle down --purge`; `cuttle up --ephemeral` opts out for a
disposable session.

Honest caveat: state resides locally *at rest*, but during an active session the
live cookies are necessarily on the remote browser (it must hold them to act as
you). `storage = "remote"` skips checkout/checkin entirely for always-on use where
your machine is not present to inject state.

## `cuttle serve`

`cuttle serve` is the in-container daemon (the image entrypoint): the CDP
multiplexer itself. It binds `0.0.0.0:9222` inside a container (detected for
docker/podman/k8s) and `127.0.0.1` on bare metal, spawns one Chrome per
`?fingerprint=` profile, answers authenticated-proxy `407`s over CDP, and rewrites
the `webSocketDebuggerUrl` host to the request's Host header so it stays correct
behind a port-forward or ssh tunnel. `CUTTLE_PROXY` sets a default proxy;
`CUTTLE_HOST` overrides the bind host; `CUTTLE_IDLE_TIMEOUT` (set by
`cuttle up --idle-timeout`) reaps an idle per-profile browser.

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
