---
type: Finding
title: The profile dir is the session's artifact store, not Chrome's scratch
description: A relaunch of the same seed must reuse its profile dir - downloads and driver outputs live there; only an ephemeral run may delete it.
tags: [serve, pool, downloads, relaunch, cuttle-pw]
status: stable
generated: { by: claude-code/fable-5, at: "2026-09-18T14:44:00+00:00" }
sources:
  - id: code
    resource: /packages/cuttle/internal/serve/pool.go
    title: terminateForRelaunch and its tests (TestDefaultSeedRelaunchKeepsDownloads, TestEphemeralProfileDir)
  - id: live
    resource: "live validation on a local image build, 2026-09-18: marker file and dir inode survived a mid-session browser kill in the previously-failing non-keep-profile config"
    title: Live validation
---

# Finding

A seed's profile dir doubles as the session's artifact store: page downloads
and `cuttle pw --filename` outputs land in its `Downloads/`, and
`cuttle downloads` serves them from there. So when a dead browser is
relaunched for the same seed, the dir must be reused, not recreated -
`terminateForRelaunch` stops the process and keeps the dir, and only the
ephemeral mode (where each launch mints a fresh scratch dir that would
otherwise leak) still deletes it.[^code]

Before the fix (aac8e40), the dead-instance path called the full
`terminate`, whose `safeRemoveTree` wiped the dir unless
`CUTTLE_KEEP_PROFILE` was set - `cuttle up` sets it, so the wipe only bit
plain `cuttle serve` runs, and it bit every seed, not just the reserved
one (named seeds were wiped on the next attach rather than on
self-heal).[^code][^live]

Do not "simplify" `terminateForRelaunch` back into `terminate`: the two
tests in the sources pin exactly this invariant, and undoing it silently
destroys user artifacts on every browser crash.

Related: [Downloads API is seed-keyed](/findings/downloads-seed-keying.md).

[^code]: terminateForRelaunch and its tests (TestDefaultSeedRelaunchKeepsDownloads, TestEphemeralProfileDir)
[^live]: Live validation
