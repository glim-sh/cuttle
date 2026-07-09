# Upgrading the Chrome engine

The whole point of cuttle: collapse a multi-upstream hand-reconciliation into
one decision and one test. To move to a new Chrome major (or patch):

1. Pick the new clark (and/or clearcote) release for the target Chrome version.
   Update the `CLARK_TAG` / `CLARK_ASSET` / `CLARK_SHA256` (and clearcote's)
   build args in the `Dockerfile`. Get the sha256 from the release's checksum,
   or `curl -fsSL <asset-url> | sha256sum`.

2. If the vendored `cloakserve` upstream moved and matters, re-sync `vendor/`
   at the new ref: run `vendor/sync.sh`, review the diff, re-apply cuttle's
   trims/patches, and bump the pinned ref in `vendor/UPSTREAM.md`.

3. Confirm the flag dialect still holds - does the new binary still honor the
   `--fingerprint-*` flags cuttleserve emits? - and watch for a new CDP quirk
   (like Chrome 148's empty service_worker `browserContextId`). The harness
   surfaces both.

4. Build the image and validate in two layers - they cover different risks:
   - **`test/harness.py` (fast, local).** Confirms the new binary still applies
     fingerprints (coherent UA/platform), isolates seeds (distinct canvas), looks
     stealthy (`navigator.webdriver` falsy), and connects cleanly under cold
     cycling. This is client-agnostic (raw CDP), so it CANNOT observe a new Chrome
     CDP quirk that crashes a playwright client (the class of bug the
     service_worker stamp fixes) - see next.
   - **Real amd64 deployment (the gate).** Run the actual playwright-core consumer
     path against live sites on a real amd64 host. This is the only thing that
     surfaces a new playwright-crashing CDP quirk AND confirms real challenge
     clears. Emulated amd64-on-arm64 is fine for a smoke; real amd64 is the gate.

5. On green, publish a new `ghcr.io/glim-sh/cuttle` image (tag `v*`), then bump
   the consumed digest wherever cuttle is deployed.

If both forks stall on a needed Chrome major, see `docs/BUILD-FROM-SOURCE.md`
(break-glass only).
