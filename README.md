# cuttle

A stealth-Chromium CDP farm. `cuttle` runs a patched Chrome DevTools Protocol
(CDP) multiplexer that spawns one stealth Chrome per fingerprint seed, giving
each seed its own coherent browser identity - fingerprint, proxy, geoip, locale,
and timezone - behind a single CDP endpoint. Point any CDP client (Playwright,
Puppeteer, `chromium.connectOverCDP`) at it and select a seed with a query
parameter.

The Chrome engine is a free, redistributable stealth-Chromium fork baked into
the image - [clark](https://github.com/clark-labs-inc/clark-browser) (MIT) by
default, [clearcote](https://github.com/clearcotelabs/clearcote-browser)
(BSD-3) as a fallback - so there is no proprietary binary and no license to
manage. The multiplexer derives from CloakHQ's MIT-licensed
[`cloakserve`](https://github.com/CloakHQ/cloakbrowser).

Maintained by [glim.sh](https://glim.sh).

## Repository layout (monorepo)

cuttle is mid-rewrite from Python to Go. Both implementations live here; each
package carries its own build/lint/test config.

| Path | What it is |
|------|------------|
| [`packages/cuttle-py`](packages/cuttle-py) | **The shipped implementation** (published to PyPI as `cuttle-browser`, image on GHCR). Now also the **reference oracle** the Go port is validated against. |
| [`packages/cuttle-go`](packages/cuttle-go) | **The future.** The Go rewrite: one owned codebase folding the CLI, the in-container daemon, and the `cloakbrowser` wrapper into first-class Go, plus remote backends (k8s/ssh/direct) and local-canonical profiles. |
| [`ops/helm/cuttle`](ops/helm/cuttle) | Helm chart for the `k8s` backend (Deployment + Service + deny-all NetworkPolicy). |
| [`docs`](docs) | Shared docs, including the [rewrite plan](docs/plans/2607-16-cuttle-go-rewrite.md). |

The Go implementation is not yet the default: it ships once it reaches
fingerprint/stealth parity with the Python oracle and passes the
stealth-verification gate, after which `cuttle-py` is retired and `cuttle-go`
is promoted to the repo root. Until then, use the Python package.

## Why

The stealth-scraping stack normally reconciles several independently-drifting
upstreams by hand: the CDP multiplexer, the base image, and the Chrome fork
binary - each moving on its own schedule, and Chrome ships a new major roughly
every four weeks. cuttle owns the orchestration in one repo and consumes the
browser as a pinned prebuilt, turning "always check what still works" into "pick
a binary release, run the harness, ship or don't." One decision, one test.

## Quickstart

```bash
docker run --rm -p 9222:9222 ghcr.io/glim-sh/cuttle:latest
```

Then connect a CDP client and pass a fingerprint seed:

```
http://127.0.0.1:9222?fingerprint=12345
http://127.0.0.1:9222?fingerprint=12345&timezone=America/New_York&locale=en-US
```

Each distinct `fingerprint` seed gets its own isolated Chrome with a stable,
coherent identity. To route a seed through an authenticated residential proxy,
pass it on the connect URL; cuttle strips the inline credentials and answers the
proxy's `407` over CDP, so fork binaries that reject inline credentials still
work. The image runs **headed by default** (headed Chrome clears escalated
anti-bot challenges that headless cannot).

## CLI (daily driver + login handoff)

For a persistent local browser you can watch and log into via VNC - then drive
over CDP - install the host CLI, published on PyPI as **`cuttle-browser`** (the
command it installs is **`cuttle`**):

```bash
brew install tenequm/tap/cuttle       # homebrew (macOS/Linux)
uv tool install cuttle-browser        # or: pipx install cuttle-browser
uvx --from cuttle-browser cuttle up   # or one-off with no install
```

```bash
cuttle up                                    # start the container + VNC viewer
cuttle login https://accounts.google.com     # sign in once via the viewer
cuttle down                                   # graceful stop, keeps the profile
```

Full workflow: [`packages/cuttle-py/SKILL.md`](packages/cuttle-py/SKILL.md) (or
`cuttle skill`). The in-progress Go CLI adds `k8s`/`ssh`/`direct` contexts,
local-canonical profiles, and `cuttle mcp` - see
[`packages/cuttle-go/README.md`](packages/cuttle-go/README.md).

## Engine swap

Both fork binaries are baked in and selected by `CLOAKBROWSER_BINARY_PATH`:

- `/opt/clark/chrome` - clark, Chrome 148 (default)
- `/opt/clearcote/chrome` - clearcote, Chrome 149 (fallback)

```bash
docker run --rm -p 9222:9222 \
  -e CLOAKBROWSER_BINARY_PATH=/opt/clearcote/chrome \
  ghcr.io/glim-sh/cuttle:latest
```

## Notes and limits

- The image is **linux/amd64 only**: the clark/clearcote prebuilts ship linux-x64
  binaries. On an Apple Silicon host it runs emulated (fine for local dev and the
  smoke); production runs it native on an amd64 server.
- cuttle does not include a browser-automation client library - use any CDP
  client. It is the farm, not the scraper.

## Licensing

cuttle is MIT ([LICENSE](LICENSE)). It vendors and redistributes third-party
software under their own terms - CloakHQ's `cloakserve`/`cloakbrowser` (MIT),
clark (MIT), and clearcote (BSD-3). No proprietary CloakBrowser binary is used
or redistributed. Full attributions and license texts are in
[`packages/cuttle-py/THIRD-PARTY.md`](packages/cuttle-py/THIRD-PARTY.md).
