# Upgrading the Chrome engine

The whole point of cuttle: collapse a multi-upstream hand-reconciliation into
one decision and one test. To move to a new Chrome major (or patch):

1. Build the new engine from source. cuttle no longer consumes a fork prebuilt -
   `packages/browser/` builds our own stealth-Chromium. Bump `CHROMIUM_VERSION` /
   `UC_TAG` in `packages/browser/versions.env`, rebase the patch series in
   `packages/browser/patches/` onto the new tag, then run the build (see
   `packages/browser/README.md`; a warm Hetzner volume makes this minutes rather
   than hours). Publish the resulting tarballs as a new `browser-vX` release and
   pin them: `BROWSER_RELEASE_TAG` + both `BROWSER_SHA256_*` in `versions.env`,
   and the matching `ARG BROWSER_TAG` / `ADD --checksum` literals in
   `ops/docker/Dockerfile` (they must agree - the Dockerfile is what the image
   actually pulls).

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
     clears. The local arm64 image is a different persona (macOS), not an
     emulated copy of the amd64 one, so it is fine for a smoke but never the gate.

5. On green, publish a new `ghcr.io/glim-sh/cuttle` image (a `vX.Y.Z` release cuts
   it - see `docs/RELEASING.md`), then bump the consumed digest wherever cuttle is
   deployed.

Building from source is now the normal path, not break-glass, so the rebase of
the `--fingerprint-*` patch series onto the new Chromium tag is the real work in
step 1. Budget ~80GB disk and ~32GB RAM on the build host; the persistent cache
volume keeps a warm rebuild to minutes.

Before any rebuild, read `packages/browser/build/README.md` - the two caches
(ninja's `out/` and sccache) behave differently and the incremental-rebuild
gotchas there will otherwise cost you a full recompile.
