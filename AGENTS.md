# AGENTS.md - cuttle

cuttle is a stealth-Chromium CDP farm: `cuttle serve` is a CDP multiplexer that
spawns one stealth Chrome per fingerprint seed, routing per-seed identity
(fingerprint, proxy, geoip) over CDP. A single static Go binary; the daemon runs
in a Python-free container.

## Layout

- `cmd/cuttle/` - CLI entrypoint. `internal/` - the packages (cli incl. the
  embedded SKILL.md, serve daemon, fingerprint arg-builder, backends, profile
  store, cdp, config). Go 1.26,
  gofumpt, golangci-lint v2, just. Module: `github.com/glim-sh/cuttle`.
- `packages/browser/` - the self-hosted stealth-Chromium build pipeline: the
  patch series, the Linux build driver (`build/`), the Hetzner build-host scripts
  (`hetzner/`), and the behavioral validate harness (`validate/`). `versions.env`
  is the version/sha pin. See its README.
- `ops/docker/` - the container build assets: `Dockerfile` (stealth-Chromium
  runtime + headed Xvfb/openbox + KasmVNC; multi-arch, amd64 = Windows persona,
  arm64 = macOS persona), `bin/` (entrypoint + VNC viewer), `winfonts/README.md`
  (how the metric-compatible free font packs are built and renamed) and
  `macfonts/metrics.json` (the macOS metrics table those packs are stamped with).
  Build context is the repo root: `just build-image` (or `docker build -f
  ops/docker/Dockerfile .`). The build-context filter is
  `ops/docker/Dockerfile.dockerignore` (BuildKit's per-Dockerfile ignore file,
  takes precedence over any root `.dockerignore` - there is none here).
- `test/smoke/` - neutral, self-contained CDP smoke harness (`go run
  ./test/smoke` against a running container).
- `ops/helm/cuttle/` - Helm chart for the k8s backend.
- `docs/` - `RELEASING.md` (release + versioning contract), `UPGRADE.md`
  (real-amd64 deployment gate), `STEALTH-VERIFICATION.md`, `THIRD-PARTY.md`,
  plus the kept post-mortem of the removed macOS backend.

## Non-negotiables

- KISS and YAGNI. Build the simplest thing that satisfies a real, present need;
  do not add config knobs, abstractions, backends, or interface seams for a
  hypothetical future caller. A second implementation earns an abstraction; one
  does not. Prefer deleting code to generalizing it. If a new file, flag, or
  indirection cannot name the concrete need it serves today, drop it.
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
- A PR body becomes the squashed commit body that release-please parses. Never
  start a body line with `word(` unless the `)` closes on that same line (code
  fences included) - the parser throws, release-please drops the whole commit,
  and the release is skipped silently with CI green.
