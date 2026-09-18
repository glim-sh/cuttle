---
type: Finding
title: playwright-cli go-back leaves the snapshot emitting dead refs
description: In the bundled playwright-cli 0.1.20, after go-back every snapshot ref is from the pre-navigation frame and clicks on it fail; only a fresh goto re-mints working refs.
tags: [playwright-cli, drivers, jev-browse, upstream-bug]
status: stable
stale_after: "2027-03-18T00:00:00+00:00"
generated: { by: claude-code/claude-opus-5, at: "2026-09-18T15:55:00+00:00" }
sources:
  - id: code
    resource: /packages/cuttle/internal/jev/loop.go
    title: jev-browse loop, back action
  - id: live
    resource: "live repro transcript, 2026-09-18: standalone playwright-cli over a CDP-attached session, no cuttle code in the path (session record, no durable link)"
    title: Live repro
  - id: upstream
    resource: "upstream report pending (microsoft/playwright-cli, 2026-09-18)"
    title: Upstream report
---

# Finding

In `@playwright/cli` 0.1.20 - the pinned bundled driver - a `go-back` leaves
the snapshot emitting refs from the pre-navigation frame generation. Every
click on those refs fails with `Ref ... not found in the current page
snapshot`. Taking more snapshots does not recover; only a fresh `goto`
re-mints refs that work.[^live]

It reproduces standalone over a CDP-attached session with no cuttle code in
the path, so it is a driver bug, not a mux artifact.[^live][^upstream]

jev-browse contains it deterministically: after executing a `back` action
the loop re-`goto`s the URL it landed on, so the next snapshot's refs are
live. The workaround is commented against the driver pin and must be
re-evaluated on every pin bump - drop it once the driver fixes the
bug.[^code]

Related: [playwright-cli attach and session model](/findings/playwright-cli-attach-model.md),
[playwright-cli is the driver interface](/decisions/playwright-cli-is-the-driver-interface.md).

[^code]: jev-browse loop, back action
[^live]: Live repro
[^upstream]: Upstream report
