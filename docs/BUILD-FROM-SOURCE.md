# Building a stealth-Chromium binary from source (break-glass)

cuttle consumes clark/clearcote as pinned prebuilt binaries. Building Chromium
from source is the single highest-maintenance job in the stack and is NOT part
of the normal pipeline. This is insurance for the case where both forks stall
on a Chrome major you need before either ships a prebuilt.

Both forks are open and buildable:

- **clark-browser** ships a reproducible build chain: `fetch-source.sh` ->
  `apply-patches.sh` -> `build.sh` (plus a `Dockerfile.linux`). The stealth
  behavior is a `--fingerprint-*` patch series applied on top of an
  ungoogled-chromium checkout.
- **clearcote-browser**'s engine is open and buildable on the same lines.

Machine requirements (per build): ~80 GB free disk, 32 GB+ RAM, roughly
4-12 hours of build time on a fast machine.

Caveat: when the Chrome base moves a major version, the `--fingerprint-*`
patch series usually needs a rebase against the new source tree. Budget time
for patch conflicts, not just compile time.

Prefer waiting for a fork prebuilt. Only build from source when there is a hard
deadline and no released binary covers the Chrome version you need.
