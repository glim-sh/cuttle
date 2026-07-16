# AGENTS.md - cuttle

cuttle is a stealth-Chromium CDP farm: `cuttle serve` is a CDP multiplexer that
spawns one stealth Chrome per fingerprint seed, routing per-seed identity
(fingerprint, proxy, geoip) over CDP. A single static Go binary; the daemon runs
in a Python-free container.

## Layout

- `cmd/cuttle/` - CLI entrypoint. `internal/` - the packages (serve daemon,
  fingerprint arg-builder, backends, profile store, cdp, config, mcp). Go 1.26,
  gofumpt, golangci-lint v2, just. Module: `github.com/glim-sh/cuttle`.
- `Dockerfile` - the stealth-Chromium runtime image (clark/clearcote forks,
  headed Xvfb/openbox, KasmVNC, pre-baked Windows fonts). linux/amd64 only.
- `winfonts/` - pre-baked metric-compatible free fonts reporting Windows family
  names (see `winfonts/README.md`).
- `test/smoke/` - neutral, self-contained CDP smoke harness (`go run
  ./test/smoke` against a running container).
- `ops/helm/cuttle/` - Helm chart for the k8s backend. `docs/` - plans + docs.

## Non-negotiables

- This is a PUBLIC repo. Never add internal infra references (clusters, k8s
  namespaces, proxies, secrets), named commercial scraping targets or
  "bypass X on <site>" framing, or any credential.
- No proprietary binaries: only free forks (clark MIT, clearcote BSD-3) and
  the MIT cloakserve/cloakbrowser subset. No CloakBrowser license/Pro code.
- Stealth parity is the whole game: fingerprint arg-building, proxy
  normalization, and geoip are locked by the golden in
  `internal/fingerprint/testdata/golden.json` (regenerate with `just
  parity-golden`). A change there must be a reviewed golden diff, never silent.
- Conventional Commits (`type(scope): description`); releases are
  release-please-driven from `main`, built and published by GoReleaser.
