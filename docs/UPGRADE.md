# Upgrading the Chrome engine

The whole point of cuttle: collapse a multi-upstream hand-reconciliation into
one decision and one test. To move to a new Chrome major (or patch):

1. Pick the new clark (and/or clearcote) release for the target Chrome version.
   Update the `CLARK_TAG` / `CLARK_ASSET` / `CLARK_SHA256` (and clearcote's)
   build args in `ops/docker/Dockerfile`. Get the sha256 from the release's
   checksum, or `curl -fsSL <asset-url> | sha256sum`.

2. If the upstream stealth/CDP behavior cuttle mirrors has changed, reconcile
   it in the Go port - the load-bearing patches (proxy-auth-over-CDP, the
   service_worker `browserContextId` stamp, fork launch-parity flags) live in
   `internal/serve/wsproxy.go`, and the fingerprint arg-building in
   `internal/fingerprint/args.go`. Any change to argv/proxy/geoip output must be
   a reviewed `internal/fingerprint/testdata/golden.json` diff (regenerate with
   `just parity-golden`).

3. Confirm the flag dialect still holds - does the new binary still honor the
   `--fingerprint-*` flags `cuttle serve` emits? - and watch for a new CDP quirk
   (like Chrome 148's empty service_worker `browserContextId`). The smoke harness
   surfaces both.

4. Build the image and validate in two layers - they cover different risks:
   - **`test/smoke` (`go run ./test/smoke`, fast, local).** Confirms the new
     binary still applies fingerprints (coherent UA/platform), isolates seeds
     (distinct canvas), looks stealthy (`navigator.webdriver` falsy, real GPU via
     ANGLE), and connects cleanly under cold cycling. This is client-agnostic (raw
     CDP), so it CANNOT observe a new Chrome CDP quirk that crashes a playwright
     client (the class of bug the service_worker stamp fixes) - see next.
   - **Real amd64 deployment (the gate).** Run the actual playwright-core consumer
     path against live sites on a real amd64 host. This is the only thing that
     surfaces a new playwright-crashing CDP quirk AND confirms real challenge
     clears. Emulated amd64-on-arm64 is fine for a smoke; real amd64 is the gate.

5. On green, publish a new `ghcr.io/glim-sh/cuttle` image (a `vX.Y.Z` release cuts
   it - see `docs/RELEASING.md`), then bump the consumed digest wherever cuttle is
   deployed.

If both forks stall on a needed Chrome major, building the fork from source is the
break-glass path; prefer waiting for a fork prebuilt (a from-source Chromium build
needs ~80GB disk, ~32GB RAM, and hours, and the `--fingerprint-*` patches must be
rebased onto the new tag).
