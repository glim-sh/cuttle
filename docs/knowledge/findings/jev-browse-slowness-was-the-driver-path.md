---
type: Finding
title: jev-browse slowness was the driver path, not the model
description: jev-browse ran a multi-page flow at nearly twice plain cuttle pw's time because of how it drove the page - modal-blind offers, dead-click repeats, missing Enter, redundant snapshots and spawns, section-less labels - and reached parity once those were fixed.
tags: [jev-browse, benchmark, performance, snapshot]
status: stable
stale_after: "2027-03-19T00:00:00+00:00"
generated: { by: claude-code/claude-opus-5, at: "2026-09-19T11:30:00+01:00" }
sources:
  - id: bench
    resource: "benchmark harness runs on 2026-09-18/19 against a signed-in job site: the same flow as the orchestrator-effort benchmark, humanize on, jev-browse driven in chunks by a claude -p orchestrator, plus one one-shot scripted run (session record, no durable link)"
    title: jev-browse benchmark runs
  - id: pr
    resource: https://github.com/glim-sh/cuttle/pull/102
    title: "feat(jev): withhold write actions and halve jev-browse run time (branch feat/jev-browse-speed)"
---

# Finding

Before the fixes, an LLM orchestrating `cuttle jev-browse` in chunks took
282-348s on the flow that plain `cuttle pw` did in 158-187s at the same
effort; a one-shot scripted run failed at 1007s with 135 of 138 clicks
failing.[^bench] The model's decision time was not the cause. The root
causes were all in how jev drove the page:[^bench]

1. **Modal-blind offers.** With an in-page modal open, jev still offered
   the elements behind it; every such click timed out after 5000ms.
2. **Dead-click repeats.** It re-picked an autocomplete option whose click
   had just failed, on an unchanged page.
3. **No Enter after typing** into a combobox, so a search never submitted.
4. **Driver cost per step.** About 2 snapshots and 3-8 process spawns per
   step, ~4.3s/step before any humanized action.
5. **Section-less labels.** Offered labels did not name the page section,
   so search values went to the site-wide search box instead of the form's.

Fixes, each aimed at one cause: scope offers to the open modal, exclude a
click that left the page unchanged, offer Enter after typing, take one
snapshot after a navigation, label options with their section and filled
state, keep one persistent driver client per run, and withhold write-shaped
actions behind a deny gate.[^pr] After them: 157s/170s at high effort -
level with plain pw at about half its cost - and 111s/120s at low effort,
against pw's 101s/103s.[^bench]

The modal facts behind fix 1 are in
[aria snapshot: focus and modal shape](/findings/aria-snapshot-focus-and-modal-shape.md);
the per-spawn cost behind fix 4 in
[cost of one cuttle pw call](/findings/pw-call-cost-breakdown.md).

[^bench]: jev-browse benchmark runs
[^pr]: feat(jev): withhold write actions and halve jev-browse run time (branch feat/jev-browse-speed)
