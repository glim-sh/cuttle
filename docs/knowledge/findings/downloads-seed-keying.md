---
type: Finding
title: Downloads API is seed-keyed; the reserved seed is unreachable in pool mode
description: GET /downloads resolves through a seed and rejects the reserved __default__ seed in pool mode, so driver-written files there must be asserted via exec, not the API.
tags: [downloads, serve, pool-mode, cuttle-pw]
status: stable
generated: { by: claude-code/fable-5, at: "2026-09-18T14:30:00+00:00" }
sources:
  - id: code
    resource: /packages/cuttle/internal/serve/downloads.go
    title: downloads handlers
  - id: ci
    resource: /.github/workflows/ci.yml
    title: CI smoke step
---

# Finding

`GET /downloads` (what `cuttle downloads` calls) resolves the directory it
serves through a seed, and seed validation rejects the reserved
`__default__` name in pool mode - so files in
`/data/__default__/Downloads/` are invisible to the API there, while in the
default single-session mode that same directory is exactly what the API
serves.[^code]

This matters because `/data/__default__/Downloads` is also the fixed exec
working directory of `cuttle pw`: driver `--filename` outputs land there so
`cuttle downloads` can fetch them in normal operation. The CI smoke job runs
the container in pool mode, so it asserts the driver-written screenshot with
`docker exec cuttle ls` on the path instead of through the API - that is a
deliberate workaround for this keying, not an arbitrary choice.[^ci]

Anyone tempted to "fix" the CI assertion to use the API, or to expose the
reserved seed through the API, should first decide whether pool mode ought
to address the reserved seed at all - today its rejection is by design
(`fingerprint.ValidSeed` excludes the reserved name).

[^code]: downloads handlers
[^ci]: CI smoke step
