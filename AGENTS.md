# AGENTS.md - cuttle monorepo

cuttle is a stealth-Chromium CDP farm being rewritten from Python to Go. The
full plan (read it before any work): `docs/plans/2607-16-cuttle-go-rewrite.md`.

## Layout

- `packages/cuttle-py/` - the original Python implementation, kept working as
  the reference oracle until the Go rewrite reaches parity. Its own
  `AGENTS.md` inside governs all work there. CRITICAL: `packages/cuttle-py/vendor/`
  is vendored upstream code - never reformat, re-lint, or restyle it.
- `packages/cuttle-go/` - the new authored Go implementation (CLI + `cuttle
  serve` daemon + fingerprint wrapper). Go 1.26, gofumpt, golangci-lint v2,
  just. Module: `github.com/glim-sh/cuttle/packages/cuttle-go`.
- `ops/helm/cuttle/` - shared Helm chart for the k8s backend.
- `docs/` - shared docs (plans). Package-specific docs live in each package.

## Non-negotiables (apply repo-wide)

- This is a PUBLIC repo. Never add internal infra references (clusters, k8s
  namespaces, proxies, secrets), named commercial scraping targets or
  "bypass X on <site>" framing, or any credential.
- No proprietary binaries: only free forks (clark MIT, clearcote BSD-3) and
  the MIT cloakserve/cloakbrowser subset. No CloakBrowser license/Pro code.
- Stealth parity is the whole game: any change to fingerprint arg-building,
  proxy normalization, or geoip in Go must keep byte-parity with the Python
  oracle (see the parity tests in `packages/cuttle-go`).
- Conventional Commits (`type(scope): description`); releases are
  release-please-driven from `main`.
