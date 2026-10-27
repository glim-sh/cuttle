---
type: Finding
title: A profile volume alone does not make a session identity durable
description: A session-mode cuttle serve run with a volume on its data dir but without --keep-profile (CUTTLE_KEEP_PROFILE=1) is disposable - every browser launch mints a new fingerprint seed while the daemon re-injects the captured cookies and storage, so one signed-in account reappears on a different device after each restart; cuttle up sets both, a hand-written docker run must too.
tags: [serve, profile, identity, fingerprint, deployment]
status: stable
generated: { by: claude-code/claude-opus-5-5, at: "2026-10-27T16:30:00+00:00" }
sources:
  - id: pool
    resource: /packages/cuttle/internal/serve/pool.go
    title: defaultFingerprintSeed, persistedSeedIn, safeRemoveTree
  - id: supervise
    resource: /packages/cuttle/internal/serve/supervise.go
    title: durableProfile, supervised, captureAndTerminate
  - id: serve
    resource: /packages/cuttle/internal/serve/serve.go
    title: --keep-profile defaults to false; the session startup log states durable or disposable
  - id: local
    resource: /packages/cuttle/internal/backend/local.go
    title: cuttle up mounts the profile volume and sets CUTTLE_KEEP_PROFILE=1 unless --ephemeral
  - id: report
    resource: "self-hosted deployment report, 2026-10-26: cuttle serve run by its own docker run with a profile volume and no CUTTLE_KEEP_PROFILE; the seed changed on every launch (65252, 27655, 31579, 79727) while cookies survived restarts"
    title: Deployment report
---

# A profile volume alone does not make a session identity durable

A session profile is durable only when `--keep-profile` is set and
`--ephemeral` is not.[^supervise] `cuttle serve` defaults `--keep-profile` to
false;[^serve] `cuttle up` mounts a named volume on the data dir and sets
`CUTTLE_KEEP_PROFILE=1` for every instance that is not `--ephemeral`.[^local]
Running the image by hand with only the volume looks persistent and is not.

What the disposable path does:[^pool][^supervise]

- **The fingerprint seed rotates.** The default seed's `--fingerprint` value is
  persisted beside the profile only when the profile is durable; otherwise
  every launch draws a new random seed.
- **The profile dir is deleted** when the daemon stops the browser.
- **Logins survive anyway.** The reserved default seed is always supervised:
  its cookies and storage are captured over CDP on shutdown and re-injected at
  the next launch.

Together that is one account returning on a different canvas, WebGL and font
identity after each restart - the returning-session correlation a stealth
browser exists to avoid. One deployment saw four seeds in four launches with
its cookies intact.[^report]

The daemon now says which mode it is in: the session startup log reads
`session profile is durable` or `disposable`, and `GET /` reports
`durable_profile`.[^serve][^pool] `--fingerprint=<seed>` is no workaround -
session mode rejects a server-level default seed.[^serve]

[^pool]: defaultFingerprintSeed, persistedSeedIn, safeRemoveTree
[^supervise]: durableProfile, supervised, captureAndTerminate
[^serve]: --keep-profile defaults to false; the session startup log states durable or disposable
[^local]: cuttle up mounts the profile volume and sets CUTTLE_KEEP_PROFILE=1 unless --ephemeral
[^report]: Deployment report
