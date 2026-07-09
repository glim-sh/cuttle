# cuttle

A stealth-Chromium CDP farm. `cuttle` runs a patched Chrome DevTools Protocol
(CDP) multiplexer that spawns one stealth Chrome per fingerprint seed, giving
each seed its own coherent browser identity - fingerprint, proxy, geoip, locale,
and timezone - behind a single CDP endpoint. Point any CDP client
(Playwright, Puppeteer, `chromium.connectOverCDP`) at it and select a seed with
a query parameter.

The Chrome engine is a free, redistributable stealth-Chromium fork baked into
the image - [clark](https://github.com/clark-labs-inc/clark-browser) (MIT) by
default, [clearcote](https://github.com/clearcotelabs/clearcote-browser)
(BSD-3) as a fallback - so there is no proprietary binary and no license to
manage. The multiplexer is a patched derivative of CloakHQ's MIT-licensed
[`cloakserve`](https://github.com/CloakHQ/cloakbrowser).

Maintained by [glim.sh](https://glim.sh).

## Why

The stealth-scraping stack normally reconciles several independently-drifting
upstreams by hand: the CDP multiplexer, the base image, and the Chrome fork
binary - each of which moves on its own schedule, and Chrome ships a new major
roughly every four weeks. cuttle owns the orchestration in one repo and consumes
the browser as a pinned prebuilt, turning "always check what still works" into
"pick a binary release, run the harness, ship or don't." One decision, one test.

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
work.

The image runs **headed by default** (the default command is
`cuttleserve --headless=false`, on a built-in Xvfb): headed Chrome clears
escalated anti-bot challenges that headless cannot. Override the command only to
change flags or the port.

## Engine swap

Both fork binaries are baked in and selected by `CLOAKBROWSER_BINARY_PATH`:

- `/opt/clark/chrome` - clark, Chrome 148 (default)
- `/opt/clearcote/chrome` - clearcote, Chrome 149 (fallback)

```bash
docker run --rm -p 9222:9222 \
  -e CLOAKBROWSER_BINARY_PATH=/opt/clearcote/chrome \
  ghcr.io/glim-sh/cuttle:latest
```

## Bumping Chrome

Update the pinned `CLARK_*` / `CLEARCOTE_*` build args in the `Dockerfile`,
rebuild, and run the harness. See [docs/UPGRADE.md](docs/UPGRADE.md). Building
a binary from source is documented as break-glass only in
[docs/BUILD-FROM-SOURCE.md](docs/BUILD-FROM-SOURCE.md).

## Testing

`test/harness.py` is a neutral, self-contained smoke (raw CDP over `websockets`)
that drives a running cuttle and checks per-seed fingerprint isolation, stealth
coherence, and connection stability under cold-cycle load. Run it before
publishing any bump. End-to-end validation against live sites is done separately
against a real amd64 deployment. See [test/README.md](test/README.md).

## Architecture

- `bin/cuttleserve` - the patched CDP multiplexer. Per-seed Chrome pool,
  transparent proxy-auth over CDP, a service_worker `browserContextId` stamp
  (so CDP clients do not crash on service workers), and fork launch-parity flags.
- `cuttle/` - a trimmed MIT subset of the `cloakbrowser` wrapper: the CDP
  argument-builders plus geoip/config helpers. No license, widevine, or
  behavioral-automation code.
- `fonts/` - a Windows font pack builder (metric-compatible free fonts renamed
  to Windows family names) so a Windows-claiming fingerprint is coherent.
- `vendor/` - provenance and a re-sync helper for the vendored upstream subset.

## Notes and limits

- The published image is **linux/amd64 only**: the clark/clearcote prebuilts
  ship linux-x64 binaries. The Python multiplexer itself is arch-agnostic.
- cuttle does not include a browser-automation client library - use any CDP
  client. It is the farm, not the scraper.

## Licensing

cuttle is MIT ([LICENSE](LICENSE)). It vendors and redistributes third-party
software under their own terms - CloakHQ's `cloakserve`/`cloakbrowser` (MIT),
clark (MIT), and clearcote (BSD-3). No proprietary CloakBrowser binary is used
or redistributed. Full attributions and license texts are in
[THIRD-PARTY.md](THIRD-PARTY.md).
