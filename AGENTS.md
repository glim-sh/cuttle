# AGENTS.md - working in cuttle

cuttle is a stealth-Chromium CDP farm: a patched CDP multiplexer (`bin/cuttleserve`)
that spawns one stealth Chrome per fingerprint seed, with a free redistributable
fork binary (clark/clearcote) baked into the image. The real deliverable is the
Docker image (`ghcr.io/glim-sh/cuttle`). Read `README.md` for the product view.

## The one rule that shapes everything: the vendored Python lives under `vendor/`

- `vendor/cloakbrowser/` (`config.py`, `geoip.py` verbatim; `browser.py` trimmed)
  and `bin/cuttleserve` (patched `cloakserve`) are vendored/patched from upstream
  CloakHQ `cloakbrowser`. **Do NOT reformat, re-lint, re-type, or restyle them.**
  Reformatting breaks the "verbatim" provenance and blows up `scripts/sync.sh` diffs.
  `ruff`/`ty` are scoped to exclude `vendor/`; keep it that way. The package imports
  as `cloakbrowser` (via `[tool.setuptools.package-dir]`) so cuttleserve's imports
  and the sync diff stay upstream-verbatim.
- To update the vendored subset: `just vendor-sync` fetches the pinned upstream
  (see `docs/UPSTREAM.md`) and prints a reviewable diff; re-apply the trims/patches
  by hand, then bump the pinned ref. Never blind-copy over the vendored files.
- Authored code you own and should keep clean: the `cuttle/` package (host CLI +
  experimental CDP-screencast viewer), `test/harness.py`, `scripts/rename-fonts.py`,
  the `Dockerfile`, docs, and the small glue in `cuttleserve` (the proxy-auth
  injection + the `_stamp_sw_context` service_worker stamp - treat those two
  patches as load-bearing; they are why the fork works).

## Dev stack (uv + ruff + ty + just)

```bash
uv sync            # create/refresh .venv from uv.lock (deps + dev tools)
just check         # ty check + ruff check --fix + ruff format (authored code only)
just build         # docker buildx build --platform linux/amd64 -> cuttle:local
just smoke         # build, run a throwaway container, run test/harness.py against it
just vendor-sync   # show upstream drift of the vendored subset
just release       # bump + changelog + tag + push; CI publishes (docs/RELEASING.md)
uv add <pkg>       # add a runtime dep (updates pyproject + uv.lock)
```

- Python 3.12 (matches the base image + vendored code). `uv.lock` is committed and
  pins the image's Python deps - the Dockerfile installs from it (reproducible builds).
- The image is **linux/amd64 only**: clark/clearcote ship linux-x64 prebuilts. On an
  arm64 host the build/run is emulated (fine for local dev + a smoke; ~2s page
  loads). Production runs it native on an amd64 server.

## Validation model (two layers, different jobs)

- `test/harness.py` - fast, local, client-agnostic (raw CDP over `websockets`).
  Checks per-seed fingerprint isolation, stealth coherence, connection stability.
  It CANNOT observe the playwright-core service_worker crash (only a playwright client
  can) and does not clear real challenges. Run it on any bump.
- **Real amd64 deployment** - runs the actual playwright-core consumer path against
  live sites. This is the gate that surfaces a new playwright-crashing CDP quirk and
  confirms real challenge clears. See `docs/UPGRADE.md`. Keep this validation OUT of
  this public repo (it names real sites); it lives with the consumer.

## Bind host (don't regress this)

`cuttleserve` serves CDP on `0.0.0.0:9222` inside a container and `127.0.0.1` on
bare metal (loopback-only there, for safety). "Inside a container" is detected for
docker, podman, AND k8s/containerd - via `/.dockerenv`, `/run/.containerenv`,
`KUBERNETES_SERVICE_HOST`, or a container cgroup - and `CUTTLESERVE_HOST` overrides.
Do NOT gate the bind on `/.dockerenv`/`/run/.containerenv` alone: under containerd
BOTH markers are absent, which silently pins the listener to loopback and refuses
every cross-container client. Docker Desktop has `/.dockerenv`, so a Docker-only
setup hides the bug - the real-amd64 gate (cross-container) is what catches it. This
is why no `touch /run/.containerenv` startup hack is needed anywhere.

## Bumping Chrome

Update the pinned `CLARK_*` / `CLEARCOTE_*` build args in the `Dockerfile`, rebuild,
`just smoke`, then run the real-amd64 gate. `docs/UPGRADE.md` has the runbook;
`docs/BUILD-FROM-SOURCE.md` is break-glass only.

## Non-negotiables

- **This is a PUBLIC repo.** Never add: internal infra references (clusters, k8s,
  namespaces, proxies, secrets), named commercial scraping targets or "bypass X on
  <site>" framing, or any credential. The harness stays neutral and self-contained.
- **No proprietary binary.** cuttle uses only free forks (clark MIT, clearcote BSD-3)
  and the MIT `cloakserve`/`cloakbrowser` subset. Keep `LICENSE` + `THIRD-PARTY.md`
  accurate; never bake a CloakBrowser binary or its base image into any layer.
- **No license/Pro code.** The vendored subset deliberately excludes CloakBrowser's
  license/widevine/launch machinery. Do not reintroduce it.
- `CLOAKBROWSER_BINARY_PATH` selects the engine (`/opt/clark/chrome` default,
  `/opt/clearcote/chrome` fallback) - kept as the fork's own override contract.
