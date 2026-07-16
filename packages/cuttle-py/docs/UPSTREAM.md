# Vendored upstream

The `vendor/cloakbrowser/` Python package and `bin/cuttleserve` derive from
CloakHQ's MIT-licensed `cloakbrowser` wrapper. This file pins the exact upstream
revision the vendored files came from so drift is a reviewable diff (see
`scripts/sync.sh`). Everything under `vendor/` is vendored; the authored `cuttle/`
package (CLI + viewer) and `test/` are ours.

- Upstream: https://github.com/CloakHQ/cloakbrowser
- Pinned ref: `v0.4.9` (commit `045219b488e79d9fa091d1e751b98e0a7449afd1`)

## Vendored files and their provenance

| vendored file                    | upstream source                    | modification |
|----------------------------------|------------------------------------|--------------|
| `vendor/cloakbrowser/config.py`  | `cloakbrowser/config.py`           | verbatim except one unreachable Pro-license branch neutered (cuttle always runs a `CLOAKBROWSER_BINARY_PATH` override, so `.license` is never imported) |
| `vendor/cloakbrowser/geoip.py`   | `cloakbrowser/geoip.py`            | verbatim except three install-hint strings (upstream named the `cloakbrowser[geoip]` pip extra; neutralized - geoip2/httpx/socksio ship in cuttle's container-only `server` dep group, not the published CLI) |
| `vendor/cloakbrowser/browser.py` | `cloakbrowser/browser.py`          | 3 module-level imports (license/widevine/human) replaced with local stubs; only the CDP argument-builders are used |
| `vendor/cloakbrowser/download.py`| `cloakbrowser/download.py`         | trimmed to the `CLOAKBROWSER_BINARY_PATH` override path only (no Pro download / httpx / license) |
| `bin/cuttleserve`                | `bin/cloakserve` (patched)         | proxy-auth-over-CDP injection + service_worker browserContextId stamp + fork launch-parity flags; imports stay upstream-verbatim (`cloakbrowser.*`) |

NOT vendored (intentionally dropped): `license.py`, `widevine.py`, `human/`,
`__main__.py`, and CloakBrowser's proprietary binary (`BINARY-LICENSE.md`).

To re-sync after an upstream bump, run `scripts/sync.sh` and review the diff.
