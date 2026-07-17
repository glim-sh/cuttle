# AGENTS.md - cuttle

cuttle is a stealth-Chromium CDP farm: `cuttle serve` is a CDP multiplexer that
spawns one stealth Chrome per fingerprint seed, routing per-seed identity
(fingerprint, proxy, geoip) over CDP. A single static Go binary; the daemon runs
in a Python-free container.

## Layout

- `cmd/cuttle/` - CLI entrypoint. `internal/` - the packages (serve daemon,
  fingerprint arg-builder, backends, profile store, cdp, config, mcp). Go 1.26,
  gofumpt, golangci-lint v2, just. Module: `github.com/glim-sh/cuttle`.
- `ops/docker/` - the container build assets: `Dockerfile` (stealth-Chromium
  runtime, clark/clearcote forks + headed Xvfb/openbox + KasmVNC, linux/amd64
  only), `bin/` (entrypoint + VNC viewer), and `winfonts/` (pre-baked
  metric-compatible free fonts reporting Windows family names; see its README).
  Build context is the repo root: `just build-image` (or `docker build -f
  ops/docker/Dockerfile .`).
- `test/smoke/` - neutral, self-contained CDP smoke harness (`go run
  ./test/smoke` against a running container).
- `ops/helm/cuttle/` - Helm chart for the k8s backend.
- `docs/` - `RELEASING.md` (release + versioning contract), `UPGRADE.md`
  (real-amd64 deployment gate), `STEALTH-VERIFICATION.md`, `THIRD-PARTY.md`.

## Non-negotiables

- This is a PUBLIC repo. Never add internal infra references (clusters, k8s
  namespaces, proxies, secrets), named commercial scraping targets or
  "bypass X on <site>" framing, or any credential.
- No proprietary binaries: only the free stealth-Chromium forks (clark MIT,
  clearcote BSD-3). The daemon and fingerprint code are authored Go, not vendored
  from any licensed browser product.
- Stealth output is the whole game: fingerprint arg-building, proxy
  normalization, and geoip are snapshotted in the golden
  `internal/fingerprint/testdata/golden.json` (regenerate with `just
  parity-golden`). The golden is a regression tripwire - it turns any change to
  that output into a diff someone must consciously regenerate and review, so a
  stealth drift can never land silently. (It was originally captured
  byte-for-byte from the now-removed Python oracle.)
- Conventional Commits (`type(scope): description`); releases are
  release-please-driven from `main`, built and published by GoReleaser. The
  commit type decides whether a release happens at all, and the rules are not
  what you would guess - read `docs/RELEASING.md` before picking a type,
  reasoning about a version, or touching release config.
