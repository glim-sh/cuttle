---
type: Finding
title: A client-side app transition can update URL and title while the body stays hidden
description: On a signed-in site, a client-side transition from one app to another updates the URL and title while the body stays hidden behind a "Navigating to ..." overlay (innerText ~34 chars); the aria snapshot or a reload already holds the real page, so a done-check or an orchestrator that trusts URL+title or reads only innerText gives up on a page that was there.
tags: [benchmark, spa, snapshot, jev-browse, cuttle-pw, orchestrator]
status: stable
stale_after: "2027-03-19T00:00:00+00:00"
generated: { by: claude-code/claude-opus-5, at: "2026-09-19T14:05:00+01:00" }
sources:
  - id: bench
    resource: "benchmark harness runs on 2026-09-19 (merged main at 9ca9386) against a signed-in site: the same 12-click flow as the orchestrator-effort benchmark, humanize on, run logs of every arm (session record, no durable link)"
    title: Merged-main browse benchmark run logs
---

# Finding

The reference flow's fourth step opens a company's "people" page from the
job-search app. The site does that as a client-side transition between two
apps: the URL and the document title change to the people page at once,
while the body stays hidden behind a full-page "Navigating to ..." overlay,
so `document.body.innerText` is about 34 characters for several seconds. The
real page is nonetheless there: the aria snapshot taken in that state already
held the people content, and a `pw reload` produced it too.[^bench]

Every arm hit the overlay, including the pre-#102 jev-browse build - it is a
property of the site's transition, not a regression in any cuttle change.
Which runs passed depended only on how they reacted:[^bench]

- **Passing runs** recovered by `pw snapshot` (the aria tree was complete) or
  by `pw reload`.
- **The low-effort orchestrator** driving `jev-browse` read `innerText`,
  saw the stub, and gave up: 0/2 on that step, while the same arm at high
  effort went 2/2.

Two traps, then:

1. **For an orchestrator**: a near-empty `innerText` after a navigation is
   not the page. Take a snapshot or reload before concluding the page is
   broken.
2. **For jev-browse**: its done-check trusts URL and title while the DOM can
   still hold the previous page or an overlay. Pre-existing, not introduced
   by the speed work; a fix would confirm on snapshot content, not on
   URL+title.

Where this bit the benchmarks:
[orchestrator effort is the browse-time lever](/findings/orchestrator-effort-is-the-browse-time-lever.md),
[jev-browse slowness was the driver path](/findings/jev-browse-slowness-was-the-driver-path.md).

[^bench]: Merged-main browse benchmark run logs
