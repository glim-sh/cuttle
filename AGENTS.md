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
uv sync            # create/refresh .venv from uv.lock (CLI deps + dev tools)
uv sync --group server   # + the container-only deps, to run bin/cuttleserve bare-metal
just check         # ty check + ruff check --fix + ruff format (authored code only)
just build         # docker buildx build --platform linux/amd64 -> cuttle:local
just smoke         # build, run a throwaway container, run test/harness.py against it
just vendor-sync   # show upstream drift of the vendored subset
just release-preview  # preview the changelog the next release-please PR will carry
uv add <pkg>       # add a runtime dep (updates pyproject + uv.lock)
```

- **Deps are split by consumer.** `[project.dependencies]` (aiohttp, websockets) is
  what the published CLI needs - it is all that pip/brew/nix install. The
  container-only `server` group (httpx, geoip2, socksio) is what `bin/cuttleserve`
  needs via `cloakbrowser.geoip`, which imports them lazily; only the Dockerfile
  installs it. Adding a dep to `[project.dependencies]` that only cuttleserve uses
  puts it in the brew formula and every user's install - put it in `server` instead.
- **`cuttle up` defaults to the published image for its own version**, not your
  local build: to drive a local image, `just build` then `cuttle up --image cuttle:local`.
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

## Releasing (release-please, PR-merge-driven)

Releases are driven by release-please (`release-please-config.json`), NOT a local
command. Never hand-craft a release PR or pick a version by hand - release-please
derives both. Full runbook: `docs/RELEASING.md`.

- **Flow.** Land conventional commits on `main` -> release-please auto-opens/updates
  one `chore(main): release X.Y.Z` PR (bumping `pyproject.toml` + `SKILL.md`'s
  frontmatter version) -> run the gates (`just smoke` + the real-amd64 check) ->
  **merge that PR**. The merge IS the release: the `release.yml` run then publishes
  PyPI/GHCR/GitHub release/homebrew and syncs `CHANGELOG.md` + `uv.lock` back to
  `main`. A feature PR is never "the release PR".
- **What opens a release PR.** Only `feat`, `fix`, `perf`, `revert` (or any
  breaking-marked commit) are user-facing enough to trigger one. A batch of only
  `chore`/`docs`/`refactor`/`ci`/`test`/`build`/`style` does NOT open a release PR -
  land a `feat`/`fix` too, or force it (below).
- **Version bump (pre-1.0, `bump-*-pre-major` enabled in the config).** Every
  non-breaking commit -> a **patch** bump (`0.3.0` -> `0.3.1`); a breaking marker ->
  a **minor** bump (`0.3.0` -> `0.4.0`), never a 1.0.0 major.
- **Breaking markers count on ANY type - the key difference from pond/release-plz.**
  release-please reads "breaking" off the message, type-agnostically: a `!` header
  (`feat!:` AND `docs!:`, `chore!:`, `refactor!:`) or a `BREAKING CHANGE:` footer
  ALL count, on any type, even on an empty commit. pond's release-plz only honors
  breaking on `feat!`/`fix!` with a real diff - do NOT carry that assumption here.
  Put `!`/`BREAKING CHANGE:` only on the commit you actually mean to cut a minor for.
- **Force a version.** Add `Release-As: X.Y.Z` (case-insensitive) to a commit body
  on `main` before merging - any commit, incl. an empty
  `git commit --allow-empty -m "chore: release X.Y.Z" -m "Release-As: X.Y.Z"`;
  newest wins. Use for a deliberate bump the commit types would not derive (0.3.0
  was forced this way over the natural 0.2.1).
- **Changelog is git-cliff, from commit subjects** (`cliff.toml`; `skip-changelog`
  keeps release-please out of it). Nothing to hand-edit on the release PR - a clear,
  scoped commit subject IS the changelog line. `CHANGELOG.md` and the GitHub release
  body are both git-cliff output; the emoji section taxonomy is owned by
  `cliff.toml`'s `commit_parsers` (ported from pond) - do not restyle it.
- **Prereq (one-time, already enabled).** The `glim-sh` org + `cuttle` repo both
  allow "GitHub Actions to create and approve pull requests"; without it
  release-please cannot open the PR. It runs on the default `github.token`.

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
